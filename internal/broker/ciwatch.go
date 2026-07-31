package broker

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"drydock/internal/remote"
	"drydock/internal/repokey"
	"drydock/internal/stage"
)

// This file is the host-side CI watcher (Increment B, D3/D4).
//
// It is a single bounded goroutine that polls the durable <id>.ci.json markers
// finishPush wrote and records one terminal, BROKER-OBSERVED conclusion per
// watched PR. Its shape is deliberately the dispatcher's (queue.go
// StartDispatcher/dispatchLoop/StopDispatcher): the same lazy channel init, the
// same "one immediate pass then tick" loop, the same stop semantics, and the
// same b.now/nowMs clock seam so tests are deterministic.
//
// Two invariants are load-bearing:
//
//   - D4: THE WATCH NEVER ACQUIRES A CONCURRENCY SLOT AND NEVER KEEPS A STAGE.
//     MaxConcurrent defaults to 2, and a poll can wait on GitHub for minutes to
//     hours; holding a slot would starve every task on the box. Nothing below
//     calls acquireSlot, registerTask, reopenStage, or touches the boot
//     keep-map. Because a retry re-clones (D2) there is nothing to keep alive:
//     the parent's stage tore down normally at push.
//
//   - D3: CI STATUS DRIVES CONTROL FLOW, CI LOG TEXT NEVER DOES. The only fact
//     that moves a marker is remote.Checks' rollup over check conclusions. No
//     log is fetched, parsed, stored, or forwarded anywhere on this path.
//
// And the rule both of them serve: ABSENCE OF EVIDENCE NEVER READS AS PASSING.
// A timeout, a run of gh errors, an unwatchable repo ref, and a PR with no
// checks each get their own honest recorded state; none of them is CIPassed.

const (
	// defaultCIPollInterval is the watch tick when Broker.CIPollInterval is
	// unset. Deliberately slow: this spends the operator's GitHub rate limit
	// on a timer (THREAT_MODEL N5), and CI runs in minutes, not milliseconds.
	defaultCIPollInterval = 60 * time.Second
	// defaultCIWatchTimeout bounds a single watch when Broker.CIWatchTimeout is
	// unset. Long enough for a slow matrix build, short enough that a PR whose
	// checks never start cannot hold a marker forever.
	defaultCIWatchTimeout = 90 * time.Minute
	// ciNoChecksMinPolls and ciNoChecksMinGrace together set the DISPATCH FLOOR:
	// the earliest a `no_checks` rollup may be believed, measured from the
	// marker's persisted CreatedAtMs (push time). Below the floor an empty
	// rollup is recorded as CIPending and the watch keeps going.
	//
	// WHY THIS EXISTS. GitHub Actions dispatch is ASYNCHRONOUS: a push's checks
	// appear on the PR seconds — sometimes tens of seconds — after the head
	// commit lands. The watcher ticks on a free-running interval, so its first
	// poll lands wherever it lands, and without a floor a poll that fell in the
	// dispatch gap would read an empty rollup, conclude `no_checks`, map that to
	// `completed`, delete the marker and end the watch. Nothing would ever
	// revisit it. Deterministic variants of the same window: a fork PR whose
	// workflows wait for maintainer approval, and CI from an external app that
	// posts its status later. `no_checks` is the ONE terminal state derived from
	// seeing nothing, so it is the one state that must be a function of time —
	// everything else in this file already fails toward "keep watching".
	//
	// WHY BOTH TERMS. The poll-count term (ciNoChecksMinPolls) is derived from
	// the operator's own ci.poll_interval so an operator who polls slowly gets
	// the same "more than one independent look" guarantee an operator who polls
	// fast does; the wall-clock term (ciNoChecksMinGrace) is the floor under
	// THAT, so a very fast poll interval — a test's, or an impatient operator's
	// 5s — cannot shrink the dispatch window to nothing. The floor is
	// max(polls*interval, grace), anchored to CreatedAtMs so it is ABSOLUTE: a
	// crash loop that restarts the watch cannot dodge it, exactly as the
	// deadline cannot be extended by one.
	//
	// Two polls, five minutes: a dispatch that has not produced a single check
	// five minutes and two independent looks after the push is a repository with
	// no CI on this branch, not a slow queue. The watch deadline still bounds
	// everything above this (a floor longer than the deadline simply yields an
	// honest timed_out instead).
	ciNoChecksMinPolls = 2
	ciNoChecksMinGrace = 5 * time.Minute
	// ciWatchMaxConsecutiveErrors is how many back-to-back check-read failures
	// a watch tolerates before giving up. Transient gh failures (network blip,
	// a 5xx, a rate-limit pause) must not end a watch; a persistent one must
	// not hold a marker until the deadline either. The counter resets on ANY
	// successful read, so a flaky link never accumulates its way to a give-up.
	ciWatchMaxConsecutiveErrors = 5
)

