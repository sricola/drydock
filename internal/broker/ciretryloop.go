package broker

import (
	"fmt"
	"log/slog"
	"path/filepath"
)

// This file is THE BOUNDED LOOP (plan Task 6): the one place an observed CI
// failure may turn into another task. ciretry.go builds the retry; this file
// decides whether one happens at all, and it is deliberately small enough to
// read in one sitting, because every line of it is a spend decision.
//
// The gates, in the order they are applied, and why each is where it is:
//
//	1. OBSERVED FAILURE ONLY (D3). CIFailed is the only state that may retry.
//	   timed_out / unknown / no_checks / passed each already terminate honestly
//	   under B1, and none of them is evidence that a build is broken — retrying
//	   on "we could not tell" would spend a fresh task_budget_usd to learn
//	   nothing, on a timer, unattended.
//	2. THE PARENT'S TERMINAL IS ALREADY DURABLE. The decision runs only after
//	   the parent's ci_failed transition reached disk. That order is what makes
//	   the enqueue-once property hold across a crash; see the analysis below.
//	3. THE RETRY IS ENABLED. ci.max_attempts default 0 = off (D6).
//	4. THE PARENT HAS A DURABLE QUEUE ITEM. Its persisted Task is the ONLY
//	   source of the attempt counter and of the invocation the retry re-runs.
//	   A synchronous (POST /tasks) task has none, so it never retries.
//	5. NO CHILD IS ALREADY RECORDED. QueueItem.RetryTaskID is the durable,
//	   broker-owned "this decision was already made" flag.
//	6. THE BOUND. Attempt < ci.max_attempts, where Attempt is the HIGHER of the
//	   persisted Task's and the marker-derived observation's (fail-closed).
//	7. THE AGGREGATE SPEND CAP.
//	8. BuildRetryTask ACCEPTS. Every refusal it returns is a refusal.
//
// ---------------------------------------------------------------------------
// CRASH WINDOWS (D6). The bound lives in the PERSISTED Task and its mirror on
// the durable <id>.ci.json marker, so nothing about it is in memory and a
// restart cannot launder it. The three windows, each resolved:
//
//	W1  KILLED BEFORE THE PARENT'S TERMINAL WRITE. The marker survives with its
//	    terminal state persisted (concludeCIWatch writes that first), the parent
//	    is still awaiting_ci, and no child exists. The next boot's first watch
//	    pass takes pollCIMarker's already-terminal branch and replays the whole
//	    observation, so the decision is simply made once, later. Nothing is
//	    lost and nothing is doubled.
//
//	W2  KILLED BETWEEN THE PARENT'S TERMINAL WRITE AND Enqueue. The parent is
//	    ci_failed; no child exists and none ever will — the replay in the next
//	    life is caught by recordCIQueueTerminal's replay check (the parent's
//	    state AND CIState already match the observation) and returns before this
//	    file is reached. THE CHILD IS LOST, AND THAT IS THE DELIBERATE
//	    DIRECTION: this window can only ever produce FEWER attempts than the
//	    bound allows, never more. An operator sees a ci_failed parent with no
//	    retry_task_id and can resubmit by hand; the alternative ordering
//	    (enqueue first, terminal second) would replay into a SECOND child every
//	    time, which is an unbounded spend bug. Fail toward under-spending.
//
//	W3  KILLED BETWEEN Enqueue AND THE PARENT'S LINK WRITE. The child exists as
//	    an ordinary queued item and the next boot's ResumeQueue re-enqueues it,
//	    so the retry happens exactly once as intended. Only the parent-side
//	    convenience link is missing; the chain is still fully followable from
//	    the child, whose own Task.RetryOf/Attempt were written durably by
//	    Enqueue itself. The replay is again refused by the replay check.
//
//	W4  THE MARKER IS DELETED MID-DECISION (a `drydock queue cancel`, a manual
//	    rm). Nothing here reads the marker: the decision reads the durable QUEUE
//	    ITEM. A cancel that lands first moves the parent out of awaiting_ci, the
//	    terminal transition is then REFUSED by the state machine, gate 2 sees a
//	    state that is not ci_failed, and no child is enqueued.
//
// The property that falls out: AT MOST ONE CHILD PER PARENT, EVER, and
// therefore at most ci.max_attempts tasks in a chain — because each child's
// Attempt is its parent's + 1, written durably at Enqueue, and gate 6 compares
// against it.
// ---------------------------------------------------------------------------

