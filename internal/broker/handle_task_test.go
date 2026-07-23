package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/gateway"
	"drydock/internal/remote"
	"drydock/internal/stage"
)

// These tests drive the full broker.HandleTask lifecycle
// (resolve -> mint -> run -> diff -> gate -> push) through the prepareStage /
// runAgent seams, so no git clone or container run is needed.

// --- fakes ---

type fakeStage struct {
	workDir    string
	diff       string
	captureErr error
	pushErr    error
	pushed     atomic.Bool // atomic so the approve-resume test can poll without a race
	pushBranch string
	cleaned    atomic.Bool // atomic so the shutdown-survival test can poll without a race
	gotPrompt  string
}

func (f *fakeStage) WorkDir() string { return f.workDir }
func (f *fakeStage) WriteTaskFiles(prompt string) error {
	f.gotPrompt = prompt
	return nil
}
func (f *fakeStage) CaptureDiff() (string, error) { return f.diff, f.captureErr }
func (f *fakeStage) Cleanup() error               { f.cleaned.Store(true); return nil }
func (f *fakeStage) Push(branch, msg string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed.Store(true)
	f.pushBranch = branch
	return nil
}
func (f *fakeStage) Commit(branch, msg string) error {
	if f.pushErr != nil {
		return nil // let PushBranch surface the failure; Commit succeeds
	}
	return nil
}
func (f *fakeStage) PushBranch(local, remote string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed.Store(true)
	f.pushBranch = remote
	return nil
}
func (f *fakeStage) PushEnv() []string { return []string{"GIT_DIR=/fake"} }

type fakeAdapter struct {
	name    string
	openErr error
	opened  bool
	gotReq  remote.Request
}

func (a *fakeAdapter) Name() string     { return a.name }
func (a *fakeAdapter) Available() error { return nil }
func (a *fakeAdapter) OpenRequest(r remote.Request) error {
	a.opened = true
	a.gotReq = r
	return a.openErr
}

type fakeGrant struct {
	revoked bool
	spent   float64
}

func (g *fakeGrant) EnvVars() []string { return []string{"ANTHROPIC_AUTH_TOKEN=tok_test"} }
func (g *fakeGrant) Revoke() error     { g.revoked = true; return nil }
func (g *fakeGrant) Spent() float64    { return g.spent }

type mintingProvider struct{ grant *fakeGrant }

func (p *mintingProvider) Mint(float64) (creds.Grant, error) { return p.grant, nil }

// --- helpers ---

func testBroker(t *testing.T, vendor string, st taskStage, grant *fakeGrant,
	run func(context.Context, []string, io.Writer, io.Writer) error) *Broker {
	t.Helper()
	return &Broker{
		Cfg:           egress.Config{},
		Providers:     map[string]creds.Provider{vendor: &mintingProvider{grant}},
		DefaultAgent:  "claude",
		ImageRef:      "test-image:latest",
		StageRoot:     t.TempDir(),
		AuditRoot:     t.TempDir(),
		Timeout:       5 * time.Second,
		Network:       "testnet",
		GatewayIP:     "10.0.0.1",
		ProxyPort:     3128,
		TaskBudget:    1.0,
		MaxConcurrent: 4,
		prepareStage:  func(root, repo string) (taskStage, error) { return st, nil },
		runAgent:      run,
		newAdapter: func(repoRef, platform string) remote.Adapter {
			return &fakeAdapter{name: remote.AdapterFor(repoRef, platform).Name()}
		},
	}
}

// parseEvents splits an NDJSON body into decoded events. terminal is the last
// event — the result or error that ends a streamed task.
func parseEvents(body string) (events []map[string]any, terminal map[string]any) {
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil {
			events = append(events, ev)
		}
	}
	if len(events) > 0 {
		terminal = events[len(events)-1]
	}
	return events, terminal
}

func submit(b *Broker, body string) (*httptest.ResponseRecorder, []map[string]any, map[string]any) {
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.HandleTask(rec, req)
	events, terminal := parseEvents(rec.Body.String())
	return rec, events, terminal
}

// stages returns the ordered list of stage names from an event slice.
func stages(events []map[string]any) []string {
	var out []string
	for _, ev := range events {
		if ev["event"] == "stage" {
			if s, ok := ev["stage"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func writesResult(line string) func(context.Context, []string, io.Writer, io.Writer) error {
	return func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, line)
		return nil
	}
}

func readAudit(t *testing.T, dir, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatalf("read audit %s: %v", id, err)
	}
	return string(b)
}

func readOnlyAudit(t *testing.T, dir string) string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			return string(b)
		}
	}
	t.Fatalf("no audit .jsonl in %s", dir)
	return ""
}

