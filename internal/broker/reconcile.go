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
		return nil
	}
	markers := ListGateMarkers(b.AuditRoot)
	// The CI markers are a SEPARATE set from the gate markers and answer a
	// different question: not "did this item reach the approval gate" but "is
	// there a live watch that will still conclude it". A read error is treated
	// as "no markers", which fails toward terminating awaiting_ci items rather
	// than leaving them parked on a watch that may not exist.
	ciMarked := map[string]bool{}
	if ms, err := listCIMarkers(b.AuditRoot); err != nil {
		slog.Warn("queue resume: could not scan ci markers; awaiting_ci items will be terminated honestly", "err", err)
	} else {
		for _, m := range ms {
			ciMarked[m.TaskID] = true
		}
	}
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
			if ciMarked[it.ID] && b.CIWatch {
				continue // the marker survived and the watch is on: StartCIWatch re-watches it
			}
			reason := "ci watch marker did not survive the restart; no CI conclusion was observed"
			if ciMarked[it.ID] {
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
				TaskID: it.ID, RepoRef: it.Task.RepoRef, PRNumber: it.PRNumber,
				State: CIUnknown, Detail: reason, ObservedAtMs: b.nowMs(),
			})
		default:
			// Terminal — history. (An unknown state also lands here: with no
			// valid outgoing transition it cannot be moved, and it can never
			// dispatch because takeDispatchable only takes QueueQueued.)
		}
	}
	if requeued > 0 {
		slog.Info("queue resume: re-enqueued surviving queued items", "count", requeued)
		select {
		case b.queueWake <- struct{}{}:
		default:
		}
	}
	return nil
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
	}
	tr.subscription = audit.ReadMeta(tr.auditPath).Subscription
	if v, ok := provider.VendorForAgent(m.Agent); ok {
		tr.taskVendor = v
	}
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
