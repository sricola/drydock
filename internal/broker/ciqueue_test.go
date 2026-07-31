package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/audit"
	"drydock/internal/remote"
)

// These tests drive B1 task 3: the CI observation becoming visible on the
// durable queue record and in the task's audit trail.
//
// The invariant every one of them protects, from a different angle:
//
//	AN UNOBSERVED OR ABSENT CI RESULT MUST NEVER READ AS SUCCESS.
//
// `completed` is reachable from awaiting_ci only via an OBSERVED pass or an
// OBSERVED "this PR has no checks". A timeout, a give-up, an unwatchable
// marker, a marker lost to a restart, a watch turned off under a live item, and
// a marker that could not be written all reach dead_letter instead.

// ---- harness ----

// seedQueuedItem writes a durable queue item in the given state, as the
// dispatcher would have left it.
func seedQueuedItem(t *testing.T, b *Broker, st QueueState) QueueItem {
	t.Helper()
	id := newID()
	it := QueueItem{
		ID:           id,
		Task:         Task{RepoRef: "https://github.com/o/r.git", Instruction: "x"},
		State:        QueueQueued,
		EnqueuedAtMs: b.nowMs(),
		UpdatedAtMs:  b.nowMs(),
	}
	// Walk the real state machine to the requested state rather than
	// blind-writing it: the fixture itself then proves the path is legal.
	path := map[QueueState][]QueueState{
		QueueQueued:         {},
		QueuePreparing:      {QueuePreparing},
		QueueRunning:        {QueuePreparing, QueueRunning},
		QueueAwaitingReview: {QueuePreparing, QueueRunning, QueueAwaitingReview},
		QueueAwaitingCI:     {QueuePreparing, QueueRunning, QueueAwaitingReview, QueueAwaitingCI},
	}[st]
	if err := writeQueueItem(b.AuditRoot, it); err != nil {
		t.Fatalf("writeQueueItem: %v", err)
	}
	for _, next := range path {
		mut := func(*QueueItem) {}
		if next == QueueAwaitingCI {
			mut = func(q *QueueItem) { q.PRNumber = 42; q.CIState = string(CIPending) }
		}
		got, err := b.setQueueState(id, next, mut)
		if err != nil {
			t.Fatalf("seed transition -> %s: %v", next, err)
		}
		it = got
	}
	return it
}

// ciAuditRow returns the last broker-authored ci_observation record for id.
func ciAuditRow(t *testing.T, b *Broker, id string) (audit.CIObservation, bool) {
	t.Helper()
	return audit.LastCIObservation(filepath.Join(b.AuditRoot, id+".jsonl"))
}

// ---- the observation -> queue terminal mapping ----

// TestCIQueueTerminal pins the whole mapping in one table. It is the single
// place the honesty rule is expressed, so it gets an exhaustive test including
// a state the code does not model.
func TestCIQueueTerminal(t *testing.T) {
	cases := []struct {
		st       CIState
		want     QueueState
		wantErrS bool // a non-empty LastError explaining the terminal
	}{
		{CIPassed, QueueCompleted, false},
		{CINoChecks, QueueCompleted, false},
		{CIFailed, QueueCIFailed, true},
		{CITimedOut, QueueDeadLetter, true},
		{CIUnknown, QueueDeadLetter, true},
		{CIPending, QueueDeadLetter, true},            // never terminal in practice; must not round to ok
		{CIState("brand_new"), QueueDeadLetter, true}, // a future state fails toward "we don't know"
	}
	for _, c := range cases {
		got, lastErr := ciQueueTerminal(c.st)
		if got != c.want {
			t.Errorf("ciQueueTerminal(%q) = %q, want %q", c.st, got, c.want)
		}
		if (lastErr != "") != c.wantErrS {
			t.Errorf("ciQueueTerminal(%q) LastError = %q, wantNonEmpty=%v", c.st, lastErr, c.wantErrS)
		}
		if got == QueueCompleted && c.st != CIPassed && c.st != CINoChecks {
			t.Errorf("ciQueueTerminal(%q) reached completed — only an OBSERVED pass or no_checks may", c.st)
		}
		if got == QueueCIFailed && c.st != CIFailed {
			t.Errorf("ciQueueTerminal(%q) reached ci_failed — only an OBSERVED failure may", c.st)
		}
	}
}

