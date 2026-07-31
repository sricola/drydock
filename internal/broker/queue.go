package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"drydock/internal/egress"
	"drydock/internal/provider"
)

// This file is the in-memory queue + dispatcher for orchestration increment A.
// The durable backing store (QueueItem, QueueState, the *.queue.json files) is
// queuestore.go; the lifecycle a dispatched item runs is taskRun.runLifecycle
// (broker.go) — the SAME code the synchronous POST /tasks path runs, driven
// headlessly here with a discard stream, mirroring resumePush (reconcile.go).
//
// Concurrency invariants:
//   - MaxConcurrent is enforced by the SAME slot semaphore HandleTask uses
//     (acquireSlot/releaseSlot): the dispatcher only dispatches after a
//     non-blocking acquireSlot succeeds, so queued and synchronous tasks
//     share one cap and the dispatcher can never over-commit.
//   - An item is dispatched at most once: takeDispatchable transitions it
//     queued->preparing AND removes it from the in-memory queue in one
//     queueMu critical section, so a second pass (or a racing cancel) can
//     never see it as dispatchable again.
//   - Every queue-item state change goes through setQueueState, which is
//     guarded by QueueState.CanTransitionTo — the state file is never
//     blind-written.

// defaultQueueTick is the dispatcher poll interval when Broker.queueTick is
// unset. The wake channel makes Enqueue latency independent of it; the tick
// only re-checks items parked on spend caps or waiting for a free slot.
const defaultQueueTick = 2 * time.Second

// nowMs is the queue's clock: the b.now seam when set (deterministic tests),
// wall-clock milliseconds otherwise.
func (b *Broker) nowMs() int64 {
	if b.now != nil {
		return b.now()
	}
	return time.Now().UnixMilli()
}

// initQueueChans lazily builds the dispatcher channels (via b.queueOnce) so a
// Broker built by struct literal — every existing caller and test — needs no
// constructor. Safe to call from any path that touches the queue.
func (b *Broker) initQueueChans() {
	b.queueOnce.Do(func() {
		b.queueWake = make(chan struct{}, 1)
		b.queueStop = make(chan struct{})
	})
}

// Enqueue validates t exactly as POST /tasks' accept path does (repo_ref
// shape, egress domain validity), durably persists it as a queued item, adds
// it to the in-memory FIFO, and nudges the dispatcher. Returns the minted
// task id. Nothing is persisted when validation fails.
func (b *Broker) Enqueue(t Task) (string, error) {
	if !gitURLRef.MatchString(t.RepoRef) {
		return "", fmt.Errorf("repo_ref must be an https/git/ssh URL (no local paths)")
	}
	if len(t.EgressExtra) > 0 {
		if err := egress.ValidateDomains(t.EgressExtra); err != nil {
			return "", fmt.Errorf("egress_extra invalid: %w", err)
		}
	}
	id := newID()
	now := b.nowMs()
	it := QueueItem{ID: id, Task: t, State: QueueQueued, EnqueuedAtMs: now, UpdatedAtMs: now}
	if err := writeQueueItem(b.AuditRoot, it); err != nil {
		return "", err
	}
	b.initQueueChans()
	b.queueMu.Lock()
	b.queue = append(b.queue, it)
	b.queueMu.Unlock()
	select {
	case b.queueWake <- struct{}{}:
	default: // a wakeup is already pending; the dispatcher will see this item
	}
	return id, nil
}

// StartDispatcher launches the dispatcher goroutine. Call once at boot;
// StopDispatcher ends it (used by tests; brokerd just exits).
func (b *Broker) StartDispatcher() {
	b.initQueueChans()
	go b.dispatchLoop()
}

// StopDispatcher stops the dispatcher goroutine. In-flight runQueued
// lifecycles are NOT interrupted here — CancelAll owns live-task teardown.
//
// Idempotent: brokerd reaches it through BeginQueueDrain from the signal
// handler and a test reaches it from a defer, and a second close of queueStop
// would panic.
func (b *Broker) StopDispatcher() {
	b.initQueueChans()
	b.queueStopOnce.Do(func() { close(b.queueStop) })
}

