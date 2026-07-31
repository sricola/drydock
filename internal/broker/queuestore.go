package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"drydock/internal/atomicfile"
)

// QueueState is the lifecycle state of a queued orchestration item. States
// only ever move forward through validTransition; the four terminal states
// (completed, ci_failed, dead_letter, cancelled) are sinks.
type QueueState string

const (
	QueueQueued         QueueState = "queued"
	QueuePreparing      QueueState = "preparing"
	QueueRunning        QueueState = "running"
	QueueVerifying      QueueState = "verifying"
	QueueAwaitingReview QueueState = "awaiting_review"
	// QueueAwaitingCI: the item's branch is PUSHED and its PR is open; the
	// host-side CI watcher (ciwatch.go) is observing that PR's checks. The
	// work is done — nothing is dispatched, no slot is held, no stage is kept
	// (D4) — but the item is not terminal, because no CI conclusion has been
	// observed yet. Reached only from awaiting_review, and only when a
	// <id>.ci.json marker was actually armed for the item.
	QueueAwaitingCI QueueState = "awaiting_ci"
	QueueCompleted  QueueState = "completed"
	// QueueCIFailed is TERMINAL and means exactly one thing: the broker
	// OBSERVED at least one of the PR's checks conclude in failure. It is
	// never written for a watch that ended without a conclusion — a timeout,
	// a run of gh errors, or a marker that did not survive a restart all land
	// on dead_letter instead, because "we never found out" is missing
	// evidence, not a failed build (and, symmetrically, not a passing one).
	QueueCIFailed   QueueState = "ci_failed"
	QueueDeadLetter QueueState = "dead_letter"
	QueueCancelled  QueueState = "cancelled"
)

// Terminal reports whether s is a sink state — no further transitions.
func (s QueueState) Terminal() bool {
	switch s {
	case QueueCompleted, QueueCIFailed, QueueDeadLetter, QueueCancelled:
		return true
	}
	return false
}

// validTransition is the single source of truth for the queue state machine.
// Terminal states are deliberately absent: they have no outgoing edges.
// running -> completed exists because some lifecycle terminals are normal
// finishes that never enter verify or review: a run that produced no diff
// (no_diff) and a plan-only run (planned) both complete straight from running.
//
// The machine is FORWARD-ONLY and ACYCLIC, and both properties are asserted
// mechanically by TestQueueStateMachineIsForwardOnly rather than trusted to a
// reader. This is what makes the queue safe to reconcile at boot: a state can
// be reasoned about as "how far did this item get", never "which lap is it
// on". The CI arc keeps it that way — awaiting_review -> awaiting_ci is a step
// FORWARD past the push, and every edge out of awaiting_ci is terminal, so
// there is no awaiting_ci -> queued (or any other) back-edge. A bounded retry
// (B2, D1) is therefore a NEW item that starts at queued with its own id, not
// a re-entry of this one.
var validTransition = map[QueueState][]QueueState{
	// queued -> dead_letter exists for ONE writer: the dispatcher dropping a
	// broker-initiated CI retry whose vendor spend cap exhausted before it
	// could dispatch (dropSpendCappedRetryLocked). A human-submitted item is
	// never dead-lettered from queued — it parks, because a person is waiting
	// for it.
	QueueQueued:         {QueuePreparing, QueueDeadLetter, QueueCancelled},
	QueuePreparing:      {QueueRunning, QueueDeadLetter, QueueCancelled},
	QueueRunning:        {QueueVerifying, QueueAwaitingReview, QueueCompleted, QueueDeadLetter, QueueCancelled},
	QueueVerifying:      {QueueAwaitingReview, QueueDeadLetter, QueueCancelled},
	QueueAwaitingReview: {QueueCompleted, QueueAwaitingCI, QueueDeadLetter, QueueCancelled},
	// The four honest endings of a CI watch. Note what is NOT here: there is
	// no edge that turns an unobserved watch into `completed`. completed is
	// reachable only from an OBSERVED pass or an OBSERVED "this PR has no
	// checks configured"; every other ending routes to ci_failed (observed
	// failure) or dead_letter (no conclusion).
	QueueAwaitingCI: {QueueCompleted, QueueCIFailed, QueueDeadLetter, QueueCancelled},
}

// CanTransitionTo reports whether the state machine permits s -> to. Unknown
// states (either side) are never valid, so a corrupted on-disk state can't
// smuggle an item into an arbitrary next state.
func (s QueueState) CanTransitionTo(to QueueState) bool {
	for _, next := range validTransition[s] {
		if next == to {
			return true
		}
	}
	return false
}