// TestApplyCIObservation_DrivesTheQueueTerminal is the core behaviour: an
// awaiting_ci item lands on the right terminal for each observation, with the
// observed CI state recorded alongside it.
func TestApplyCIObservation_DrivesTheQueueTerminal(t *testing.T) {
	cases := []struct {
		name      string
		state     CIState
		want      QueueState
		wantError bool
	}{
		{"observed pass completes", CIPassed, QueueCompleted, false},
		{"observed no_checks completes", CINoChecks, QueueCompleted, false},
		{"OBSERVED failure is ci_failed", CIFailed, QueueCIFailed, true},
		{"timeout is an honest dead_letter, never completed", CITimedOut, QueueDeadLetter, true},
		{"give-up is an honest dead_letter, never completed", CIUnknown, QueueDeadLetter, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &Broker{AuditRoot: t.TempDir()}
			it := seedQueuedItem(t, b, QueueAwaitingCI)
			b.applyCIObservation(CIObservation{
				TaskID: it.ID, PRNumber: 42, State: c.state,
				Summary:      remote.CheckSummary{Total: 2, Passed: 1, Failed: 1},
				ObservedAtMs: 1700000000000,
			})
			got := queueItemState(t, b, it.ID)
			if got.State != c.want {
				t.Fatalf("state = %q, want %q", got.State, c.want)
			}
			if !got.State.Terminal() {
				t.Errorf("state %q is not terminal; the item would hang", got.State)
			}
			if got.CIState != string(c.state) {
				t.Errorf("ci_state = %q, want %q", got.CIState, c.state)
			}
			if got.PRNumber != 42 {
				t.Errorf("pr_number = %d, want 42", got.PRNumber)
			}
			if (got.LastError != "") != c.wantError {
				t.Errorf("last_error = %q, wantNonEmpty=%v", got.LastError, c.wantError)
			}
		})
	}
}

// TestApplyCIObservation_UnobservedNeverReadsAsSuccess is the headline
// assertion stated once, directly: of every terminal a watch can end in, only
// the two that carry POSITIVE evidence reach `completed`.
func TestApplyCIObservation_UnobservedNeverReadsAsSuccess(t *testing.T) {
	for _, st := range []CIState{CIFailed, CITimedOut, CIUnknown, CIPending, CIState("weird")} {
		b := &Broker{AuditRoot: t.TempDir()}
		it := seedQueuedItem(t, b, QueueAwaitingCI)
		b.applyCIObservation(CIObservation{TaskID: it.ID, State: st, ObservedAtMs: 1})
		if got := queueItemState(t, b, it.ID); got.State == QueueCompleted {
			t.Errorf("observation %q landed on completed — an unobserved CI outcome read as success", st)
		}
	}
}