// --- tests ---

func TestHandleTask_ClaudeAutoApprove_Pushes(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.02,"num_turns":2}`

	b := testBroker(t, "anthropic", st, grant, writesResult(resultLine))
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(events) == 0 {
		t.Fatalf("no events in response; body=%s", rec.Body)
	}
	// The instruction must be routed to the stage as the agent's prompt.
	if st.gotPrompt != "do x" {
		t.Errorf("WriteTaskFiles prompt=%q, want %q", st.gotPrompt, "do x")
	}
	if term["event"] != "result" || term["outcome"] != "pushed" {
		t.Fatalf("terminal=%v, want result/pushed (body=%s)", term, rec.Body)
	}
	id, _ := events[0]["task_id"].(string)
	if events[0]["event"] != "accepted" || id == "" {
		t.Fatalf("first event=%v, want accepted with task_id", events[0])
	}
	if term["branch"] != "agent/"+id {
		t.Errorf("branch=%v, want agent/%s", term["branch"], id)
	}
	if term["platform"] != "github" {
		t.Errorf("platform=%v, want github", term["platform"])
	}
	if !st.pushed.Load() || st.pushBranch != "agent/"+id {
		t.Errorf("stage.Push wrong: pushed=%v branch=%q", st.pushed.Load(), st.pushBranch)
	}
	if !grant.revoked {
		t.Error("grant.Revoke not called (defer)")
	}
	if !st.cleaned.Load() {
		t.Error("stage.Cleanup not called (defer)")
	}
	// The broker authors the terminal result (src:broker) for every agent,
	// including claude, so the displayed cost/outcome and the aggregate-cap seed
	// never trust the agent's own (forgeable) result line.
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"src":"broker"`) {
		t.Errorf("claude task missing the broker-authored result line:\n%s", audit)
	}
}

func TestHandleTask_CodexAutoApprove_SynthesizesResultWithCost(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/r b/r\n+z\n"}
	grant := &fakeGrant{spent: 0.0731}
	// codex exec emits no Claude-style result trailer; the broker synthesizes
	// one from the elapsed time and the metered gateway spend.
	b := testBroker(t, "openai", st, grant, writesResult("codex: applied an edit"))
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"codex","auto_approve":true}`)
	if rec.Code != http.StatusOK || term["outcome"] != "pushed" {
		t.Fatalf("code=%d outcome=%v body=%s", rec.Code, term["outcome"], rec.Body)
	}
	if len(events) == 0 {
		t.Fatalf("no events in response; body=%s", rec.Body)
	}
	id, _ := events[0]["task_id"].(string)
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"type":"result"`) || !strings.Contains(audit, `"subtype":"success"`) {
		t.Errorf("no synthesized success result:\n%s", audit)
	}
	if !strings.Contains(audit, "0.073100") {
		t.Errorf("synthesized cost (grant.Spent=0.0731 -> 0.073100) not in audit:\n%s", audit)
	}
}

func TestHandleTask_EmptyDiff_NoPush(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: ""} // agent changed nothing
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if term["event"] != "result" || term["outcome"] != "no_diff" {
		t.Errorf("terminal=%v, want result/no_diff", term)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called when the diff is empty")
	}
}

func TestHandleTask_AgentRunFails_ErrorEvent(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "ignored-on-failure"}
	grant := &fakeGrant{}
	run := func(context.Context, []string, io.Writer, io.Writer) error {
		return fmt.Errorf("container exited 1")
	}
	b := testBroker(t, "anthropic", st, grant, run)
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
	// Streaming has already sent 200 by the time the run fails; the failure is
	// the terminal error event, not an HTTP status.
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200 (streamed); body=%s", rec.Code, rec.Body)
	}
	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error event", term)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called when the agent run fails")
	}
	// The broker appends a synthetic error result so `drydock tasks` doesn't
	// show the failed task as `running?` forever.
	audit := readOnlyAudit(t, b.AuditRoot)
	if !strings.Contains(audit, `"subtype":"error"`) || !strings.Contains(audit, `"is_error":true`) {
		t.Errorf("no synthesized error result:\n%s", audit)
	}
}