// QueueItem is the durable record of one queued task. It persists the FULL
// Task — including AutoApprove, which the gate marker deliberately omits —
// because a queued item that loses its headless-approval intent across a
// restart would silently re-gate (or worse, a lost Sensitive flag would
// silently un-gate).
type QueueItem struct {
	ID           string     `json:"id"`
	Task         Task       `json:"task"`
	State        QueueState `json:"state"`
	EnqueuedAtMs int64      `json:"enqueued_at_ms"`
	StartedAtMs  int64      `json:"started_at_ms"`
	UpdatedAtMs  int64      `json:"updated_at_ms"`
	Attempts     int        `json:"attempts"`
	LastError    string     `json:"last_error"`
	// PRNumber is the pull request this item's push opened, recorded when the
	// CI watch is armed. Display only (`drydock queue list`); the watch's own
	// query coordinate lives on the <id>.ci.json marker, which is the ONLY
	// thing that may aim a gh call.
	PRNumber int `json:"pr_number,omitempty"`
	// CIState is the broker's last OBSERVED CI conclusion for PRNumber, in the
	// ciwatch vocabulary (pending/passed/failed/no_checks/timed_out/unknown).
	//
	// It has a second, load-bearing job beyond display: a NON-EMPTY CIState is
	// the durable record that the CI watch took ownership of this item's
	// terminal. runQueued and finalizeQueuedResume both consult it (see
	// Broker.ciOwnsTerminal) and decline to write their own `completed` when
	// it is set, which is what stops a pushed-and-watched item from racing to
	// `completed` before its CI verdict exists. Empty means "no watch" — the
	// stock, watch-disabled behavior — never "CI passed".
	CIState string `json:"ci_state,omitempty"`
	// RetryTaskID is the BOUNDED RETRY this item's observed CI failure enqueued
	// (B2, D6): a NEW task with a new id, its own credential lease, and its own
	// human diff gate — never a re-run of this one. It is the forward link an
	// operator follows down a chain; the backward link is the child's own
	// Task.RetryOf, written durably by Enqueue.
	//
	// It is written by linkCIRetryChild only, first-writer-wins, onto an item
	// that is already terminal — see ciretryloop.go.
	//
	// IT IS NOT THE ENQUEUE-ONCE FLAG. It cannot be: it is written AFTER Enqueue
	// returns, so between those two instants a crash — or one failed write —
	// leaves a parent with a real child and an empty link. CIRetryEnqueued is the
	// enqueue-once flag, and it is written BEFORE Enqueue for exactly that reason.
	RetryTaskID string `json:"retry_task_id,omitempty"`
	// CIRetryEnqueued is the durable ENQUEUE-ONCE flag: "an enqueue was ATTEMPTED
	// for this parent". It is written BEFORE broker.Enqueue is called, and the
	// retry decision refuses outright when it is already set.
	//
	// It is separate from RetryTaskID, and the separation is the whole point.
	// RetryTaskID is written AFTER the child exists, so the interval between
	// Enqueue returning and the link landing is a window in which the parent has
	// a child and does not say so. That window used to be harmless because the
	// replay guard returned before the decision could run again — but a PARKED
	// decision makes the guard fall through (CIRetryDeferred), and then a crash in
	// that window, or a single correlated write failure across the two
	// best-effort writes that follow Enqueue, re-asked a decision that had already
	// enqueued and minted a SECOND child. That violates "at most one child per
	// parent, ever", which is the bound on an unattended spend loop.
	//
	// Written before rather than after, so the failure direction is a LOST retry
	// (the same direction crash window W2 already takes) rather than a doubled
	// one. If the marker cannot be persisted, no enqueue happens at all.
	CIRetryEnqueued bool `json:"ci_retry_enqueued,omitempty"`
	// CIRetryDeferred says the bounded-retry decision for this item's observed
	// CI failure has NOT been made yet, because something it needs could not be
	// MEASURED OR WRITTEN at the moment it was asked: the global usage ceiling
	// could not be read (a ledger that could not be opened, a store not yet
	// opened, an agent that would not resolve), or the durable enqueue-once mark
	// could not be persisted (a full or read-only disk).
	//
	// It exists because that decision is a ONE-SHOT: applyCIObservation runs it
	// exactly once per observation and the crash-window replay guard returns
	// before it on every later pass. Treating "I could not tell" as "no retry"
	// therefore destroyed the chain permanently over a fault that usually clears
	// in seconds — the same conflation the dispatcher carefully avoids
	// (queue.go's `unmeasured` branch parks rather than drops), and the opposite
	// of what docs/configuration.md and docs/THREAT_MODEL.md promise about a
	// transient fault never destroying unattended work.
	//
	// While it is set AND no enqueue has been attempted, the replay guard falls
	// THROUGH to the retry decision so the next watch tick re-asks. Both halves
	// are required: falling through on the deferral ALONE re-opened a decision
	// that had already enqueued, which minted a second child. CIRetryEnqueued is
	// the enqueue-once flag that closes it.
	//
	// It is also BOUNDED (ciRetryParkBoundMs): a fault that never clears must not
	// park forever, or the item's CI marker is kept for the daemon's life.
	// CIRetryDeferredAtMs is when the park started.
	//
	// It has a PROCESS-LOCAL SHADOW (Broker.ciRetryParked) for the one case this
	// durable field cannot cover: when the fault being parked on is that queue-item
	// writes are failing, the write of THIS flag fails with it. The durable copy is
	// authoritative whenever it exists; the shadow only ever widens "the decision is
	// unmade", never "an enqueue happened".
	CIRetryDeferred bool `json:"ci_retry_deferred,omitempty"`
	// CIRetryDeferredAtMs is the CORRECTED broker clock (globalcap.ceilingNowMs, the
	// same instant the ceiling measures its own rolling window against) at which the
	// FIRST park of this item's retry decision happened. It is the anchor the park
	// bound is measured from, and it is durable so a crash loop cannot keep
	// restarting the bound. It is not the raw wall clock: on that clock a forward
	// host-clock jump one tick into a park ended the park immediately, against a
	// bound the ceiling itself had seen no time elapse on.
	CIRetryDeferredAtMs int64 `json:"ci_retry_deferred_at_ms,omitempty"`
}

