package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"drydock/internal/creds"
)

type countingGrant struct {
	fakeGrant
	requests int
}

func (g *countingGrant) Requests() int { return g.requests }

type staticProvider struct{ g creds.Grant }

func (p *staticProvider) Mint(float64) (creds.Grant, error) { return p.g, nil }

// lastMetricsLine returns the parsed last {"type":"metrics"} line and asserts
// it is the FINAL line of the audit file (guaranteed-last is the trust rule).
func lastMetricsLine(t *testing.T, auditData string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(auditData, "\n"), "\n")
	var m map[string]any
	if json.Unmarshal([]byte(lines[len(lines)-1]), &m) != nil || m["type"] != "metrics" {
		t.Fatalf("last audit line is not a metrics row:\n%s", auditData)
	}
	return m
}

func TestHandleTask_Success_WritesMetricsRowLast(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["src"] != "broker" || m["task_id"] != id {
		t.Fatalf("metrics row identity wrong: %v", m)
	}
	if m["agent"] != "claude" || m["vendor"] != "anthropic" || m["auth"] != "api_key" {
		t.Errorf("dimensions wrong: agent=%v vendor=%v auth=%v", m["agent"], m["vendor"], m["auth"])
	}
	if repo, _ := m["repo"].(string); repo == "" || strings.Contains(repo, "do x") {
		t.Errorf("repo=%q, want redacted non-empty repo ref", repo)
	}
	if _, ok := m["stage_ms"].(map[string]any); !ok {
		t.Errorf("stage_ms missing: %v", m)
	}
	if m["widen_outcome"] != "none" {
		t.Errorf("widen_outcome=%v, want none (no extras requested)", m["widen_outcome"])
	}
}

func TestHandleTask_AgentError_StillWritesMetricsRow(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir()}
	grant := &fakeGrant{spent: 0.01}
	b := testBroker(t, "anthropic", st, grant,
		func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
			return errors.New("container exploded")
		})
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
	id, _ := events[0]["task_id"].(string)
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["src"] != "broker" {
		t.Fatalf("metrics row missing on the error path: %v", m)
	}
}

// stage_ms must PARTITION the task's wall-clock. Preparing ends when the
// FIRST stage after it begins: with a setup phase that is setup, not the
// agent run — so preparing must exclude the setup commands' duration
// (previously it spanned prep+setup, double-counting setup, because
// runSandbox overwrites taskStart only when the agent starts).
func TestAppendMetrics_SetupNotDoubleCountedInPreparing(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	log := &runLog{}
	const setupSleep = 250 * time.Millisecond
	setupFn := func(_ context.Context, _ []string, _, _ io.Writer) error {
		time.Sleep(setupSleep)
		return nil
	}
	b := testBroker(t, "anthropic", st, grant, setupSplitRun(log, setupFn))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"npm", "ci"}}},
	}
	_, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed", term["outcome"])
	}
	id := taskID(t, events)
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	sm, _ := m["stage_ms"].(map[string]any)
	if sm == nil {
		t.Fatalf("no stage_ms in metrics row: %v", m)
	}
	setupMs, _ := sm["setup"].(float64)
	prepMs, _ := sm["preparing"].(float64)
	if setupMs < float64(setupSleep.Milliseconds()) {
		t.Fatalf("stage_ms.setup=%v, want >= %d (the setup command slept that long)",
			setupMs, setupSleep.Milliseconds())
	}
	if prepMs >= float64(setupSleep.Milliseconds()) {
		t.Errorf("stage_ms.preparing=%v ms swallowed the %v setup phase; preparing must end when setting_up begins",
			prepMs, setupSleep)
	}
}

func TestAppendBrokerResult_NumTurnsFromGrantRequests(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &countingGrant{fakeGrant: fakeGrant{spent: 0.02}, requests: 7}
	b := testBroker(t, "anthropic", st, &grant.fakeGrant, writesResult(`{"type":"result","subtype":"success"}`))
	// testBroker takes *fakeGrant; swap the provider to mint the counting grant instead.
	b.Providers["anthropic"] = &staticProvider{g: grant}
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"num_turns":7`) {
		t.Errorf("result row does not carry the lease request count:\n%s", audit)
	}
}

// approveWhenPending waits for a task to reach the approval gate, sleeps
// briefly so the measured wall-clock gate wait rounds to at least 1ms, then
// approves it. Thin wrapper over the existing waitForPending/approve helpers
// (handle_task_test.go); it does not duplicate their polling/approve logic.
func approveWhenPending(t *testing.T, b *Broker) string {
	t.Helper()
	id := waitForPending(t, b)
	time.Sleep(5 * time.Millisecond)
	approve(t, b, id)
	return id
}

func TestHandleTask_ApprovalGate_RecordsWaitAndDiffFacts(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	done := make(chan string, 1)
	go func() {
		_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
		id, _ := events[0]["task_id"].(string)
		done <- id
	}()
	id := approveWhenPending(t, b)
	got := <-done
	if got != id {
		t.Fatalf("approved %s but submit returned %s", id, got)
	}

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if w, _ := m["approval_gate_wait_ms"].(float64); w <= 0 {
		t.Errorf("approval_gate_wait_ms=%v, want > 0", m["approval_gate_wait_ms"])
	}
	if f, _ := m["diff_files"].(float64); f != 1 {
		t.Errorf("diff_files=%v, want 1", m["diff_files"])
	}
	if bts, _ := m["diff_bytes"].(float64); bts <= 0 {
		t.Errorf("diff_bytes=%v, want > 0", m["diff_bytes"])
	}
	if p, ok := m["stage_ms"].(map[string]any); !ok || p["pushing"] == nil {
		t.Errorf("stage_ms.pushing missing: %v", m)
	}
}

func TestHandleTask_WidenApproved_RecordsOutcomeAndWait(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	yes := true
	b.Cfg.PerTaskWidening.RequiresApproval = &yes

	done := make(chan string, 1)
	go func() {
		_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true,"egress_extra":[{"host":"proxy.example.com","ports":[443]}]}`)
		id, _ := events[0]["task_id"].(string)
		done <- id
	}()
	approveWhenPending(t, b)
	id := <-done

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["widen_outcome"] != "approved" {
		t.Errorf("widen_outcome=%v, want approved", m["widen_outcome"])
	}
	if n, _ := m["widen_requested"].(float64); n != 1 {
		t.Errorf("widen_requested=%v, want 1", m["widen_requested"])
	}
	if w, _ := m["egress_gate_wait_ms"].(float64); w <= 0 {
		t.Errorf("egress_gate_wait_ms=%v, want > 0", m["egress_gate_wait_ms"])
	}
}