// V-01: a staged diff that exceeds the review cap must fail the task closed,
// with an operator-facing reason that names the cap rather than the generic
// "diff capture failed" message. The wrapped error proves the broker's
// errors.Is(err, stage.ErrDiffTooLarge) check unwraps correctly.
func TestHandleTask_DiffTooLarge_FailsClosedWithReason(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), captureErr: fmt.Errorf("wrapped: %w", stage.ErrDiffTooLarge)}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200 (streamed); body=%s", rec.Code, rec.Body)
	}
	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error event", term)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "task failed closed: staged diff exceeds the 32 MiB review cap") {
		t.Errorf("reason=%q, want the V-01 fail-closed reason", reason)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called when the diff is too large")
	}
}

// A generic (non-ErrDiffTooLarge) capture failure must still surface the
// original, less specific "diff capture failed" reason, not the V-01 message.
func TestHandleTask_DiffCaptureFails_GenericReason(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), captureErr: fmt.Errorf("disk i/o error")}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error event", term)
	}
	reason, _ := term["reason"].(string)
	if reason != "diff capture failed" {
		t.Errorf("reason=%q, want %q", reason, "diff capture failed")
	}
}

func TestHandleTask_GatedApprove_Pushes(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	id := waitForPending(t, b)
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

func TestHandleTask_GatedDeny_NoPush(t *testing.T) {
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

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return after deny")
	}

	_, term := parseEvents(rec.Body.String())
	if term["event"] != "result" || term["outcome"] != "denied" {
		t.Errorf("terminal=%v, want result/denied; body=%s", term, rec.Body)
	}
	if st.pushed.Load() {
		t.Error("stage.Push must not be called after deny")
	}
}

// waitForPending blocks until a task registers at the approval gate and
// returns its id.
func waitForPending(t *testing.T, b *Broker) string {
	t.Helper()
	var id string
	if !waitFor(500*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		for k := range b.pending {
			id = k
			return true
		}
		return false
	}) {
		t.Fatal("task never reached the approval gate")
	}
	return id
}

func approve(t *testing.T, b *Broker, id string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/admin/approve/"+id, nil)
	r.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, r)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("approve code=%d, want 204", rr.Code)
	}
}

func deny(t *testing.T, b *Broker, id string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/admin/deny/"+id, nil)
	r.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	b.HandleDeny(rr, r)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("deny code=%d, want 204", rr.Code)
	}
}

// CancelAll must fire every registered task's cancel — that's what lets a
// graceful brokerd shutdown tear down each in-flight VM and unblock its client.
func TestCancelAll_CancelsEveryRegisteredTask(t *testing.T) {
	b := &Broker{}
	var calls int32
	for i := 0; i < 3; i++ {
		b.registerTask(fmt.Sprintf("task%d", i), "repo", "instr", func(error) { atomic.AddInt32(&calls, 1) })
	}
	b.CancelAll()
	if calls != 3 {
		t.Errorf("CancelAll fired %d cancels, want 3", calls)
	}
}

func TestHandleTask_StreamsStageSequence(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	rec, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if len(events) == 0 {
		t.Fatalf("no events in response; body=%s", rec.Body)
	}
	got := stages(events)
	want := []string{"preparing", "running", "pushing"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stage sequence = %v, want %v", got, want)
	}
	if events[0]["event"] != "accepted" {
		t.Errorf("first event = %v, want accepted", events[0])
	}
}

func TestHandleTask_ApprovalGateEvent(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	id := waitForPending(t, b)
	approve(t, b, id)
	<-done

	events, _ := parseEvents(rec.Body.String())
	var gate map[string]any
	for _, ev := range events {
		if ev["stage"] == "awaiting_approval" {
			gate = ev
		}
	}
	if gate == nil {
		t.Fatalf("no awaiting_approval event; body=%s", rec.Body)
	}
	if gate["approve"] != "drydock approve "+id {
		t.Errorf("approve hint = %v, want 'drydock approve %s'", gate["approve"], id)
	}
}

func TestHandleTask_BootFailure_DistilledReasonAndHint(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "ignored"}
	grant := &fakeGrant{}
	run := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, "[6/6] Starting container [0s]")
		fmt.Fprintln(stdout, "/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip")
		return fmt.Errorf("exit status 1")
	}
	b := testBroker(t, "anthropic", st, grant, run)
	_, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error", term)
	}
	if r, _ := term["reason"].(string); !strings.Contains(r, "missing gateway ip") {
		t.Errorf("reason=%q, want the distilled entrypoint line", term["reason"])
	}
	if h, _ := term["hint"].(string); !strings.Contains(h, "drydock doctor") {
		t.Errorf("hint=%q, want a 'drydock doctor' nudge", term["hint"])
	}
}

