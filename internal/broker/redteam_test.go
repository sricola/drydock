package broker

import (
	"context"
	"fmt"
	"io"
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

// A6 — auto_approve must NOT bypass the human egress-widening gate. auto_approve
// only opts a caller out of the *diff-push* gate; it must never let a task widen
// its own egress unattended. A task submitted with BOTH auto_approve:true AND
// egress_extra must still stop at the awaiting_egress gate — it must not
// auto-widen (register the extras with squid) nor proceed to run/push. This pins
// the gate in HandleTask, which keys only on RequiresApproval and never consults
// AutoApprove. A regression that OR'd auto_approve into the bypass would sail
// past waitForPending (never registering a human gate) and trip this test.
func TestRedteam_A6_AutoApproveCannotBypassWideningGate(t *testing.T) {
	var staged, ran atomic.Bool
	fs := &fakeSquid{}
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := &Broker{
		Providers:     map[string]creds.Provider{"anthropic": &mintingProvider{&fakeGrant{}}},
		DefaultAgent:  "claude",
		StageRoot:     t.TempDir(),
		AuditRoot:     t.TempDir(),
		Timeout:       5 * time.Second,
		TaskBudget:    1.0,
		MaxConcurrent: 4,
		Squid:         fs,
		prepareStage: func(string, string) (taskStage, error) {
			staged.Store(true) // reaching staging means the gate was bypassed
			return st, nil
		},
		runAgent: func(_ context.Context, _ []string, stdout, _ io.Writer) error {
			ran.Store(true)
			fmt.Fprintln(stdout, `{"type":"result","subtype":"success"}`)
			return nil
		},
	}
	b.Cfg.PerTaskWidening.RequiresApproval = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true,"egress_extra":[{"host":"evil.example.com","ports":[443]}]}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	// The core anti-bypass proof: despite auto_approve:true, the task registers
	// at a human gate. b.pending is only populated by the egress gate here
	// (gatePush registers nothing when auto_approve is set), so reaching it means
	// the widening gate held. A bypass regression would run straight to push and
	// waitForPending would time out.
	id := waitForPending(t, b)

	// While blocked at the gate, nothing downstream may have happened: no squid
	// widening registered, no clone, no agent run, no push.
	if len(fs.added) != 0 {
		t.Errorf("A6 BREACH: squid AddTask called %v before human approval — auto_approve auto-widened egress", fs.added)
	}
	if staged.Load() {
		t.Error("A6 BREACH: task reached staging while at the egress gate — auto_approve bypassed the gate")
	}
	if ran.Load() {
		t.Error("A6 BREACH: agent ran while at the egress gate — auto_approve bypassed the gate")
	}

	// Deny the widening: the task must abort with the widening-denied error and
	// still never widen, stage, run, or push.
	deny(t, b, id)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return after deny")
	}

	events, term := parseEvents(rec.Body.String())
	var sawEgressGate bool
	for _, ev := range events {
		if ev["stage"] == "awaiting_egress" {
			sawEgressGate = true
		}
	}
	if !sawEgressGate {
		t.Errorf("A6 BREACH: no awaiting_egress stage event — the widening gate was skipped; body=%s", rec.Body)
	}
	if term["event"] != "error" || term["reason"] != "egress widening denied" {
		t.Errorf("A6: terminal=%v, want error/egress widening denied", term)
	}
	if len(fs.added) != 0 {
		t.Errorf("A6 BREACH: squid AddTask called %v — extras widened despite denial", fs.added)
	}
	if staged.Load() || ran.Load() || st.pushed {
		t.Errorf("A6 BREACH: post-deny staged=%v ran=%v pushed=%v — task proceeded past a denied widening",
			staged.Load(), ran.Load(), st.pushed)
	}
}