// BeginQueueDrain latches the queue shut and stops the dispatcher. Call it
// FIRST in shutdown, before StopCIWatch and CancelAll.
//
// The window it closes. StopCIWatch deliberately does not wait for an in-flight
// poll (blocking the drain behind a GitHub round trip is the starvation D4
// exists to prevent), so a poll that is mid-flight when the signal arrives can
// still conclude, reach maybeEnqueueCIRetry, and Enqueue a child. Before this
// latch the dispatcher was still running — brokerd never called StopDispatcher —
// so that child could be taken and START A FRESH VM after CancelAll had already
// torn down every live task, leaving a VM and a stage the exiting process never
// cleans up. Nothing but a signal's timing was stopping it, and B2 is what made
// the trigger unattended.
//
// After this returns, the two brokerd-side producers of new work are both shut:
// maybeEnqueueCIRetry declines (gate 0) and takeDispatchable dispatches nothing.
// Nothing durable is lost either way — an item that was already enqueued stays
// `queued` on disk and the next boot's ResumeQueue dispatches it.
//
// The HTTP surface is NOT latched here: POST /queue keeps accepting items until
// srv.Shutdown closes the listener, and those items simply wait for the next
// boot, exactly as an item enqueued one second before the signal always did.
func (b *Broker) BeginQueueDrain() {
	b.draining.Store(true)
	b.StopDispatcher()
}

// WaitQueueDrain blocks until every in-flight runQueued lifecycle has returned,
// or ctx expires. Call it AFTER CancelAll, which is what makes those lifecycles
// finish promptly; this only waits for the teardown they then run (force-delete
// the VM, clean the stage, write the terminal). Returns whether the wait
// completed rather than timed out.
//
// Without it the process could exit while a cancelled queued task was still
// deleting its VM — the same truncation risk brokerd already avoids for
// synchronous tasks by waiting on srv.Shutdown, which cannot see these
// goroutines because they are not HTTP handlers.
//
// On a timeout the waiter goroutine is left parked on the WaitGroup. That is
// deliberate and costs nothing: the only caller is a process on its way out,
// and the alternative (a cancellable wait) would mean threading a context
// through every lifecycle for a case whose whole meaning is "we are giving up
// and exiting anyway".
func (b *Broker) WaitQueueDrain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		b.queueRunning.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *Broker) dispatchLoop() {
	tick := b.queueTick
	if tick <= 0 {
		tick = defaultQueueTick
	}
	tk := time.NewTicker(tick)
	defer tk.Stop()
	for {
		b.dispatchPass()
		select {
		case <-b.queueStop:
			return
		case <-tk.C:
		case <-b.queueWake:
		}
	}
}

// dispatchPass dispatches every currently-dispatchable item (oldest first),
// stopping when the queue has no eligible item or the slot cap is reached.
func (b *Broker) dispatchPass() {
	for {
		it, ok := b.takeDispatchable()
		if !ok {
			return
		}
		// Counted BEFORE the goroutine starts, so a WaitQueueDrain that begins
		// in the same instant cannot observe zero and return early.
		b.queueRunning.Add(1)
		go b.runQueued(it)
	}
}