// queueIDRE matches newID's output shape (32 lowercase hex chars). Validated
// before any path is built from an id, so a hostile id can't traverse out of
// auditRoot.
var queueIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

const queueSuffix = ".queue.json"

func queueMarkerPath(auditRoot, id string) string {
	return filepath.Join(auditRoot, id+queueSuffix)
}

// writeQueueItem durably persists it under auditRoot. Timestamps are the
// caller's problem (the broker passes its clock) — nothing here calls
// time.Now, so tests are deterministic.
//
// atomicfile.Write (temp + rename) is the crash-atomicity guarantee: a crash
// mid-write can never leave a truncated item where a whole one is expected.
// Belt-and-suspenders would add an fsync of auditRoot itself so the rename
// survives a power loss in the same instant, but the rename's
// atomicity is the primary guarantee and matches the rest of the audit dir.
func writeQueueItem(auditRoot string, it QueueItem) error {
	if !queueIDRE.MatchString(it.ID) {
		return fmt.Errorf("queuestore: invalid queue item id %q", it.ID)
	}
	if err := os.MkdirAll(auditRoot, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(queueMarkerPath(auditRoot, it.ID), payload, 0o600)
}

// readQueueItem loads one item by id. The id shape is validated BEFORE the
// path is built (traversal guard), and the open refuses symlinks (O_NOFOLLOW,
// parity with the write side) so a planted <id>.queue.json -> elsewhere can't
// feed the queue substituted bytes.
func readQueueItem(auditRoot, id string) (QueueItem, error) {
	var it QueueItem
	if !queueIDRE.MatchString(id) {
		return it, fmt.Errorf("queuestore: invalid queue item id %q", id)
	}
	f, err := os.OpenFile(queueMarkerPath(auditRoot, id), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return it, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return it, err
	}
	err = json.Unmarshal(data, &it)
	return it, err
}

// removeQueueItem deletes the item's marker (and any stray .tmp a crashed
// atomic write left behind). Idempotent: a missing file is success, so a
// remove raced against a restart's boot-scan cleanup can't fail the caller.
func removeQueueItem(auditRoot, id string) error {
	if !queueIDRE.MatchString(id) {
		return fmt.Errorf("queuestore: invalid queue item id %q", id)
	}
	path := queueMarkerPath(auditRoot, id)
	if err := os.Remove(path + ".tmp"); err != nil && !os.IsNotExist(err) {
		return err
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// listQueueItems scans auditRoot for *.queue.json and returns the parseable
// items sorted by EnqueuedAtMs (FIFO). A garbage, unreadable, or symlinked
// file is skipped with a warning rather than failing the whole boot scan —
// one corrupted item must not strand every other queued task.
func listQueueItems(auditRoot string) ([]QueueItem, error) {
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []QueueItem
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), queueSuffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), queueSuffix)
		it, err := readQueueItem(auditRoot, id)
		if err != nil {
			slog.Warn("queuestore: skipping unreadable queue item", "file", e.Name(), "err", err)
			continue
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].EnqueuedAtMs < out[j].EnqueuedAtMs })
	return out, nil
}