// The watcher's own terminal CIState values. cimarker.go owns the four states
// finishPush and the rollup produce (pending/passed/failed/no_checks); these
// two name the ways a watch can end WITHOUT a conclusion, and exist here so
// Task 1's file needs no edit. Neither is a pass, and no consumer may read
// either one as one.
const (
	// CITimedOut: the absolute watch deadline expired with no terminal
	// conclusion observed. Says nothing about whether CI would have passed.
	CITimedOut CIState = "timed_out"
	// CIUnknown: the watch gave up — repeated check-read failures, or a marker
	// whose repo ref cannot be watched at all. Missing evidence, recorded
	// honestly rather than rounded to a verdict.
	CIUnknown CIState = "unknown"
)

// CIObservation is ONE terminal, broker-observed CI conclusion for a watched
// PR, delivered to Broker.OnCIObserved. It carries the marker's identity (so a
// consumer needs no second read of a file that is about to be deleted), the
// observed state, and the conclusion counts.
//
// It carries NO CI log text, by construction: Summary is remote.CheckSummary,
// which has no log field, and Detail is broker-authored English describing why
// the watch ended — never anything a repo's workflow printed.
type CIObservation struct {
	TaskID   string
	RepoRef  string
	Branch   string
	PRNumber int
	PRURL    string
	// Attempt/RetryOf carry the bounded-retry chain forward for B2.
	Attempt int
	RetryOf string
	// State is terminal: passed, failed, no_checks, timed_out, or unknown.
	State CIState
	// Summary is the conclusion rollup when one was obtained; the zero value
	// (Rollup "") when the watch ended without one. The zero value is NOT a
	// verdict — read State, never Summary.Rollup, to decide anything.
	Summary remote.CheckSummary
	// Detail is a short, broker-authored reason for a non-conclusive end.
	Detail string
	// ObservedAtMs is the broker clock (b.nowMs) at the moment of record.
	ObservedAtMs int64
}

// initCIChans lazily builds the watcher's stop channel (via b.ciOnce) so a
// Broker built by struct literal — every existing caller and test — needs no
// constructor. Mirrors initQueueChans.
func (b *Broker) initCIChans() {
	b.ciOnce.Do(func() {
		b.ciStop = make(chan struct{})
		b.ciDone = make(chan struct{})
	})
}

// StartCIWatch launches the watch goroutine. Call once at boot, AFTER
// ResumeQueue/StartDispatcher, and only when the watch is enabled — there is no
// separate resume entry point because the loop's first pass reads the markers
// off disk, so a marker that survived a restart is picked up immediately.
func (b *Broker) StartCIWatch() {
	b.initCIChans()
	go func() {
		defer close(b.ciDone)
		b.ciWatchLoop()
	}()
}

// StopCIWatch ends the watch goroutine. It does NOT wait: a poll already in
// flight is bounded by the vendor-CLI timeout, and blocking shutdown behind a
// GitHub round trip would be the same starvation D4 exists to prevent. The
// in-flight marker simply stays on disk and is re-watched at the next boot with
// the same absolute deadline. Callers that need to observe the exit (tests) can
// wait on b.ciDone.
//
// Idempotent, and safe to call whether or not StartCIWatch ever ran: brokerd
// calls it from the signal handler on every shutdown, tests call it from a
// defer, and closing an already-closed channel would panic.
func (b *Broker) StopCIWatch() {
	b.initCIChans()
	b.ciStopOnce.Do(func() { close(b.ciStop) })
}

func (b *Broker) ciWatchLoop() {
	tick := b.CIPollInterval
	if tick <= 0 {
		tick = defaultCIPollInterval
	}
	tk := time.NewTicker(tick)
	defer tk.Stop()
	// consecutive check-read failures per task id. Owned exclusively by this
	// goroutine — never shared, so it needs no lock and can never race.
	errRuns := map[string]int{}
	for {
		b.ciWatchPass(errRuns)
		select {
		case <-b.ciStop:
			return
		case <-tk.C:
		}
	}
}

