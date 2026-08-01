package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"drydock/internal/audit"
	"drydock/internal/broker"
	"drydock/internal/provider"
)

// BOOT RECONCILIATION for the global usage ceiling (docs/superpowers/plans/
// 2026-07-31-global-ceiling.md, G3 + Task 3).
//
// The durable ledger (internal/broker/globalledger.go) is AUTHORITATIVE; this
// sweep cross-checks it against the audit trail and ADDS what a crash lost.
// The one gap it exists to close is narrow and real: a task's terminal ledger
// write is the LAST thing its lifecycle does (globalrecord.go), so a task
// killed between its audit terminal and that write leaves full evidence in the
// audit and nothing in the ledger. Without this sweep that task's start — and
// its spend — silently stop counting against the ceiling forever.
//
// THE DIRECTION IS FIXED: on any ambiguity this over-counts, never under. That
// governs every decision below, and it is why the sweep only ever ADDS. It
// never removes a ledger entry that has no matching audit trace: a trace can be
// pruned (`drydock prune`), moved, or simply predate the retention an operator
// chose, while the ledger's own record of a task start is the thing the ceiling
// is measured on. Deleting a counted start because its evidence aged out would
// hand an operator a way to reset the ceiling by clearing the audit dir.
//
// ---------------------------------------------------------------------------
// THE RULE THIS FILE IS BUILT AROUND: A TRACE'S EXISTENCE IS BROKER-OBSERVED.
// ITS CONTENT IS NOT.
// ---------------------------------------------------------------------------
//
// <audit_root>/<id>.jsonl is CREATED BY THE BROKER, under a 32-hex task id the
// broker minted, in a directory nothing in a VM can reach. That a file is there
// is therefore a fact the broker established: a task with that id ran. It is
// exact, it cannot be forged from inside a sandbox, and it is the fact the
// task-start limb — the only bound on a subscription lane (G1/G7) — is measured
// on.
//
// EVERYTHING INSIDE THE FILE IS A DIFFERENT MATTER. The agent's stdout is copied
// into that same trace verbatim (broker.go's io.MultiWriter), so every byte past
// the broker-written header lines is attacker-supplied text. Both fields a scan
// could filter on — `type` and `src` — are part of that text. ciqueue.go says so
// in as many words and refuses to build a typed reader over it for exactly this
// reason. A financial control that reads a src:"broker" row out of the tail is
// reading a self-declared string:
//
//   - an agent that prints {"type":"result","src":"broker","total_cost_usd":
//     999999} once, at the START of its run, and then arranges to be killed,
//     leaves that row as the only broker-looking result in the trace. Boot
//     reconciliation used to copy it into the ledger's TRUSTED USD limb, where
//     one task then refused every task start on the install, across all vendors
//     and both auth modes, until it aged out — forever, in total mode. The
//     deflating direction worked identically.
//   - the same forgery in a {"type":"metrics","src":"broker"} row controlled
//     ended_at_ms, and therefore WHERE in the rolling window the entry landed.
//     Future-dated, it parked spend in the window for a year. BACK-dated, the
//     entry fell below the lookback floor and THE START VANISHED — an agent in
//     an unattended retry loop could make every crashed start invisible to the
//     limb that is supposed to bound it.
//
// So the rule here is absolute, and the structural test in
// globalreconcile_test.go pins it: NO TRUSTED DOLLAR FIGURE AND NO TIMESTAMP
// COMES FROM TRACE CONTENT. This sweep does not read the trace tail at all. It
// reads only the two BROKER-WRITTEN HEADER LINES (drydock_meta and
// drydock_task, both emitted before the agent process exists, so their position
// makes them broker-authored) for the lane's identity, and it takes its
// timestamp from filesystem metadata the sandbox cannot touch.
//
// WHAT THAT COSTS, stated plainly rather than hidden: a task killed between its
// audit terminal and its ledger write really did have a broker-metered figure in
// its trace, and we are now declining to believe it. Every reconciled entry is
// therefore recorded with USD 0 and USDTrusted false — "we know a task ran, we
// do not know what it cost" — so the USD limb under-counts by that task's spend.
//
// AND THE BOUND ON THAT UNDER-COUNT IS CONDITIONAL, which an earlier version of
// this comment did not say. The claim is: at most the tasks in flight at the
// crash (max_concurrent_tasks), because the START limb counts every one of them
// exactly — the backstop G7 names. That is true ONLY WHEN global_max_tasks > 0.
//
// On a USD-ONLY install (global_max_tasks: 0) there is no start limb, so nothing
// counts them at all: the dollars are unknown, no limb registers the tasks, and
// the ceiling admits freely. It does not even bound at max_concurrent_tasks,
// because it REPEATS on every crash — ten crash-recovery cycles with four tasks
// in flight is forty invisible starts and $800 of real spend the ceiling never
// sees. applyGlobalCeiling warns about exactly this configuration at boot, and
// docs/THREAT_MODEL.md states it as a residual.
//
// The alternative — degrading the USD limb whenever a reconciled entry is added
// and the task limb is off — was considered and rejected for the reason below:
// degrade is sticky for the daemon's life, so it would refuse every start after
// an ordinary crash, and an unattended install bricked by a routine event is a
// worse failure than a stated blind spot with a one-line config remedy.
//
// WHAT IT DELIBERATELY DOES NOT DO IS DEGRADE THE USD LIMB FOR THIS. Degrade is
// sticky for the daemon's life, so degrading on every reconciled entry would
// mean any crash with a task in flight refuses every subsequent start until an
// operator restarts — an unattended install bricked by an ordinary crash, which
// is a worse failure than a bounded under-count that the start limb already
// covers. It is logged loudly instead, and named as a residual in
// docs/THREAT_MODEL.md. (A ReadDir failure still degrades: there the sweep
// cannot enumerate the STARTS either, and that is the limb with no backstop.)
//
// A future increment can recover the dollars honestly by having the broker
// write its metered figure somewhere the sandbox cannot append to — a sidecar
// under the ledger's own 0700 directory, say — rather than by re-trusting the
// trace.
//
// ---------------------------------------------------------------------------
// THE OTHER TWO THINGS IT DOES BETTER THAN seedAggregateFromAudit.
// ---------------------------------------------------------------------------
//
//  1. IT DOES NOT RETURN SILENTLY ON A ReadDir ERROR, which fails OPEN: the cap
//     is seeded from nothing and admits freely. For a ceiling that is backwards
//     (G2). A read failure here DEGRADES the ledger instead, which globalcap.go
//     turns into a refusal on every enforced limb — "I don't know" means "no".
//
//  2. IT COUNTS A TRACE IT CANNOT READ AT ALL. seedAggregateFromAudit skips it;
//     here the file's existence under a valid task id is already the whole of
//     the start-limb evidence, so an unreadable trace still records a start.
//
// ---------------------------------------------------------------------------
// THE CASES IT HAS TO HANDLE.
// ---------------------------------------------------------------------------
//
//	a task in the audit but not the ledger  -> ADDED (the whole point)
//	a ledger entry with no audit trace      -> LEFT ALONE (see above)
//	a task that was running at the crash    -> ADDED, start counted exactly,
//	                                           dollars recorded as unknown.
//	a task resumed at the diff gate         -> ADDED here, and resumePush's own
//	                                           terminal write later is deduped by
//	                                           task id. This is what converges
//	                                           globalcap.go's documented residual
//	                                           (such a task was admitted in a
//	                                           previous process and holds no
//	                                           in-flight claim in this one).
//	clock movement                          -> the lookback anchor is the EARLIER
//	                                           of now and the ledger's own newest
//	                                           event, so a forward jump cannot
//	                                           push the whole window into the
//	                                           future and make the sweep add
//	                                           nothing. See reconcileFloorMs. The
//	                                           caller passes the jump-corrected
//	                                           instant (b.CeilingNowMs), which is
//	                                           also the upper clamp on every
//	                                           timestamp this sweep stamps.
//	total-mode replay                       -> BOUNDED. See reconcileFloorMs.

