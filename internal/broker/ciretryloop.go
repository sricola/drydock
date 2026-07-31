package broker

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
)

// This file is THE BOUNDED LOOP (plan Task 6): the one place an observed CI
// failure may turn into another task. ciretry.go builds the retry; this file
// decides whether one happens at all, and it is deliberately small enough to
// read in one sitting, because every line of it is a spend decision.
//
// The gates, in the order they are applied, and why each is where it is:
//
//	0. THE DAEMON IS NOT SHUTTING DOWN. A retry authored during the drain would
//	   be the one task nothing tears down; see BeginQueueDrain.
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
//	7. THE AGGREGATE SPEND CAP. Per-vendor, api_key-only, in-memory, fail-open.
//	8. THE GLOBAL USAGE CEILING. Cross-vendor, cross-auth-mode, durable and
//	   fail-closed (globalcap.go). It is the only gate here that bounds a
//	   subscription lane, and the only one that refuses when it cannot measure.
//	9. BuildRetryTask ACCEPTS. Every refusal it returns is a refusal.
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
func (b *Broker) maybeEnqueueCIRetry(obs CIObservation, qs QueueState) (retryID, detail string, park bool) {
	// Gate 0 — SHUTDOWN. StopCIWatch does not wait for an in-flight poll, so a
	// poll that concludes during the drain can reach here after the daemon has
	// begun tearing every task down. Authoring NEW work then is exactly the
	// orphan-VM window BeginQueueDrain closes; the parent's terminal is already
	// durable, and the operator can resubmit. Fail toward under-spending, as
	// crash window W2 does for the same reason.
	if b.draining.Load() {
		return "", "", false
	}
	// Gate 1 — D3. The ONLY control input: a broker-observed conclusion bucket.
	// Not a rollup, not a count, and emphatically not anything a repository's
	// workflow printed into a check name or a log.
	if obs.State != CIFailed {
		return "", "", false
	}
	// Gate 2. The parent's ci_failed transition must actually have landed. A
	// refused transition (cancelled, already concluded) returns some other
	// state; a retryable write failure returns the pre-write state. In both
	// cases the caller will either never come back or will replay the whole
	// observation later, and enqueuing now would double the retry.
	if qs != QueueCIFailed {
		return "", "", false
	}
	// Gate 3 — D6. Off by default, and off means no child is ever built.
	if b.CIMaxAttempts <= 0 {
		return "", "", false
	}
	// Gate 4. The durable queue item is the ONLY source of the parent's Task.
	// A synchronous task has none: there is no persisted invocation to re-run
	// and no record to carry a bound, so it terminates and stops.
	it, err := readQueueItem(b.AuditRoot, obs.TaskID)
	if err != nil {
		return "", "", false
	}
	// Gate 5. THE DURABLE ENQUEUE-ONCE FLAG. Broker-owned and unreachable from
	// any task VM, unlike the audit trace (see appendCIObservationRow).
	//
	// CIRetryEnqueued is the authority and RetryTaskID is the belt-and-braces
	// second read. They are NOT interchangeable: RetryTaskID is written after
	// Enqueue returns, so it is empty for a parent whose child exists but whose
	// link write was lost, and that parent must still be refused. CIRetryEnqueued
	// is written before Enqueue is called and covers that window. See crash
	// window W3 above, and QueueItem's own note on the two fields.
	if it.CIRetryEnqueued || it.RetryTaskID != "" {
		return "", "", false
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
			parent.Attempt, b.CIMaxAttempts), false
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
	//
	// THIS GATE ONLY HOLDS AT THE INSTANT OF THE DECISION, and that is not
	// where the cap usually exhausts: the parent's own spend has often not
	// settled into the ledger yet when its child is enqueued, so the COMMON
	// case is a cap that exhausts in the gap between here and dispatch. The
	// dispatcher therefore enforces the same choice on the other side —
	// dropSpendCappedRetryLocked drops (dead_letters) a queued item with
	// Task.RetryOf set rather than parking it. Without that half the claim
	// above was true of a minority of the cases it described.
	if b.vendorExceeded(parent.Agent) {
		return "", "no retry: the aggregate vendor spend cap is exhausted; refused rather than parked because a ci retry is broker-initiated and unattended", false
	}
	// Gate 8 — THE GLOBAL USAGE CEILING (globalcap.go, plan G1/G2). It is a
	// different bound from gate 7, not a stronger version of it: gate 7 is
	// per-vendor, api_key-mode-only, in-memory and FAIL-OPEN, so on a
	// subscription install — the likeliest unattended configuration, and the
	// one B2 made spend money in — it is inert by construction. The ceiling's
	// task-start limb counts an event the broker itself causes, so it bounds
	// that install; and it REFUSES rather than admits when it cannot measure.
	//
	// Placed AFTER gate 7 so an install that has not enabled the ceiling
	// records exactly the reason it records today, and BEFORE the diff read and
	// BuildRetryTask so no work is done for a retry that cannot start.
	//
	// A CHECK, not a claim: enqueuing is not starting. The child becomes an
	// ordinary queued item and the dispatcher re-asks — and does claim — when
	// it actually dispatches. Refusing here is the same choice gate 7 makes and
	// for the same reason (an unattended item parked at a ceiling dispatches
	// hours later at a gate nobody is waiting at), and the dispatcher's own
	// drop enforces it again for the far commoner case where the ceiling
	// exhausts in the gap between this decision and dispatch.
	//
	// AND IT SPLITS park FROM drop, exactly as the dispatcher does. "Over the
	// limb" is a measured fact with a remedy that arrives on its own schedule,
	// and a retry that waits hours for it lands at a gate nobody is at — so it
	// is refused, and the chain ends honestly. "COULD NOT MEASURE" is a FAULT
	// (a ledger that could not be read this second, a store not yet opened),
	// and this decision is a ONE-SHOT: refusing on it destroys the chain
	// permanently over something that usually clears before the next tick.
	// Parking sets QueueItem.CIRetryDeferred, which makes the replay guard fall
	// through so the next tick re-asks; RetryTaskID still guarantees at most one
	// child. The detail variant is used because the bare globalCeilingExceeded
	// DISCARDS the very bit this distinction rests on.
	//
	// AND THE PARK IS BOUNDED. A fault that never clears — a ledger file that
	// stays unreadable, a store that is never opened — would otherwise park this
	// decision on every watch tick forever: concludeCIWatch keeps the marker on a
	// not-recorded return, and pollCIMarker's already-terminal branch runs BEFORE
	// the deadline check, so a marker held open by a park is never timed out
	// either. Nothing spends, but the marker leaks for the daemon's life. Past the
	// bound the park becomes an honest refusal, which ends the chain and lets the
	// marker be removed. See ciRetryParkBoundMs and ciRetryParkExpired — the bound
	// is measured on ceilingNowMs, the same jump-corrected instant the ceiling
	// this park is waiting on measures its own window against.
	//
	// The SAME park, with the same bound, now also covers the one other fault on
	// this path that a one-shot decision could not survive: an enqueue-once mark
	// that could not be written. See markCIRetryEnqueued's call site below.
	if blocked, why, unmeasured := b.globalCeilingExceededDetail(parent.Agent); blocked {
		if unmeasured {
			if expired, parked := b.ciRetryParkExpired(it); expired {
				slog.Warn("ci retry: the bounded-retry decision has been parked past its bound; refusing rather than parking forever",
					"task_id", obs.TaskID, "parked_ms", parked, "reason", why)
				return "", fmt.Sprintf("no retry: the global usage ceiling was still unmeasurable after %s of deferral (%s)",
					(time.Duration(b.ciRetryParkBoundMs()) * time.Millisecond).String(), why), false
			}
			return "", "retry deferred: " + why, true
		}
		return "", "no retry: " + why, false
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

	// Gate 9. Every refusal BuildRetryTask returns is a refusal: a plan-only
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
		return "", "no retry: " + safeStr(err.Error()), false
	}

	// From here the child is an ORDINARY QUEUED TASK (D1). Enqueue is the same
	// entry point POST /queue uses: it validates, persists a fresh QueueItem in
	// `queued`, and wakes the dispatcher. The child therefore gets its own slot,
	// its own lease, its own VM, its own verify, and — because AutoApprove was
	// forced false by BuildRetryTask (D5) — ITS OWN HUMAN DIFF GATE. Nothing on
	// this path can skip that gate, and nothing tries to.
	// THE ENQUEUE-ONCE MARK, WRITTEN FIRST. Everything after this point either
	// creates a child or fails trying, and from here on no replay of this
	// observation may ask the question again — gate 5 reads this flag.
	//
	// If it cannot be persisted we do NOT enqueue. A child whose parent does not
	// durably record that an enqueue happened is a child a replay can duplicate,
	// and duplicating an unattended spend loop is the one failure this whole file
	// exists to prevent.
	//
	// AND THE TWO WAYS IT CAN FAIL ARE NOT THE SAME FAILURE, which is exactly the
	// distinction gate 8 already draws between a verdict and a fault:
	//
	//   - ALREADY CLAIMED is a VERDICT. A concurrent decision won the
	//     compare-and-set, so a child either exists or is about to. Re-asking
	//     could only ever mint a second one. Refuse, terminally.
	//   - UNWRITABLE is a FAULT — a full disk, a read-only mount, a directory
	//     sitting on the atomic-write temp path — and it is the same
	//     "could not write, may clear next tick" class the round-2 park was built
	//     for. Refusing on it destroyed the retry chain PERMANENTLY over something
	//     that usually clears before the next tick: concludeCIWatch dropped the
	//     marker on a recorded return and nothing ever re-asked.
	//
	// PARKING HERE CANNOT REOPEN THE ENQUEUE-ONCE HOLE, in any interleaving. A
	// mark that failed is a mark that did not reach Enqueue, so there is no child
	// to duplicate. A mark whose write LANDED but reported an error leaves
	// CIRetryEnqueued durably true, and every gate that stands between a re-ask
	// and a child reads that durable flag: recordCIQueueTerminal's replay check,
	// gate 5 above, and markCIRetryEnqueued's own compare-and-set. All three
	// refuse. The park moves only the "has this decision been made" bit, never the
	// "has an enqueue happened" one.
	//
	// It is bounded by the SAME bound as the ceiling park, so an unwritable disk
	// that never recovers still ends the chain honestly and lets the marker go.
	if claimed, transient := b.markCIRetryEnqueued(obs.TaskID); !claimed {
		if !transient {
			return "", "no retry: the durable enqueue-once marker was already claimed by a concurrent decision, and enqueuing without claiming it could duplicate the retry", false
		}
		if expired, parked := b.ciRetryParkExpired(it); expired {
			slog.Warn("ci retry: the enqueue-once mark has been unwritable past the park bound; refusing rather than parking forever",
				"task_id", obs.TaskID, "parked_ms", parked)
			return "", fmt.Sprintf("no retry: the durable enqueue-once marker was still unwritable after %s of deferral",
				(time.Duration(b.ciRetryParkBoundMs()) * time.Millisecond).String()), false
		}
		return "", "retry deferred: the durable enqueue-once marker could not be written (a full or read-only disk); nothing was enqueued, so the decision is re-asked next tick", true
	}
	childID, err := b.Enqueue(child)
	if err != nil {
		slog.Warn("ci retry: could not enqueue the retry task", "task_id", obs.TaskID, "err", safeErr(err))
		return "", "no retry: enqueue failed: " + safeStr(err.Error()), false
	}
	// Record the link on the parent so an operator can follow the chain
	// forwards. Best effort by construction: the child is already durable and
	// its own Task.RetryOf names the parent, so the chain is followable
	// backwards regardless (see crash window W3).
	b.linkCIRetryChild(obs.TaskID, childID)
	slog.Info("ci retry: enqueued a bounded retry for an observed CI failure",
		"task_id", obs.TaskID, "retry_task_id", childID,
		"attempt", child.Attempt, "max_attempts", b.CIMaxAttempts)
	return childID, fmt.Sprintf("enqueued retry attempt %d of %d", child.Attempt, b.CIMaxAttempts), false
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

// markCIRetryEnqueued stamps the parent's durable ENQUEUE-ONCE flag, and it is
// the ONE write in this file that is not best-effort: it reports whether the
// flag is durable, and its caller refuses to enqueue when it is not.
//
// It is linkCIRetryChild's sibling in mechanics (not a setQueueState call — the
// parent is already terminal; one field on an already-final record; under
// queueMu; State never touched) and its opposite in ORDERING, which is the whole
// reason it exists. linkCIRetryChild runs after the child is durable, so it can
// only ever record a child that already exists. This runs BEFORE, so it records
// the INTENT — and an intent recorded before the act is the only thing a replay
// after a crash in between can see.
//
// It is a COMPARE-AND-SET, not an idempotent write, and that distinction is the
// last interleaving: gate 5 reads the flag outside this lock, so two decisions
// racing on one parent can both pass it. Reporting an already-set flag as
// "claimed" would let the loser enqueue a second child. Only the writer that
// actually flipped it may go on to Enqueue; the loser is refused. Under
// sequential execution the flag is never already set here, because gate 5 would
// have returned.
//
// It reports TWO bits, because its caller has two different jobs to do:
// claimed says whether this decision owns the enqueue, and transient says
// whether a non-claim was a FAULT that may clear (an unreadable or unwritable
// queue item) rather than a VERDICT (the flag was already claimed). A verdict is
// terminal; a fault parks. Never both — a claim is never transient.
func (b *Broker) markCIRetryEnqueued(parentID string) (claimed, transient bool) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	it, err := readQueueItem(b.AuditRoot, parentID)
	if err != nil {
		slog.Warn("ci retry: could not read the parent to mark the enqueue-once flag; deferring the retry",
			"task_id", parentID, "err", err)
		return false, true
	}
	if it.CIRetryEnqueued {
		slog.Warn("ci retry: the enqueue-once flag was already claimed for this parent; refusing this decision so it cannot mint a second child",
			"task_id", parentID)
		return false, false
	}
	it.CIRetryEnqueued = true
	it.UpdatedAtMs = b.nowMs()
	if err := writeQueueItem(b.AuditRoot, it); err != nil {
		slog.Warn("ci retry: could not persist the enqueue-once flag on the parent; deferring the retry rather than risking a duplicate child",
			"task_id", parentID, "err", err)
		return false, true
	}
	for i := range b.queue {
		if b.queue[i].ID == parentID {
			b.queue[i] = it
			break
		}
	}
	return true, false
}