// ciWatchPass polls every live marker once. Markers are processed
// oldest-marker-first (listCIMarkers' order) and independently: an unwatchable
// or failing marker can never strand another one.
//
// The scan is deliberately tolerant of the ONE crash window finishPush can
// leave: a marker whose task has no terminal result row in the audit (SIGKILL
// between the two writes). Nothing here reads the audit — a marker is
// self-describing — so such a marker is watched and concluded normally.
// finishPush nonetheless emits its terminal event BEFORE writing the marker, so
// the state that actually survives a kill is the other one: a completed task
// with no watch, which is exactly the documented "no PR identity => no watch".
func (b *Broker) ciWatchPass(errRuns map[string]int) {
	markers, err := listCIMarkers(b.AuditRoot)
	if err != nil {
		slog.Warn("ci watch: could not scan markers", "err", err)
		return
	}
	live := make(map[string]bool, len(markers))
	for _, m := range markers {
		live[m.TaskID] = true
		// Stop promptly on shutdown rather than working through a long list.
		select {
		case <-b.ciStop:
			return
		default:
		}
		b.pollCIMarker(m, errRuns)
	}
	// Forget error runs for markers that no longer exist, so the map cannot
	// grow without bound on a long-lived daemon.
	for id := range errRuns {
		if !live[id] {
			delete(errRuns, id)
		}
	}
}

// pollCIMarker advances one marker by one poll. Exactly one of these happens:
// the watch concludes (terminal state recorded, hook fired, marker deleted),
// the marker is refreshed as still-pending, or a tolerated error is counted.
func (b *Broker) pollCIMarker(m ciMarker, errRuns map[string]int) {
	// A marker whose recorded state is ALREADY terminal is the crash window
	// between "record the conclusion" and "remove the marker". Re-report the
	// persisted conclusion and clean up; never re-decide an already-decided
	// watch, and never spend a gh call on it.
	if ciTerminal(m.State) {
		b.concludeCIWatch(m, m.State, "", remote.CheckSummary{}, errRuns)
		return
	}

	// The deadline is absolute and anchored to persisted data, so a restart —
	// or a crash loop — can never extend a watch.
	if now := b.nowMs(); now >= b.ciDeadline(m) {
		b.concludeCIWatch(m, CITimedOut, "watch deadline expired with no terminal CI conclusion",
			remote.CheckSummary{}, errRuns)
		return
	}

	// The HOST gate reads the task's repo ref; the OWNER/REPO the watch queries
	// come from the marker's validated PR fields. Both halves matter:
	//
	//   - remote.Checks pins github.com inside the --repo value (the GH_HOST
	//     defense), so a marker whose task ref names any other host is not
	//     watchable at all. Reading the host off the task ref is sound because
	//     the capture already refused any PR URL whose host differed from it:
	//     "the task ref is on github.com" therefore implies "the PR is too".
	//   - The PR's OWN owner/repo is where the checks live, and it legitimately
	//     differs from the task's repo when the PR was opened from a fork
	//     against the upstream. So the query target is PROwner/PRRepo — the
	//     values validated at capture time — never a re-parse of PRURL and
	//     never the task ref's own owner/repo.
	if !ciRefHostWatchable(m.RepoRef) {
		// Belt and braces. recordCIMarker refuses to write a marker for an
		// unwatchable ref at all (a push on such a ref completes unwatched
		// instead), so reaching here means a marker written by an older build,
		// or one planted by hand. Conclude now with missing evidence rather
		// than failing every poll until the deadline.
		b.concludeCIWatch(m, CIUnknown, "repo_ref is not a github.com owner/repo reference; this PR cannot be watched",
			remote.CheckSummary{}, errRuns)
		return
	}
	owner, repo := m.PROwner, m.PRRepo
	if !remote.ValidOwnerRepo(owner) || !remote.ValidOwnerRepo(repo) {
		// A marker with no validated PR coordinate cannot be turned into a gh
		// argv, and guessing one from PRURL is exactly what the explicit fields
		// exist to prevent. Fail closed, without spending a gh call.
		b.concludeCIWatch(m, CIUnknown, "marker carries no validated PR owner/repo; this PR cannot be watched",
			remote.CheckSummary{}, errRuns)
		return
	}

	sum, err := b.runChecks(b.ciEnv(), owner, repo, m.PRNumber)
	if err != nil {
		n := errRuns[m.TaskID] + 1
		errRuns[m.TaskID] = n
		slog.Warn("ci watch: reading check conclusions failed",
			"task_id", m.TaskID, "pr", m.PRNumber, "consecutive_errors", n, "err", safeErr(err))
		if n >= ciWatchMaxConsecutiveErrors {
			// Give up — but NEVER as a pass, and never silently. The recorded
			// state says exactly what happened: nothing was observed.
			b.concludeCIWatch(m, CIUnknown,
				fmt.Sprintf("gave up after %d consecutive check-read failures", n),
				remote.CheckSummary{}, errRuns)
		}
		return
	}
	// Any successful read clears the run: the tolerance is for CONSECUTIVE
	// failures, so an intermittently flaky gh never accumulates to a give-up.
	delete(errRuns, m.TaskID)

	st := ciStateFor(sum.Rollup)
	// The dispatch floor: "this PR has no checks" is the only terminal derived
	// from seeing NOTHING, so it is not believed until CI has had time to
	// dispatch. Below the floor it is demoted to pending and the watch goes on;
	// above it, it is a legitimate observation. See ciNoChecksMinGrace.
	//
	// It covers BOTH spellings of the rollup — an empty check list and a list
	// where nothing passed — deliberately: an early all-skipped read is the same
	// partial view of a dispatch still in progress (a second workflow whose
	// filters do match can still land), and the extra wait costs a repository
	// with genuinely no CI a few minutes of `awaiting_ci` and nothing else.
	if st == CINoChecks && b.nowMs() < b.ciNoChecksFloor(m) {
		slog.Debug("ci watch: empty check rollup before the dispatch floor; still pending",
			"task_id", m.TaskID, "pr", m.PRNumber)
		b.refreshCIMarker(m, CIPending)
		return
	}
	if !ciTerminal(st) {
		b.refreshCIMarker(m, st)
		return
	}
	b.concludeCIWatch(m, st, "", sum, errRuns)
}

