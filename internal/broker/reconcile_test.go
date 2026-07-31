package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/audit"
	"drydock/internal/config"
	"drydock/internal/egress"
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
	// src:"broker" — the check is now "is there a BROKER-authored result row",
	// so an agent-authored one no longer suppresses the honest synthetic
	// terminal. See audit.HasBrokerResultLine.
	body := `{"type":"stream_event"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"src":"broker"}` + "\n"
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
	b.WriteString(`{"type":"result","subtype":"success","is_error":false,"src":"broker"}` + "\n")
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
	// A diff IS present: this test must reach the stage-reopen (which fails),
	// not short-circuit at the diff fail-safe that now runs first.
	os.WriteFile(filepath.Join(dir, "gone.diff"), []byte("diff"), 0o600)
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

// TestResumeAwaiting_MissingDiff_FailsSafe is the fail-open regression test:
// the resumed gate recomputes its second-look acknowledgment requirement from
// the persisted <id>.diff, so a missing (or unreadable/empty) diff must NOT
// resume the task as an approvable gate — an empty diff has no flags, so
// requiredAcks would be nil and a bare approve would push a branch whose
// original gate required acks. Expected: same handling as a gone stage —
// honest interrupted terminal, marker dropped, task never pending.
func TestResumeAwaiting_MissingDiff_FailsSafe(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "nodiff", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	auditFile := filepath.Join(dir, "nodiff.jsonl")
	os.WriteFile(auditFile, []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1}`+"\n"), 0o600)
	// Deliberately NO nodiff.diff on disk, though the stage itself survived.
	writeGateMarker(dir, "nodiff", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "nodiff", "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}
	b.ResumeAwaiting(stageRoot)

	// The fail-safe branch runs inline in ResumeAwaiting (no goroutine), so the
	// terminal line and marker drop are visible on return.
	data, _ := os.ReadFile(auditFile)
	if !strings.Contains(string(data), `"subtype":"interrupted"`) {
		t.Errorf("missing-diff task should get an interrupted line, got:\n%s", data)
	}
	if _, err := os.Stat(gateMarkerPath(dir, "nodiff")); !os.IsNotExist(err) {
		t.Error("marker should be removed after the missing-diff fallback")
	}
	// Grace period: if a resume goroutine had (wrongly) been spawned, it would
	// register the task as pending within this window.
	time.Sleep(50 * time.Millisecond)
	b.pendingMu.Lock()
	_, pending := b.pending["nodiff"]
	_, registered := b.tasks["nodiff"]
	b.pendingMu.Unlock()
	if pending || registered {
		t.Errorf("missing-diff task must not be resumed as approvable (pending=%v registered=%v)", pending, registered)
	}
}

// TestReadDiffNoFollow_RefusesSymlink gives the resume-side diff read the same
// symlink defense as persistDiff's write side: a planted <id>.diff -> elsewhere
// must fail (and thus fail SAFE via the missing-diff branch), not feed the
// resumed gate substituted bytes.
func TestReadDiffNoFollow_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.diff")
	if err := os.WriteFile(target, []byte("diff --git a/x b/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.diff")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if s, err := readDiffNoFollow(link); err == nil {
		t.Fatalf("readDiffNoFollow should refuse a symlinked path (O_NOFOLLOW), got %q", s)
	}
}

// TestResumeAwaiting_RecomputesRequiredAcks proves the second-look
// acknowledgment requirement survives a brokerd restart: a task parked at the
// gate with a flagged diff must come back pending WITH its recomputed
// b.requiredAcks entry, so an ack-less approve is still rejected after the
// bounce (the ack-bypass the recompute in resumePush exists to prevent).
func TestResumeAwaiting_RecomputesRequiredAcks(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "ackid", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The persisted diff touches a CI workflow (flag kind "ci-workflow"), and
	// the policy second-looks workflow paths: the resumed gate must require it.
	os.WriteFile(filepath.Join(dir, "ackid.diff"), []byte(workflowDiff), 0o600)
	writeGateMarker(dir, "ackid", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "ackid", "work")}
	b := &Broker{AuditRoot: dir,
		DiffPolicy:  config.DiffPolicy{SecondLookPaths: []string{".github/workflows/**"}},
		reopenStage: func(root string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}
	b.ResumeAwaiting(stageRoot)

	if !waitFor(2*time.Second, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["ackid"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("resumed task never reached the approval gate")
	}
	// awaitGate registers b.requiredAcks atomically with b.pending, so once the
	// task is pending the requirement (if any) is already in place.
	b.pendingMu.Lock()
	acks := append([]string(nil), b.requiredAcks["ackid"]...)
	ch := b.pending["ackid"]
	b.pendingMu.Unlock()
	if len(acks) != 1 || acks[0] != "ci-workflow" {
		t.Errorf("requiredAcks after resume = %v, want [ci-workflow] (restart must not drop the second-look requirement)", acks)
	}

	// Resolve the gate so the resume goroutine and its deferred cleanup exit
	// promptly instead of leaking past the test.
	if ch != nil {
		ch <- gateReply{ok: true, acks: acks}
	}
	waitFor(2*time.Second, func() bool {
		b.pendingMu.Lock()
		_, still := b.tasks["ackid"]
		b.pendingMu.Unlock()
		return !still
	})
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
			ch <- gateReply{ok: true}
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
		ch <- gateReply{ok: true}
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

// ---- ResumeQueue: boot resume of the durable task queue (Task 3) ----

// queueTestID returns a valid 32-hex queue id built from one repeated rune, so
// each fixture item in these tests has a distinct, readable id.
func queueTestID(r rune) string { return strings.Repeat(string(r), 32) }

// countingAgent returns a runAgent fake that counts invocations and writes a
// successful terminal result — the probe every ResumeQueue test uses to prove
// whether a task's VM was (or was NOT) booted.
func countingAgent(runs *int64) func(context.Context, []string, io.Writer, io.Writer) error {
	return func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		atomic.AddInt64(runs, 1)
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success"}`)
		return nil
	}
}

