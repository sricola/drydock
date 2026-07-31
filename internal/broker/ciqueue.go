package broker

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"drydock/internal/audit"
)

// This file is the seam between the host-side CI watcher (ciwatch.go) and the
// two places a CI conclusion has to become visible: the durable queue record
// and the task's audit trail. The watcher itself never touches either — it
// observes, and hands one CIObservation here.
//
// THE RULE THE WHOLE FILE SERVES: an unobserved CI outcome must never read as
// success. Three separate spellings of "we don't know" exist (timed_out,
// unknown, and a marker that did not survive a restart) and NONE of them
// reaches `completed`. Only two things do: an observed pass, and an observed
// "this PR has no checks configured" — which is positive evidence about the
// repository, not an absence of evidence about the run.
//
// The second invariant, less obvious but equally load-bearing: a queued item
// whose push armed a CI watch must not race to `completed` on its own. Its
// terminal now belongs to the watch. QueueItem.CIState is the durable flag
// that says so (see ciOwnsTerminal), written in the SAME atomic queue-file
// write that moves the item to awaiting_ci.

// armCIWatch takes ownership of a queued item's terminal on behalf of the CI
// watch, moving it awaiting_review -> awaiting_ci and stamping the PR number
// and an explicit `pending` CI state.
//
// It reports whether the caller may go on to write the <id>.ci.json marker,
// which is what makes the ORDER here matter. Two cases, both deliberate:
//
//   - NO QUEUE ITEM (the synchronous POST /tasks path, and a resumed task that
//     was never queued): there is nothing to arm. Reports true — the watch is
//     still armed, the observation still lands in the audit, and every queue
//     interaction downstream no-ops exactly as finalizeQueuedResume does.
//
//   - THE TRANSITION FAILED for an item that DOES exist (it was cancelled, or
//     the write failed): reports false, and the caller writes no marker. The
//     alternative — a live marker over an item the watch cannot finalize —
//     would let runQueued write `completed` for a task whose CI later failed,
//     which is precisely the "absence of evidence reads as success" failure
//     this increment exists to prevent. Declining the watch is honest: it is
//     the documented "no PR identity => no watch => the task completes exactly
//     as it does today".
//
// Called BEFORE the marker is written, so the invariant "a live CI marker for
// a queued item implies that item is in awaiting_ci" holds at every instant,
// and the watcher can never observe a marker whose item has not been armed. A
// crash in the gap leaves an armed item with no marker, which ResumeQueue
// resolves to an honest terminal rather than a hang.
// beginCIArm marks id as "being armed", and returns the function that clears
// it. recordCIMarker holds this across BOTH of its writes (the queue
// transition and the marker), which is what makes the pair look atomic to
// ResumeQueue's boot sweep. See Broker.ciArming.
func (b *Broker) beginCIArm(id string) func() {
	b.ciArmingMu.Lock()
	if b.ciArming == nil {
		b.ciArming = map[string]bool{}
	}
	b.ciArming[id] = true
	b.ciArmingMu.Unlock()
	return func() {
		b.ciArmingMu.Lock()
		delete(b.ciArming, id)
		b.ciArmingMu.Unlock()
	}
}

// ciArmInFlight reports whether id is inside recordCIMarker's arm->write gap.
func (b *Broker) ciArmInFlight(id string) bool {
	b.ciArmingMu.Lock()
	defer b.ciArmingMu.Unlock()
	return b.ciArming[id]
}

func (b *Broker) armCIWatch(id string, prNumber int) bool {
	if _, err := readQueueItem(b.AuditRoot, id); err != nil {
		return true // not a queue-driven task; nothing to arm
	}
	if _, err := b.setQueueState(id, QueueAwaitingCI, func(q *QueueItem) {
		q.PRNumber = prNumber
		// Observed nothing yet — recorded as pending, never defaulted to a
		// verdict. This is also the flag ciOwnsTerminal reads.
		q.CIState = string(CIPending)
	}); err != nil {
		slog.Warn("ci watch: could not move the queue item to awaiting_ci; this push will not be watched",
			"task_id", id, "pr", prNumber, "err", err)
		return false
	}
	return true
}

// ciOwnsTerminal reports whether the CI watch has taken ownership of this
// item's terminal state, i.e. whether armCIWatch stamped a CIState on it.
//
// runQueued and finalizeQueuedResume both consult this before writing their
// own terminal: a task that pushed under an armed watch is NOT finished when
// its lifecycle returns, and letting queueTerminal's `pushed -> completed`
// mapping run would mark it clean before any CI evidence exists. A task with
// no queue item, no watch, or a watch that was declined reads false and takes
// the unchanged path.
func (b *Broker) ciOwnsTerminal(id string) bool {
	it, err := readQueueItem(b.AuditRoot, id)
	return err == nil && it.CIState != ""
}