// takeDispatchable finds the oldest queued item whose vendor is not
// spend-capped, claims a concurrency slot for it, transitions it
// queued->preparing (persisted), and removes it from the in-memory queue —
// all under one queueMu hold, so an item can never be taken twice and a
// racing cancelQueued can never cancel a dispatched item.
//
// A spend-parked item is skipped in place: it stays queued with Attempts
// untouched (parking is not an attempt) and later items may overtake it.
// When no slot is available the pass ends — the semaphore is global, so no
// other item could dispatch either; the next tick/wake retries.
//
// A spend-capped BROKER-INITIATED CI RETRY (Task.RetryOf != "") is DROPPED
// instead, to dead_letter. See dropSpendCappedRetryLocked.
func (b *Broker) takeDispatchable() (QueueItem, bool) {
	// Shutdown latch (BeginQueueDrain). Checked here rather than only in the
	// dispatch loop because a pass can already be mid-flight when the signal
	// lands, and starting a VM after CancelAll would orphan it.
	if b.draining.Load() {
		return QueueItem{}, false
	}
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	for i := 0; i < len(b.queue); i++ {
		it := b.queue[i]
		if it.State != QueueQueued {
			continue // defensive: the in-memory queue should only hold queued items
		}
		if b.vendorExceeded(it.Task.Agent) {
			if it.Task.RetryOf == "" {
				continue // spend-parked: stays queued, next tick re-checks
			}
			// Dropped either way. The durable write can fail (a full or
			// read-only disk), but the DECISION does not depend on it: a
			// spend-capped broker-initiated retry is never dispatched, so
			// leaving the in-memory copy behind bought nothing except a
			// re-decide and a re-logged warning on every pass of every tick
			// for as long as the disk stayed broken. The durable record then
			// still says `queued`, which the next boot's ResumeQueue re-loads
			// and re-drops — self-healing, and in the safe direction (an item
			// that does not run).
			b.dropSpendCappedRetryLocked(it)
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			i--
			continue
		}
		if !b.acquireSlot() {
			return QueueItem{}, false // cap reached; retry next tick/wake
		}
		fresh, err := b.setQueueStateLocked(it.ID, QueuePreparing, nil)
		if err != nil {
			// Could not persist the claim: release the slot and leave the
			// item queued for the next pass rather than running it with a
			// state file that still says "queued".
			slog.Warn("queue: dispatch transition failed", "task_id", it.ID, "err", err)
			b.releaseSlot()
			return QueueItem{}, false
		}
		b.queue = append(b.queue[:i], b.queue[i+1:]...)
		return fresh, true
	}
	return QueueItem{}, false
}

// dropSpendCappedRetryLocked terminates a queued CI-retry child that the
// aggregate spend cap caught BEFORE it could dispatch. Caller holds queueMu and
// drops the in-memory copy regardless of what this returns; the failure of a
// durable write must not turn into a re-decide on every dispatch pass.
//
// WHY DROP RATHER THAN PARK, given the dispatcher parks every other item.
// ciretryloop.go's gate 7 already refuses to ENQUEUE a retry against an
// exhausted cap, with the reasoning spelled out there: an unattended,
// broker-authored task that sits queued for a rolling spend window and then
// dispatches hours later — against a base that has moved on, into a diff gate
// nobody is waiting at — is a hazard, not a courtesy. But that refusal only
// held at the INSTANT of the decision, and the cap far more often exhausts in
// the gap that follows: the parent's own spend has usually not settled into the
// ledger when its child is enqueued, so the common case was the dispatcher
// silently parking exactly what gate 7 refuses. This makes the two agree.
//
// A HUMAN-submitted item is untouched by this and still parks: a person asked
// for it and wants it run when the window rolls over. The distinguishing fact
// is Task.RetryOf, which only the broker's own BuildRetryTask sets (the HTTP
// surface zeroes it — see HandleQueueAdd).
//
// dead_letter, not cancelled: nobody cancelled it. It is the queue's existing
// "did not reach a clean finish" terminal, it is visibly not a success in every
// surface that renders an item, and last_error says exactly what happened.
func (b *Broker) dropSpendCappedRetryLocked(it QueueItem) {
	const why = "dropped before dispatch: the aggregate vendor spend cap was exhausted, and a broker-initiated ci retry is refused rather than parked — an unattended retry that dispatches hours later would land on a base that has moved on, at a diff gate nobody is waiting at"
	if _, err := b.setQueueStateLocked(it.ID, QueueDeadLetter, func(q *QueueItem) {
		q.LastError = why
	}); err != nil {
		slog.Warn("queue: could not persist the drop of a spend-capped ci retry; it will not dispatch in this process and the next boot re-drops it",
			"task_id", it.ID, "err", err)
		return
	}
	slog.Info("queue: dropped a spend-capped ci retry rather than parking it",
		"task_id", it.ID, "retry_of", it.Task.RetryOf, "attempt", it.Task.Attempt)
}