// ciNoChecksFloor is the ABSOLUTE earliest time, in unix millis, at which an
// empty check rollup for this marker may be concluded as no_checks. Anchored to
// the marker's persisted CreatedAtMs (stamped at push time) for the same reason
// ciDeadline is: a restart, or a crash loop, must not be able to move it.
func (b *Broker) ciNoChecksFloor(m ciMarker) int64 {
	tick := b.CIPollInterval
	if tick <= 0 {
		tick = defaultCIPollInterval
	}
	floor := time.Duration(ciNoChecksMinPolls) * tick
	if floor < ciNoChecksMinGrace {
		floor = ciNoChecksMinGrace
	}
	return m.CreatedAtMs + floor.Milliseconds()
}

// refreshCIMarker persists a still-pending observation: the state, the update
// stamp, and the now-materialized absolute deadline (so the bound is on disk
// even if the operator later changes ci.watch_timeout). Atomic write, reusing
// Task 1's writer. A write failure is logged and the watch continues — the
// marker on disk is still valid, just staler.
func (b *Broker) refreshCIMarker(m ciMarker, st CIState) {
	m.State = st
	m.UpdatedAtMs = b.nowMs()
	m.DeadlineMs = b.ciDeadline(m)
	if err := writeCIMarker(b.AuditRoot, m); err != nil {
		slog.Warn("ci watch: could not refresh marker", "task_id", m.TaskID, "err", err)
	}
}

// concludeCIWatch ends one watch. Order is chosen for crash safety:
//
//  1. Persist the terminal state to the marker, so a crash before the hook
//     runs leaves the conclusion on disk and the next pass re-reports it
//     (rather than losing the observation or re-querying GitHub).
//  2. Fire OnCIObserved.
//  3. Delete the marker.
//
// The cost of that order is that a crash between 2 and 3 delivers the same
// observation twice, so OnCIObserved MUST be idempotent. That is the safe
// direction: Task 3's consumer is a forward-only queue transition, which
// rejects the duplicate, whereas losing the observation would strand a task.
func (b *Broker) concludeCIWatch(m ciMarker, st CIState, detail string, sum remote.CheckSummary, errRuns map[string]int) {
	delete(errRuns, m.TaskID)
	now := b.nowMs()
	if m.State != st {
		m.State = st
		m.UpdatedAtMs = now
		m.DeadlineMs = b.ciDeadline(m)
		if err := writeCIMarker(b.AuditRoot, m); err != nil {
			slog.Warn("ci watch: could not persist the terminal ci state",
				"task_id", m.TaskID, "state", string(st), "err", err)
		}
	}
	obs := CIObservation{
		TaskID:       m.TaskID,
		RepoRef:      m.RepoRef,
		Branch:       m.Branch,
		PRNumber:     m.PRNumber,
		PRURL:        m.PRURL,
		Attempt:      m.Attempt,
		RetryOf:      m.RetryOf,
		State:        st,
		Summary:      sum,
		Detail:       detail,
		ObservedAtMs: now,
	}
	// Surface it (ciqueue.go): the durable queue terminal plus the audit
	// record. Idempotent, and a clean no-op for a synchronous task that has no
	// queue item. This runs unconditionally — it is the observation's whole
	// point — whereas OnCIObserved stays the optional extension seam (B2's
	// bounded retry hangs off it).
	b.applyCIObservation(obs)
	if b.OnCIObserved != nil {
		b.OnCIObserved(obs)
	}
	if err := removeCIMarker(b.AuditRoot, m.TaskID); err != nil {
		slog.Warn("ci watch: could not remove a concluded marker",
			"task_id", m.TaskID, "err", err)
	}
	slog.Info("ci watch: observed", "task_id", m.TaskID, "pr", m.PRNumber,
		"state", string(st), "checks", sum.Total, "failed", sum.Failed)
}

