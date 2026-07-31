package broker

import (
	"context"
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
func (b *Broker) StopDispatcher() {
	b.initQueueChans()
	close(b.queueStop)
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
func (b *Broker) takeDispatchable() (QueueItem, bool) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	for i := 0; i < len(b.queue); i++ {
		it := b.queue[i]
		if it.State != QueueQueued {
			continue // defensive: the in-memory queue should only hold queued items
		}
		if b.vendorExceeded(it.Task.Agent) {
			continue // spend-parked: stays queued, next tick re-checks
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
		return it, fmt.Errorf("queue: invalid transition %s -> %s for %s", it.State, to, id)
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
	if _, err := b.setQueueState(it.ID, QueueRunning, func(q *QueueItem) {
		q.StartedAtMs = b.nowMs()
		q.Attempts++
	}); err != nil {
		slog.Warn("queue: could not persist running state", "task_id", it.ID, "err", err)
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