const (
	// globalReconcileTotalLookback bounds a TOTAL-mode sweep's walk.
	//
	// Total mode (global_window == 0) has no decay, so a naive sweep would
	// replay the entire audit history on every boot. That is not merely slow:
	// total-mode compaction FOLDS all but the newest entries into a checkpoint,
	// which destroys their task ids, so GlobalLedger.Has answers false for them
	// and every boot would re-add tasks already inside the checkpoint's sums —
	// an over-count that repeats and grows without bound. reconcileFloorMs
	// clamps to the ledger's own oldest addressable entry for exactly that; this
	// constant is the second bound, for a ledger too young to have folded
	// anything (a fresh install, or the ceiling just switched on) where the
	// clamp is 0 and something still has to stop the walk at the top of a
	// multi-year audit dir. A week is far longer than any crash window and far
	// shorter than any plausible history.
	globalReconcileTotalLookback = 7 * 24 * time.Hour

	// globalReconcileMTimeSlack is the margin on the mtime pre-filter. The
	// filter is sound without it (a file last written before the floor cannot
	// hold a task that ended after it), but mtime comes from the filesystem's
	// clock rather than the broker's, and the cost of being generous is reading
	// a few extra traces while the cost of being tight is dropping a real one.
	globalReconcileMTimeSlack = time.Hour
)

