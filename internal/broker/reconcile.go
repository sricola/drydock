package broker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"drydock/internal/audit"
	"drydock/internal/provider"
	"drydock/internal/trustbrief"
)

// interruptedResultLine is the synthetic terminal event appended to a task
// trace that a brokerd crash left without a result line. subtype "interrupted"
// (distinct from "error") tells `drydock tasks` the daemon died under the task
// rather than the task itself failing; duration_ms is 0 (death time unknown).
const interruptedResultLine = `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}` + "\n"

// TerminateStuckAudits scans auditRoot for <id>.jsonl traces with no terminal
// result line — tasks that were running when a prior brokerd crashed — and
// appends a synthetic "interrupted" result so `drydock tasks` resolves them
// instead of showing "running?" forever. Idempotent: a trace that already has a
// result line is left untouched. SAFE ONLY AT BOOT, when no task is live.
// Returns the count terminated and the first error (per-file errors are
// non-fatal).
func TerminateStuckAudits(auditRoot string) (int, error) {
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(auditRoot, e.Name())
		has, herr := audit.HasResultLine(path)
		if herr != nil {
			if firstErr == nil {
				firstErr = herr
			}
			continue
		}
		if has {
			continue
		}
		if aerr := appendLine(path, interruptedResultLine); aerr != nil {
			if firstErr == nil {
				firstErr = aerr
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// appendLine appends s to the file at path, refusing symlinks (O_NOFOLLOW so a
// planted <id>.jsonl -> elsewhere can't redirect the boot-time write) and
// fsyncing so the interrupted marker survives an immediate re-crash.
func appendLine(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteString(s); err != nil {
		return err
	}
	return f.Sync()
}

// ResumeAwaiting reconciles awaiting-approval push gates left by a prior
// brokerd life. For each gate marker: if the persisted diff and the stage both
// survived, re-register the task as pending and resume the gate+push
// headlessly; otherwise append an honest interrupted line and drop the marker
// (never leave a false ok). The diff check FAILS SAFE by design: the resumed
// gate recomputes its second-look acknowledgment requirement from the
// persisted diff, and a no-diff task can never legitimately reach the gate
// (it terminates before pushing), so a missing/unreadable/empty <id>.diff
// means the gate cannot be honestly re-posed — resuming it approvable would
// let a bare approve push a branch whose original gate required acks.
// Each marker is handled in its own goroutine so one slow reopen cannot
// block the others, and the function itself returns immediately.
func (b *Broker) ResumeAwaiting(stageRoot string) {
	reopen := b.reopenStage
	if reopen == nil {
		reopen = defaultReopenStage
	}
	for id, m := range ListGateMarkers(b.AuditRoot) {
		id, m := id, m // capture loop vars for goroutine
		auditPath := filepath.Join(b.AuditRoot, id+".jsonl")
		// Read the diff BEFORE reopening the stage: bailing after a reopen
		// would leave a quota-backed stage's image reattached with no owner.
		diff, derr := readDiffNoFollow(filepath.Join(b.AuditRoot, id+".diff"))
		if derr != nil || diff == "" {
			slog.Warn("resume: persisted diff missing or empty, marking interrupted", "task_id", id, "err", derr)
			_ = appendLine(auditPath, interruptedResultLine)
			_ = removeGateMarker(b.AuditRoot, id)
			continue
		}
		st, err := reopen(filepath.Join(stageRoot, id))
		if err != nil {
			slog.Warn("resume: stage gone, marking interrupted", "task_id", id, "err", err)
			_ = appendLine(auditPath, interruptedResultLine)
			_ = removeGateMarker(b.AuditRoot, id)
			continue
		}
		logf, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			slog.Warn("resume: cannot open audit for append", "task_id", id, "err", err)
			continue
		}
		slog.Info("resuming awaiting-approval task", "task_id", id)
		go b.resumePush(id, m, st, diff, logf)
	}
}

// ResumeQueue reconciles the durable queue records left by a prior brokerd
// life. THE invariant: an item that already left `queued` may have spent
// budget, run an agent VM, or pushed — it is NEVER re-dispatched. Only a
// still-`queued` item (provably never started) re-enters the in-memory queue.
// Per state:
//
//   - queued: re-appended to the in-memory queue for the dispatcher, deduped
//     by id, so running ResumeQueue twice can never double-enqueue.
//   - preparing/running/verifying: dead_letter "interrupted by restart",
//     uniformly. TerminateStuckAudits (earlier in boot) already gave any
//     crashed trace an honest `interrupted` terminal; we do NOT try to infer
//     "it actually finished" from a success line on the trace — the agent's
//     own success row proves nothing about the un-run verify/gate/push half,
//     and preparing→completed is deliberately not a legal transition. The
//     crux is: never re-run, always reach a terminal. (Increment B may turn
//     some of these into bounded retries.) Exception: a record whose
//     diff-approval gate marker survived provably reached the gate (its
//     awaiting_review persist raced the crash), so it is handed to
//     ResumeAwaiting by moving it to awaiting_review instead.
//   - awaiting_review: untouched when its gate marker survives —
//     ResumeAwaiting owns the re-drive (call it BEFORE ResumeQueue), and the
//     resumed gate's resolution writes the terminal via finalizeQueuedResume.
//     With no marker the gate can never be re-posed (ResumeAwaiting's
//     fail-safe dropped it and wrote an interrupted audit line), so the item
//     dead-letters honestly.
//   - awaiting_ci: the SAME reasoning, one marker over. The item has already
//     PUSHED, so it is never re-dispatched under any condition; the only
//     question is who finishes it. The <id>.ci.json marker — not the gate
//     marker — owns that re-drive: it is the watch's entire cross-restart
//     state, and StartCIWatch's first pass re-reads it off disk with its
//     ORIGINAL absolute deadline. So a marked item is left exactly as it is.
//     With no surviving marker nothing will ever conclude the watch, so the
//     item must reach an honest terminal here rather than hang forever in a
//     non-terminal state: it becomes dead_letter with "no CI conclusion was
//     observed", never `completed`. The same applies when the watch has since
//     been turned OFF in config — a surviving marker will never be polled, so
//     leaving the item parked would strand it; it is terminated honestly and
//     the orphaned marker is removed.
//   - terminal (completed/ci_failed/dead_letter/cancelled): untouched —
//     history for `drydock queue list`.
//
// After the per-item pass it runs reclaimOrphanCIMarkers, which covers the one
// thing an item-driven sweep structurally cannot see: a CI marker with no queue
// item behind it (a synchronous POST /tasks push). That call runs even when
// there are no queue items at all.
//
// stageRoot is accepted for signature parity with ResumeAwaiting (and for
// future increments that reconcile against the stage); this sweep decides
// purely from the queue records and gate markers. Call StartDispatcher after
// this returns so the dispatcher's first pass sees a reconciled queue.
func (b *Broker) ResumeQueue(stageRoot string) error {
	items, err := listQueueItems(b.AuditRoot)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		// No queue items is NOT nothing to do: a synchronous (POST /tasks) task
		// can leave a CI marker behind and has no queue item at all, so the
		// orphan sweep still has to run. See reclaimOrphanCIMarkers.
		b.reclaimOrphanCIMarkers()
		return nil
	}
	markers := ListGateMarkers(b.AuditRoot)
	b.initQueueChans()
	deadLetter := func(id string) {
		if _, err := b.setQueueState(id, QueueDeadLetter, func(q *QueueItem) {
			q.LastError = "interrupted by restart"
		}); err != nil {
			slog.Warn("queue resume: could not dead-letter interrupted item", "task_id", id, "err", err)
		}
	}
	requeued := 0
	for _, it := range items {
		if b.onQueueResumeItem != nil {
			b.onQueueResumeItem(it.ID)
		}
		switch it.State {
		case QueueQueued:
			b.queueMu.Lock()
			dup := false
			for i := range b.queue {
				if b.queue[i].ID == it.ID {
					dup = true
					break
				}
			}
			if !dup {
				b.queue = append(b.queue, it)
				requeued++
			}
			b.queueMu.Unlock()
		case QueuePreparing, QueueRunning, QueueVerifying:
			if _, marked := markers[it.ID]; marked {
				// Reached the gate; the awaiting_review persist raced the crash.
				// Defer to ResumeAwaiting's re-drive. Falls through to dead_letter
				// only if the transition is invalid (a marker against `preparing`
				// is contradictory evidence; fail toward the honest terminal).
				if _, err := b.setQueueState(it.ID, QueueAwaitingReview, nil); err == nil {
					slog.Info("queue resume: gate-marked item handed to awaiting-review resume", "task_id", it.ID)
					continue
				}
			}
			deadLetter(it.ID)
		case QueueAwaitingReview:
			if _, marked := markers[it.ID]; marked {
				continue // ResumeAwaiting re-drives the gate; its resolution finalizes the record
			}
			deadLetter(it.ID)
		case QueueAwaitingCI:
			// NEVER re-dispatched: this item already pushed. The only question
			// is whether anything will still conclude its watch.
			//
			// The marker is read HERE, per item, and deliberately not from a
			// snapshot taken at entry. ResumeAwaiting (which runs first) hands
			// each surviving gate to its own goroutine, so a resumed
			// auto-approve push can arm and write its marker WHILE this loop is
			// running. A snapshot taken before that write would dead-letter an
			// item whose live watch is polling it — the queue record and the
			// watcher disagreeing about the same task, which is the one state
			// this reconciliation exists to prevent.
			//
			// Two live-state re-reads before anything is terminated, because
			// `items` is a SNAPSHOT taken at entry and both halves of an arming
			// push can land after it:
			//
			//  1. A stale snapshot state. The item may have left awaiting_ci
			//     entirely since the snapshot (a concurrent conclusion), in
			//     which case there is nothing here to terminate.
			//  2. The arm->write gap. recordCIMarker's queue write and marker
			//     write are two separate disk ops; observing an armed item
			//     between them and terminating it would be immediately
			//     overwritten by the marker write, leaving the watcher polling a
			//     dead-lettered item whose every transition is refused.
			//     beginCIArm publishes that gap; we skip it and let the watch
			//     (started right after this sweep) pick the marker up.
			//
			// THE ORDER OF THESE TWO IS LOAD-BEARING and is the state re-read
			// FIRST. An arm that begins after the in-flight check would slip
			// through if the check came first; but an arm that has not yet
			// written awaiting_ci cannot pass the state re-read at all, and one
			// that has written it is inside the bracket the second check sees.
			fresh, ferr := readQueueItem(b.AuditRoot, it.ID)
			if ferr != nil || fresh.State != QueueAwaitingCI {
				continue
			}
			if b.ciArmInFlight(it.ID) {
				continue
			}
			// A read error is treated as "no marker", which fails toward
			// terminating the item honestly rather than leaving it parked on a
			// watch that may not exist.
			_, mErr := readCIMarker(b.AuditRoot, it.ID)
			ciMarked := mErr == nil
			if ciMarked && b.CIWatch {
				continue // the marker survived and the watch is on: StartCIWatch re-watches it
			}
			reason := "ci watch marker did not survive the restart; no CI conclusion was observed"
			if ciMarked {
				// Marker present but the watch is disabled — nothing will ever
				// poll it. Terminate honestly and drop the orphan, which is the
				// one thing that would otherwise accumulate (the marker is
				// deliberately not prunable by age; see cmd/drydock/prune.go).
				reason = "ci watch is disabled; no CI conclusion will be observed for this push"
				if err := removeCIMarker(b.AuditRoot, it.ID); err != nil {
					slog.Warn("queue resume: could not remove an orphaned ci marker", "task_id", it.ID, "err", err)
				}
			}
			b.applyCIObservation(CIObservation{
				TaskID: fresh.ID, RepoRef: fresh.Task.RepoRef, PRNumber: fresh.PRNumber,
				State: CIUnknown, Detail: reason, ObservedAtMs: b.nowMs(),
			})
		default:
			// Terminal — history. (An unknown state also lands here: with no
			// valid outgoing transition it cannot be moved, and it can never
			// dispatch because takeDispatchable only takes QueueQueued.)
		}
	}
	// AFTER the item loop, never before: the loop's awaiting_ci branch reports
	// the accurate reason for each item it terminates and removes that item's
	// marker itself. The sweep then handles what the loop structurally cannot
	// reach — a marker with no queue item at all.
	b.reclaimOrphanCIMarkers()
	if requeued > 0 {
		slog.Info("queue resume: re-enqueued surviving queued items", "count", requeued)
		select {
		case b.queueWake <- struct{}{}:
		default:
		}
	}
	return nil
}

// reclaimOrphanCIMarkers removes CI markers that nothing will ever conclude.
//
// It exists because the item loop above iterates QUEUE ITEMS, and a marker does
// not need one: finishPush writes a marker for a SYNCHRONOUS (POST /tasks) task
// too, and that task has no queue record for the sweep to find it by. `.ci.json`
// is deliberately not prunable by age (see cmd/drydock/prune.go, whose whole
// rationale is that each marker is removed by the component that owns it), so
// without this sweep a marker left by a synchronous push on a daemon whose watch
// is off would sit in the audit dir forever and the prune rationale would simply
// be false.
//
// It only acts when the watch is OFF, and that bound is the point. With the
// watch ON every marker has an owner: the watcher polls it and concludes it, at
// the latest when its absolute deadline expires — including markers for
// synchronous tasks and for tasks whose queue item is already terminal. Deleting
// one here would be cancelling live work. With the watch off nothing will ever
// poll any of them, so every marker on disk is by definition an orphan.
//
// The removal is not silent: each orphan gets an honest recorded observation
// first (CIUnknown — the watch was turned off, so nothing was observed), which
// for a queued task also drives its durable terminal and for a synchronous one
// lands in the audit trace. Never `completed`: a push whose CI was never looked
// at has produced no evidence of anything.
func (b *Broker) reclaimOrphanCIMarkers() {
	if b.CIWatch {
		return
	}
	ms, err := listCIMarkers(b.AuditRoot)
	if err != nil {
		slog.Warn("queue resume: could not scan ci markers for orphans", "err", err)
		return
	}
	for _, m := range ms {
		if !b.applyCIObservation(CIObservation{
			TaskID: m.TaskID, RepoRef: m.RepoRef, Branch: m.Branch,
			PRNumber: m.PRNumber, PRURL: m.PRURL, Attempt: m.Attempt, RetryOf: m.RetryOf,
			State:        CIUnknown,
			Detail:       "ci watch is disabled; no CI conclusion will be observed for this push",
			ObservedAtMs: b.nowMs(),
		}) {
			// The terminal did not reach disk for a retryable reason. Keep the
			// marker so the NEXT boot's sweep retries it; removing it now would
			// leave the item parked in awaiting_ci with nothing that even knows
			// to look at it. Same rule concludeCIWatch applies.
			slog.Warn("queue resume: keeping an orphan ci marker whose terminal write failed", "task_id", m.TaskID)
			continue
		}
		if err := removeCIMarker(b.AuditRoot, m.TaskID); err != nil {
			slog.Warn("queue resume: could not remove an orphaned ci marker", "task_id", m.TaskID, "err", err)
			continue
		}
		slog.Info("queue resume: removed an orphaned ci marker (the watch is off)", "task_id", m.TaskID)
	}
}

// finalizeQueuedResume bridges a resumed gate's resolution back onto the
// durable queue record. A queue-driven task shut down at the diff-approval
// gate stays awaiting_review across the restart (runQueued deliberately
// writes no terminal on gateShutdown); when ResumeAwaiting's resumePush later
// resolves the gate, this maps the resolution onto the queue file exactly as
// runQueued's own terminal mapping would have. No-op for tasks with no queue
// file (the synchronous POST /tasks path — resumePush serves both).
func (b *Broker) finalizeQueuedResume(id, outcome string) {
	if _, err := readQueueItem(b.AuditRoot, id); err != nil {
		return // not a queue-driven task
	}
	// Same rule as runQueued's: a resumed gate that ended in a push under an
	// armed CI watch has already moved the item to awaiting_ci, and the watch
	// owns its terminal. Writing `completed` here would mark it clean with no
	// CI evidence.
	if b.ciOwnsTerminal(id) {
		slog.Info("queue: resumed push is under ci observation; the watch owns this item's terminal",
			"task_id", id)
		return
	}
	to, lastErr := queueTerminal(outcome, false)
	if outcome == "" {
		// A resumed gate with no outcome is the approval-timeout auto-deny
		// (resumePush already wrote the `interrupted` result row); name that
		// instead of queueTerminal's generic pre-audit-abort message.
		to, lastErr = QueueDeadLetter, "interrupted"
	}
	if _, err := b.setQueueState(id, to, func(q *QueueItem) {
		q.LastError = lastErr
	}); err != nil {
		slog.Warn("queue: could not persist resumed terminal state",
			"task_id", id, "state", string(to), "err", err)
	}
}

// readDiffNoFollow reads the persisted review diff, refusing symlinks —
// O_NOFOLLOW parity with persistDiff's write side, so a planted
// <id>.diff -> elsewhere can't feed the resumed gate substituted bytes.
func readDiffNoFollow(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// resumePush re-registers a resumed task as pending and drives the gate+push
// tail headlessly (no live client). Approve pushes the surviving branch;
// deny/timeout write the honest terminal outcome. On shutdown the marker is
// left for the next boot (idempotent).
func (b *Broker) resumePush(id string, m gateMarker, st taskStage, diff string, logf *os.File) {
	// The resumed task owns this O_APPEND audit fd. Flush the terminal result and
	// close it, matching HandleTask's Sync-then-Close for the live path: without
	// this the fd leaks per resumed task and the terminal line is never fsynced.
	defer func() { _ = logf.Sync(); _ = logf.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	// This resume is, by construction, a task that was sitting at the
	// diff-approval gate when brokerd last died (only gate-marked tasks reach
	// resumePush), so register it as StagePending directly: registerTaskAt
	// records it in the same critical section instead of a plain registerTask
	// (always StageRunning) followed by a later setStage correction, which
	// would let a reader observe "running" instead of "pending_approval" in
	// between (the HTTP admin listener is already serving during boot
	// reconciliation). Mirrors pushAndOpenPR's own setStage(StagePending)
	// before its live-path gate wait.
	b.registerTaskAt(id, m.RepoRef, m.Instruction, cancel, StagePending)
	defer b.unregisterTask(id)

	// Bind this resume's context to the reopened stage so a kill/shutdown aborts
	// its git push (the stage was reopened at boot, before this context existed).
	if wc, ok := st.(interface{ WithContext(context.Context) }); ok {
		wc.WithContext(ctx)
	}

	tr := &taskRun{
		b: b, ctx: ctx, sw: newDiscardStream(), id: id,
		repoRef: m.RepoRef, instruction: m.Instruction, platform: m.Platform,
		draft: m.Draft, agentName: m.Agent, st: st, logf: logf,
		auditPath: filepath.Join(b.AuditRoot, id+".jsonl"),
		taskStart: time.UnixMilli(m.TaskStartMs),
		// This task's agent ran in a PREVIOUS brokerd life, so there is no
		// lease here to meter it. recordGlobalUsage reads that as "recover the
		// broker-metered figure from the previous process's own src:"broker"
		// result row" rather than as a measured $0. See globalrecord.go.
		resumed: true,
	}
	tr.subscription = audit.ReadMeta(tr.auditPath).Subscription
	if v, ok := provider.VendorForAgent(m.Agent); ok {
		tr.taskVendor = v
	}
	// THE GLOBAL CEILING'S TERMINAL WRITE for the resume path. resumePush is
	// the ONLY task terminal that does not run through taskRun.runLifecycle
	// (which defers the same call on its FIRST line), so it needs its own, and
	// it is registered here — immediately after tr exists and before any exit
	// from this function — so every path out of the resumed gate records:
	// approve, deny, timeout, kill, push_failed, and the shutdown re-park.
	//
	// This is also what closes globalcap.go's documented residual: a task
	// resumed at the diff gate was admitted in a PREVIOUS process, so this one
	// holds no in-flight claim for it and the ledger entry is the only thing
	// that makes it visible to the ceiling. The shutdown re-park records too,
	// deliberately — the task already ran and its spend is already real, and
	// GlobalLedger.Record is idempotent on task id, so the next boot's re-drive
	// resolves the same task without counting it twice. No-op with no ledger.
	defer tr.recordGlobalUsage()
	// Registered after the Sync/Close defer above, so it runs before them
	// (LIFO): the metrics row lands as the last line, matching the live path.
	defer tr.appendMetrics()
	if c, ok := st.(interface{ Cleanup() error }); ok {
		defer func() {
			if !tr.keepStage {
				_ = c.Cleanup()
			}
		}()
	}

	// Recompute the second-look acknowledgment requirement from the persisted
	// diff before re-entering the gate: the requirement must survive a brokerd
	// restart, or bouncing the daemon while a flagged task sits at the gate
	// would silently downgrade its approve to ack-less (an ack-bypass). Same
	// requiredAcks(Analyze(diff), policy) computation as the live path.
	tr.requiredAcks = requiredAcks(trustbrief.Analyze(diff), b.DiffPolicy)
	b.setSecondLook(id, tr.requiredAcks)

	// Recorded unconditionally, unlike pushAndOpenPR's !tr.autoApprove guard:
	// auto-approved tasks return from gatePushMarked before its onReady ever
	// writes a gate marker, and resumePush only runs for marked tasks, so this
	// wait is always a real human-gate wait.
	gateStart := time.Now()
	ok, cause := b.gatePushMarked(ctx, tr, diff)
	tr.approvalGateWait = time.Since(gateStart)
	if cause == gateShutdown {
		tr.keepStage = true
		return // leave the marker; next boot resumes
	}
	files, insertions, deletions := diffStat(diff)
	tr.diffFiles = files
	tr.diffBytes = int64(len(diff))
	if !ok {
		// See gateOutcome's doc comment for how this maps and where this
		// resumed path intentionally diverges from the live pushAndOpenPR path.
		outcome, subtype := gateOutcome(cause, true)
		tr.outcome = outcome
		// Broker-authored (src:broker) and carrying the metered cost, so a resumed
		// task's real spend still seeds the aggregate ledger.
		fmt.Fprintf(logf,
			`{"type":"result","subtype":%q,"is_error":false,"duration_ms":0,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
			subtype, audit.TotalCost(tr.auditPath))
		b.finalizeQueuedResume(id, tr.outcome)
		return
	}
	tr.finishPush(files, insertions, deletions)
	b.finalizeQueuedResume(id, tr.outcome)
}