// writeQueueFixture persists a queue item in the given state, as a prior
// brokerd life would have left it.
func writeQueueFixture(t *testing.T, b *Broker, id string, state QueueState) QueueItem {
	t.Helper()
	now := time.Now().UnixMilli()
	it := QueueItem{
		ID:           id,
		Task:         Task{RepoRef: "https://github.com/o/r.git", Instruction: "do x", AutoApprove: true},
		State:        state,
		EnqueuedAtMs: now,
		UpdatedAtMs:  now,
	}
	if state != QueueQueued {
		it.StartedAtMs = now
		it.Attempts = 1
	}
	if err := writeQueueItem(b.AuditRoot, it); err != nil {
		t.Fatalf("writeQueueItem(%s): %v", id, err)
	}
	return it
}

// TestResumeQueue_QueuedItemRedispatched: a still-`queued` item (provably never
// dispatched) survives a restart and is re-dispatched by the new life's
// dispatcher — exactly once, even when ResumeQueue itself runs twice
// (idempotency: the re-append dedupes by id against the in-memory queue).
func TestResumeQueue_QueuedItemRedispatched(t *testing.T) {
	var agentRuns int64
	b := queueBroker(t, 2, countingAgent(&agentRuns))
	id := queueTestID('a')
	writeQueueFixture(t, b, id, QueueQueued)

	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue (second run): %v", err)
	}
	b.queueMu.Lock()
	n := len(b.queue)
	b.queueMu.Unlock()
	if n != 1 {
		t.Fatalf("in-memory queue holds %d copies after two ResumeQueue runs, want 1", n)
	}

	b.StartDispatcher()
	defer b.StopDispatcher()
	waitForQueueState(t, b, id, QueueCompleted)
	if got := atomic.LoadInt64(&agentRuns); got != 1 {
		t.Errorf("agent ran %d times for one resumed queued item, want exactly 1", got)
	}

	// A third ResumeQueue after the item went terminal must not resurrect it.
	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue after terminal: %v", err)
	}
	b.queueMu.Lock()
	n = len(b.queue)
	b.queueMu.Unlock()
	if n != 0 {
		t.Errorf("terminal item re-entered the in-memory queue (len=%d), want 0", n)
	}
}

