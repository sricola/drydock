package broker

import (
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/egress"
	"drydock/internal/trustbrief"
)

// writeBrief is exercised directly (deterministic inputs) and via the full
// HandleTask flow below (artifact appears at the gate for real submissions).
func TestWriteBrief_BrokerObservedFields(t *testing.T) {
	auditRoot := t.TempDir()
	b := &Broker{
		AuditRoot: auditRoot, ImageRef: "sandbox:test", TaskBudget: 2,
		MaxRequestCostUSD: 0.5, TaskMaxRequests: 100, Timeout: 30 * time.Minute,
	}
	b.Cfg.Default.Domains = []egress.Domain{{Host: "api.anthropic.com", Ports: []int{443}}}
	tr := &taskRun{
		b: b, id: "0123456789abcdef0123456789abcdef",
		repoRef:     "https://user:sekret@github.com/o/r.git",
		instruction: "fix the race",
		sensitive:   true,
		autoApprove: false,
		egressExtra: []egress.Domain{{Host: "api.example.com", Ports: []int{443}}},
		agentName:   "claude", taskVendor: "anthropic", model: "model-x",
		grant:     &fakeGrant{spent: 0.12},
		taskStart: time.Now().Add(-time.Minute),
		st:        &fakeStage{workDir: t.TempDir()},
	}

	b.writeBrief(tr, "diff --git a/x b/x\nnew file mode 100755\n+y\n")

	got, err := trustbrief.Read(auditRoot, tr.id)
	if err != nil {
		t.Fatalf("brief not written: %v", err)
	}
	if got.SchemaVersion != 1 || got.TaskID != tr.id {
		t.Errorf("identity = %d/%q", got.SchemaVersion, got.TaskID)
	}
	if got.Task.RepoRef != "https://github.com/o/r.git" {
		t.Errorf("repo not redacted: %q", got.Task.RepoRef)
	}
	if got.Task.InstructionSHA256 != trustbrief.HashInstruction("fix the race") {
		t.Error("instruction hash mismatch")
	}
	if !got.Task.Sensitive || got.Task.AutoApprove {
		t.Errorf("task facts = %+v", got.Task)
	}
	if got.Runtime.Agent != "claude" || got.Runtime.Vendor != "anthropic" ||
		got.Runtime.Model != "model-x" || got.Runtime.ImageRef != "sandbox:test" {
		t.Errorf("runtime = %+v", got.Runtime)
	}
	if got.Policy.BudgetUSD != 2 || !got.Policy.BudgetHard || got.Policy.MaxRequests != 100 ||
		got.Policy.TimeoutSeconds != 1800 {
		t.Errorf("policy = %+v", got.Policy)
	}
	if len(got.Policy.EgressDefault) != 1 || got.Policy.EgressDefault[0] != "api.anthropic.com:443" {
		t.Errorf("egress default = %v", got.Policy.EgressDefault)
	}
	if len(got.Policy.EgressWidened) != 1 || got.Policy.EgressWidened[0] != "api.example.com:443" {
		t.Errorf("egress widened = %v", got.Policy.EgressWidened)
	}
	if got.Policy.SnapshotSHA256 == "" || got.Policy.SnapshotSHA256 != got.Policy.Fingerprint() {
		t.Errorf("policy fingerprint = %q, want self-consistent hash", got.Policy.SnapshotSHA256)
	}
	if got.Spend.USDBrokerMetered != 0.12 || got.Spend.DurationMs <= 0 {
		t.Errorf("spend = %+v", got.Spend)
	}
	if len(got.Diff.Flags) == 0 {
		t.Errorf("diff flags = %+v, want exec-bit flagged", got.Diff)
	}
	if got.Verification.Status != trustbrief.VerificationNotConfigured {
		t.Errorf("verification = %+v", got.Verification)
	}
	// fakeStage has no BaseCommit method — that absence must be surfaced,
	// not silently dropped.
	foundGap := false
	for _, m := range got.MissingEvidence {
		if m == "base_commit unavailable" {
			foundGap = true
		}
	}
	if !foundGap {
		t.Errorf("missing_evidence = %v, want base_commit gap recorded", got.MissingEvidence)
	}
}

// A soft budget (no per-request reservation) must be reported as such.
func TestWriteBrief_SoftBudgetReported(t *testing.T) {
	auditRoot := t.TempDir()
	b := &Broker{AuditRoot: auditRoot, TaskBudget: 2, Timeout: time.Minute}
	tr := &taskRun{
		b: b, id: "0123456789abcdef0123456789abcdef",
		repoRef: "https://github.com/o/r.git", agentName: "claude",
		grant: &fakeGrant{}, taskStart: time.Now(), st: &fakeStage{workDir: t.TempDir()},
	}
	b.writeBrief(tr, "diff --git a/x b/x\n+y\n")
	got, err := trustbrief.Read(auditRoot, tr.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy.BudgetHard {
		t.Error("BudgetHard = true with MaxRequestCostUSD unset; the soft cap must be reported honestly")
	}
}

func TestHandleTask_WritesBriefAtGate(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	id, _ := events[0]["task_id"].(string)
	if id == "" {
		t.Fatalf("no task_id in accepted event: %v", events[0])
	}
	got, err := trustbrief.Read(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("no brief after auto-approved flow: %v", err)
	}
	if !got.Task.AutoApprove {
		t.Error("auto_approve not recorded in brief")
	}
	if filepath.Join(b.AuditRoot, id+trustbrief.Suffix) == "" {
		t.Fatal("unreachable")
	}
}
