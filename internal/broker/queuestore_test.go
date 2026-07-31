package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/egress"
)

const testQueueID = "0123456789abcdef0123456789abcdef"

func sampleQueueItem() QueueItem {
	return QueueItem{
		ID: testQueueID,
		Task: Task{
			RepoRef:     "https://github.com/example/repo.git",
			Instruction: "fix the flaky test",
			EgressExtra: []egress.Domain{{Host: "pypi.org", Ports: []int{443}}},
			Sensitive:   true,
			AutoApprove: true, // the gate marker omits this; the queue item must not
			Platform:    "github",
			Model:       "claude-sonnet-4-5",
			Agent:       "claude",
			Draft:       true,
			PlanOnly:    true,
			IssueURL:    "https://github.com/example/repo/issues/7",
		},
		State:        QueueQueued,
		EnqueuedAtMs: 1700000000123,
		StartedAtMs:  1700000000456,
		UpdatedAtMs:  1700000000789,
		Attempts:     2,
		LastError:    "previous attempt: vm boot timeout",
	}
}

func TestQueueItemRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := sampleQueueItem()
	if err := writeQueueItem(root, want); err != nil {
		t.Fatalf("writeQueueItem: %v", err)
	}
	// 0600 — queue items carry the full instruction text.
	fi, err := os.Stat(queueMarkerPath(root, want.ID))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
	got, err := readQueueItem(root, want.ID)
	if err != nil {
		t.Fatalf("readQueueItem: %v", err)
	}
	// Field-by-field on the security-relevant Task fields so a silently
	// dropped json tag names itself in the failure.
	if !got.Task.AutoApprove {
		t.Error("AutoApprove not preserved — headless intent must survive a restart")
	}
	if !got.Task.Sensitive {
		t.Error("Sensitive not preserved")
	}
	if got.Task.RepoRef != want.Task.RepoRef ||
		got.Task.Instruction != want.Task.Instruction ||
		got.Task.Platform != want.Task.Platform ||
		got.Task.Model != want.Task.Model ||
		got.Task.Agent != want.Task.Agent ||
		got.Task.Draft != want.Task.Draft ||
		got.Task.PlanOnly != want.Task.PlanOnly ||
		got.Task.IssueURL != want.Task.IssueURL {
		t.Errorf("Task fields mangled: got %+v want %+v", got.Task, want.Task)
	}
	if len(got.Task.EgressExtra) != 1 || got.Task.EgressExtra[0].Host != "pypi.org" {
		t.Errorf("EgressExtra mangled: %+v", got.Task.EgressExtra)
	}
	if got.State != want.State {
		t.Errorf("State = %q, want %q", got.State, want.State)
	}
	if got.EnqueuedAtMs != want.EnqueuedAtMs || got.StartedAtMs != want.StartedAtMs || got.UpdatedAtMs != want.UpdatedAtMs {
		t.Errorf("timestamps mangled: got %d/%d/%d want %d/%d/%d",
			got.EnqueuedAtMs, got.StartedAtMs, got.UpdatedAtMs,
			want.EnqueuedAtMs, want.StartedAtMs, want.UpdatedAtMs)
	}
	if got.Attempts != want.Attempts || got.LastError != want.LastError {
		t.Errorf("Attempts/LastError mangled: %d %q", got.Attempts, got.LastError)
	}
}

func TestWriteQueueItemRejectsBadID(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "short", "../../etc/passwd", strings.Repeat("g", 32), strings.Repeat("A", 32)} {
		it := sampleQueueItem()
		it.ID = id
		if err := writeQueueItem(root, it); err == nil {
			t.Errorf("writeQueueItem accepted id %q", id)
		}
	}
}

func TestReadQueueItemRejectsBadID(t *testing.T) {
	root := t.TempDir()
	// A traversal id must be rejected before any path is built, so plant a
	// decoy outside the root that would be reachable if the guard is missing.
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(root, "audit")
	if err := os.MkdirAll(audit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.queue.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "short", "../outside/evil", strings.Repeat("A", 32), strings.Repeat("z", 31) + "!"} {
		if _, err := readQueueItem(audit, id); err == nil {
			t.Errorf("readQueueItem accepted id %q", id)
		}
	}
}

func TestReadQueueItemRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"id":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, queueMarkerPath(root, testQueueID)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := readQueueItem(root, testQueueID); err == nil {
		t.Error("readQueueItem followed a symlink")
	}
}

func TestRemoveQueueItemIdempotent(t *testing.T) {
	root := t.TempDir()
	it := sampleQueueItem()
	if err := writeQueueItem(root, it); err != nil {
		t.Fatal(err)
	}
	if err := removeQueueItem(root, it.ID); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if _, err := os.Stat(queueMarkerPath(root, it.ID)); !os.IsNotExist(err) {
		t.Fatalf("item still present after remove: %v", err)
	}
	if err := removeQueueItem(root, it.ID); err != nil {
		t.Fatalf("second remove not idempotent: %v", err)
	}
	// A stray .tmp from a crashed atomic write is cleaned up too.
	tmp := queueMarkerPath(root, it.ID) + ".tmp"
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeQueueItem(root, it.ID); err != nil {
		t.Fatalf("remove with stray tmp: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("stray .tmp survived remove: %v", err)
	}
	if err := removeQueueItem(root, "../evil"); err == nil {
		t.Error("removeQueueItem accepted a traversal id")
	}
}

func TestListQueueItemsSortsAndTolerates(t *testing.T) {
	root := t.TempDir()
	ids := []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"cccccccccccccccccccccccccccccccc",
	}
	// Write out of order; expect EnqueuedAtMs order back.
	for i, enq := range []int64{300, 100, 200} {
		it := sampleQueueItem()
		it.ID = ids[i]
		it.EnqueuedAtMs = enq
		if err := writeQueueItem(root, it); err != nil {
			t.Fatal(err)
		}
	}
	// Garbage that must be skipped, not fail the scan.
	if err := os.WriteFile(filepath.Join(root, "dddddddddddddddddddddddddddddddd.queue.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Non-queue files in the same audit dir are ignored.
	if err := os.WriteFile(filepath.Join(root, ids[0]+".gate.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlinked queue file is refused (O_NOFOLLOW) and skipped.
	real := filepath.Join(root, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.queue.json")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := listQueueItems(root)
	if err != nil {
		t.Fatalf("listQueueItems: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (garbage and symlink skipped): %+v", len(got), got)
	}
	wantOrder := []string{ids[1], ids[2], ids[0]} // enq 100, 200, 300
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("got[%d].ID = %s, want %s (EnqueuedAtMs order)", i, got[i].ID, w)
		}
	}
}

func TestListQueueItemsEmptyOrMissingDir(t *testing.T) {
	root := t.TempDir()
	got, err := listQueueItems(root)
	if err != nil || len(got) != 0 {
		t.Errorf("empty dir: got %v, %v", got, err)
	}
	got, err = listQueueItems(filepath.Join(root, "nope"))
	if err != nil || len(got) != 0 {
		t.Errorf("missing dir: got %v, %v", got, err)
	}
}

func TestQueueStateTerminal(t *testing.T) {
	terminals := map[QueueState]bool{
		QueueCompleted:      true,
		QueueDeadLetter:     true,
		QueueCancelled:      true,
		QueueCIFailed:       true,
		QueueQueued:         false,
		QueuePreparing:      false,
		QueueRunning:        false,
		QueueVerifying:      false,
		QueueAwaitingReview: false,
		QueueAwaitingCI:     false,
		QueueState("bogus"): false,
	}
	for s, want := range terminals {
		if got := s.Terminal(); got != want {
			t.Errorf("%q.Terminal() = %v, want %v", s, got, want)
		}
	}
}

// allQueueStates is every declared state, so the table test below is exhaustive
// by construction: a new state added to queuestore.go without a row in `valid`
// makes every edge out of it assert "forbidden", which is the safe direction.
var allQueueStates = []QueueState{
	QueueQueued, QueuePreparing, QueueRunning, QueueVerifying,
	QueueAwaitingReview, QueueAwaitingCI,
	QueueCompleted, QueueCIFailed, QueueDeadLetter, QueueCancelled,
}

func TestQueueStateCanTransitionTo(t *testing.T) {
	valid := map[QueueState][]QueueState{
		// queued -> dead_letter: the dispatcher DROPS a broker-initiated CI
		// retry whose vendor spend cap exhausted before it could dispatch,
		// rather than parking it (dropSpendCappedRetryLocked). A
		// human-submitted item still parks.
		QueueQueued:    {QueuePreparing, QueueDeadLetter, QueueCancelled},
		QueuePreparing: {QueueRunning, QueueDeadLetter, QueueCancelled},
		// running -> completed: no_diff and plan-only runs finish without
		// ever entering verify or review.
		QueueRunning:        {QueueVerifying, QueueAwaitingReview, QueueCompleted, QueueDeadLetter, QueueCancelled},
		QueueVerifying:      {QueueAwaitingReview, QueueDeadLetter, QueueCancelled},
		QueueAwaitingReview: {QueueCompleted, QueueAwaitingCI, QueueDeadLetter, QueueCancelled},
		// awaiting_ci -> completed (observed pass / observed no-checks),
		// ci_failed (an OBSERVED failure), dead_letter (the watch ended with
		// NO conclusion — timed out, gave up, or its marker did not survive a
		// restart), cancelled.
		QueueAwaitingCI: {QueueCompleted, QueueCIFailed, QueueDeadLetter, QueueCancelled},
		QueueCompleted:  {},
		QueueCIFailed:   {},
		QueueDeadLetter: {},
		QueueCancelled:  {},
	}
	for _, from := range allQueueStates {
		allowed := map[QueueState]bool{}
		for _, to := range valid[from] {
			allowed[to] = true
		}
		for _, to := range allQueueStates {
			if got := from.CanTransitionTo(to); got != allowed[to] {
				t.Errorf("%q -> %q = %v, want %v", from, to, got, allowed[to])
			}
		}
	}
	// Unknown states can't go anywhere and nothing can reach them.
	if QueueState("bogus").CanTransitionTo(QueueQueued) {
		t.Error("bogus state can transition")
	}
	if QueueQueued.CanTransitionTo(QueueState("bogus")) {
		t.Error("transition to bogus state allowed")
	}
	// Self-transitions are not valid anywhere.
	for _, s := range allQueueStates {
		if s.CanTransitionTo(s) {
			t.Errorf("%q self-transition allowed", s)
		}
	}
}

// TestQueueStateMachineIsForwardOnly is the structural guard the doc comment on
// validTransition asserts, checked mechanically rather than by reading:
//
//   - every terminal state (Terminal() == true) has ZERO outgoing edges, so a
//     concluded item can never be re-opened; and
//   - the graph is acyclic, so no sequence of legal transitions can return an
//     item to a state it already left (which is what would let a retry
//     laundering loop exist, D1).
//
// Both are asserted over the DECLARED table, so adding an edge that creates a
// cycle — awaiting_ci -> queued, say — fails here without anyone remembering to
// extend a case list.
func TestQueueStateMachineIsForwardOnly(t *testing.T) {
	for _, s := range allQueueStates {
		if s.Terminal() && len(validTransition[s]) != 0 {
			t.Errorf("terminal state %q has outgoing edges %v; terminals must be sinks", s, validTransition[s])
		}
	}
	// Depth-first cycle detection over the declared edges (white/grey/black).
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[QueueState]int{}
	var path []QueueState
	var visit func(QueueState)
	visit = func(s QueueState) {
		color[s] = grey
		path = append(path, s)
		for _, next := range validTransition[s] {
			switch color[next] {
			case grey:
				t.Errorf("cycle in the queue state machine: %v -> %q", path, next)
			case white:
				visit(next)
			}
		}
		path = path[:len(path)-1]
		color[s] = black
	}
	for _, s := range allQueueStates {
		if color[s] == white {
			visit(s)
		}
	}
	// Every state named in an edge must be a declared state: a typo'd target
	// would otherwise be an unreachable dead end nothing could ever leave.
	declared := map[QueueState]bool{}
	for _, s := range allQueueStates {
		declared[s] = true
	}
	for from, tos := range validTransition {
		if !declared[from] {
			t.Errorf("validTransition has an edge FROM undeclared state %q", from)
		}
		for _, to := range tos {
			if !declared[to] {
				t.Errorf("validTransition has an edge %q -> undeclared state %q", from, to)
			}
		}
	}
}