// TestResumeQueue_RunningItemDeadLettersNeverReRuns is THE crux test: an item
// that was `running` when the previous brokerd died may have spent budget or
// pushed — it must NEVER be re-dispatched (no second VM). ResumeQueue instead
// reconciles it to dead_letter ("interrupted by restart"), and the fake agent
// proves no dispatch ever happens.
func TestResumeQueue_RunningItemDeadLettersNeverReRuns(t *testing.T) {
	var agentRuns int64
	b := queueBroker(t, 2, countingAgent(&agentRuns))
	id := queueTestID('b')
	writeQueueFixture(t, b, id, QueueRunning)
	// The audit trace the crashed life left behind: no terminal result line
	// (the daemon died under the task).
	if err := os.WriteFile(filepath.Join(b.AuditRoot, id+".jsonl"),
		[]byte(`{"type":"stream_event"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	it := queueItemState(t, b, id)
	if it.State != QueueDeadLetter {
		t.Fatalf("running-at-boot item state = %q, want dead_letter (never re-run)", it.State)
	}
	if it.LastError != "interrupted by restart" {
		t.Errorf("LastError = %q, want %q", it.LastError, "interrupted by restart")
	}

	// Even with the dispatcher running, no second VM may ever boot.
	b.StartDispatcher()
	defer b.StopDispatcher()
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&agentRuns); got != 0 {
		t.Fatalf("agent ran %d times for an interrupted running item — a second VM was booted", got)
	}
}

// TestResumeQueue_PreparingAndVerifyingDeadLetterUniformly pins the carried
// fix-3 decision: preparing/verifying items at boot dead-letter uniformly —
// even a stranded `preparing` whose audit trace shows a SUCCESS terminal (the
// failed preparing->running persist case). preparing->completed is
// deliberately not a legal transition; the honest, never-re-run terminal is
// dead_letter "interrupted by restart".
func TestResumeQueue_PreparingAndVerifyingDeadLetterUniformly(t *testing.T) {
	var agentRuns int64
	b := queueBroker(t, 2, countingAgent(&agentRuns))
	prepID, verID := queueTestID('c'), queueTestID('d')
	writeQueueFixture(t, b, prepID, QueuePreparing)
	writeQueueFixture(t, b, verID, QueueVerifying)
	// The stranded-preparing case: the task actually finished (success
	// terminal on the trace) but the preparing->running persist failed.
	if err := os.WriteFile(filepath.Join(b.AuditRoot, prepID+".jsonl"),
		[]byte(`{"type":"result","subtype":"success","is_error":false}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	for _, id := range []string{prepID, verID} {
		it := queueItemState(t, b, id)
		if it.State != QueueDeadLetter || it.LastError != "interrupted by restart" {
			t.Errorf("item %s = (%q, %q), want (dead_letter, interrupted by restart)", id, it.State, it.LastError)
		}
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&agentRuns); got != 0 {
		t.Fatalf("agent ran %d times for interrupted items, want 0", got)
	}
}

// TestResumeQueue_AwaitingReviewDefersToResumeAwaiting: an awaiting_review
// item whose gate marker survived is owned by ResumeAwaiting (the marker
// drives the headless gate re-drive); ResumeQueue must leave it untouched and
// never re-dispatch it. Without a marker the gate can never be re-posed
// (ResumeAwaiting's fail-safe dropped it), so the item dead-letters honestly.
func TestResumeQueue_AwaitingReviewDefersToResumeAwaiting(t *testing.T) {
	t.Run("marker survives: untouched, not re-dispatched", func(t *testing.T) {
		var agentRuns int64
		b := queueBroker(t, 2, countingAgent(&agentRuns))
		id := queueTestID('e')
		before := writeQueueFixture(t, b, id, QueueAwaitingReview)
		if err := writeGateMarker(b.AuditRoot, id, gateMarker{RepoRef: "https://github.com/o/r.git", Agent: "claude"}); err != nil {
			t.Fatal(err)
		}

		if err := b.ResumeQueue(b.StageRoot); err != nil {
			t.Fatalf("ResumeQueue: %v", err)
		}
		after := queueItemState(t, b, id)
		if after.State != QueueAwaitingReview || after.UpdatedAtMs != before.UpdatedAtMs {
			t.Errorf("awaiting_review item modified: state=%q updated=%d, want untouched (%q, %d)",
				after.State, after.UpdatedAtMs, before.State, before.UpdatedAtMs)
		}
		b.StartDispatcher()
		defer b.StopDispatcher()
		time.Sleep(50 * time.Millisecond)
		if got := atomic.LoadInt64(&agentRuns); got != 0 {
			t.Fatalf("agent ran %d times for an awaiting_review item, want 0 (ResumeAwaiting owns it)", got)
		}
	})
	t.Run("marker gone: dead-lettered honestly", func(t *testing.T) {
		b := queueBroker(t, 2, countingAgent(new(int64)))
		id := queueTestID('f')
		writeQueueFixture(t, b, id, QueueAwaitingReview) // deliberately NO gate marker
		if err := b.ResumeQueue(b.StageRoot); err != nil {
			t.Fatalf("ResumeQueue: %v", err)
		}
		it := queueItemState(t, b, id)
		if it.State != QueueDeadLetter || it.LastError != "interrupted by restart" {
			t.Errorf("marker-less awaiting_review item = (%q, %q), want (dead_letter, interrupted by restart)", it.State, it.LastError)
		}
	})
}

// TestResumeQueue_RunningWithGateMarkerHandsToResumeAwaiting: a `running`
// record whose gate marker survived proves the task actually reached the gate
// (the awaiting_review persist raced the crash). ResumeQueue must move it to
// awaiting_review — deferring to ResumeAwaiting's re-drive — instead of
// dead-lettering a task whose completed work is resumable.
func TestResumeQueue_RunningWithGateMarkerHandsToResumeAwaiting(t *testing.T) {
	var agentRuns int64
	b := queueBroker(t, 2, countingAgent(&agentRuns))
	id := queueTestID('9')
	writeQueueFixture(t, b, id, QueueRunning)
	if err := writeGateMarker(b.AuditRoot, id, gateMarker{RepoRef: "https://github.com/o/r.git", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	if it := queueItemState(t, b, id); it.State != QueueAwaitingReview {
		t.Fatalf("running-with-marker item state = %q, want awaiting_review (gate marker owns the resume)", it.State)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&agentRuns); got != 0 {
		t.Fatalf("agent ran %d times, want 0 (no second VM)", got)
	}
}

// TestResumeQueue_TerminalItemsUntouched: completed/dead_letter/cancelled
// records are history for `drydock queue list` — ResumeQueue leaves them
// byte-identical and never re-enqueues them.
func TestResumeQueue_TerminalItemsUntouched(t *testing.T) {
	b := queueBroker(t, 2, countingAgent(new(int64)))
	fixtures := map[string]QueueState{
		queueTestID('1'): QueueCompleted,
		queueTestID('2'): QueueDeadLetter,
		queueTestID('3'): QueueCancelled,
	}
	before := map[string]QueueItem{}
	for id, st := range fixtures {
		before[id] = writeQueueFixture(t, b, id, st)
	}
	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	for id := range fixtures {
		if got := queueItemState(t, b, id); !reflect.DeepEqual(got, before[id]) {
			t.Errorf("terminal item %s modified:\n got %+v\nwant %+v", id, got, before[id])
		}
	}
	b.queueMu.Lock()
	n := len(b.queue)
	b.queueMu.Unlock()
	if n != 0 {
		t.Errorf("terminal items re-entered the in-memory queue (len=%d), want 0", n)
	}
}

// TestRunQueued_ShutdownAtGateStaysNonTerminal is the carried-fix-1 test: a
// queued task shut down while parked at the diff-approval gate keeps its gate
// marker (gatePushMarked leaves it on gateShutdown) and WILL be resumed and
// pushed by the next boot's ResumeAwaiting — so the queue record must stay
// non-terminal (awaiting_review), never `cancelled`. The second half proves
// the full round trip: the next life resumes the gate, an approve pushes, and
// the gate resolution finalizes the queue record to completed with no second
// agent run.
func TestRunQueued_ShutdownAtGateStaysNonTerminal(t *testing.T) {
	var agentRuns int64
	b := queueBroker(t, 2, countingAgent(&agentRuns))
	// AutoApprove deliberately false: the task must park at the human gate.
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "do x"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	waitForQueueState(t, b, id, QueueAwaitingReview)

	// brokerd shutdown while the task waits at the gate.
	b.CancelAll()
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		_, live := b.tasks[id]
		b.pendingMu.Unlock()
		return !live
	}) {
		t.Fatal("queued task never unregistered after shutdown")
	}

	it := queueItemState(t, b, id)
	if it.State != QueueAwaitingReview {
		t.Fatalf("shutdown-at-gate item state = %q, want awaiting_review (non-terminal: the next boot resumes it)", it.State)
	}
	if _, err := os.Stat(gateMarkerPath(b.AuditRoot, id)); err != nil {
		t.Fatalf("gate marker must survive shutdown for the next boot: %v", err)
	}

	// ---- next boot: ResumeAwaiting re-drives the gate; approve pushes and
	// finalizes the queue record. No agent re-run.
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, id, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	fs := &fakeStage{workDir: filepath.Join(stageRoot, id, "work")}
	b2 := &Broker{AuditRoot: b.AuditRoot, StageRoot: stageRoot,
		reopenStage: func(string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}
	b2.ResumeAwaiting(stageRoot)
	if err := b2.ResumeQueue(stageRoot); err != nil {
		t.Fatalf("ResumeQueue (second life): %v", err)
	}
	// ResumeQueue must not have touched the awaiting_review record or
	// re-enqueued it (b2 has no runAgent: a dispatch would nil-panic).
	b2.queueMu.Lock()
	n := len(b2.queue)
	b2.queueMu.Unlock()
	if n != 0 {
		t.Fatalf("awaiting_review item re-entered the in-memory queue (len=%d), want 0", n)
	}
	if !waitFor(2*time.Second, func() bool {
		b2.pendingMu.Lock()
		_, ok := b2.pending[id]
		b2.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("resumed task never reached the approval gate in the second life")
	}
	b2.pendingMu.Lock()
	ch := b2.pending[id]
	b2.pendingMu.Unlock()
	ch <- gateReply{ok: true}

	waitForQueueState(t, b2, id, QueueCompleted)
	if !fs.pushed.Load() {
		t.Error("approve after resume should have pushed the surviving branch")
	}
	if got := atomic.LoadInt64(&agentRuns); got != 1 {
		t.Errorf("agent ran %d times across both lives, want exactly 1 (never re-run)", got)
	}
}

// TestRunQueued_KillAtGateCancels guards the other half of carried fix 1: a
// GENUINE kill at the gate (not a shutdown) is a real terminal — the gate
// marker is removed and the queue record lands `cancelled`.
func TestRunQueued_KillAtGateCancels(t *testing.T) {
	b := queueBroker(t, 2, countingAgent(new(int64)))
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "do x"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	waitForQueueState(t, b, id, QueueAwaitingReview)

	killReq := httptest.NewRequest("POST", "/admin/kill/"+id, nil)
	killReq.SetPathValue("id", id)
	killRec := httptest.NewRecorder()
	b.HandleKill(killRec, killReq)
	if killRec.Code != http.StatusNoContent {
		t.Fatalf("kill code=%d, want 204", killRec.Code)
	}

	waitForQueueState(t, b, id, QueueCancelled)
	if _, err := os.Stat(gateMarkerPath(b.AuditRoot, id)); !os.IsNotExist(err) {
		t.Errorf("gate marker should be removed after a genuine kill, stat err=%v", err)
	}
}

// TestRunQueued_EgressDenyCancelsNotDeadLetter is the carried-fix-2 test: a
// deliberate human DENY at the egress-widening gate is a cancellation (like
// the diff-gate deny), not a system failure — the queue record must land
// `cancelled` with no misleading LastError, and the agent must never run.
func TestRunQueued_EgressDenyCancelsNotDeadLetter(t *testing.T) {
	var agentRuns int64
	b := queueBroker(t, 2, countingAgent(&agentRuns)) // egress.Config{} -> widening gate ON (fail-closed default)
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "do x", AutoApprove: true,
		EgressExtra: []egress.Domain{{Host: "example.com", Ports: []int{443}}}})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()

	// Wait for the task to park at the egress gate, then deny it.
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending[id]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("queued task never reached the egress gate")
	}
	b.pendingMu.Lock()
	ch := b.pending[id]
	b.pendingMu.Unlock()
	ch <- gateReply{ok: false}

	waitForQueueState(t, b, id, QueueCancelled)
	it := queueItemState(t, b, id)
	if it.LastError != "" {
		t.Errorf("egress-denied item LastError = %q, want empty (a deny is not a failure)", it.LastError)
	}
	if got := atomic.LoadInt64(&agentRuns); got != 0 {
		t.Errorf("agent ran %d times for an egress-denied task, want 0", got)
	}
}

// TestTerminateStuckAudits_AnAgentResultRowDoesNotSuppressTheHonestTerminal.
//
// The idempotency check used to be "does this trace hold a result line", and a
// task trace is a file the agent's own stdout is copied into. EVERY agent CLI
// prints a {"type":"result"} line, so in practice the check was always
// satisfied by the agent and the synthetic terminal was almost never appended
// to a real crashed run. A crashed task's trace therefore ended on the AGENT's
// row — the row seedAggregateFromAudit reads for the per-vendor cap reseed, and
// the row every cost renderer shows.
//
// The check is now "does this trace hold a BROKER-AUTHORED result row", so the
// honest terminal lands after the agent's, and last-wins readers see the truth.
//
// THE RESIDUAL IS STATED IN THE SUBTEST BELOW and is not fixable at this layer:
// `src` is a self-declared string in an agent-writable file, so an agent that
// forges src:"broker" still suppresses this. That is exactly why the global
// usage ceiling stopped reading trace content altogether rather than trying to
// authenticate it (cmd/brokerd/globalreconcile.go).
func TestTerminateStuckAudits_AnAgentResultRowDoesNotSuppressTheHonestTerminal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0123456789abcdef0123456789abcdef.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"drydock_task","agent":"claude"}` + "\n" +
		// The agent's own result row, claiming a clean finish and a cost.
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":999999,"num_turns":1}` + "\n" +
		`{"type":"stream_event"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("terminated %d traces, want 1: an agent-printed result row suppressed the honest synthetic terminal", n)
	}
	assertLastLineInterrupted(t, path)

	// The honest row is broker-authored (so the check stays idempotent) but
	// carries no spend information, so it can neither be read as a trusted $0
	// nor shadow a REAL broker row beneath it.
	res, ok := audit.LastBrokerResult(path)
	if ok {
		t.Errorf("LastBrokerResult returned %+v; the synthetic terminal must not be read as a metered figure", res)
	}
	// Running it again adds nothing.
	before, _ := os.ReadFile(path)
	if n2, err := TerminateStuckAudits(dir); err != nil || n2 != 0 {
		t.Fatalf("second sweep = (%d,%v), want (0,nil)", n2, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("the sweep is not idempotent: a second boot appended another terminal")
	}
}

// A REAL broker result row beneath the synthetic terminal is still found: the
// synthetic row is skipped, not treated as the last word on spend. This is what
// keeps a resumed task's own metered figure recoverable.
func TestTerminateStuckAudits_SyntheticTerminalDoesNotShadowARealBrokerRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0123456789abcdef0123456789abcdee.jsonl")
	body := `{"type":"result","subtype":"success","is_error":false,"total_cost_usd":4.25,"num_turns":1,"src":"broker"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force the synthetic terminal on regardless (this is the shape ResumeAwaiting
	// produces when it gives up on a gate whose stage is gone).
	if err := appendLine(path, interruptedResultLine); err != nil {
		t.Fatal(err)
	}
	res, ok := audit.LastBrokerResult(path)
	if !ok || res.TotalCostUSD != 4.25 {
		t.Errorf("LastBrokerResult = (%+v,%v), want the real 4.25 row beneath the synthetic terminal", res, ok)
	}
}

// TestTerminateStuckAudits_AForgedSrcBrokerRowIsAKNOWNRESIDUAL pins the limit
// of the layer above, so nobody reads the src filter as a trust boundary.
//
// `src` is a self-declared string in a file the agent's stdout is copied into,
// so an agent that prints src:"broker" is indistinguishable from the broker. It
// therefore still suppresses the synthetic terminal, and it can still reach
// seedAggregateFromAudit's per-vendor cap reseed and the cost columns in
// `drydock tasks` / `drydock stats` / the web UI (where it renders WITHOUT the
// agent-reported "?" mark, because by construction it claims not to be
// agent-reported).
//
// The blast radius is bounded and it is fail-CLOSED: an inflated per-vendor
// aggregate cap refuses tasks, it does not admit them. The GLOBAL ceiling's two
// limbs are unaffected in either direction — it does not read this file — which
// is asserted directly in
// cmd/brokerd:TestReconcile_ForgedBrokerRowCannotRaiseTheUSDLimb.
//
// The real fix is to stop the broker sharing a byte stream with the agent (a
// broker-only sidecar for metered spend). Documented in docs/THREAT_MODEL.md.
func TestTerminateStuckAudits_AForgedSrcBrokerRowIsAKNOWNRESIDUAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0123456789abcdef0123456789abcdea.jsonl")
	body := `{"type":"result","subtype":"success","is_error":false,"total_cost_usd":999999,"num_turns":1,"src":"broker"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("terminated %d, want 0 — if this now terminates, the residual documented above has been "+
			"closed and both this test and docs/THREAT_MODEL.md should say so", n)
	}
}