// globalReconcileIDRE mirrors the broker's own task-id grammar. A file in the
// audit root whose name is not a task id is not a task trace, and a name is
// never trusted before it is matched — the queuestore idiom.
var globalReconcileIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// reconcileGlobalLedger cross-checks the durable global ledger against the
// audit trail and records everything the ledger is missing. nowMs is the
// caller's clock (the broker's own seam is unexported, and taking it as a
// parameter keeps the sweep deterministic under test).
//
// It is a no-op when the ceiling has no store, which is the stock install.
//
// MUST run before anything can start a task — before the dispatcher and before
// the HTTP listener — or the first admissions of the new process are decided
// against an under-counted ledger. See its call site in main.
func reconcileGlobalLedger(b *broker.Broker, nowMs int64) {
	l := b.GlobalLedger
	if l == nil {
		return
	}
	floor := reconcileFloorMs(l, nowMs)
	mtimeFloor := floor - globalReconcileMTimeSlack.Milliseconds()

	dir, err := os.ReadDir(b.AuditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return // an install that has never run a task has nothing to reconcile
		}
		// FAIL CLOSED (G2). We cannot enumerate the evidence, so we do not know
		// how many starts or how many dollars the ledger is missing — not just
		// the dollars. Degrade marks BOTH limbs as lower bounds, which
		// globalcap.go turns into a refusal for whichever limbs are enforced.
		// An install with the ceiling off is untouched.
		// The reason is PATH-FREE on purpose: it becomes GlobalUsage's degrade
		// reason, which globalcap.go renders into a 402 body for whoever
		// submitted the task. The operator gets the audit root from the log line
		// below; the submitting client does not need the host's layout. An
		// *os.PathError's Error() is the host's absolute layout, so it is
		// stripped rather than interpolated.
		l.Degrade(fmt.Sprintf(
			"the global ledger could not be reconciled at boot: the audit directory could not be read (%s), so recorded usage is a lower bound",
			broker.PathFreeError(err)))
		slog.Warn("global ledger: boot reconciliation could not read the audit directory; the ceiling will refuse enforced limbs",
			"audit_root", b.AuditRoot, "err", err)
		return
	}

	var (
		batch      []broker.GlobalEntry
		unreadable int
	)
	for _, de := range dir {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(de.Name(), ".jsonl")
		if !globalReconcileIDRE.MatchString(id) {
			continue
		}
		// The ledger is authoritative: a task it already knows about is already
		// counted, and Record would suppress the duplicate anyway. Checking here
		// keeps the sweep from opening thousands of traces it has no use for.
		if l.Has(id) {
			continue
		}
		var mtimeMs int64
		if info, ierr := de.Info(); ierr == nil {
			mtimeMs = info.ModTime().UnixMilli()
		}
		if mtimeMs > 0 && mtimeMs < mtimeFloor {
			continue // sound cheap filter; never used as the entry's timestamp
		}

		e, ok, readable := reconcileEntryFromAudit(b, filepath.Join(b.AuditRoot, de.Name()), id, mtimeMs, nowMs)
		if !readable {
			unreadable++
		}
		if !ok {
			continue
		}
		if e.EndedAtMs <= floor {
			continue // outside the lookback: it cannot affect either limb
		}
		batch = append(batch, e)
	}

	// ONE fsync for the whole sweep, not one per entry: Record fsyncs per call
	// (~4ms), which is right for a single task terminal and ruinous for a boot
	// sweep that may recover thousands. RecordBatch has identical per-entry
	// semantics, including the id dedupe.
	if len(batch) > 0 {
		added, rerr := l.RecordBatch(nowMs, batch)
		if rerr != nil {
			slog.Warn("global ledger: boot reconciliation could not durably write every recovered entry; they are counted in memory",
				"added", added, "err", rerr)
		}
		// EVERY recovered entry is usd_untrusted by construction — see this
		// file's header. The count is the size of the USD limb's known blind
		// spot for this boot, and it is logged rather than degraded because the
		// start limb already counts all of them exactly.
		slog.Warn("global ledger: boot reconciliation recovered task starts missing from the ledger; their spend is UNKNOWN and is not counted against global_budget_usd",
			"added", added, "scanned_missing", len(batch), "usd_untrusted", added,
			"note", "a task killed between its audit terminal and its ledger write leaves only agent-writable evidence of what it spent; the task-start limb counts it exactly",
			"path", l.Path())
	}
	if unreadable > 0 {
		// An unreadable trace is NOT a degradation here, and that is a change
		// worth stating. Under the rule in this file's header, reading a trace
		// contributes NOTHING to either limb — the start comes from the file's
		// existence and the dollars are recorded as unknown either way — so a
		// trace we could not open leaves us in exactly the state a trace we
		// could open leaves us in. Degrading on it would refuse every task start
		// for the daemon's life over a permissions glitch on one file, while
		// telling the operator nothing the log line below does not.
		slog.Warn("global ledger: boot reconciliation could not read some audit traces; their starts are still counted, their lane is recorded as unknown",
			"count", unreadable, "audit_root", b.AuditRoot)
	}
}