// vendorExceeded reports whether the aggregate spend cap is exhausted for the
// vendor this item's agent resolves to. A nil hook or an unresolvable agent
// reads as not-exceeded — mirroring HandleTask's submit-time pre-check, where
// an unknown agent falls through to the lifecycle's own fail-closed
// resolveAgent error instead of being silently parked forever.
func (b *Broker) vendorExceeded(taskAgent string) bool {
	if b.AggregateExceeded == nil {
		return false
	}
	an, _, err := b.resolveAgent(taskAgent)
	if err != nil {
		return false
	}
	v, _ := provider.VendorForAgent(an)
	return v != "" && b.AggregateExceeded(v)
}

// errInvalidTransition marks the ONE setQueueState failure that is the state
// machine working as designed — the item moved on and the caller's write is
// no longer legal — as opposed to a failure to WRITE (a full or read-only disk,
// a permissions problem), which is a fault the caller may want to retry. The
// CI watch is the caller that needs the distinction: it deletes the marker that
// drives the retry, so it must not delete it on a write it can still redo. The
// message text is unchanged from the pre-sentinel version by construction (the
// verb phrase lives in the sentinel).
var errInvalidTransition = errors.New("queue: invalid transition")

// setQueueState transitions the durable item id to state to, guarded by the
// CanTransitionTo state machine. It reads-modifies-writes the queue file and
// refreshes any in-memory copy, all under queueMu; mut (optional) edits the
// item inside the same write (attempt stamps, LastError). Returns the item
// as persisted.
func (b *Broker) setQueueState(id string, to QueueState, mut func(*QueueItem)) (QueueItem, error) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return b.setQueueStateLocked(id, to, mut)
}

func (b *Broker) setQueueStateLocked(id string, to QueueState, mut func(*QueueItem)) (QueueItem, error) {
	it, err := readQueueItem(b.AuditRoot, id)
	if err != nil {
		return it, err
	}
	if !it.State.CanTransitionTo(to) {
		return it, fmt.Errorf("%w %s -> %s for %s", errInvalidTransition, it.State, to, id)
	}
	it.State = to
	it.UpdatedAtMs = b.nowMs()
	if mut != nil {
		mut(&it)
	}
	if err := writeQueueItem(b.AuditRoot, it); err != nil {
		return it, err
	}
	for i := range b.queue {
		if b.queue[i].ID == id {
			b.queue[i] = it
			break
		}
	}
	return it, nil
}