// ciRetryParkedSinceMs is the instant a bounded-retry park on it began, or 0
// when the decision is not parked.
//
// The DURABLE stamp wins whenever there is one: it survives a restart, so a
// crash loop cannot keep restarting the bound. The process-local fallback covers
// the case the durable stamp cannot: a park whose cause is that queue-item
// WRITES are failing, which takes the stamp's own write down with it.
func (b *Broker) ciRetryParkedSinceMs(it QueueItem) int64 {
	if it.CIRetryDeferredAtMs > 0 {
		return it.CIRetryDeferredAtMs
	}
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return b.ciRetryParked[it.ID]
}

// ciRetryParkExpired reports whether a bounded-retry park on it has outlived
// ciRetryParkBoundMs, and how long it has been parked. False for a decision that
// is not parked at all — the first park of a decision is never expired.
//
// Measured on ceilingNowMs, the same JUMP-CORRECTED instant the ceiling measures
// its own rolling window against and the same one setCIRetryDeferred stamps the
// park with. On the raw wall clock the two disagreed: a forward host-clock jump
// one tick into a park ended the park immediately, against a bound no real time
// had elapsed on.
func (b *Broker) ciRetryParkExpired(it QueueItem) (expired bool, parkedMs int64) {
	since := b.ciRetryParkedSinceMs(it)
	if since <= 0 {
		return false, 0
	}
	parkedMs = b.ceilingNowMs() - since
	return parkedMs >= b.ciRetryParkBoundMs(), parkedMs
}

