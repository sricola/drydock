package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"drydock/internal/config"
	"drydock/internal/trustbrief"
)

// These tests drive the second-look acknowledgment flow (config
// diff_policy.second_look_paths): a diff touching a second-look path still
// reaches the human gate, but the approve must explicitly acknowledge each
// flagged category. The fail-safe direction is ALWAYS "stay pending / do not
// push": an approve with missing acks returns 422, never signals the gate,
// and the task remains approvable with a corrected request.

// workflowDiff touches a CI workflow, which trustbrief.Analyze flags as
// kind "ci-workflow" — the canonical second-look category in these tests.
const workflowDiff = "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n" +
	"@@ -1 +1 @@\n" +
	"-old\n" +
	"+new\n"

// approveWithAcks POSTs /admin/approve/{id} with an {"acknowledge":[...]}
// body and returns the recorder so callers can assert 204 or 422.
func approveWithAcks(t *testing.T, b *Broker, id string, acks []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"acknowledge": acks})
	if err != nil {
		t.Fatalf("marshal acks: %v", err)
	}
	r := httptest.NewRequest("POST", "/admin/approve/"+id, strings.NewReader(string(body)))
	r.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, r)
	return rr
}

// approveWithBody POSTs a raw approve body (for malformed-JSON cases).
func approveWithBody(t *testing.T, b *Broker, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/admin/approve/"+id, strings.NewReader(body))
	r.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, r)
	return rr
}

// stillPending asserts the task is (still) registered at the approval gate.
func stillPending(t *testing.T, b *Broker, id string) {
	t.Helper()
	b.pendingMu.Lock()
	_, ok := b.pending[id]
	b.pendingMu.Unlock()
	if !ok {
		t.Fatal("task no longer in b.pending; a refused approve must keep the gate open")
	}
}

// submitGated submits a task expected to park at the approval gate and returns
// the recorder, the done channel, and the pending task id.
func submitGated(t *testing.T, b *Broker) (*httptest.ResponseRecorder, chan struct{}, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()
	id := waitForPending(t, b)
	return rec, done, id
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return")
	}
}

func TestSecondLook_RequiredAcksSurfacedInGateEvent(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: workflowDiff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**"}}

	rec, done, id := submitGated(t, b)

	// The operator-facing TaskState must carry the categories while pending
	// (this is what HandleTasks serves to `drydock tasks` / the web UI).
	b.pendingMu.Lock()
	var gotState []string
	if ts, ok := b.tasks[id]; ok {
		gotState = ts.SecondLook
	}
	b.pendingMu.Unlock()
	if len(gotState) != 1 || gotState[0] != "ci-workflow" {
		t.Errorf("TaskState.SecondLook = %v, want [ci-workflow]", gotState)
	}

	if rr := approveWithAcks(t, b, id, []string{"ci-workflow"}); rr.Code != http.StatusNoContent {
		t.Fatalf("approve with acks code=%d, want 204 (body=%s)", rr.Code, rr.Body)
	}
	waitDone(t, done)

	events, term := parseEvents(rec.Body.String())
	var gateEv map[string]any
	for _, ev := range events {
		if ev["event"] == "stage" && ev["stage"] == "awaiting_approval" {
			gateEv = ev
		}
	}
	if gateEv == nil {
		t.Fatalf("no awaiting_approval event; body=%s", rec.Body)
	}
	sl, ok := gateEv["second_look"].([]any)
	if !ok || len(sl) != 1 || sl[0] != "ci-workflow" {
		t.Errorf("awaiting_approval second_look = %v, want [ci-workflow]", gateEv["second_look"])
	}
	if term["outcome"] != "pushed" {
		t.Errorf("terminal=%v, want pushed", term)
	}
}

// The red-team fail-safe test: an approve that does not acknowledge the
// required categories must be refused with 422, must NOT push, and must leave
// the task pending so a corrected approve can still succeed.
func TestSecondLook_ApproveWithoutAcksRefusedStaysPending(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: workflowDiff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**"}}

	rec, done, id := submitGated(t, b)

	// Bare approve (no body at all — the pre-second-look client shape).
	r := httptest.NewRequest("POST", "/admin/approve/"+id, nil)
	r.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, r)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ack-less approve code=%d, want 422", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "ci-workflow") {
		t.Errorf("422 body=%q, want it to name the missing category ci-workflow", body)
	}
	if st.pushed.Load() {
		t.Fatal("stage.Push called after a refused approve — the fail-safe is broken")
	}
	stillPending(t, b, id)

	// Wrong category acknowledged: still refused, still pending.
	if rr := approveWithAcks(t, b, id, []string{"lockfile"}); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong-ack approve code=%d, want 422", rr.Code)
	}
	if st.pushed.Load() {
		t.Fatal("stage.Push called after a wrong-category approve")
	}
	stillPending(t, b, id)

	// A corrected approve WITH the ack must still work: the gate stayed open.
	if rr := approveWithAcks(t, b, id, []string{"ci-workflow"}); rr.Code != http.StatusNoContent {
		t.Fatalf("corrected approve code=%d, want 204 (body=%s)", rr.Code, rr.Body)
	}
	waitDone(t, done)
	_, term := parseEvents(rec.Body.String())
	if term["outcome"] != "pushed" {
		t.Errorf("terminal=%v, want pushed after the corrected approve", term)
	}
	if !st.pushed.Load() {
		t.Error("stage.Push not called after the corrected approve")
	}
}