// runQueued drives one dispatched item through the SAME post-accept lifecycle
// the synchronous path runs (taskRun.runLifecycle), headlessly with a discard
// stream — mirroring resumePush. The dispatcher already claimed the item's
// concurrency slot; it is released here on exit. The terminal queue state is
// mapped from the lifecycle's outcome and persisted; the terminal file is
// KEPT for `drydock queue list` history (a later prune sweep cleans it).
func (b *Broker) runQueued(it QueueItem) {
	defer b.queueRunning.Done() // paired with dispatchPass's Add
	defer b.releaseSlot()
	t := it.Task
	// Rooted at Background like every task context: cancellation comes from
	// /admin/kill (the stored cancel) or brokerd shutdown (CancelAll).
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	b.registerTask(it.ID, t.RepoRef, t.Instruction, cancel)
	defer b.unregisterTask(it.ID)

	// preparing -> running, stamping the attempt. Only an actual dispatch
	// counts as an attempt — spend-parked items never reach here.
	// queuedMs (enqueue -> dispatch wait) feeds the metrics row's
	// stage_ms.queued; when the running transition could not be persisted we
	// fall back to now, so the metrics row still records the wait.
	queuedMs := b.nowMs() - it.EnqueuedAtMs
	if fresh, err := b.setQueueState(it.ID, QueueRunning, func(q *QueueItem) {
		q.StartedAtMs = b.nowMs()
		q.Attempts++
	}); err != nil {
		slog.Warn("queue: could not persist running state", "task_id", it.ID, "err", err)
	} else {
		queuedMs = fresh.StartedAtMs - fresh.EnqueuedAtMs
	}
	if queuedMs < 0 {
		queuedMs = 0 // a skewed clock must not write a negative stage duration
	}

	tr := &taskRun{
		b:           b,
		ctx:         ctx,
		sw:          newDiscardStream(),
		id:          it.ID,
		repoRef:     t.RepoRef,
		instruction: t.Instruction,
		egressExtra: t.EgressExtra,
		autoApprove: t.AutoApprove,
		sensitive:   t.Sensitive,
		draft:       t.Draft,
		platform:    t.Platform,
		model:       t.Model,
		planOnly:    t.PlanOnly,
		issueURL:    t.IssueURL,
		taskAgent:   t.Agent,
		queuedMs:    queuedMs,
	}
	// Bridge the diff-approval gate entry into the durable queue state
	// (running -> awaiting_review) so `drydock queue list` shows a queued
	// task blocked at review as exactly that, not as still running.
	tr.onAwaitingReview = func() {
		if _, err := b.setQueueState(it.ID, QueueAwaitingReview, nil); err != nil {
			slog.Warn("queue: could not persist awaiting_review state", "task_id", it.ID, "err", err)
		}
	}
	tr.runLifecycle()

	// Shutdown at the diff-approval gate is a pause, not a terminal:
	// gatePushMarked kept the gate marker and the item is already durably
	// awaiting_review, so the next boot's ResumeAwaiting re-drives the gate
	// headlessly and its resolution writes the real terminal (via
	// finalizeQueuedResume) — which may be a PUSH. Writing `cancelled` here
	// would permanently contradict that. Scoped tightly: only a shutdown
	// cause (a genuine kill is errTaskKilled and still terminal-cancels),
	// with no lifecycle outcome, for an item that actually reached the gate —
	// a mid-run or egress-gate shutdown has no resumable marker and falls
	// through to the terminal mapping as before.
	if tr.outcome == "" && context.Cause(ctx) == errShutdown {
		if cur, err := readQueueItem(b.AuditRoot, it.ID); err == nil && cur.State == QueueAwaitingReview {
			slog.Info("queue: shutdown at the approval gate; item stays awaiting_review for boot resume",
				"task_id", it.ID)
			return
		}
	}

	// A push that armed the CI watch is NOT terminal here: finishPush already
	// moved the item awaiting_review -> awaiting_ci, and its real terminal is
	// whatever the watcher observes (ciqueue.go). Writing queueTerminal's
	// `pushed -> completed` now would mark the item clean before any CI
	// evidence exists — the exact "absence of evidence reads as success"
	// failure the watch exists to prevent. Everything else (no watch, watch
	// disabled, watch declined, a non-push outcome) falls through unchanged.
	if b.ciOwnsTerminal(it.ID) {
		slog.Info("queue: push is under ci observation; the watch owns this item's terminal",
			"task_id", it.ID)
		return
	}

	to, lastErr := queueTerminal(tr.outcome, ctx.Err() != nil)
	if _, err := b.setQueueState(it.ID, to, func(q *QueueItem) {
		q.LastError = lastErr
	}); err != nil {
		slog.Warn("queue: could not persist terminal state",
			"task_id", it.ID, "state", string(to), "err", err)
	}
}