// ciDeadline is the watch's ABSOLUTE end, in unix millis. It is the marker's
// own persisted DeadlineMs when set, and otherwise CreatedAtMs (also persisted,
// stamped at push time) plus the configured timeout. Both anchors survive a
// restart unchanged, which is the point: a task whose daemon crash-loops cannot
// keep resetting its watch window.
func (b *Broker) ciDeadline(m ciMarker) int64 {
	if m.DeadlineMs > 0 {
		return m.DeadlineMs
	}
	to := b.CIWatchTimeout
	if to <= 0 {
		to = defaultCIWatchTimeout
	}
	return m.CreatedAtMs + to.Milliseconds()
}

// ciStateFor maps a check rollup to the marker state. Note the default: an
// unmodeled rollup is NOT a pass and NOT a terminal — it keeps the watch alive
// until the deadline, which then records an honest timeout.
//
// The CINoChecks it returns is a rollup FACT, not yet a conclusion: pollCIMarker
// additionally holds it against the dispatch floor before letting it end a
// watch. Nothing else may treat this function's output as terminal-by-itself.
func ciStateFor(r remote.CheckRollup) CIState {
	switch r {
	case remote.RollupPassed:
		return CIPassed
	case remote.RollupFailed:
		return CIFailed
	case remote.RollupNoChecks:
		return CINoChecks
	default: // RollupPending, "" (a zero summary), anything unrecognized
		return CIPending
	}
}

// ciTerminal reports whether a state ends the watch. CIPending is the only
// non-terminal state; everything else — including the two "no verdict" states —
// concludes it.
func ciTerminal(s CIState) bool {
	switch s {
	case CIPassed, CIFailed, CINoChecks, CITimedOut, CIUnknown:
		return true
	default:
		return false
	}
}

// ciRefHostWatchable reports whether a task's repo_ref names a github.com
// repository, across every spelling repokey.Normalize canonicalizes (https,
// ssh://, scp-style git@host:owner/repo, with or without .git). The host must
// be github.com: remote.Checks hard-pins github.com inside its --repo value as
// the GH_HOST defense, so a marker naming any other host is honestly
// unwatchable rather than quietly retargeted.
//
// It answers the HOST question only. The owner/repo the watch actually queries
// come from the marker's validated PROwner/PRRepo, which for a fork-opened PR
// are a different repository on the same host (see pollCIMarker).
//
// THE PRIMARY CALLER IS recordCIMarker, not this file: an unwatchable ref must
// be refused BEFORE a watch is armed, or an enterprise-hosted push would arm,
// then dead-letter on its first poll. See recordCIMarker for that reasoning and
// for why GHE support is a deliberate future increment rather than a relaxation
// of this predicate.
func ciRefHostWatchable(ref string) bool {
	parts := strings.Split(repokey.Normalize(ref), "/")
	if len(parts) != 3 {
		return false
	}
	if parts[0] != "github.com" { // Normalize already lowercased the host
		return false
	}
	return parts[1] != "" && parts[2] != ""
}

// runChecks is the check-read seam: b.checksFn when a test injected one,
// remote.Checks otherwise. Tests never shell out to gh.
func (b *Broker) runChecks(env []string, owner, repo string, number int) (remote.CheckSummary, error) {
	if b.checksFn != nil {
		return b.checksFn(env, owner, repo, number)
	}
	return remote.Checks(env, owner, repo, number)
}

// ciEnv is the environment every gh call in this file runs with: the CURATED
// adapter env, never os.Environ(). Same rule as every other host-side vendor
// CLI invocation — the broker's own environment (credentials, lease material,
// operator secrets) must not leak into a subprocess.
func (b *Broker) ciEnv() []string {
	if b.ciEnvFn != nil {
		return b.ciEnvFn()
	}
	return stage.CuratedEnv()
}
