package broker

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/creds"
)

// Red-team tests: each runs an actual attack from THREAT_MODEL.md and asserts
// containment. `make redteam` collects these (TestRedteam_*) alongside the
// stage-package enforcers for A3/A4. A failure here is a containment breach,
// not a flake. The VM-backed claims (A1, A2, A7) live in the integration suite.

// A5 — a malicious diff must not reach origin without explicit approval. The
// agent produces a backdoor diff; with no auto_approve and an operator DENY,
// nothing is pushed.
func TestRedteam_A5_GateBlocksUnapprovedPush(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/auth.go b/auth.go\n+// backdoor: skip authn\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	id := waitForPending(t, b)
	deny(t, b, id)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return after deny")
	}

	_, term := parseEvents(rec.Body.String())
	if term["event"] != "result" || term["outcome"] != "denied" {
		t.Errorf("A5 BREACH: terminal=%v after deny — the diff gate did not contain the change", term)
	}
	if st.pushed {
		t.Error("A5 BREACH: stage.Push was called despite the operator denying the diff")
	}
}

// A6 — an agent that requests extra egress must not get it without approval.
// With requires_approval set, a DENIED widening request is rejected (403) and
// the task never reaches staging, so the requested hosts never compile into
// squid.
func TestRedteam_A6_EgressWidenDenied(t *testing.T) {
	var staged atomic.Bool
	b := &Broker{
		Providers:     map[string]creds.Provider{"anthropic": &mintingProvider{&fakeGrant{}}},
		DefaultAgent:  "claude",
		StageRoot:     t.TempDir(),
		AuditRoot:     t.TempDir(),
		Timeout:       5 * time.Second,
		TaskBudget:    1.0,
		MaxConcurrent: 4,
		prepareStage: func(string, string) (taskStage, error) {
			staged.Store(true) // reaching staging means the gate failed
			return &fakeStage{workDir: t.TempDir()}, nil
		},
		runAgent: writesResult(""),
	}
	b.Cfg.PerTaskWidening.RequiresApproval = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","egress_extra":[{"host":"evil.example.com","ports":[443]}]}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	id := waitForPending(t, b)
	deny(t, b, id)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return after deny")
	}

	_, term := parseEvents(rec.Body.String())
	if term["event"] != "error" || term["reason"] != "egress widening denied" {
		t.Errorf("A6: terminal=%v, want error/egress widening denied", term)
	}
	if staged.Load() {
		t.Error("A6 BREACH: task reached staging after egress widening was denied — extras could reach squid")
	}
}