// queueTerminal maps a lifecycle outcome (taskRun.outcome) to the item's
// terminal queue state and LastError. Increment A has no retry: every
// failure outcome dead-letters immediately (Increment B turns retryable
// ones into re-queues). cancelled covers the pre-outcome aborts too when
// the task context was cancelled (kill/shutdown before a terminal emit).
//
// `pushed -> completed` stays exactly as written, and is deliberately NOT
// conditioned on the CI watch: the diversion to awaiting_ci is made by the
// CALLERS (runQueued, finalizeQueuedResume), which skip this mapping entirely
// when ciOwnsTerminal reports the watch owns the item. Keeping the condition
// out of here means this function stays a pure outcome->state mapping with no
// disk read in it, and the one place that decides "is this item finished at
// all" is the one place that reads the durable item.
func queueTerminal(outcome string, ctxCancelled bool) (QueueState, string) {
	switch outcome {
	case "pushed", "no_diff", "planned":
		return QueueCompleted, ""
	case "cancelled", "denied":
		return QueueCancelled, ""
	case "":
		// The lifecycle aborted before any terminal outcome was recorded
		// (pre-audit validation failures: clone, preflight, mint, ...).
		if ctxCancelled {
			return QueueCancelled, ""
		}
		return QueueDeadLetter, "lifecycle aborted before a terminal outcome (see the audit log)"
	default: // error | setup_failed | verify_failed | policy_blocked | push_failed
		return QueueDeadLetter, outcome
	}
}

// cancelQueued cancels an item that is still queued (never dispatched):
// persists queued->cancelled, drops it from the in-memory queue, and reports
// true. Anything else — already dispatched, terminal, unknown — reports
// false and the caller falls back to the live-task kill path. The check and
// the cancel share one queueMu hold, so it can never race takeDispatchable
// into cancelling a dispatched item.
func (b *Broker) cancelQueued(id string) bool {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	for i, it := range b.queue {
		if it.ID != id || it.State != QueueQueued {
			continue
		}
		if _, err := b.setQueueStateLocked(id, QueueCancelled, nil); err != nil {
			slog.Warn("queue: could not persist cancelled state", "task_id", id, "err", err)
			return false
		}
		b.queue = append(b.queue[:i], b.queue[i+1:]...)
		return true
	}
	return false
}

// cancelAwaitingCI cancels an item parked in awaiting_ci: it removes the
// <id>.ci.json marker (which is the whole of the watch's state — deleting it is
// how you cancel a watch) and persists awaiting_ci -> cancelled. Reports
// whether it did so; anything not currently in awaiting_ci reports false and the
// caller falls through to its next option.
//
// It exists because an awaiting_ci item is reachable by NOTHING else: its
// lifecycle goroutine has already returned, so there is no live task for the
// kill path to find, and it is not in the in-memory queue for cancelQueued to
// match. Without this, `drydock queue cancel <id>` 404s on a pushed-and-watched
// item and the only way out is to wait out ci.watch_timeout — while the state
// machine and the docs both say cancelled is reachable from every non-terminal
// state.
//
// ORDER: marker first, then the transition. A crash in that gap leaves an
// awaiting_ci item with no marker, which the next boot terminates honestly
// (ResumeQueue's awaiting_ci branch) — a bounded, already-modeled outcome. The
// other order would leave a live marker over a terminal item, which the watcher
// would then poll (spending real gh calls) until its deadline for a task the
// operator has already cancelled.
//
// A watcher poll racing this loses harmlessly in both directions: it either
// finds no marker, or concludes one whose queue write is then refused by the
// state machine — cancelled is a sink, and the refusal is the guard working.
func (b *Broker) cancelAwaitingCI(id string) bool {
	cur, err := readQueueItem(b.AuditRoot, id)
	if err != nil || cur.State != QueueAwaitingCI {
		return false
	}
	if err := removeCIMarker(b.AuditRoot, id); err != nil {
		slog.Warn("queue: could not remove the ci marker of a cancelled item", "task_id", id, "err", err)
	}
	if _, err := b.setQueueState(id, QueueCancelled, func(q *QueueItem) {
		q.LastError = "cancelled by the operator while awaiting ci"
	}); err != nil {
		// It raced to a terminal (the watch concluded first). That is a real
		// terminal reached honestly, so report the cancel as not-applicable
		// rather than forcing anything.
		slog.Warn("queue: could not cancel an awaiting_ci item", "task_id", id, "err", err)
		return false
	}
	return true
}