// reconcileFloorMs is the oldest instant the sweep will look back to.
//
// THE ANCHOR IS THE EARLIER OF the caller's now AND the ledger's own newest
// real event. That mirrors globalledger's pruning anchor, deliberately, because
// the two operations have opposite risks and the same defence: pruning is
// destructive and must not trust a clock that jumped FORWARD, while this sweep
// is purely additive and its failure mode is adding NOTHING. Without the anchor,
// a forward clock jump (an NTP correction, a misconfigured host) would put the
// entire lookback window in the future, the sweep would find nothing to
// reconcile, and a crash-lost terminal would be lost for good. Anchoring to the
// ledger's own history means the sweep always covers at least the span
// compaction has not yet deleted. A BACKWARDS jump needs no defence: it moves
// the floor earlier, which reconciles more — the over-counting direction.
//
// WINDOWED MODE stops at the window: an entry outside it cannot affect either
// limb, so recovering it would be pure cost.
//
// TOTAL MODE has no window, so it takes the LATER of a fixed lookback and the
// ledger's OLDEST ADDRESSABLE ENTRY. The second clamp is the important one and
// it is Task 1's fold horizon showing through: total-mode compaction folds older
// entries into a checkpoint that preserves their sums but destroys their ids,
// so Has answers false for them and replaying anything at or before the fold
// boundary would re-add tasks already counted — on every boot, forever. Bounded
// under-counting of history older than the ledger's own memory is the correct
// trade against an unbounded, repeating over-count; a task that ran before the
// oldest entry the ledger can still name either predates the ledger or is
// already inside the checkpoint's totals.
func reconcileFloorMs(l *broker.GlobalLedger, nowMs int64) int64 {
	anchor := nowMs
	if ev := l.NewestEventMs(); ev > 0 && ev < anchor {
		anchor = ev
	}
	if w := l.WindowMs(); w > 0 {
		return anchor - w
	}
	floor := anchor - globalReconcileTotalLookback.Milliseconds()
	if oldest := l.Usage(nowMs).OldestMs; oldest > floor {
		floor = oldest
	}
	return floor
}