// ciQueueTerminal maps a TERMINAL CI observation to the durable queue terminal
// and the LastError text that explains it.
//
// The mapping is the honesty rule made mechanical:
//
//	passed     -> completed   observed success
//	no_checks  -> completed   observed: this PR has no checks configured. Not
//	                          an absence of evidence — it is evidence that
//	                          there is no CI gate to wait for, so the item
//	                          finishes exactly as it would with the watch off.
//	                          The distinction from `passed` survives in
//	                          QueueItem.CIState and the audit record.
//	failed     -> ci_failed    OBSERVED failure — the only path to ci_failed.
//	timed_out  -> dead_letter  the watch window closed with no conclusion
//	unknown    -> dead_letter  repeated read failures, or an unwatchable PR
//	anything   -> dead_letter  fail toward "we don't know", never toward ok
//
// dead_letter is the right home for the last three: it is already the queue's
// "this item did not reach a clean finish" terminal, it is visibly not a
// success in every surface that renders it, and it needs no new state. Note
// what is deliberately absent: no CI state maps to `completed` except the two
// that carry positive evidence, and `ci_failed` is never used to mean "we
// couldn't tell" — conflating those two would make every operator dashboard
// unable to distinguish a broken build from a broken watch.
func ciQueueTerminal(st CIState) (QueueState, string) {
	switch st {
	case CIPassed, CINoChecks:
		return QueueCompleted, ""
	case CIFailed:
		return QueueCIFailed, "ci checks failed on the pull request this task opened"
	case CITimedOut:
		return QueueDeadLetter, "ci watch ended with no observed conclusion (timed out)"
	case CIUnknown:
		return QueueDeadLetter, "ci watch ended with no observed conclusion (gave up)"
	default:
		// An unmodeled state must not be rounded to a verdict in either
		// direction. It is missing evidence, and it says so.
		return QueueDeadLetter, "ci watch ended in an unrecognised state; no conclusion was observed"
	}
}

// applyCIObservation records ONE terminal CI observation on the durable queue
// item and in the task's audit trail.
//
// It MUST be idempotent: concludeCIWatch persists the terminal state, then
// calls here, then removes the marker, so a crash in that last gap replays the
// whole observation on the next boot. The replay is detected on the DURABLE
// QUEUE ITEM and nowhere else — that record is broker-owned and unreachable
// from the task's VM, unlike the audit trace (see appendCIObservationRow's
// note on why the audit tail must never be an idempotency anchor).
//
// It must also no-op cleanly for a SYNCHRONOUS (POST /tasks, non-queued) task,
// which has no queue item at all — the same contract finalizeQueuedResume
// holds, for the same reason: resumePush and finishPush serve both paths.
// It reports whether the observation is FULLY RECORDED — i.e. whether the
// caller may now discard the marker that drives the retry. False means the
// durable queue write failed for a reason that may succeed later (a full disk,
// a read-only mount), so the marker must stay and the next pass must try again;
// see concludeCIWatch. A REFUSED transition is not a failure in that sense: the
// item legitimately moved on, and no amount of retrying will change it.
func (b *Broker) applyCIObservation(obs CIObservation) bool {
	to, lastErr := ciQueueTerminal(obs.State)
	qs, replay, persisted := b.recordCIQueueTerminal(obs, to, lastErr)
	if replay {
		// A crash-window replay. Returning HERE is what makes the bounded retry
		// enqueue-once: the decision below never runs a second time for an
		// observation whose terminal is already on the parent's record. See
		// ciretryloop.go's crash-window analysis.
		//
		// Clear a STALE deferral on the way out. A parent that reaches here with
		// the flag still set is one whose enqueue landed but whose flag-clearing
		// write did not; the decision is over, and leaving the flag would tell an
		// operator the retry is still pending forever. A no-op when already clear.
		b.setCIRetryDeferred(obs.TaskID, false)
		return true
	}
	// The bounded retry (B2, D6), and note the ORDER: the parent's terminal is
	// already durable at this point, so a crash between the two can only lose a
	// child, never duplicate one. Nothing here can change the terminal that was
	// just written.
	retryID, retryDetail, park := b.maybeEnqueueCIRetry(obs, qs)
	if park {
		// THE DECISION HAS NOT BEEN MADE. The global ceiling could not be
		// MEASURED — a fault, not a verdict — and this decision runs once per
		// observation, so recording "no retry" now would destroy the chain over
		// something that usually clears before the next tick. Mark the parent
		// deferred (durable, VM-unwritable) and return NOT-RECORDED so
		// concludeCIWatch keeps the marker and the next pass re-asks. No
		// observation row is written: there is nothing to say yet, and a row per
		// tick would be noise in the operator's trace.
		// Logged on the TRANSITION only: a ceiling that is unmeasurable for a
		// lasting reason (an unreadable ledger file) re-parks on every watch
		// tick, and a line per tick would bury the one that matters.
		if b.setCIRetryDeferred(obs.TaskID, true) {
			slog.Warn("ci retry: deferring the bounded-retry decision; the global ceiling could not be evaluated",
				"task_id", obs.TaskID, "reason", retryDetail)
		}
		return false
	}
	// The decision is made either way now, so the deferral (if any) is over.
	b.setCIRetryDeferred(obs.TaskID, false)
	b.appendCIObservationRow(obs, qs, retryID, retryDetail)
	return persisted
}