// maybeEnqueueCIRetry is the decision. It runs AFTER the parent's terminal is
// durable (see the ordering argument above) and returns the enqueued child's
// task id (empty when none) plus a short broker-authored reason, which is
// recorded on the ci_observation audit row so an operator can see why a retry
// did or did not happen.
//
// It never fails the caller: a CI retry is an enhancement to an already-final
// terminal, and no failure here may change the parent's recorded outcome.
func (b *Broker) maybeEnqueueCIRetry(obs CIObservation, qs QueueState) (string, string) {
	// Gate 1 — D3. The ONLY control input: a broker-observed conclusion bucket.
	// Not a rollup, not a count, and emphatically not anything a repository's
	// workflow printed into a check name or a log.
	if obs.State != CIFailed {
		return "", ""
	}
	// Gate 2. The parent's ci_failed transition must actually have landed. A
	// refused transition (cancelled, already concluded) returns some other
	// state; a retryable write failure returns the pre-write state. In both
	// cases the caller will either never come back or will replay the whole
	// observation later, and enqueuing now would double the retry.
	if qs != QueueCIFailed {
		return "", ""
	}
	// Gate 3 — D6. Off by default, and off means no child is ever built.
	if b.CIMaxAttempts <= 0 {
		return "", ""
	}
	// Gate 4. The durable queue item is the ONLY source of the parent's Task.
	// A synchronous task has none: there is no persisted invocation to re-run
	// and no record to carry a bound, so it terminates and stops.
	it, err := readQueueItem(b.AuditRoot, obs.TaskID)
	if err != nil {
		return "", ""
	}
	// Gate 5. The durable enqueue-once flag. Broker-owned and unreachable from
	// any task VM, unlike the audit trace (see appendCIObservationRow).
	if it.RetryTaskID != "" {
		return "", ""
	}

	parent := it.Task
	// Gate 6 — the bound. Attempt counts RETRIES: an operator-submitted task is
	// 0 and the Nth automatic retry is N, so `Attempt < max` yields exactly
	// max_attempts retries in a chain.
	//
	// The two counters should be identical — recordCIMarker copies the marker's
	// from this same persisted Task — so the tie-break is a fail-closed guard
	// against a marker and an item that somehow disagree, not a feature. Taking
	// the HIGHER can only ever shorten a chain.
	if obs.Attempt > parent.Attempt {
		parent.Attempt = obs.Attempt
	}
	// And clamp the floor at 0. Task.Attempt is an ordinary JSON field, so an
	// operator-submitted body may carry ANY int; a NEGATIVE one would make
	// `Attempt < max` true for max+|n| hops and lengthen the chain past the
	// bound — the one direction that costs real money. A higher-than-real value
	// needs no guard: it only ever shortens a chain. Clamped here rather than
	// rejected in Enqueue because this is the only reader of the field, and a
	// 400 on the operator's own submit path would be a new failure mode for a
	// value that is otherwise inert.
	if parent.Attempt < 0 {
		parent.Attempt = 0
	}
	if parent.Attempt >= b.CIMaxAttempts {
		return "", fmt.Sprintf("no retry: attempt %d of a bound of %d ci.max_attempts is the last",
			parent.Attempt, b.CIMaxAttempts)
	}
	// Gate 7 — the aggregate spend cap, the same check the dispatcher applies
	// (vendorExceeded).
	//
	// REFUSE, DO NOT PARK — and this is a choice, so it is recorded. The
	// dispatcher parks a spend-capped item because a HUMAN submitted it and
	// wants it run when the window rolls over. A CI retry is BROKER-INITIATED
	// and unattended: parking it would leave an item nobody asked for sitting
	// queued for up to a full rolling window, then dispatching hours later
	// against a base that has moved on, with a diff gate nobody is waiting at.
	// Refusing costs the operator one honest line in the audit and spends
	// nothing; they can resubmit deliberately.
	if b.vendorExceeded(parent.Agent) {
		return "", "no retry: the aggregate vendor spend cap is exhausted; refused rather than parked because a ci retry is broker-initiated and unattended"
	}

	// The prior attempt's diff crosses over as capped TEXT only (D2). Read with
	// the same O_NOFOLLOW defense as the resume path: a planted
	// <id>.diff -> elsewhere must not feed host bytes into an instruction.
	// Unreadable is not fatal — missing evidence is recorded, never invented,
	// and BuildRetryTask says so in the assembled instruction.
	diff, derr := readDiffNoFollow(filepath.Join(b.AuditRoot, obs.TaskID+".diff"))
	if derr != nil {
		diff = ""
		slog.Debug("ci retry: the prior attempt's diff could not be read; retrying on the CI evidence alone",
			"task_id", obs.TaskID, "err", derr)
	}

	// Gate 8. Every refusal BuildRetryTask returns is a refusal: a plan-only
	// parent, an instruction that leaves no room for the CI evidence, a
	// malformed identity. None of them may be read as "retry allowed".
	child, err := BuildRetryTask(RetryRequest{
		ParentID:    obs.TaskID,
		Parent:      parent,
		Observation: obs,
		PriorDiff:   diff,
	})
	if err != nil {
		slog.Info("ci retry: refusing to build a retry", "task_id", obs.TaskID, "err", safeErr(err))
		return "", "no retry: " + safeStr(err.Error())
	}

	// From here the child is an ORDINARY QUEUED TASK (D1). Enqueue is the same
	// entry point POST /queue uses: it validates, persists a fresh QueueItem in
	// `queued`, and wakes the dispatcher. The child therefore gets its own slot,
	// its own lease, its own VM, its own verify, and — because AutoApprove was
	// forced false by BuildRetryTask (D5) — ITS OWN HUMAN DIFF GATE. Nothing on
	// this path can skip that gate, and nothing tries to.
	childID, err := b.Enqueue(child)
	if err != nil {
		slog.Warn("ci retry: could not enqueue the retry task", "task_id", obs.TaskID, "err", safeErr(err))
		return "", "no retry: enqueue failed: " + safeStr(err.Error())
	}
	// Record the link on the parent so an operator can follow the chain
	// forwards. Best effort by construction: the child is already durable and
	// its own Task.RetryOf names the parent, so the chain is followable
	// backwards regardless (see crash window W3).
	b.linkCIRetryChild(obs.TaskID, childID)
	slog.Info("ci retry: enqueued a bounded retry for an observed CI failure",
		"task_id", obs.TaskID, "retry_task_id", childID,
		"attempt", child.Attempt, "max_attempts", b.CIMaxAttempts)
	return childID, fmt.Sprintf("enqueued retry attempt %d of %d", child.Attempt, b.CIMaxAttempts)
}

