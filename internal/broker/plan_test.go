package broker

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/trustbrief"
)

// These tests pin THE plan-mode invariant: a PlanOnly task terminates
// "planned" after the agent run and NEVER reaches runVerify or pushAndOpenPR,
// regardless of what the agent left in the work tree. They drive the full
// HandleTask lifecycle through the same seams as the push/no_diff tests.

// submitPlan runs a PlanOnly task against a broker whose repo has a REQUIRED
// verify config: if plan mode ever fell through to runVerify, the fakeStage
// (no export capability) would make verification inconclusive and the task
// would terminate verify_failed instead of planned — so the "planned"
// terminal doubles as proof that the verify stage was structurally skipped.
func submitPlan(t *testing.T, st *fakeStage, body string) (*Broker, []map[string]any, map[string]any) {
	t.Helper()
	grant := &fakeGrant{spent: 0.01}
	resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"total_cost_usd":0.01,"num_turns":1}`
	b := testBroker(t, "anthropic", st, grant, writesResult(resultLine))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"go", "test", "./..."}}, Required: true},
	}
	rec, events, term := submit(b, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	return b, events, term
}

func TestPlanMode_TerminatesPlannedNeverPushes(t *testing.T) {
	// A NON-EMPTY diff plus auto_approve is the worst case: on a normal task
	// this combination pushes without any human gate. Plan mode must still
	// terminate "planned" and never push.
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n",
		plan: "## Plan\n1. do x\n", planOK: true}
	b, events, term := submitPlan(t, st,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"plan x","agent":"claude","auto_approve":true,"plan_only":true}`)

	if term["event"] != "result" || term["outcome"] != "planned" {
		t.Fatalf("terminal=%v, want result/planned", term)
	}
	if st.pushed.Load() {
		t.Error("plan mode pushed a branch — THE invariant is violated")
	}
	for _, s := range stages(events) {
		switch s {
		case "awaiting_approval", "pushing", "verifying":
			t.Errorf("plan mode entered stage %q; must terminate before verify/gate/push", s)
		}
	}
	// The broker-authored planned result row must be the last result line in
	// the audit log (last-wins outcome parsing).
	id, _ := events[0]["task_id"].(string)
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"subtype":"planned"`) || !strings.Contains(audit, `"src":"broker"`) {
		t.Errorf("audit missing broker-authored planned result row:\n%s", audit)
	}
	// The planned terminal carries duration/cost like the other broker-authored
	// terminals, plus the inspect hint.
	if _, ok := term["duration_ms"]; !ok {
		t.Error("planned terminal missing duration_ms")
	}
	hint, _ := term["hint"].(string)
	if !strings.Contains(hint, "drydock inspect "+id) || !strings.Contains(hint, "--plan") {
		t.Errorf("hint=%q, want inspect hint mentioning --plan", hint)
	}
}

func TestPlanMode_EmptyDiffStillTerminatesPlanned(t *testing.T) {
	// A planning agent typically changes nothing outside .task/ — the empty
	// diff must classify as "planned", not fall into the no_diff terminal.
	st := &fakeStage{workDir: t.TempDir(), diff: "", plan: "## Plan\n", planOK: true}
	_, _, term := submitPlan(t, st,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"plan x","agent":"claude","plan_only":true}`)
	if term["outcome"] != "planned" {
		t.Fatalf("terminal=%v, want result/planned (not no_diff)", term)
	}
	if st.pushed.Load() {
		t.Error("plan mode pushed a branch")
	}
}