// recordCIQueueTerminal persists the queue terminal. It returns the state
// actually persisted ("" when there is no queue item), whether this was a
// replay of an already-applied observation, and whether the durable record now
// reflects the observation (false ONLY for a retryable write failure — a
// refused transition counts as recorded, because the item's real state is
// already terminal and nothing better will ever be written).
func (b *Broker) recordCIQueueTerminal(obs CIObservation, to QueueState, lastErr string) (QueueState, bool, bool) {
	cur, err := readQueueItem(b.AuditRoot, obs.TaskID)
	if err != nil {
		return "", false, true // synchronous task: no queue item, nothing to move
	}
	if cur.State == to && cur.CIState == string(obs.State) {
		// THE FALL-THROUGH IS NARROW, AND THE NARROWNESS IS THE FIX. A parked
		// decision must be re-asked (see below), but a decision that ALREADY
		// ENQUEUED must never be, and "parked" alone does not distinguish the two:
		// the deferral flag and the parent's link are separate best-effort writes,
		// so a crash — or one correlated write failure — between Enqueue and those
		// writes leaves a parent that has a child, still says `CIRetryDeferred`,
		// and has an empty `RetryTaskID`. Falling through on the deferral alone
		// re-ran the decision for that parent and minted a SECOND child, breaking
		// "at most one child per parent, ever" — ciretryloop.go's crash window W3.
		//
		// CIRetryEnqueued is written BEFORE Enqueue, so it covers that window
		// exactly. RetryTaskID is checked too, belt and braces, for items written
		// by a build that predates the flag.
		if !cur.CIRetryDeferred || cur.CIRetryEnqueued || cur.RetryTaskID != "" {
			return to, true, true // already applied; a crash-window replay
		}
		// The terminal is already durable, but the bounded-retry decision was
		// PARKED because the global usage ceiling could not be measured, and
		// nothing has been enqueued for it. That is the one case where a repeat
		// pass must NOT short-circuit: the decision runs once per observation, and
		// short-circuiting it here is what turned a transient fault into a
		// permanently destroyed retry chain. Report it as recorded-and-not-a-replay
		// so the caller re-asks.
		return cur.State, false, true
	}
	// The observation's own Detail (why the watch ended without a conclusion:
	// a give-up count, a lost marker, a disabled watch) is strictly more
	// informative than the mapped category, so it is appended rather than
	// discarded. Sanitized like every operator-reflected string even though it
	// is broker-authored — the cheap habit is what keeps the one day it isn't
	// from mattering.
	if d := safeStr(obs.Detail); d != "" && lastErr != "" {
		lastErr += " — " + d
	} else if d != "" && to != QueueCompleted {
		lastErr = d
	}
	it, err := b.setQueueState(obs.TaskID, to, func(q *QueueItem) {
		q.CIState = string(obs.State)
		q.LastError = lastErr
		if obs.PRNumber > 0 {
			q.PRNumber = obs.PRNumber
		}
	})
	if err != nil {
		// Two very different failures share this branch, and the caller has to
		// tell them apart:
		//
		//  - A REFUSED TRANSITION: the item moved on without us (it was
		//    cancelled, or a concurrent pass already concluded it). Never force
		//    it — the state machine's refusal is the guard working — and never
		//    retry it either, because the item is already terminal. Recorded.
		//  - A WRITE FAILURE: the disk is full, or read-only. Nothing durable
		//    says what the watch observed, and the item is still sitting in
		//    awaiting_ci. This one IS retryable, and must be, or the item is
		//    stranded in a non-terminal state until the next boot notices.
		slog.Warn("ci watch: could not persist the ci terminal on the queue item",
			"task_id", obs.TaskID, "from", string(cur.State), "to", string(to), "err", err)
		return cur.State, false, errors.Is(err, errInvalidTransition)
	}
	return it.State, false, true
}