// TestApplyCIObservation_SynchronousTaskNoOps: the CRITICAL no-queue-item case.
// A synchronous POST /tasks task has no <id>.queue.json at all, exactly like
// finalizeQueuedResume's contract. The hook must not error, must not create a
// queue file, and must still record the observation in the audit.
func TestApplyCIObservation_SynchronousTaskNoOps(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	id := newID()
	auditPath := filepath.Join(b.AuditRoot, id+".jsonl")
	if err := os.WriteFile(auditPath,
		[]byte(`{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.25,"src":"broker"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	b.applyCIObservation(CIObservation{TaskID: id, PRNumber: 7, State: CIFailed, ObservedAtMs: 5})

	if _, err := readQueueItem(b.AuditRoot, id); err == nil {
		t.Fatal("a queue item was created for a synchronous task")
	}
	files, _ := filepath.Glob(filepath.Join(b.AuditRoot, "*.queue.json"))
	if len(files) != 0 {
		t.Fatalf("queue files = %v, want none", files)
	}
	// The observation is still recorded, with no queue_state (there is no item).
	rec, ok := ciAuditRow(t, b, id)
	if !ok {
		t.Fatal("no ci_observation row for a synchronous task")
	}
	if rec.QueueState != "" {
		t.Errorf("queue_state = %q, want empty for a task with no queue item", rec.QueueState)
	}
	if rec.State != string(CIFailed) || rec.PRNumber != 7 {
		t.Errorf("record = %+v", rec)
	}
}

// TestApplyCIObservation_IsIdempotent: concludeCIWatch persists, then hooks,
// then removes the marker, so a crash in the last gap REPLAYS the observation
// at the next boot. A replay must not double-write either surface.
func TestApplyCIObservation_IsIdempotent(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	it := seedQueuedItem(t, b, QueueAwaitingCI)
	auditPath := filepath.Join(b.AuditRoot, it.ID+".jsonl")
	if err := os.WriteFile(auditPath, []byte(`{"type":"result","subtype":"success","src":"broker"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := CIObservation{TaskID: it.ID, PRNumber: 42, State: CIFailed, ObservedAtMs: 9}

	b.applyCIObservation(obs)
	b.applyCIObservation(obs)
	b.applyCIObservation(obs)

	if got := queueItemState(t, b, it.ID); got.State != QueueCIFailed {
		t.Fatalf("state = %q, want ci_failed", got.State)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"ci_observation"`); n != 1 {
		t.Errorf("ci_observation rows = %d, want exactly 1 after 3 replays", n)
	}
}

// TestApplyCIObservation_TerminalItemIsNotReopened: the forward-only machine is
// the backstop. An item already cancelled (an operator killed it) must not be
// dragged back into a CI terminal by a late observation.
func TestApplyCIObservation_TerminalItemIsNotReopened(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	it := seedQueuedItem(t, b, QueueAwaitingCI)
	if _, err := b.setQueueState(it.ID, QueueCancelled, nil); err != nil {
		t.Fatalf("seed cancel: %v", err)
	}
	b.applyCIObservation(CIObservation{TaskID: it.ID, State: CIPassed, ObservedAtMs: 3})
	if got := queueItemState(t, b, it.ID); got.State != QueueCancelled {
		t.Errorf("state = %q, want cancelled (a terminal must stay sealed)", got.State)
	}
}

// ---- the audit-ordering decision ----

// TestCIObservationRow_DoesNotDisturbTheAggregateReseed is the test the audit
// decision has to survive. seedAggregateFromAudit (cmd/brokerd) reseeds the
// rolling spend cap from the LAST {"type":"result"} line's broker-authored
// total_cost_usd. Appending the CI observation as a `result` row would have
// made that row the last one and either erased the task's metered spend (cost
// 0) or restated a number it never measured. Since it is its own record type,
// LastResult / LastMetricsFile / LastResultAndMetricsFile all still select
// exactly the rows they selected before.
func TestCIObservationRow_DoesNotDisturbTheAggregateReseed(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	it := seedQueuedItem(t, b, QueueAwaitingCI)
	auditPath := filepath.Join(b.AuditRoot, it.ID+".jsonl")
	fixture := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.25,"num_turns":3,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"` + it.ID + `","agent":"claude","vendor":"anthropic","outcome":"pushed","cost_usd":0.25}
`
	if err := os.WriteFile(auditPath, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	before, okBefore := audit.LastResult(auditPath, int64(len(fixture)))

	b.applyCIObservation(CIObservation{TaskID: it.ID, PRNumber: 42, State: CIFailed, ObservedAtMs: 11})

	fi, err := os.Stat(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	after, okAfter := audit.LastResult(auditPath, fi.Size())
	if !okBefore || !okAfter {
		t.Fatalf("result row lookup: before=%v after=%v", okBefore, okAfter)
	}
	if after != before {
		t.Fatalf("the last result row CHANGED after the ci observation:\n before %+v\n after  %+v", before, after)
	}
	// The seed filter's exact predicate: broker-authored, positive cost.
	if after.Src != "broker" || after.TotalCostUSD != 0.25 {
		t.Errorf("aggregate seed would read src=%q cost=%v, want broker/0.25", after.Src, after.TotalCostUSD)
	}
	// The metrics row and the single-pass combined reader are equally unmoved.
	f, err := audit.OpenRead(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, hasM := audit.LastMetricsFile(f)
	if !hasM || m.Outcome != "pushed" || m.CostUSD != 0.25 {
		t.Errorf("LastMetricsFile = %+v (ok=%v), want the unchanged broker metrics row", m, hasM)
	}
	r2, okR2, m2, okM2 := audit.LastResultAndMetricsFile(f)
	if !okR2 || !okM2 || r2 != after || m2.Outcome != "pushed" {
		t.Errorf("LastResultAndMetricsFile drifted: %+v/%v %+v/%v", r2, okR2, m2, okM2)
	}
	// And the operator-facing classification is unchanged: the TASK pushed
	// cleanly. The CI verdict lives on the queue item, not on the task's
	// outcome key.
	if key := audit.OutcomeKeyWithMetrics(r2, okR2, m2, okM2); key != "ok" {
		t.Errorf("outcome key = %q, want ok — a CI failure must not relabel a successful push", key)
	}
}

// TestCIObservationRow_ShapeAndSanitization: the record is broker-authored,
// carries the observed conclusion and the counts, carries NO CI log text (D3),
// and every reflected string goes through the sanitizer.
func TestCIObservationRow_ShapeAndSanitization(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	it := seedQueuedItem(t, b, QueueAwaitingCI)
	auditPath := filepath.Join(b.AuditRoot, it.ID+".jsonl")
	if err := os.WriteFile(auditPath, []byte(`{"type":"result","subtype":"success","src":"broker"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.applyCIObservation(CIObservation{
		TaskID: it.ID, PRNumber: 42,
		PRURL:   "https://github.com/o/r/pull/42\x1b[31mBOOM\x07",
		State:   CITimedOut,
		Detail:  "watch deadline expired\x1b[2Jwith no terminal CI conclusion",
		Summary: remote.CheckSummary{Total: 3, Passed: 1, Failed: 0, Pending: 2},

		ObservedAtMs: 1700000000123,
	})
	rec, ok := ciAuditRow(t, b, it.ID)
	if !ok {
		t.Fatal("no ci_observation row")
	}
	if rec.Type != "ci_observation" || rec.Src != "broker" {
		t.Errorf("type/src = %q/%q, want ci_observation/broker", rec.Type, rec.Src)
	}
	if rec.State != string(CITimedOut) || rec.QueueState != string(QueueDeadLetter) {
		t.Errorf("state/queue_state = %q/%q", rec.State, rec.QueueState)
	}
	if rec.Checks != 3 || rec.Passed != 1 || rec.Pending != 2 {
		t.Errorf("counts = %d/%d/%d", rec.Checks, rec.Passed, rec.Pending)
	}
	if rec.ObservedAtMs != 1700000000123 {
		t.Errorf("observed_at_ms = %d", rec.ObservedAtMs)
	}
	for _, s := range []string{rec.PRURL, rec.Detail} {
		if strings.ContainsAny(s, "\x1b\x07") {
			t.Errorf("unsanitized control bytes reached the audit record: %q", s)
		}
	}
	// The raw bytes must not contain an escape either (the JSON encoder would
	// escape them rather than emit them, so check the decoded strings AND the
	// literal file).
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\\u001b") || strings.Contains(string(data), "BOOM\\u0007") {
		t.Errorf("audit line carries escape sequences: %s", data)
	}
	// D3: there is no field on the record a repository's workflow LOG could
	// reach. Assert structurally on the marshalled key set.
	var generic map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var probe map[string]any
		if json.Unmarshal([]byte(line), &probe) == nil && probe["type"] == "ci_observation" {
			generic = probe
		}
	}
	if generic == nil {
		t.Fatal("could not re-read the ci_observation line")
	}
	for _, forbidden := range []string{"log", "logs", "output", "annotations", "description", "text", "body"} {
		if _, present := generic[forbidden]; present {
			t.Errorf("ci_observation carries a %q field — CI log text must never be recorded (D3)", forbidden)
		}
	}
}

// ---- arming ----

// TestArmCIWatch_MovesAwaitingReviewToAwaitingCI is the transition wiring.
func TestArmCIWatch_MovesAwaitingReviewToAwaitingCI(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	it := seedQueuedItem(t, b, QueueAwaitingReview)
	if !b.armCIWatch(it.ID, 42) {
		t.Fatal("armCIWatch declined an awaiting_review item")
	}
	got := queueItemState(t, b, it.ID)
	if got.State != QueueAwaitingCI {
		t.Fatalf("state = %q, want awaiting_ci", got.State)
	}
	if got.PRNumber != 42 || got.CIState != string(CIPending) {
		t.Errorf("pr_number/ci_state = %d/%q, want 42/pending", got.PRNumber, got.CIState)
	}
	if !b.ciOwnsTerminal(it.ID) {
		t.Error("ciOwnsTerminal false after arming — runQueued would race the watch to completed")
	}
}

// TestArmCIWatch_NoQueueItemIsSilentlyFine: the synchronous path. There is
// nothing to arm and the watch is still armed (the marker gets written).
func TestArmCIWatch_NoQueueItemIsSilentlyFine(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	id := newID()
	if !b.armCIWatch(id, 42) {
		t.Fatal("armCIWatch declined a task with no queue item; the synchronous path must still watch")
	}
	if b.ciOwnsTerminal(id) {
		t.Error("ciOwnsTerminal true for a task with no queue item")
	}
}

// TestArmCIWatch_DeclinesWhenTheItemCannotBeMoved: an item that is not in
// awaiting_review (here: already cancelled) cannot be armed, and the caller is
// told so, so no marker is written over an item the watch could never
// finalize.
func TestArmCIWatch_DeclinesWhenTheItemCannotBeMoved(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	it := seedQueuedItem(t, b, QueueAwaitingReview)
	if _, err := b.setQueueState(it.ID, QueueCancelled, nil); err != nil {
		t.Fatalf("seed cancel: %v", err)
	}
	if b.armCIWatch(it.ID, 42) {
		t.Fatal("armCIWatch armed a cancelled item")
	}
	if got := queueItemState(t, b, it.ID); got.State != QueueCancelled || got.CIState != "" {
		t.Errorf("item = %q/%q, want cancelled with no ci_state", got.State, got.CIState)
	}
}

// ---- ResumeQueue ----

// TestResumeQueue_AwaitingCIWithSurvivingMarker: the marker is the watch's
// only cross-restart state, so an awaiting_ci item whose marker survived is
// LEFT ALONE — StartCIWatch's first pass re-watches it. And it is never
// re-dispatched: the item already pushed.
func TestResumeQueue_AwaitingCIWithSurvivingMarker(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir(), StageRoot: t.TempDir(), CIWatch: true}
	it := seedQueuedItem(t, b, QueueAwaitingCI)
	seedMarker(t, b, ciMarker{TaskID: it.ID})

	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}

	if got := queueItemState(t, b, it.ID); got.State != QueueAwaitingCI {
		t.Fatalf("state = %q, want awaiting_ci (the surviving marker owns the re-drive)", got.State)
	}
	if markerGone(t, b, it.ID) {
		t.Error("the surviving ci marker was removed; the watch would never re-drive")
	}
	b.queueMu.Lock()
	n := len(b.queue)
	b.queueMu.Unlock()
	if n != 0 {
		t.Errorf("in-memory queue has %d items; an awaiting_ci item must NEVER be re-dispatched (it already pushed)", n)
	}
}

// TestResumeQueue_AwaitingCIWithoutMarker: no marker means nothing will ever
// conclude the watch. The item must reach an honest terminal instead of
// hanging forever — and that terminal is NOT completed.
func TestResumeQueue_AwaitingCIWithoutMarker(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir(), StageRoot: t.TempDir(), CIWatch: true}
	it := seedQueuedItem(t, b, QueueAwaitingCI)

	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}

	got := queueItemState(t, b, it.ID)
	if got.State == QueueCompleted {
		t.Fatal("an awaiting_ci item with no marker COMPLETED — absence of evidence read as success")
	}
	if got.State != QueueDeadLetter {
		t.Fatalf("state = %q, want dead_letter", got.State)
	}
	if !got.State.Terminal() {
		t.Error("the item is not terminal; it would hang forever")
	}
	if got.CIState != string(CIUnknown) {
		t.Errorf("ci_state = %q, want unknown", got.CIState)
	}
	if !strings.Contains(got.LastError, "no CI conclusion") {
		t.Errorf("last_error = %q, want an explicit no-conclusion reason", got.LastError)
	}
	b.queueMu.Lock()
	n := len(b.queue)
	b.queueMu.Unlock()
	if n != 0 {
		t.Errorf("in-memory queue has %d items; an awaiting_ci item must NEVER be re-dispatched", n)
	}
}

// TestResumeQueue_AwaitingCIWithWatchDisabled: the marker survived but the
// operator turned ci.watch off. Nothing will poll it, so leaving the item
// parked would strand it. It terminates honestly and the orphaned marker is
// removed (it is deliberately not prunable by age).
func TestResumeQueue_AwaitingCIWithWatchDisabled(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir(), StageRoot: t.TempDir(), CIWatch: false}
	it := seedQueuedItem(t, b, QueueAwaitingCI)
	seedMarker(t, b, ciMarker{TaskID: it.ID})

	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}

	got := queueItemState(t, b, it.ID)
	if got.State != QueueDeadLetter {
		t.Fatalf("state = %q, want dead_letter", got.State)
	}
	if !strings.Contains(got.LastError, "disabled") {
		t.Errorf("last_error = %q, want it to name the disabled watch", got.LastError)
	}
	if !markerGone(t, b, it.ID) {
		t.Error("the orphaned ci marker was left behind; nothing else would ever remove it")
	}
}

// TestResumeQueue_OtherStatesUnaffectedByTheCIBranch guards the states the CI
// arc must not have changed: a queued item still re-enqueues, and an
// awaiting_review item with no gate marker still dead-letters — even with a
// stray CI marker sitting next to it.
func TestResumeQueue_OtherStatesUnaffectedByTheCIBranch(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir(), StageRoot: t.TempDir(), CIWatch: true}
	queued := seedQueuedItem(t, b, QueueQueued)
	review := seedQueuedItem(t, b, QueueAwaitingReview)
	seedMarker(t, b, ciMarker{TaskID: review.ID}) // a CI marker is NOT a gate marker

	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	if got := queueItemState(t, b, queued.ID); got.State != QueueQueued {
		t.Errorf("queued item = %q, want still queued", got.State)
	}
	b.queueMu.Lock()
	n := len(b.queue)
	b.queueMu.Unlock()
	if n != 1 {
		t.Errorf("in-memory queue = %d items, want the 1 still-queued item", n)
	}
	if got := queueItemState(t, b, review.ID); got.State != QueueDeadLetter {
		t.Errorf("awaiting_review with no GATE marker = %q, want dead_letter", got.State)
	}
}

// ---- end to end ----

// TestQueue_PushedUnderCIWatchParksInAwaitingCI is the wiring proof: a queued
// task that pushes with the watch armed does NOT go straight to completed. It
// parks in awaiting_ci, and only the observation finishes it.
func TestQueue_PushedUnderCIWatchParksInAwaitingCI(t *testing.T) {
	for _, c := range []struct {
		name  string
		state CIState
		sum   remote.CheckSummary
		want  QueueState
	}{
		{"passing CI completes", CIPassed, passing(), QueueCompleted},
		{"failing CI is ci_failed", CIFailed, failing(), QueueCIFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
			b.CIWatch = true
			b.CIPollInterval = 200 * time.Microsecond
			b.CIWatchTimeout = time.Hour
			b.ciEnvFn = func() []string { return []string{"PATH=/usr/bin"} }
			b.newAdapter = func(string, string) remote.Adapter {
				return &capturingAdapter{fakeAdapter: fakeAdapter{name: "github"}, pr: prIdentity(42, "o", "r")}
			}
			blocked := make(chan struct{})
			b.checksFn = func([]string, string, string, int) (remote.CheckSummary, error) {
				<-blocked
				return c.sum, nil
			}

			id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			b.StartDispatcher()
			defer b.StopDispatcher()
			b.StartCIWatch()
			defer stopWatch(t, b)

			// The push lands and the item parks — it must NOT be completed.
			waitForQueueState(t, b, id, QueueAwaitingCI)
			it := queueItemState(t, b, id)
			if it.PRNumber != 42 || it.CIState != string(CIPending) {
				t.Errorf("parked item pr/ci_state = %d/%q, want 42/pending", it.PRNumber, it.CIState)
			}
			// Give runQueued's terminal mapping every chance to fire and be wrong.
			time.Sleep(20 * time.Millisecond)
			if got := queueItemState(t, b, id); got.State != QueueAwaitingCI {
				t.Fatalf("state = %q after the lifecycle returned, want it to stay awaiting_ci", got.State)
			}

			close(blocked)
			waitForQueueState(t, b, id, c.want)
			final := queueItemState(t, b, id)
			if final.CIState != string(c.state) {
				t.Errorf("final ci_state = %q, want %q", final.CIState, c.state)
			}
			if !waitFor(5*time.Second, func() bool { return markerGone(t, b, id) }) {
				t.Error("the ci marker was not removed after a terminal observation")
			}
			// The task's own audit still classifies as a clean push.
			f, err := audit.OpenRead(filepath.Join(b.AuditRoot, id+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			r, okR, m, okM := audit.LastResultAndMetricsFile(f)
			if key := audit.OutcomeKeyWithMetrics(r, okR, m, okM); key != "ok" {
				t.Errorf("task outcome key = %q, want ok — the task pushed; only its PR's CI is at issue", key)
			}
			rec, ok := audit.LastCIObservationFile(f)
			if !ok || rec.State != string(c.state) || rec.QueueState != string(c.want) {
				t.Errorf("ci_observation row = %+v (ok=%v)", rec, ok)
			}
		})
	}
}

// TestQueue_WatchOffStillCompletesImmediately is the back-compat half: with
// ci.watch off (the default) a queued push completes exactly as before, with
// no CI fields on the item at all.
func TestQueue_WatchOffStillCompletesImmediately(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	b.newAdapter = func(string, string) remote.Adapter {
		return &capturingAdapter{fakeAdapter: fakeAdapter{name: "github"}, pr: prIdentity(42, "o", "r")}
	}
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	waitForQueueState(t, b, id, QueueCompleted)
	it := queueItemState(t, b, id)
	if it.CIState != "" || it.PRNumber != 0 {
		t.Errorf("watch-off item carries ci_state=%q pr=%d, want neither", it.CIState, it.PRNumber)
	}
	if files, _ := filepath.Glob(filepath.Join(b.AuditRoot, "*.ci.json")); len(files) != 0 {
		t.Errorf("ci markers = %v, want none with the watch off", files)
	}
}

// TestQueue_MarkerWriteFailureUnwindsToAnHonestTerminal: the item was armed
// and then the marker could not be persisted, so nothing will ever conclude
// the watch. It must not sit in awaiting_ci, and it must not quietly complete
// — the operator asked for a CI watch and did not get one. Sabotage is a
// directory planted at the marker path (atomicfile's rename then fails).
func TestQueue_MarkerWriteFailureUnwindsToAnHonestTerminal(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	b.CIWatch = true
	b.newAdapter = func(string, string) remote.Adapter {
		ad := &capturingAdapter{fakeAdapter: fakeAdapter{name: "github"}, pr: prIdentity(42, "o", "r")}
		ad.onResult = func(r remote.Request) {
			id := strings.TrimPrefix(r.Branch, "agent/")
			if err := os.Mkdir(filepath.Join(b.AuditRoot, id+".ci.json"), 0o700); err != nil {
				t.Errorf("planting the sabotage dir: %v", err)
			}
		}
		return ad
	}
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()

	waitForQueueState(t, b, id, QueueDeadLetter)
	got := queueItemState(t, b, id)
	if got.CIState != string(CIUnknown) {
		t.Errorf("ci_state = %q, want unknown", got.CIState)
	}
	if !strings.Contains(got.LastError, "never watched") {
		t.Errorf("last_error = %q, want it to say the push was never watched", got.LastError)
	}
}

// TestQueue_NonPushOutcomeIsUnaffected: only a landed push arms the watch, so
// a failing run still dead-letters on the unchanged path.
func TestQueue_NonPushOutcomeIsUnaffected(t *testing.T) {
	b := queueBroker(t, 2, func(context.Context, []string, io.Writer, io.Writer) error {
		return errors.New("container exited 1")
	})
	b.CIWatch = true
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	waitForQueueState(t, b, id, QueueDeadLetter)
	if it := queueItemState(t, b, id); it.CIState != "" {
		t.Errorf("ci_state = %q on a task that never pushed", it.CIState)
	}
}