// A malicious agent that prints an ANSI escape as its last line, then exits
// non-zero, must not get that sequence reflected raw into the operator's
// terminal via the error event's reason (the distilled audit line is the
// agent's own output, so it goes through the same sanitizer as every other
// reflected error).
func TestHandleTask_RunErrorReasonIsSanitized(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir()}
	grant := &fakeGrant{}
	crafted := "entrypoint failed \x1b[31mBOOM\x1b[0m\x00"
	run := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, crafted) // teed into the audit log
		return fmt.Errorf("exit status 1")
	}
	b := testBroker(t, "anthropic", st, grant, run)

	_, _, term := submit(b, `{"repo_ref":"git@github.com:o/r","instruction":"x"}`)
	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error", term)
	}
	reason, _ := term["reason"].(string)
	if strings.ContainsRune(reason, 0x1b) || strings.ContainsRune(reason, 0x00) {
		t.Errorf("reason carries control bytes (ANSI/NUL injection): %q", reason)
	}
	if !strings.Contains(reason, "BOOM") {
		t.Errorf("reason should keep the human-readable text, got %q", reason)
	}
}

// TestHandleTask_SurvivesClientDisconnect verifies that cancelling the submit
// request context (^C / web-UI disconnect) does NOT cancel the running task.
// The task must complete and write a terminal result line to the audit log.
func TestHandleTask_SurvivesClientDisconnect(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	run := func(ctx context.Context, _ []string, stdout, _ io.Writer) error {
		close(started)
		// Wait on either the release channel or the task context being cancelled.
		// If taskCtx is (wrongly) derived from r.Context(), a client disconnect
		// will cancel ctx here and we'll return an error before emitting a result.
		select {
		case <-ctx.Done():
			return ctx.Err() // task was killed — no result line written
		case <-release:
		}
		// emit a minimal result so the task terminates like a real run
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":0,"num_turns":1}`)
		return nil
	}
	b := testBroker(t, "anthropic", st, grant, run)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	<-started
	cancel()       // client "disconnects" while the agent is mid-run
	close(release) // let the agent finish
	<-done

	// The task must have completed and written a result line despite the disconnect.
	assertAuditHasResult(t, b.AuditRoot)
}

// assertAuditHasResult scans the AuditRoot directory for a *.jsonl file
// containing a {"type":"result"} line, failing the test if none is found.
func assertAuditHasResult(t *testing.T, auditRoot string) {
	t.Helper()
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		t.Fatalf("assertAuditHasResult: ReadDir %s: %v", auditRoot, err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(auditRoot, e.Name()))
		if err != nil {
			t.Fatalf("assertAuditHasResult: read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(data), `"type":"result"`) {
			return // found it
		}
	}
	t.Fatalf("assertAuditHasResult: no *.jsonl in %s contains a {\"type\":\"result\"} line", auditRoot)
}