// ciRetryParkBoundMs is how long a bounded-retry decision may stay PARKED on an
// unmeasurable global ceiling before it becomes an honest refusal.
//
// The bound exists because a park keeps the item's <id>.ci.json marker alive
// (concludeCIWatch keeps it on a not-recorded return) and pollCIMarker's
// already-terminal branch short-circuits before the deadline check, so a marker
// held open by a park is never timed out. A permanently unmeasurable ceiling
// therefore parked forever and leaked the marker for the daemon's life.
//
// It is the CI watch's own timeout, so the rule is easy to state and impossible
// to get out of step with: a retry decision cannot be deferred for longer than
// the watch that produced it could have run.
func (b *Broker) ciRetryParkBoundMs() int64 {
	to := b.CIWatchTimeout
	if to <= 0 {
		to = defaultCIWatchTimeout
	}
	return to.Milliseconds()
}

// setCIRetryDeferred stamps (or clears) the parent's durable "the bounded-retry
// decision has not been made yet" flag.
//
// It is linkCIRetryChild's sibling and follows the same rules for the same
// reasons: NOT a setQueueState call (the parent is already terminal and
// terminal states have no outgoing transitions — that is the state machine
// working), one field written on an already-final record, under queueMu like
// every other queue-file read-modify-write, and State never touched.
//
// The DURABLE write is best effort, but the park itself is not: a process-local
// copy is recorded first (b.ciRetryParked), so a fault that takes the flag's own
// write down with it — the exact case an unwritable enqueue-once mark parks on —
// still leaves the next watch pass able to see that the decision is unmade. See
// the field's own note for why that cannot mint a second child.
func (b *Broker) setCIRetryDeferred(parentID string, deferred bool) (changed bool) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	// The in-memory copy is maintained UNCONDITIONALLY and BEFORE the durable
	// write, including for a parent with no queue item (a synchronous task, whose
	// decision never gets this far anyway) — recording an unmade decision costs
	// nothing and failing to record one destroys a chain.
	if deferred {
		if b.ciRetryParked == nil {
			b.ciRetryParked = map[string]int64{}
		}
		if _, ok := b.ciRetryParked[parentID]; !ok {
			b.ciRetryParked[parentID] = b.ceilingNowMs()
		}
	} else {
		delete(b.ciRetryParked, parentID)
	}
	it, err := readQueueItem(b.AuditRoot, parentID)
	if err != nil {
		return false // a synchronous task has no queue item; nothing to defer
	}
	if it.CIRetryDeferred == deferred {
		return false
	}
	it.CIRetryDeferred = deferred
	// The park's own anchor, stamped on the transition into a park and cleared on
	// the way out. It is DURABLE so a crash loop cannot keep restarting the bound
	// — the same property ciDeadline holds for the watch itself.
	//
	// ceilingNowMs, NOT the raw wall clock: the thing this park is waiting on is
	// the global ceiling, which measures its own window against the jump-CORRECTED
	// instant. Stamping the park against the raw clock made the two disagree about
	// time — a forward host-clock jump one tick into a park ended the park
	// immediately, on a bound the ceiling itself had not seen a millisecond of.
	if deferred {
		it.CIRetryDeferredAtMs = b.ciRetryParked[parentID]
	} else {
		it.CIRetryDeferredAtMs = 0
	}
	it.UpdatedAtMs = b.nowMs()
	if err := writeQueueItem(b.AuditRoot, it); err != nil {
		slog.Warn("ci retry: could not persist the deferred-retry flag on the parent",
			"task_id", parentID, "deferred", deferred, "err", err)
		return false
	}
	for i := range b.queue {
		if b.queue[i].ID == parentID {
			b.queue[i] = it
			break
		}
	}
	return true
}
