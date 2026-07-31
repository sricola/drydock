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
// only ever move forward through validTransition; the three terminal states
// (completed, dead_letter, cancelled) are sinks.
type QueueState string

const (
	QueueQueued         QueueState = "queued"
	QueuePreparing      QueueState = "preparing"
	QueueRunning        QueueState = "running"
	QueueVerifying      QueueState = "verifying"
	QueueAwaitingReview QueueState = "awaiting_review"
	QueueCompleted      QueueState = "completed"
	QueueDeadLetter     QueueState = "dead_letter"
	QueueCancelled      QueueState = "cancelled"
)

// Terminal reports whether s is a sink state — no further transitions.
func (s QueueState) Terminal() bool {
	switch s {
	case QueueCompleted, QueueDeadLetter, QueueCancelled:
		return true
	}
	return false
}

// validTransition is the single source of truth for the queue state machine.
// Terminal states are deliberately absent: they have no outgoing edges.
// running -> completed exists because some lifecycle terminals are normal
// finishes that never enter verify or review: a run that produced no diff
// (no_diff) and a plan-only run (planned) both complete straight from running.
var validTransition = map[QueueState][]QueueState{
	QueueQueued:         {QueuePreparing, QueueCancelled},
	QueuePreparing:      {QueueRunning, QueueDeadLetter, QueueCancelled},
	QueueRunning:        {QueueVerifying, QueueAwaitingReview, QueueCompleted, QueueDeadLetter, QueueCancelled},
	QueueVerifying:      {QueueAwaitingReview, QueueDeadLetter, QueueCancelled},
	QueueAwaitingReview: {QueueCompleted, QueueDeadLetter, QueueCancelled},
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