// A1 — the real upstream API key must never enter the sandbox VM. The env the
// broker assembles and hands to runner.BuildRunArgs (encoded as `--env K=V`
// args to the container) must carry only the ephemeral bearer and the
// proxy/base-URL vars — never the real key the provider holds on the host.
//
// This test uses a REAL gateway.Provider backed by gateway.StaticKey so that
// the assertion is genuine: realKey actually lives inside the gateway and the
// test verifies it cannot be observed in the container args.
func TestHandleTask_RealKeyNeverEntersContainerEnv(t *testing.T) {
	const realKey = "sk-ant-REALKEY-do-not-leak-3f9a2b"

	g, err := gateway.New(gateway.Backend{Vendor: gateway.AnthropicVendor(), Cred: gateway.StaticKey(realKey)})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	prov := &gateway.Provider{
		GW:         g,
		Vendor:     "anthropic",
		BaseURL:    "http://10.0.0.1:8088",
		BaseURLEnv: "ANTHROPIC_BASE_URL",
		TokenEnv:   "ANTHROPIC_AUTH_TOKEN",
		TTL:        time.Minute,
	}

	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	var gotArgs []string
	run := func(_ context.Context, args []string, stdout, _ io.Writer) error {
		gotArgs = append([]string(nil), args...)
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success"}`)
		return nil
	}
	b := &Broker{
		Providers:     map[string]creds.Provider{"anthropic": prov},
		DefaultAgent:  "claude",
		ImageRef:      "test-image:latest",
		StageRoot:     t.TempDir(),
		AuditRoot:     t.TempDir(),
		Timeout:       5 * time.Second,
		Network:       "testnet",
		GatewayIP:     "10.0.0.1",
		ProxyPort:     3128,
		TaskBudget:    1.0,
		MaxConcurrent: 4,
		prepareStage:  func(string, string) (taskStage, error) { return st, nil },
		runAgent:      run,
		newAdapter:    func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} },
	}

	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if rec.Code != http.StatusOK || term["outcome"] != "pushed" {
		t.Fatalf("code=%d outcome=%v body=%s", rec.Code, term["outcome"], rec.Body)
	}
	if gotArgs == nil {
		t.Fatal("runAgent was never called; nothing to assert about the container env")
	}

	joined := strings.Join(gotArgs, "\x00")

	// (a) The security property: the real key is absent from every arg handed to
	// the container. This is now genuine — realKey lives inside gateway.StaticKey
	// and never surfaces via EnvVars().
	if strings.Contains(joined, realKey) {
		t.Errorf("A1 BREACH: real API key leaked into the container env args: %v", gotArgs)
	}

	// (b) Positive control: a tok_-prefixed token IS present, proving we're
	// inspecting the real env assembled by the broker (the assertion above isn't vacuous).
	if !strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=tok_") {
		t.Errorf("expected ANTHROPIC_AUTH_TOKEN=tok_... in container env, got args: %v", gotArgs)
	}

	// (c) The gateway base URL must be present so the VM can reach the proxy.
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=http://10.0.0.1:8088") {
		t.Errorf("expected ANTHROPIC_BASE_URL=http://10.0.0.1:8088 in container env, got args: %v", gotArgs)
	}

	// And the audit trail must not carry the real key either.
	if audit := readOnlyAudit(t, b.AuditRoot); strings.Contains(audit, realKey) {
		t.Errorf("A1 BREACH: real API key leaked into the audit log:\n%s", audit)
	}
}

// A PR-open failure must NOT downgrade a successful push to a failure: the
// branch is saved, so the result is "pushed" with pr_opened=false.
func TestHandleTask_PROpenFailure_StillPushed(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	grant := &fakeGrant{}
	resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.01,"num_turns":1}`
	b := testBroker(t, "anthropic", st, grant, writesResult(resultLine))
	b.newAdapter = func(repoRef, platform string) remote.Adapter {
		return &fakeAdapter{name: "github", openErr: fmt.Errorf("gh: not authenticated")}
	}
	_, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed (a saved branch must never report failure)", term["outcome"])
	}
	if term["pr_opened"] != false {
		t.Errorf("pr_opened = %v, want false", term["pr_opened"])
	}
	if term["pr_error"] == nil {
		t.Error("pr_error should carry the adapter failure reason")
	}
	if !st.pushed.Load() {
		t.Error("the branch must still have been pushed")
	}
}

func TestHandleTask_PushFailed_TerminalOutcome(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x", pushErr: errors.New("fatal: Could not resolve host")}
	grant := &fakeGrant{}
	resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.02,"num_turns":1}`
	b := testBroker(t, "anthropic", st, grant, writesResult(resultLine))
	b.PushMaxRetries = 1
	b.PushRetryBackoff = 0 // no real sleeps in the test
	b.PushFreshBranchTries = 0

	_, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "push_failed" {
		t.Fatalf("outcome = %v, want push_failed", term["outcome"])
	}
	if term["reason"] != "transient" {
		t.Errorf("reason = %v, want transient", term["reason"])
	}
	if audit := readOnlyAudit(t, b.AuditRoot); !strings.Contains(audit, `"subtype":"push_failed"`) {
		t.Errorf("audit log missing synthetic push_failed result line:\n%s", audit)
	}
}

// TestHandleTask_AggregateBudgetExhausted verifies that when AggregateExceeded
// returns true for the task's vendor, HandleTask rejects the submission with
// HTTP 402 before the stream starts and before any lease is minted.
func TestHandleTask_AggregateBudgetExhausted(t *testing.T) {
	b := &Broker{
		AuditRoot:         t.TempDir(),
		Providers:         map[string]creds.Provider{"anthropic": fakeProvider{}},
		AggregateExceeded: func(vendor string) bool { return true },
	}
	body := `{"repo_ref":"https://github.com/o/r","instruction":"x","agent":"claude"}`
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(body))
	rr := httptest.NewRecorder()
	b.HandleTask(rr, req)
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "aggregate budget exhausted") {
		t.Errorf("body = %q, want the aggregate-exhausted message", rr.Body.String())
	}
}