func TestPlanMode_CapturesPlanArtifact(t *testing.T) {
	planText := "## Plan\n1. add feature\n2. test it\n"
	st := &fakeStage{workDir: t.TempDir(), diff: "", plan: planText, planOK: true}
	b, events, term := submitPlan(t, st,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"plan x","agent":"claude","plan_only":true}`)

	if term["outcome"] != "planned" {
		t.Fatalf("terminal=%v, want planned", term)
	}
	if term["has_plan"] != true {
		t.Errorf("has_plan=%v, want true", term["has_plan"])
	}
	if pb, ok := term["plan_bytes"].(float64); !ok || int(pb) != len(planText) {
		t.Errorf("plan_bytes=%v, want %d", term["plan_bytes"], len(planText))
	}
	id, _ := events[0]["task_id"].(string)
	planPath := filepath.Join(b.AuditRoot, id+".plan.md")
	got, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("plan artifact not persisted at %s: %v", planPath, err)
	}
	if string(got) != planText {
		t.Errorf("plan artifact=%q, want %q", got, planText)
	}
	fi, _ := os.Stat(planPath)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("plan artifact mode=%v, want 0600", fi.Mode().Perm())
	}
}

func TestPlanMode_NoPlanFileStillTerminatesPlanned(t *testing.T) {
	// The agent never wrote .task/plan.md (or it was a symlink and refused):
	// the run still terminates planned — with honest has_plan:false — and no
	// artifact appears.
	st := &fakeStage{workDir: t.TempDir(), diff: "", plan: "", planOK: false}
	b, events, term := submitPlan(t, st,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"plan x","agent":"claude","plan_only":true}`)

	if term["outcome"] != "planned" {
		t.Fatalf("terminal=%v, want planned", term)
	}
	if term["has_plan"] != false {
		t.Errorf("has_plan=%v, want false", term["has_plan"])
	}
	if pb, ok := term["plan_bytes"].(float64); !ok || pb != 0 {
		t.Errorf("plan_bytes=%v, want 0", term["plan_bytes"])
	}
	id, _ := events[0]["task_id"].(string)
	if _, err := os.Stat(filepath.Join(b.AuditRoot, id+".plan.md")); !os.IsNotExist(err) {
		t.Errorf("plan artifact must not exist when no plan was captured: err=%v", err)
	}
	if st.pushed.Load() {
		t.Error("plan mode pushed a branch")
	}
}

// The persisted {"type":"drydock_task",...} invocation line must carry
// plan_only and issue_url: `drydock retry` rebuilds the request from that
// line, and without these fields a retried plan run would silently escalate
// to an implementing run (and lose the issue provenance).
func TestPlanMode_InvocationRecordCarriesPlanOnlyAndIssueURL(t *testing.T) {
	issueURL := "https://github.com/o/r/issues/42"
	st := &fakeStage{workDir: t.TempDir(), diff: "", plan: "## Plan\n", planOK: true}
	b, events, term := submitPlan(t, st, fmt.Sprintf(
		`{"repo_ref":"https://github.com/o/r.git","instruction":"plan x","agent":"claude","plan_only":true,"issue_url":%q}`,
		issueURL))
	if term["outcome"] != "planned" {
		t.Fatalf("terminal=%v, want planned", term)
	}
	id, _ := events[0]["task_id"].(string)
	audit := readAudit(t, b.AuditRoot, id)
	var invocation string
	for _, line := range strings.Split(audit, "\n") {
		if strings.Contains(line, `"drydock_task"`) {
			invocation = line
			break
		}
	}
	if invocation == "" {
		t.Fatalf("no drydock_task invocation line in audit:\n%s", audit)
	}
	if !strings.Contains(invocation, `"plan_only":true`) {
		t.Errorf("invocation missing plan_only:\n%s", invocation)
	}
	if !strings.Contains(invocation, fmt.Sprintf(`"issue_url":%q`, issueURL)) {
		t.Errorf("invocation missing issue_url:\n%s", invocation)
	}
}

func TestPlanMode_BriefRecordsIssueAndPlanFlag(t *testing.T) {
	issueURL := "https://github.com/o/r/issues/42"
	st := &fakeStage{workDir: t.TempDir(), diff: "", plan: "## Plan\n", planOK: true}
	b, events, term := submitPlan(t, st, fmt.Sprintf(
		`{"repo_ref":"https://github.com/o/r.git","instruction":"plan x","agent":"claude","plan_only":true,"issue_url":%q}`,
		issueURL))

	if term["outcome"] != "planned" {
		t.Fatalf("terminal=%v, want planned", term)
	}
	id, _ := events[0]["task_id"].(string)
	var brief trustbrief.Brief
	var err error
	// writeBrief is synchronous before the terminal event, but give the FS a beat.
	for i := 0; i < 10; i++ {
		brief, err = trustbrief.Read(b.AuditRoot, id)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("brief not written for plan run: %v", err)
	}
	if !brief.Task.PlanOnly {
		t.Error("brief.Task.PlanOnly=false, want true")
	}
	if brief.Task.IssueURL != issueURL {
		t.Errorf("brief.Task.IssueURL=%q, want %q", brief.Task.IssueURL, issueURL)
	}
}