func TestSecondLook_MalformedAckBodyFailsClosed(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: workflowDiff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**"}}

	rec, done, id := submitGated(t, b)

	if rr := approveWithBody(t, b, id, `{"acknowledge": not-json`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed approve code=%d, want 422 (fail closed, not 500)", rr.Code)
	}
	if st.pushed.Load() {
		t.Fatal("stage.Push called after a malformed approve")
	}
	stillPending(t, b, id)

	if rr := approveWithAcks(t, b, id, []string{"ci-workflow"}); rr.Code != http.StatusNoContent {
		t.Fatalf("recovery approve code=%d, want 204", rr.Code)
	}
	waitDone(t, done)
	_, term := parseEvents(rec.Body.String())
	if term["outcome"] != "pushed" {
		t.Errorf("terminal=%v, want pushed", term)
	}
}

func TestSecondLook_ApproveWithAcksPushes(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: workflowDiff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**"}}

	rec, done, id := submitGated(t, b)
	if rr := approveWithAcks(t, b, id, []string{"ci-workflow"}); rr.Code != http.StatusNoContent {
		t.Fatalf("approve code=%d, want 204 (body=%s)", rr.Code, rr.Body)
	}
	waitDone(t, done)

	_, term := parseEvents(rec.Body.String())
	if term["event"] != "result" || term["outcome"] != "pushed" {
		t.Errorf("terminal=%v, want result/pushed", term)
	}
	if !st.pushed.Load() {
		t.Error("stage.Push not called after an acknowledged approve")
	}
}

func TestSecondLook_NoConfigNoAcksNeeded(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: workflowDiff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	// SecondLookPaths empty: feature off — the flagged workflow file needs no ack.

	rec, done, id := submitGated(t, b)
	approve(t, b, id) // the pre-existing empty-body approve helper must keep working
	waitDone(t, done)

	events, term := parseEvents(rec.Body.String())
	if term["outcome"] != "pushed" {
		t.Errorf("terminal=%v, want pushed", term)
	}
	for _, ev := range events {
		if ev["event"] == "stage" && ev["stage"] == "awaiting_approval" {
			if _, present := ev["second_look"]; present {
				t.Errorf("awaiting_approval carries second_look=%v with the feature off", ev["second_look"])
			}
		}
	}
	if !st.pushed.Load() {
		t.Error("stage.Push not called")
	}
}

func TestSecondLook_DenyIgnoresAcks(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: workflowDiff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**"}}

	rec, done, id := submitGated(t, b)
	deny(t, b, id) // empty body; deny must not demand acknowledgments
	waitDone(t, done)

	_, term := parseEvents(rec.Body.String())
	if term["event"] != "result" || term["outcome"] != "denied" {
		t.Errorf("terminal=%v, want result/denied", term)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called after deny")
	}
}

// --- requiredAcks unit tests (pure function, synthetic DiffFacts) ---

func TestRequiredAcks_FeatureOffReturnsNil(t *testing.T) {
	facts := trustbrief.DiffFacts{Flags: []trustbrief.Flag{
		{Kind: "ci-workflow", Paths: []string{".github/workflows/ci.yml"}},
	}}
	if got := requiredAcks(facts, config.DiffPolicy{}); got != nil {
		t.Errorf("requiredAcks with empty SecondLookPaths = %v, want nil", got)
	}
}

func TestRequiredAcks_SortedUniqueMatchingKinds(t *testing.T) {
	facts := trustbrief.DiffFacts{Flags: []trustbrief.Flag{
		{Kind: "lockfile", Paths: []string{"vendor/go.sum", "go.sum"}},
		{Kind: "ci-workflow", Paths: []string{".github/workflows/ci.yml"}},
		{Kind: "binary-changed", Paths: []string{"assets/logo.png"}}, // no pattern matches
	}}
	dp := config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**", "go.sum"}}
	got := requiredAcks(facts, dp)
	want := []string{"ci-workflow", "lockfile"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("requiredAcks = %v, want %v (sorted, matching kinds only)", got, want)
	}
}

func TestRequiredAcks_NoMatchesReturnsNil(t *testing.T) {
	facts := trustbrief.DiffFacts{Flags: []trustbrief.Flag{
		{Kind: "dependency-manifest", Paths: []string{"go.mod"}},
	}}
	dp := config.DiffPolicy{SecondLookPaths: []string{"secrets/**"}}
	if got := requiredAcks(facts, dp); got != nil {
		t.Errorf("requiredAcks with no matching paths = %v, want nil", got)
	}
}

func TestRequiredAcks_OmittedFilesStillComputeOverTracked(t *testing.T) {
	// FilesOmitted>0 with second_look_paths set: second-look is a review aid,
	// not a containment boundary (blocked_paths/max_lines fail closed in
	// checkDiffCaps instead), so acks are computed over the tracked flags only.
	facts := trustbrief.DiffFacts{
		FilesOmitted: 500,
		Flags: []trustbrief.Flag{
			{Kind: "ci-workflow", Paths: []string{".github/workflows/ci.yml"}},
		},
	}
	dp := config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**"}}
	got := requiredAcks(facts, dp)
	if len(got) != 1 || got[0] != "ci-workflow" {
		t.Errorf("requiredAcks with FilesOmitted>0 = %v, want [ci-workflow]", got)
	}
}
