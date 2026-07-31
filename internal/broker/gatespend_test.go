package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE G4 PUSH-GATE DISPLAY, from the broker side.
//
// The web UI used to render "spent: $X" at the push gate by fetching the task's
// audit jsonl and taking the last total_cost_usd it could find on any line.
// That file carries the AGENT's stdout verbatim, so the number rendered beside
// the Approve button was one an in-VM agent could choose. These tests pin the
// replacement: the broker publishes its OWN metered figure on the live task
// state, and it is provably not the agent's.

// End to end through the real lifecycle: the agent prints an absurd cost, the
// gateway lease meters something else, and GET /admin/tasks — the exact body
// the web UI's /api/tasks proxies — carries the LEASE's number.
func TestGateSpend_PublishedFigureIsTheLeaseNotTheAgent(t *testing.T) {
	forged := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":0.0001,"num_turns":1}`
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{spent: 4.75}, writesResult(forged))

	id := driveToPushGate(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	ts := taskStateFromAdmin(t, b, id)
	if ts.SpentSrc != "broker" {
		t.Fatalf("spent_src = %q, want %q", ts.SpentSrc, "broker")
	}
	if ts.SpentUSD != 4.75 {
		t.Errorf("spent_usd = %v, want the lease's 4.75 — never the agent-printed 0.0001 (plan G4)", ts.SpentUSD)
	}
	approve(t, b, id)
	settle(t, b)
}

// An UNMETERED lane (subscription, or an openai_compat lane with no prices)
// meters $0 by construction, not by measurement. Publishing a bare 0 would read
// at the gate as "this task was free"; the surface says "no USD metering here"
// instead, using the SAME UnmeteredVendors signal the trust brief and the
// ledger key on.
func TestGateSpend_UnmeteredLaneSaysSoRatherThanShowingZero(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{spent: 0},
		writesResult(`{"type":"result","subtype":"success","total_cost_usd":123.45}`))
	b.UnmeteredVendors = map[string]bool{"anthropic": true}

	id := driveToPushGate(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	ts := taskStateFromAdmin(t, b, id)
	if ts.SpentSrc != "unmetered" {
		t.Errorf("spent_src = %q, want %q on a lane with no USD metering", ts.SpentSrc, "unmetered")
	}
	if ts.SpentUSD != 0 {
		t.Errorf("spent_usd = %v, want 0 on an unmetered lane", ts.SpentUSD)
	}
	approve(t, b, id)
	settle(t, b)
}

// publishGateSpend's three answers, unit-tested where the end-to-end drive
// cannot reach them: no lease and not resumed (nothing could have spent), and a
// RESUMED task whose previous process left no broker result row — which must
// publish "unknown" rather than a $0 that would read as measured.
func TestGateSpend_PublishesUnknownWhenNothingBrokerAuthoredExists(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	dir := t.TempDir()
	audPath := filepath.Join(dir, "t1.jsonl")
	// Only the AGENT's own row survives from the previous process.
	if err := os.WriteFile(audPath, []byte(
		`{"type":"result","subtype":"success","total_cost_usd":88.8}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.registerTask("t1", "r", "i", nil)
	tr := &taskRun{b: b, id: "t1", taskVendor: "anthropic", resumed: true, auditPath: audPath}
	tr.publishGateSpend()

	ts := taskStateFromAdmin(t, b, "t1")
	if ts.SpentSrc != "" || ts.SpentUSD != 0 {
		t.Errorf("resumed with no broker row published %v/%q, want 0/\"\" (unknown) — an agent-authored 88.8 must not surface",
			ts.SpentUSD, ts.SpentSrc)
	}

	// And the case where the previous process DID leave a broker row: that row
	// is broker-observed at one remove, so it publishes as "broker".
	if err := os.WriteFile(audPath, []byte(
		`{"type":"result","subtype":"success","total_cost_usd":2.25,"src":"broker"}`+"\n"+
			`{"type":"result","subtype":"success","total_cost_usd":88.8}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr.publishGateSpend()
	ts = taskStateFromAdmin(t, b, "t1")
	if ts.SpentSrc != "broker" || ts.SpentUSD != 2.25 {
		t.Errorf("resumed with a broker row published %v/%q, want 2.25/broker", ts.SpentUSD, ts.SpentSrc)
	}
}

// A task that never reaches the gate must serialise exactly as it did before
// these fields existed — both are omitempty, so no consumer sees a new key.
func TestGateSpend_AbsentFieldsAreOmitted(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	b.registerTask("plain", "r", "i", nil)
	rec := httptest.NewRecorder()
	b.HandleTasks(rec, httptest.NewRequest(http.MethodGet, "/admin/tasks", nil))
	if body := rec.Body.String(); strings.Contains(body, "spent_usd") || strings.Contains(body, "spent_src") {
		t.Errorf("a task that never gated serialised spend keys: %s", body)
	}
}

// ---- helpers ----

// driveToPushGate submits a task WITHOUT auto_approve and waits for it to
// register at the human diff gate. The caller must approve or deny.
func driveToPushGate(t *testing.T, b *Broker, body string) string {
	t.Helper()
	go func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
		b.HandleTask(rr, req)
	}()
	// The id is discovered from the pending set rather than from the stream,
	// because the stream is still open while the task waits at the gate.
	return waitForPending(t, b)
}

// settle lets an approved task's lifecycle finish so the test's temp dirs are
// not torn down under a live goroutine.
func settle(t *testing.T, b *Broker) {
	t.Helper()
	waitFor(3*time.Second, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		return len(b.tasks) == 0
	})
}

// taskStateFromAdmin reads one task's state back through the REAL admin
// handler, so the test asserts on the JSON the web UI actually receives rather
// than on the in-memory struct.
func taskStateFromAdmin(t *testing.T, b *Broker, id string) TaskState {
	t.Helper()
	rec := httptest.NewRecorder()
	b.HandleTasks(rec, httptest.NewRequest(http.MethodGet, "/admin/tasks", nil))
	var out []TaskState
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /admin/tasks: %v (%s)", err, rec.Body.String())
	}
	for _, ts := range out {
		if ts.ID == id {
			return ts
		}
	}
	t.Fatalf("task %s not in /admin/tasks: %s", id, rec.Body.String())
	return TaskState{}
}