// --- terminal outcome taxonomy (#200): the metrics row's Outcome field must
// reflect the terminal path the broker actually took. "denied" and
// "cancelled" are the two paths the result row's subtype alone cannot
// express (see audit.OutcomeKeyWithMetrics), so those two get their own
// full-flow tests below alongside the more direct pushed/no_diff/error/
// push_failed paths.

func TestAppendMetrics_Outcome_Pushed(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["outcome"] != "pushed" {
		t.Errorf("outcome=%v, want pushed", m["outcome"])
	}
}

func TestAppendMetrics_Outcome_NoDiff(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: ""} // agent changed nothing
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["outcome"] != "no_diff" {
		t.Errorf("outcome=%v, want no_diff", m["outcome"])
	}
}

func TestAppendMetrics_Outcome_Error(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "ignored-on-failure"}
	grant := &fakeGrant{}
	run := func(context.Context, []string, io.Writer, io.Writer) error {
		return errors.New("container exited 1")
	}
	b := testBroker(t, "anthropic", st, grant, run)
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
	id, _ := events[0]["task_id"].(string)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["outcome"] != "error" {
		t.Errorf("outcome=%v, want error", m["outcome"])
	}
}

func TestAppendMetrics_Outcome_PushFailed(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x", pushErr: errors.New("fatal: Could not resolve host")}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.PushMaxRetries = 1
	b.PushRetryBackoff = 0
	b.PushFreshBranchTries = 0
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["outcome"] != "push_failed" {
		t.Errorf("outcome=%v, want push_failed", m["outcome"])
	}
}

// TestAppendMetrics_Outcome_Denied is the regression test for the bug that
// motivated this field: pushAndOpenPR only streams outcome=denied to the
// live client, it never rewrites the audit log's result row (still the
// agent's own pre-gate "success" line), so before this field existed a
// denied task's audit trail was indistinguishable from a pushed one.
func TestAppendMetrics_Outcome_Denied(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()
	id := waitForPending(t, b)
	deny(t, b, id)
	<-done

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["outcome"] != "denied" {
		t.Errorf("outcome=%v, want denied", m["outcome"])
	}
}

// TestAppendMetrics_Outcome_Shutdown is the regression test for change 2: a
// task at the push gate when brokerd shuts down is only parked, not
// cancelled, since it will resume alive at the next boot. Before this fix
// the gateShutdown branch recorded outcome="cancelled", so `drydock tasks`
// and the web UI History showed it as terminal for the whole time it sat
// parked. The metrics row must omit outcome entirely, and classification
// must fall back to the result row (the agent's own pre-gate "success"
// line here), exactly as the resumed gateTimeout case already does.
func TestAppendMetrics_Outcome_Shutdown(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()
	id := waitForPending(t, b)
	b.CancelAll() // simulate a graceful brokerd shutdown while parked at the gate
	<-done

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if v, present := m["outcome"]; present {
		t.Errorf("outcome=%v, want the field omitted entirely (shutdown-parked task is not terminal)", v)
	}
}

// TestAppendMetrics_Outcome_Cancelled is the regression test for the other
// half of the bug: a task killed mid-run writes a generic subtype:"error"
// result row (appendBrokerResult doesn't know WHY the run ended), so before
// this field existed a killed task showed "error" in `drydock tasks`
// indistinguishably from a real agent failure.
func TestAppendMetrics_Outcome_Cancelled(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "d"}
	release := make(chan struct{})
	run := func(ctx context.Context, _ []string, _, _ io.Writer) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, run)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	var id string
	if !waitFor(500*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		for k := range b.cancellers {
			id = k
			return true
		}
		return false
	}) {
		t.Fatal("task never registered")
	}

	killReq := httptest.NewRequest("POST", "/admin/kill/"+id, nil)
	killReq.SetPathValue("id", id)
	killRec := httptest.NewRecorder()
	b.HandleKill(killRec, killReq)
	if killRec.Code != http.StatusNoContent {
		t.Fatalf("kill code=%d, want 204", killRec.Code)
	}
	<-done

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["outcome"] != "cancelled" {
		t.Errorf("outcome=%v, want cancelled", m["outcome"])
	}
}