// appendCIObservationRow appends the broker-authored {"type":"ci_observation"}
// record to the task's already-CLOSED audit file, using the same
// O_APPEND|O_NOFOLLOW|fsync append TerminateStuckAudits uses for its synthetic
// terminal line. See audit.CIObservation for why this is deliberately NOT a
// {"type":"result"} row.
//
// Every string that lands in the record goes through safeStr, matching the
// treatment every other operator-reflected, subprocess-derived string gets. No
// CI log text exists to sanitize (D3): the record carries broker-authored
// state/detail and broker-computed counts, and there is no field a repository
// workflow's output could reach.
//
// THERE IS DELIBERATELY NO AUDIT-TAIL DEDUP HERE. An earlier version skipped
// the append when audit.LastCIObservation already reported the same state — a
// guard anchored on an AGENT-WRITABLE file (the VM's stdout is copied into the
// trace verbatim, and both fields that scan filters on, type and src, are
// attacker-supplied). An agent that printed a forged ci_observation row would
// have SUPPRESSED the broker's own later, genuine one, leaving the forgery as
// the only record in the trace. The idempotency guard belongs on the durable
// queue item, which nothing inside the VM can write, and that is where it now
// lives exclusively (recordCIQueueTerminal's replay check, which returns before
// this function is called). No control decision anywhere may read a
// ci_observation row — a rule B2's retry gate inherits.
//
// The cost is a possible DUPLICATE row for a SYNCHRONOUS (non-queued) task
// replayed through the concludeCIWatch crash window, which has no queue item to
// dedup against. That is the right trade: the audit is a log, two identical
// broker-authored rows are honest, and no reader branches on them.
//
// retryID/retryDetail carry the bounded-retry decision (B2). They are recorded
// as EVIDENCE for an operator following a chain — the authoritative link is
// QueueItem.RetryTaskID and the child's own Task.RetryOf, both broker-owned —
// and, like every other field here, no reader may branch on them.
func (b *Broker) appendCIObservationRow(obs CIObservation, qs QueueState, retryID, retryDetail string) {
	path := filepath.Join(b.AuditRoot, obs.TaskID+".jsonl")
	line, err := json.Marshal(audit.CIObservation{
		Type:         "ci_observation",
		Src:          "broker",
		TaskID:       obs.TaskID,
		State:        string(obs.State),
		PRNumber:     obs.PRNumber,
		PRURL:        safeStr(obs.PRURL),
		QueueState:   string(qs),
		Checks:       obs.Summary.Total,
		Passed:       obs.Summary.Passed,
		Failed:       obs.Summary.Failed,
		Pending:      obs.Summary.Pending,
		Detail:       safeStr(obs.Detail),
		Attempt:      obs.Attempt,
		RetryOf:      safeStr(obs.RetryOf),
		RetryTaskID:  safeStr(retryID),
		RetryDetail:  safeStr(retryDetail),
		ObservedAtMs: obs.ObservedAtMs,
	})
	if err != nil {
		return
	}
	if err := appendPreservingMTime(path, string(line)+"\n"); err != nil {
		if os.IsNotExist(err) {
			// No audit file for this id (a pruned trace, or a test fixture).
			// The queue item still carries the conclusion; nothing is lost
			// that the audit was the sole record of.
			slog.Debug("ci watch: no audit trace to append the ci observation to", "task_id", obs.TaskID)
			return
		}
		slog.Warn("ci watch: could not append the ci observation to the task audit",
			"task_id", obs.TaskID, "err", err)
	}
}

// appendPreservingMTime appends s to path (appendLine's O_APPEND|O_NOFOLLOW|
// fsync write) and then restores the file's previous modification time.
//
// The restore is not cosmetic. Several host-side consumers read a task trace's
// mtime as "when this task happened", and this append happens minutes to hours
// after the task ended, about work that ran on GitHub rather than on this host:
//
//   - seedAggregateFromAudit (cmd/brokerd) uses ModTime BOTH as the rolling
//     spend-window cutoff AND as the timestamp it seeds the aggregate ledger
//     with. A bumped mtime would re-seed a task's spend at observation time,
//     keeping it inside the rolling cap window up to a full watch_timeout
//     longer than the spend actually occurred. Conservative in direction, but
//     it silently mis-states WHEN money was spent, which is the thing the
//     ledger exists to record.
//   - `drydock prune` ages a task by its newest artifact's mtime.
//   - The web UI's history strip orders by the same stamp.
//
// A zero time in os.Chtimes leaves that timestamp unchanged, so only mtime is
// restored and atime is left to the filesystem. A failed restore is logged and
// ignored: the row is written and durable, which is the part that matters.
func appendPreservingMTime(path, s string) error {
	var prev time.Time
	if fi, err := os.Stat(path); err == nil {
		prev = fi.ModTime()
	}
	if err := appendLine(path, s); err != nil {
		return err
	}
	if !prev.IsZero() {
		if err := os.Chtimes(path, time.Time{}, prev); err != nil {
			slog.Debug("ci watch: could not restore the audit trace mtime", "path", path, "err", err)
		}
	}
	return nil
}
