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

	"drydock/internal/audit"
	"drydock/internal/remote"
)

func assertLastLineInterrupted(t *testing.T, path string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	last := lines[len(lines)-1]
	var x struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(last), &x); err != nil {
		t.Fatalf("last line not JSON: %q", last)
	}
	if x.Type != "result" || x.Subtype != "interrupted" || !x.IsError {
		t.Errorf("last line = %+v, want type=result subtype=interrupted is_error=true", x)
	}
}

func TestTerminateStuckAudits_AppendsInterruptedAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-a.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"stream_event"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("first pass = (%d,%v), want (1,nil)", n, err)
	}
	assertLastLineInterrupted(t, path)

	// Second pass: the interrupted line is itself a result line → no-op.
	after1, _ := os.ReadFile(path)
	n2, _ := TerminateStuckAudits(dir)
	after2, _ := os.ReadFile(path)
	if n2 != 0 || string(after1) != string(after2) {
		t.Errorf("second pass modified the trace (n=%d)", n2)
	}
}

// Guards the detection rule: a stream event whose TEXT payload contains the
// literal `"type":"result"` must NOT be mistaken for a real result line —
// otherwise a genuinely-crashed task would be skipped and stay "running?".
func TestTerminateStuckAudits_SubstringInPayloadIsNotAResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-b.jsonl")
	body := `{"type":"stream_event","text":"emitted {\"type\":\"result\"} as text"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("got (%d,%v), want (1,nil) — substring must not count as a result", n, err)
	}
	assertLastLineInterrupted(t, path)
}

func TestTerminateStuckAudits_LeavesCompletedTraceUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-c.jsonl")
	body := `{"type":"stream_event"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 0 {
		t.Fatalf("completed trace = (%d,%v), want (0,nil)", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("completed trace modified:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestTerminateStuckAudits_MissingRootIsNoop(t *testing.T) {
	if n, err := TerminateStuckAudits(filepath.Join(t.TempDir(), "nope")); err != nil || n != 0 {
		t.Errorf("missing root = (%d,%v), want (0,nil)", n, err)
	}
}

// A trace larger than the 16KB tail window with NO result line must still be
// detected as stuck and terminated — exercises the hasResultLine seek branch.
func TestTerminateStuckAudits_LargeTraceNoResultIsTerminated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-big.jsonl")
	var b strings.Builder
	for b.Len() < 64*1024 { // well past the 16KB tail window
		b.WriteString(`{"type":"stream_event","delta":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("large no-result trace = (%d,%v), want (1,nil)", n, err)
	}
	assertLastLineInterrupted(t, path)
}

// A trace larger than the tail window WITH a result line as its last line must
// be left untouched — the seek must land such that the final result line is
// within the tail and is parsed.
func TestTerminateStuckAudits_LargeTraceWithResultIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-big2.jsonl")
	var b strings.Builder
	for b.Len() < 64*1024 {
		b.WriteString(`{"type":"stream_event","delta":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}` + "\n")
	}
	b.WriteString(`{"type":"result","subtype":"success","is_error":false}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 0 {
		t.Fatalf("large completed trace = (%d,%v), want (0,nil)", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("large completed trace must be left untouched")
	}
}

// TestAppendLine_RefusesSymlink verifies the O_NOFOLLOW hardening: a symlink
// planted at the audit path must not let the boot-time interrupted-marker write
// pass through to another file.
func TestAppendLine_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	if err := os.WriteFile(target, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := appendLine(link, "x\n"); err == nil {
		t.Fatal("appendLine should refuse a symlinked path (O_NOFOLLOW)")
	}
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Errorf("write leaked through the symlink to the target: %q", b)
	}
}

func TestResumeAwaiting_StageGone_WritesInterrupted(t *testing.T) {
	dir := t.TempDir()
	// A task that finished the agent (ok) then hit the gate, whose stage did NOT survive.
	auditFile := filepath.Join(dir, "gone.jsonl")
	os.WriteFile(auditFile, []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1}`+"\n"), 0o600)
	writeGateMarker(dir, "gone", gateMarker{RepoRef: "r", Agent: "claude"})

	b := &Broker{AuditRoot: dir}
	b.ResumeAwaiting(t.TempDir()) // stageRoot has no "gone" dir

	data, _ := os.ReadFile(auditFile)
	if !strings.Contains(string(data), `"subtype":"interrupted"`) {
		t.Errorf("stage-gone task should get an interrupted line, got:\n%s", data)
	}
	if _, err := os.Stat(gateMarkerPath(dir, "gone")); !os.IsNotExist(err) {
		t.Error("marker should be removed after the interrupted fallback")
	}
}

func TestResumeAwaiting_StageSurvives_ApprovePushes(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "live", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(stageRoot, "live", "git"), 0o700)
	os.WriteFile(filepath.Join(dir, "live.diff"), []byte("diff"), 0o600)
	writeGateMarker(dir, "live", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "live", "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil }, // test seam
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}
	b.ResumeAwaiting(stageRoot)

	// The task is now pending; approve it and assert the surviving branch pushed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.pendingMu.Lock()
		ch := b.pending["live"]
		b.pendingMu.Unlock()
		if ch != nil {
			ch <- true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the resume goroutine time to push.
	for i := 0; i < 200 && !fs.pushed.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !fs.pushed.Load() {
		t.Error("approve after resume should have pushed the surviving branch")
	}

	data, err := os.ReadFile(filepath.Join(dir, "live.jsonl"))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	m := lastMetricsLine(t, string(data))
	if m["task_id"] != "live" {
		t.Errorf("metrics row task_id=%v, want live", m["task_id"])
	}
}

// TestResumeAwaiting_CountsAsPendingApprovalNotRunning is the regression test
// for the healthz bug: registerTask always stamps StageRunning, and
// resumePush used to leave it there instead of moving it to StagePending like
// the live pushAndOpenPR path does, so a task resumed at the diff-approval
// gate after a brokerd restart showed up in healthz/`drydock status` as
// "running" instead of "pending_approval" until the gate resolved.
func TestResumeAwaiting_CountsAsPendingApprovalNotRunning(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "live2", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(stageRoot, "live2", "git"), 0o700)
	os.WriteFile(filepath.Join(dir, "live2.diff"), []byte("diff"), 0o600)
	writeGateMarker(dir, "live2", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "live2", "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}
	b.ResumeAwaiting(stageRoot)

	if !waitFor(2*time.Second, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["live2"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("resumed task never reached the approval gate")
	}

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	b.HandleHealth(rr, req)
	var body struct {
		Running         int `json:"running"`
		PendingApproval int `json:"pending_approval"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Running != 0 || body.PendingApproval != 1 {
		t.Errorf("breakdown = %+v, want running=0 pending_approval=1", body)
	}
}

// TestResumePush_RegisteredStageNeverObservedRunning is change 4's regression
// test: registerTask always stamps StageRunning, and resumePush used to
// correct that to StagePending under a SECOND lock acquisition. Since the
// HTTP admin listener is already serving during boot reconciliation, a reader
// could win the race and observe the wrong stage in that window. registerTaskAt
// closes the window by recording the correct stage in the same critical
// section as registration, so the very first sighting of the resumed task in
// b.tasks must already be StagePending, never StageRunning, even
// momentarily.
func TestResumePush_RegisteredStageNeverObservedRunning(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "rpid", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "rpid.diff"), []byte("diff"), 0o600)
	writeGateMarker(dir, "rpid", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "rpid", "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}

	b.ResumeAwaiting(stageRoot)

	deadline := time.Now().Add(2 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		b.pendingMu.Lock()
		ts, ok := b.tasks["rpid"]
		var stage TaskStage
		if ok {
			stage = ts.Stage
		}
		b.pendingMu.Unlock()
		if ok {
			seen = true
			if stage != StagePending {
				t.Fatalf("first sighting of resumed task has stage=%v, want StagePending immediately", stage)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !seen {
		t.Fatal("resumed task never registered")
	}

	// Let the gate resolve so the goroutine and its deferred cleanup exit
	// promptly instead of leaking past the test.
	b.pendingMu.Lock()
	ch := b.pending["rpid"]
	b.pendingMu.Unlock()
	if ch != nil {
		ch <- true
	}
	waitFor(2*time.Second, func() bool {
		b.pendingMu.Lock()
		_, still := b.tasks["rpid"]
		b.pendingMu.Unlock()
		return !still
	})
}

// TestResumePush_KilledClassifiesAsCancelled is the end-to-end regression
// test for the change-3/change-6 interaction the review flagged: a task
// parked at the diff gate resumes after a restart, the operator kills it
// (not a shutdown), and resumePush writes an on-disk result row with the
// coarse subtype "denied" (gateOutcome's resumed vocabulary has no
// "killed"/"cancelled" subtype of its own) while the metrics row carries the
// finer outcome "cancelled". OutcomeKeyWithMetrics must still refine that
// "denied" result-row key to "cancelled" (audit.OutcomeKeyWithMetrics's
// override rule applies to any key except "push_failed", not just "ok"/
// "error"), so `drydock tasks` and the web UI History classify the task the
// same way a live kill would, not as a plain "denied".
func TestResumePush_KilledClassifiesAsCancelled(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "killid", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "killid.diff"), []byte("diff"), 0o600)
	writeGateMarker(dir, "killid", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "killid", "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}

	b.ResumeAwaiting(stageRoot)
	if !waitFor(2*time.Second, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["killid"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("resumed task never reached the approval gate")
	}

	killReq := httptest.NewRequest("POST", "/admin/kill/killid", nil)
	killReq.SetPathValue("id", "killid")
	killRec := httptest.NewRecorder()
	b.HandleKill(killRec, killReq)
	if killRec.Code != http.StatusNoContent {
		t.Fatalf("kill code=%d, want 204", killRec.Code)
	}

	if !waitFor(2*time.Second, func() bool {
		b.pendingMu.Lock()
		_, still := b.tasks["killid"]
		b.pendingMu.Unlock()
		return !still
	}) {
		t.Fatal("resumed task never unregistered after kill")
	}

	f, err := audit.OpenRead(filepath.Join(dir, "killid.jsonl"))
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}
	defer f.Close()
	last, ok, m, hasMetrics := audit.LastResultAndMetricsFile(f)
	if !ok || last.Subtype != "denied" {
		t.Fatalf("result row = %+v ok=%v, want subtype=denied", last, ok)
	}
	if !hasMetrics || m.Outcome != "cancelled" {
		t.Fatalf("metrics row = %+v hasMetrics=%v, want outcome=cancelled", m, hasMetrics)
	}
	if key := audit.OutcomeKeyWithMetrics(last, ok, m, hasMetrics); key != "cancelled" {
		t.Errorf("OutcomeKeyWithMetrics = %q, want cancelled (a resume-kill must classify the same as a live kill)", key)
	}
}

// TestResumePush_ShutdownKeepsStage verifies that when a resumed task's gate is
// interrupted by brokerd shutdown (errShutdown cancels the context), the stage
// is NOT cleaned and keepStage is true, so the next boot can resume again.
func TestResumePush_ShutdownKeepsStage(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "shutid", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "shutid.diff"), []byte("diff"), 0o600)
	writeGateMarker(dir, "shutid", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "shutid", "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}

	// Start resume: the gate will block waiting for approval.
	b.ResumeAwaiting(stageRoot)

	// Wait for the task to register as pending.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.pendingMu.Lock()
		_, ok := b.pending["shutid"]
		b.pendingMu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate brokerd shutdown: cancel all tasks with errShutdown.
	b.CancelAll()

	// Wait for the resume goroutine to finish.
	for i := 0; i < 200; i++ {
		b.pendingMu.Lock()
		_, still := b.pending["shutid"]
		b.pendingMu.Unlock()
		if !still {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the goroutine a moment to run its deferred cleanup.
	time.Sleep(50 * time.Millisecond)

	if fs.cleaned.Load() {
		t.Error("shutdown must NOT clean the stage: it must survive for the next boot to resume")
	}
	// The gate marker must still be present (left for next boot).
	if _, err := os.Stat(gateMarkerPath(dir, "shutid")); err != nil {
		t.Errorf("gate marker must survive shutdown, got err: %v", err)
	}
}
