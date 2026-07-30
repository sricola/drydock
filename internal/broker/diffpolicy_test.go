package broker

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"drydock/internal/config"
	"drydock/internal/trustbrief"
)

// These tests drive the diff-policy caps (config diff_policy) through the full
// HandleTask lifecycle: a diff that violates max_files_changed,
// max_lines_changed, or blocked_paths must fail closed as the terminal outcome
// policy_blocked BEFORE the approval gate — nothing pushed, the task never
// registered in b.pending, and a broker-authored policy_blocked result row in
// the audit. Caps are enforcement, so auto_approve does not bypass them.

// submitTerminates submits a task that is expected to reach a terminal event
// on its own (no gate interaction). A task that instead parks at the approval
// gate would block HandleTask forever, so the wait is bounded and failure
// names the likely cause.
func submitTerminates(t *testing.T, b *Broker, body string) (*httptest.ResponseRecorder, []map[string]any, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(body))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return — the task likely entered the approval gate instead of terminating")
	}
	events, term := parseEvents(rec.Body.String())
	return rec, events, term
}

// assertNeverPending polls briefly and fails if the task ever registered at
// the approval gate. Caps must block BEFORE the gate, not deny at it.
func assertNeverPending(t *testing.T, b *Broker) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		b.pendingMu.Lock()
		n := len(b.pending)
		b.pendingMu.Unlock()
		if n != 0 {
			t.Fatal("task entered b.pending; diff-policy caps must block before the gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const twoFileDiff = "diff --git a/a.go b/a.go\n" +
	"@@ -0,0 +1 @@\n" +
	"+x\n" +
	"diff --git a/b.go b/b.go\n" +
	"@@ -0,0 +1 @@\n" +
	"+y\n"

func TestDiffCaps_MaxFilesBlocksBeforeGate(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: twoFileDiff}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{MaxFilesChanged: 1}

	_, _, term := submitTerminates(t, b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	if term["event"] != "result" || term["outcome"] != "policy_blocked" {
		t.Fatalf("terminal=%v, want result/policy_blocked", term)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "2") || !strings.Contains(reason, "max_files_changed") {
		t.Errorf("reason=%q, want it to name the max_files_changed cap and the file count", reason)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called when the diff is policy-blocked")
	}
	assertNeverPending(t, b)
	audit := readOnlyAudit(t, b.AuditRoot)
	if !strings.Contains(audit, `"subtype":"policy_blocked"`) || !strings.Contains(audit, `"src":"broker"`) {
		t.Errorf("audit log missing broker-authored policy_blocked result row:\n%s", audit)
	}
}

func TestDiffCaps_MaxLinesBlocks(t *testing.T) {
	// One file (under any file cap) but 3 changed lines against a cap of 2.
	diff := "diff --git a/a.go b/a.go\n" +
		"@@ -1,1 +1,2 @@\n" +
		"-old\n" +
		"+new1\n" +
		"+new2\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{MaxLinesChanged: 2}

	_, _, term := submitTerminates(t, b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	if term["event"] != "result" || term["outcome"] != "policy_blocked" {
		t.Fatalf("terminal=%v, want result/policy_blocked", term)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "max_lines_changed") {
		t.Errorf("reason=%q, want it to name the max_lines_changed cap", reason)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called when the diff is policy-blocked")
	}
	assertNeverPending(t, b)
}

func TestDiffCaps_BlockedPathBlocks(t *testing.T) {
	diff := "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{BlockedPaths: []string{".github/workflows/**"}}

	_, _, term := submitTerminates(t, b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	if term["event"] != "result" || term["outcome"] != "policy_blocked" {
		t.Fatalf("terminal=%v, want result/policy_blocked", term)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, ".github/workflows/ci.yml") {
		t.Errorf("reason=%q, want it to name the blocked path .github/workflows/ci.yml", reason)
	}
	if !strings.Contains(reason, ".github/workflows/**") {
		t.Errorf("reason=%q, want it to name the matching pattern .github/workflows/**", reason)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called when the diff is policy-blocked")
	}
	assertNeverPending(t, b)
}

func TestDiffCaps_UnderCapsProceedsToGate(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: twoFileDiff}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{
		MaxFilesChanged: 5,
		MaxLinesChanged: 100,
		BlockedPaths:    []string{"secrets/**"},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	id := waitForPending(t, b) // the whole point: an under-caps diff still reaches the gate
	approve(t, b, id)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return after approve")
	}
	_, term := parseEvents(rec.Body.String())
	if term["event"] != "result" || term["outcome"] != "pushed" {
		t.Errorf("terminal=%v, want result/pushed; body=%s", term, rec.Body)
	}
	if !st.pushed.Load() {
		t.Error("stage.Push not called after approve")
	}
}

func TestDiffCaps_AutoApproveStillBlocked(t *testing.T) {
	diff := "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n" +
		"@@ -1 +1 @@\n" +
		"+new\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.DiffPolicy = config.DiffPolicy{BlockedPaths: []string{".github/workflows/**"}}

	_, _, term := submitTerminates(t, b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["event"] != "result" || term["outcome"] != "policy_blocked" {
		t.Fatalf("terminal=%v, want result/policy_blocked (caps are enforcement, auto_approve must not bypass them)", term)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called: auto_approve must not bypass diff-policy caps")
	}
	assertNeverPending(t, b)
}

// Omitted-files fail-closed guard. trustbrief.Analyze caps Files at its
// tracking bound; files past it are counted in FilesOmitted with no path or
// line data, so blocked_paths and max_lines_changed cannot see them. These
// tests call checkDiffCaps directly with a synthetic DiffFacts — routing a
// >4096-file diff through HandleTask would prove nothing more.

func capsRun(p config.DiffPolicy) *taskRun {
	return &taskRun{b: &Broker{DiffPolicy: p}}
}

func TestDiffCaps_OmittedFilesFailClosedForBlockedPaths(t *testing.T) {
	tr := capsRun(config.DiffPolicy{BlockedPaths: []string{"secrets/**"}})
	facts := trustbrief.DiffFacts{
		Files:        []trustbrief.FileChange{{Path: "a.go", Adds: 1}},
		FilesOmitted: 904, // e.g. secrets/key.pem is file #5000, untracked
	}
	blocked, reason := tr.checkDiffCaps(facts)
	if !blocked {
		t.Fatal("blocked_paths set with FilesOmitted>0 must fail closed: an omitted file could match a blocked pattern")
	}
	if !strings.Contains(reason, "904 files omitted") || !strings.Contains(reason, "incompletely-analyzed") {
		t.Errorf("reason=%q, want the incomplete-analysis reason naming 904 omitted files", reason)
	}
}

func TestDiffCaps_OmittedFilesFailClosedForMaxLines(t *testing.T) {
	tr := capsRun(config.DiffPolicy{MaxLinesChanged: 1000})
	facts := trustbrief.DiffFacts{
		Files:        []trustbrief.FileChange{{Path: "a.go", Adds: 1}}, // tracked lines well under cap
		FilesOmitted: 3,
	}
	blocked, reason := tr.checkDiffCaps(facts)
	if !blocked {
		t.Fatal("max_lines_changed set with FilesOmitted>0 must fail closed: omitted files carry no line counts")
	}
	if !strings.Contains(reason, "files omitted") {
		t.Errorf("reason=%q, want the incomplete-analysis reason", reason)
	}
}

func TestDiffCaps_OmittedFilesMaxFilesReasonWins(t *testing.T) {
	// Only max_files_changed is set, and the count (tracked+omitted) exceeds
	// it. The count-based reason must win — not the incomplete-analysis one.
	tr := capsRun(config.DiffPolicy{MaxFilesChanged: 10})
	facts := trustbrief.DiffFacts{
		Files:        []trustbrief.FileChange{{Path: "a.go", Adds: 1}, {Path: "b.go", Adds: 1}},
		FilesOmitted: 20,
	}
	blocked, reason := tr.checkDiffCaps(facts)
	if !blocked {
		t.Fatal("22 files against a max_files_changed cap of 10 must block")
	}
	if !strings.Contains(reason, "max_files_changed") || !strings.Contains(reason, "22") {
		t.Errorf("reason=%q, want the max_files_changed count reason naming 22 files", reason)
	}
	if strings.Contains(reason, "incompletely-analyzed") {
		t.Errorf("reason=%q, the count reason must win over the incomplete-analysis reason", reason)
	}
}

func TestDiffCaps_OmittedFilesNoPolicyProceeds(t *testing.T) {
	// diff_policy fully disabled: omitted files alone must not block.
	tr := capsRun(config.DiffPolicy{})
	facts := trustbrief.DiffFacts{
		Files:        []trustbrief.FileChange{{Path: "a.go", Adds: 1}},
		FilesOmitted: 500,
	}
	if blocked, reason := tr.checkDiffCaps(facts); blocked {
		t.Fatalf("zero DiffPolicy must not block, got blocked with reason %q", reason)
	}
}

func TestDiffCaps_DisabledByDefault(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: twoFileDiff}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	// b.DiffPolicy left at its zero value: no caps, no blocked paths.

	_, _, term := submitTerminates(t, b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["event"] != "result" || term["outcome"] != "pushed" {
		t.Fatalf("terminal=%v, want result/pushed (zero DiffPolicy must not block anything)", term)
	}
	if !st.pushed.Load() {
		t.Error("stage.Push should have been called with the zero-value DiffPolicy")
	}
}