// linkCIRetryChild stamps the child's id on the parent's durable queue item.
//
// It is deliberately NOT a setQueueState call: the parent is already terminal
// (ci_failed) and terminal states have no outgoing transitions, which is the
// state machine working. This writes one additional field on an already-final
// record, under queueMu like every other queue-file read-modify-write, and it
// NEVER touches State — asserted by leaving the field alone rather than
// re-deriving it.
//
// A first writer wins: an existing RetryTaskID is left in place, so this can
// never silently repoint a parent at a second child.
func (b *Broker) linkCIRetryChild(parentID, childID string) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	it, err := readQueueItem(b.AuditRoot, parentID)
	if err != nil {
		slog.Warn("ci retry: could not record the retry link on the parent",
			"task_id", parentID, "retry_task_id", childID, "err", err)
		return
	}
	if it.RetryTaskID != "" {
		return
	}
	it.RetryTaskID = childID
	it.UpdatedAtMs = b.nowMs()
	if err := writeQueueItem(b.AuditRoot, it); err != nil {
		slog.Warn("ci retry: could not persist the retry link on the parent",
			"task_id", parentID, "retry_task_id", childID, "err", err)
		return
	}
	for i := range b.queue {
		if b.queue[i].ID == parentID {
			b.queue[i] = it
			break
		}
	}
}