// reconcileEventMs is the instant a reconciled entry is stamped with, and it is
// the ONLY timestamp source this file has.
//
// It is filesystem metadata, CLAMPED AT BOTH ENDS. mtime is not a great answer
// to "when did this task run" — it is when the file was last written — but it
// has the one property the ceiling actually requires: nothing inside a sandbox
// can set it. The agent can cause an append (which moves mtime forward, to
// roughly now), and that is the whole of its influence. It cannot back-date a
// trace to slip a start below the lookback floor, and it cannot future-date one
// to park spend in the window for a year. An `ended_at_ms` read out of the
// trace could do both, and did.
//
// The clamps:
//
//	mtime <= 0 (Info() failed)  -> nowMs. Counting it in-window over-counts,
//	                               which is the direction G3 requires.
//	mtime > nowMs               -> nowMs. A trace dated after the (already
//	                               jump-corrected) clock is a clock artefact or
//	                               a filesystem with its own clock; nothing may
//	                               be dated into the future, which is the same
//	                               invariant GlobalLedger.clampEventMs enforces
//	                               one layer down.
//
// There is no lower clamp toward the floor on purpose: a genuinely old trace
// SHOULD fall out of the lookback, and the caller drops it there.
func reconcileEventMs(mtimeMs, nowMs int64) int64 {
	if mtimeMs <= 0 || mtimeMs > nowMs {
		return nowMs
	}
	return mtimeMs
}

// reconcileEntryFromAudit builds the ledger entry for one audit trace.
//
// It records A START AND NOTHING ELSE THAT ANY LIMB IS MEASURED ON. USD is 0 and
// USDTrusted is false unconditionally, the timestamp comes from filesystem
// metadata, and the only bytes read out of the file are the two broker-written
// header lines — see this file's header for why, and
// TestReconcile_NeverSourcesATrustedValueFromTraceContent for the structural
// pin.
//
// ok is false when there is nothing to record. readable is false when the trace
// could not be opened — in which case ok is still TRUE and the entry is
// returned anyway, because the file's existence under a valid task id is itself
// the evidence, and dropping it would under-count the limb that bounds
// subscription mode.
func reconcileEntryFromAudit(b *broker.Broker, path, id string, mtimeMs, nowMs int64) (e broker.GlobalEntry, ok, readable bool) {
	e = broker.GlobalEntry{
		Kind:      broker.GlobalEntryTask,
		TaskID:    id,
		Src:       broker.GlobalSrcReconcile,
		EndedAtMs: reconcileEventMs(mtimeMs, nowMs),
		// "unknown", never the trace's own outcome: `type` and `src` are
		// attacker-supplied text, so an outcome read from the tail is the
		// agent's claim about how its own task ended. It affects no limb, but it
		// lands in a durable operator-facing artifact and there is no honest
		// version of it here — a task the ledger never heard about is a task
		// whose ending we did not observe.
		Outcome: "unknown",
		// THE DOLLARS ARE UNKNOWN, ALWAYS. Not "recovered from a broker row if
		// one survives" — there is no way to tell a broker row from an agent
		// row inside an agent-writable file. See the file header.
		USD:        0,
		USDTrusted: false,
	}
	// O_NOFOLLOW: a planted <id>.jsonl -> elsewhere must not feed the ceiling
	// substituted numbers, the same rule every other audit read follows.
	f, oerr := audit.OpenRead(path)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			return e, false, true // raced a prune; there is nothing to read
		}
		// The lane is unknown; the start still counts. Metered stays false,
		// which keeps this entry out of the USD limb entirely.
		return e, true, false
	}
	defer f.Close()

	// THE ONLY TWO READS, and both are position-authenticated rather than
	// src-authenticated: drydock_meta is the first line of the trace and
	// drydock_task is within the first four, and BOTH are written by the broker
	// before the agent process exists. Nothing the agent appends can reach them,
	// because an append can only add lines at the end.
	meta := audit.ReadMetaFile(f)
	agent := audit.TaskAgentFile(f)
	if agent == "" {
		agent = b.DefaultAgent
	}
	vendor, _ := provider.VendorForAgent(agent)

	e.Agent = agent
	e.Vendor = vendor
	e.Auth = "api_key"
	if meta.Subscription {
		e.Auth = "subscription"
	}
	// Same signal the live terminal path and writeBrief use, plus the trace's
	// own recorded auth mode: a subscription run meters at $0 by construction
	// however the CURRENT config is written, and this trace may predate a
	// config change. It is informational on a reconciled entry — USDTrusted is
	// false either way, so this steers no limb.
	e.Metered = vendor != "" && !b.UnmeteredVendors[vendor] && !meta.Subscription
	return e, true, true
}
