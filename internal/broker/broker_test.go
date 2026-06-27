package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"drydock/internal/egress"
)

// HandleTask is exercised by the host-integration end-to-end test (Task 10);
// its pure helpers now live in the gateway and creds packages.

// TestMain silences the operator-facing macOS notification that the approval
// gate would otherwise pop up on every test run on a developer's Mac.
func TestMain(m *testing.M) {
	os.Setenv("DRYDOCK_NO_NOTIFY", "1")
	os.Exit(m.Run())
}

func TestGitURLRef(t *testing.T) {
	cases := []struct {
		in    string
		valid bool
	}{
		// Accept the four URL shapes for GitHub, GitLab.com, self-hosted
		// GitLab, and other generic git hosts — adapter selection is now
		// separate from URL validation.
		{"https://github.com/sricola/drydock", true},
		{"https://github.com/sricola/drydock.git", true},
		{"git@github.com:sricola/drydock", true},
		{"git@github.com:sricola/drydock.git", true},
		{"ssh://git@github.com/sricola/drydock.git", true},
		{"https://gitlab.com/group/project", true},
		{"git@gitlab.com:group/project.git", true},
		{"git@gitlab.mycorp.com:group/project", true},
		{"https://gitlab.mycorp.com/group/project", true},
		{"git@bitbucket.org:owner/repo", true},
		{"ssh://git@git.kernel.org/torvalds/linux", true},
		// Reject local paths (gh/glab can't operate on those).
		{"/Users/sray/gits/drydock", false},
		{"./drydock", false},
		{"file:///Users/sray/gits/drydock", false},
		// Reject malformed inputs.
		{"", false},
		{"https://github.com/", false},
		{"https://github.com/onlyowner", false},
		{"github.com/x/y", false},
	}
	for _, tc := range cases {
		got := gitURLRef.MatchString(tc.in)
		if got != tc.valid {
			t.Errorf("MatchString(%q) = %v, want %v", tc.in, got, tc.valid)
		}
	}
}

func TestGatePush_AutoApproveBypassesGate(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	if !b.gatePush(context.Background(), "task1", "diff", true) {
		t.Fatal("AutoApprove=true must return true without waiting")
	}
}

func TestGatePush_BlocksUntilApprove(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	done := make(chan bool, 1)
	go func() { done <- b.gatePush(context.Background(), "task2", "diff", false) }()

	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["task2"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("task never registered as pending")
	}

	req := httptest.NewRequest("POST", "/admin/approve/task2", nil)
	req.SetPathValue("id", "task2")
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, req)
	if rr.Code != 204 {
		t.Fatalf("approve handler returned %d, want 204", rr.Code)
	}

	select {
	case got := <-done:
		if !got {
			t.Fatal("gatePush returned false after approve")
		}
	case <-time.After(time.Second):
		t.Fatal("gatePush did not return after approve")
	}
}

func TestGatePush_DenyReturnsFalse(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	done := make(chan bool, 1)
	go func() { done <- b.gatePush(context.Background(), "task3", "diff", false) }()

	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["task3"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("task never registered as pending")
	}

	req := httptest.NewRequest("POST", "/admin/deny/task3", nil)
	req.SetPathValue("id", "task3")
	rr := httptest.NewRecorder()
	b.HandleDeny(rr, req)
	if rr.Code != 204 {
		t.Fatalf("deny handler returned %d, want 204", rr.Code)
	}

	select {
	case got := <-done:
		if got {
			t.Fatal("gatePush returned true after deny")
		}
	case <-time.After(time.Second):
		t.Fatal("gatePush did not return after deny")
	}
}

func TestGatePush_ClientDisconnectAbortsPush(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- b.gatePush(ctx, "task4", "diff", false) }()

	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["task4"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("task never registered as pending")
	}
	cancel()
	select {
	case got := <-done:
		if got {
			t.Fatal("gatePush returned true after client disconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("gatePush did not abort after client disconnect")
	}
}

func TestGatePush_UnknownIDReturns404(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	req := httptest.NewRequest("POST", "/admin/approve/does-not-exist", nil)
	req.SetPathValue("id", "does-not-exist")
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, req)
	if rr.Code != 404 {
		t.Fatalf("approve for unknown id: got %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no such pending task") {
		t.Errorf("404 body = %q", rr.Body.String())
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestAcquireSlot_DefaultsToTwo(t *testing.T) {
	b := &Broker{}
	if !b.acquireSlot() {
		t.Fatal("first acquire must succeed")
	}
	if !b.acquireSlot() {
		t.Fatal("second acquire must succeed (default cap is 2)")
	}
	if b.acquireSlot() {
		t.Fatal("third acquire must be rejected at default cap")
	}
	b.releaseSlot()
	if !b.acquireSlot() {
		t.Fatal("after release, a slot must be acquirable")
	}
}

func TestAcquireSlot_RespectsMaxConcurrent(t *testing.T) {
	b := &Broker{MaxConcurrent: 1}
	if !b.acquireSlot() {
		t.Fatal("first must succeed")
	}
	if b.acquireSlot() {
		t.Fatal("second must be rejected (cap = 1)")
	}
}

func TestAcquireSlot_RaceClean(t *testing.T) {
	// Race detector should not flag concurrent acquires.
	b := &Broker{MaxConcurrent: 3}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.acquireSlot() {
				time.Sleep(2 * time.Millisecond)
				b.releaseSlot()
			}
		}()
	}
	wg.Wait()
}

func TestHandleTask_503WhenSaturated(t *testing.T) {
	b := &Broker{MaxConcurrent: 1}
	// Pre-fill the only slot so HandleTask hits the cap on entry.
	if !b.acquireSlot() {
		t.Fatal("setup acquire failed")
	}
	defer b.releaseSlot()

	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(
		`{"repo_ref":"git@github.com:o/r","instruction":"x"}`))
	rr := httptest.NewRecorder()
	b.HandleTask(rr, req)

	if rr.Code != 503 {
		t.Fatalf("got %d, want 503; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "DRYDOCK_MAX_CONCURRENT_TASKS") {
		t.Errorf("503 body lacks env knob hint: %q", rr.Body.String())
	}
}

func TestHandleTasks_ReturnsRegisteredState(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	b.registerTask("t-running", "git@github.com:o/r", "do thing 1", nil)
	b.registerTask("t-pending", "git@github.com:o/r2", "do thing 2", nil)
	b.setStage("t-pending", StagePending)

	req := httptest.NewRequest("GET", "/admin/tasks", nil)
	rr := httptest.NewRecorder()
	b.HandleTasks(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []TaskState
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tasks, got %d: %+v", len(got), got)
	}
	stages := map[string]TaskStage{}
	for _, ts := range got {
		stages[ts.ID] = ts.Stage
	}
	if stages["t-running"] != StageRunning {
		t.Errorf("t-running stage = %q, want %q", stages["t-running"], StageRunning)
	}
	if stages["t-pending"] != StagePending {
		t.Errorf("t-pending stage = %q, want %q", stages["t-pending"], StagePending)
	}
}

func TestHandleKill_404Unknown(t *testing.T) {
	b := &Broker{}
	req := httptest.NewRequest("POST", "/admin/kill/does-not-exist", nil)
	req.SetPathValue("id", "does-not-exist")
	rr := httptest.NewRecorder()
	b.HandleKill(rr, req)
	if rr.Code != 404 {
		t.Fatalf("got %d, want 404", rr.Code)
	}
}

func TestHandleKill_FiresStoredCancel(t *testing.T) {
	b := &Broker{}
	ctx, cancel := context.WithCancel(context.Background())
	b.registerTask("t-kill", "git@github.com:o/r", "go", cancel)

	req := httptest.NewRequest("POST", "/admin/kill/t-kill", nil)
	req.SetPathValue("id", "t-kill")
	rr := httptest.NewRecorder()
	b.HandleKill(rr, req)
	if rr.Code != 204 {
		t.Fatalf("got %d, want 204", rr.Code)
	}
	select {
	case <-ctx.Done():
		// success
	case <-time.After(time.Second):
		t.Fatal("cancel was never fired on the stored context")
	}
}

// TestHandleKill_AlsoUnblocksApprovalGate is the integration of the two
// concerns: a task waiting at the approval gate should be unblocked when
// /admin/kill cancels its context, returning false from gatePush. This
// is what makes "drydock kill" useful when a task is sitting at approval.
func TestHandleKill_AlsoUnblocksApprovalGate(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	b.registerTask("t-gate", "git@github.com:o/r", "go", cancel)

	done := make(chan bool, 1)
	go func() { done <- b.gatePush(ctx, "t-gate", "diff", false) }()

	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["t-gate"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("gate never registered as pending")
	}

	req := httptest.NewRequest("POST", "/admin/kill/t-gate", nil)
	req.SetPathValue("id", "t-gate")
	rr := httptest.NewRecorder()
	b.HandleKill(rr, req)
	if rr.Code != 204 {
		t.Fatalf("HandleKill returned %d", rr.Code)
	}

	select {
	case got := <-done:
		if got {
			t.Fatal("gatePush returned true after kill")
		}
	case <-time.After(time.Second):
		t.Fatal("gatePush did not return after kill")
	}
}

func TestGateEgressWiden_Approve(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	extras := []egress.Domain{{Host: "internal.example.com", Ports: []int{443}}}
	done := make(chan bool, 1)
	go func() { done <- b.gateEgressWiden(context.Background(), "te-1", extras) }()

	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["te-1"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("egress gate never registered")
	}
	req := httptest.NewRequest("POST", "/admin/approve/te-1", nil)
	req.SetPathValue("id", "te-1")
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, req)
	if rr.Code != 204 {
		t.Fatalf("approve returned %d", rr.Code)
	}
	select {
	case got := <-done:
		if !got {
			t.Fatal("gate returned false after approve")
		}
	case <-time.After(time.Second):
		t.Fatal("gate did not return after approve")
	}

	// The widen.json persistence happens for the human reviewer; verify
	// it exists with the right content.
	body, err := os.ReadFile(b.AuditRoot + "/te-1.widen.json")
	if err != nil {
		t.Fatalf("widen.json missing: %v", err)
	}
	if !strings.Contains(string(body), "internal.example.com") {
		t.Errorf("widen.json missing host: %q", body)
	}
}

func TestGateEgressWiden_Deny(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	extras := []egress.Domain{{Host: "evil.example.com", Ports: []int{443}}}
	done := make(chan bool, 1)
	go func() { done <- b.gateEgressWiden(context.Background(), "te-2", extras) }()

	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["te-2"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("egress gate never registered")
	}
	req := httptest.NewRequest("POST", "/admin/deny/te-2", nil)
	req.SetPathValue("id", "te-2")
	rr := httptest.NewRecorder()
	b.HandleDeny(rr, req)
	if rr.Code != 204 {
		t.Fatalf("deny returned %d", rr.Code)
	}
	select {
	case got := <-done:
		if got {
			t.Fatal("gate returned true after deny — the security claim is broken")
		}
	case <-time.After(time.Second):
		t.Fatal("gate did not return after deny")
	}
}

func TestGateEgressWiden_CancelAborts(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- b.gateEgressWiden(ctx, "te-3",
			[]egress.Domain{{Host: "x.example.com", Ports: []int{443}}})
	}()

	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["te-3"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("egress gate never registered")
	}
	cancel()
	select {
	case got := <-done:
		if got {
			t.Fatal("gate returned true after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("gate did not abort after cancel")
	}
}

func TestSummariseExtras(t *testing.T) {
	cases := []struct {
		in   []egress.Domain
		want string
	}{
		{nil, "no hosts"},
		{[]egress.Domain{{Host: "a.example.com", Ports: []int{443}}}, "a.example.com:443"},
		{[]egress.Domain{
			{Host: "a.example.com", Ports: []int{443, 8443}},
			{Host: "b.example.com", Ports: []int{80}},
		}, "a.example.com:443,8443 b.example.com:80"},
	}
	for _, tc := range cases {
		if got := summariseExtras(tc.in); got != tc.want {
			t.Errorf("summariseExtras(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstLine_Sanitization(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"hello\nworld", "hello"},
		// Strip ANSI escape (ESC [31m).
		{"\x1b[31mred\x1b[0m", "[31mred[0m"},
		// Strip ASCII control characters.
		{"a\x07b\x08c", "abc"},
		// Drop a leading hyphen so it can't be confused for a flag.
		{"--unsafe flag", "unsafe flag"},
		{"-x", "x"},
		// Cap at 72 chars.
		{strings.Repeat("a", 100), strings.Repeat("a", 72)},
		// All-control + empty becomes placeholder.
		{"\x01\x02\x03", "agent task"},
		{"", "agent task"},
	}
	for _, tc := range cases {
		got := firstLine(tc.in)
		if got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSafeErr_StripsControls(t *testing.T) {
	if got := safeErr(nil); got != "" {
		t.Errorf("nil err = %q", got)
	}
	got := safeErr(errors.New("bad\x1b[31mthing\x00"))
	if got != "bad[31mthing" {
		t.Errorf("safeErr = %q", got)
	}
	long := strings.Repeat("x", 500)
	gotLong := safeErr(errors.New(long))
	if !strings.HasSuffix(gotLong, "…") || len(gotLong) > 250 {
		t.Errorf("safeErr did not cap long input: len=%d", len(gotLong))
	}
}

func TestHandleTask_400OnBadJSON(t *testing.T) {
	b := &Broker{}
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader("{not json"))
	rr := httptest.NewRecorder()
	b.HandleTask(rr, req)
	if rr.Code != 400 {
		t.Errorf("bad JSON should 400, got %d", rr.Code)
	}
}

func TestHandleTask_400OnLocalPathRepoRef(t *testing.T) {
	b := &Broker{}
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(
		`{"repo_ref":"/Users/sray/repo","instruction":"x"}`))
	rr := httptest.NewRecorder()
	b.HandleTask(rr, req)
	if rr.Code != 400 {
		t.Errorf("local-path repo_ref should 400, got %d body=%q", rr.Code, rr.Body.String())
	}
}

func TestHandleApprove_AlreadySignaledIs409(t *testing.T) {
	// Two approvals to the same task must not race-double-send into the
	// channel; the second must observe the conflict, not panic.
	b := &Broker{AuditRoot: t.TempDir()}
	done := make(chan bool, 1)
	go func() { done <- b.gatePush(context.Background(), "twice", "diff", false) }()
	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["twice"]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("never registered")
	}

	// First approve succeeds (204) and the gate returns.
	req := httptest.NewRequest("POST", "/admin/approve/twice", nil)
	req.SetPathValue("id", "twice")
	rr := httptest.NewRecorder()
	b.HandleApprove(rr, req)
	if rr.Code != 204 {
		t.Fatalf("first approve = %d", rr.Code)
	}
	<-done // drain so the gate goroutine finishes

	// Wait for gate to actually deregister; otherwise we race the cleanup.
	if !waitFor(50*time.Millisecond, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending["twice"]
		b.pendingMu.Unlock()
		return !ok
	}) {
		t.Fatal("gate did not deregister")
	}

	// A second approval against the now-gone task must return 404, not panic.
	rr2 := httptest.NewRecorder()
	b.HandleApprove(rr2, req)
	if rr2.Code != 404 {
		t.Errorf("second approve = %d, want 404", rr2.Code)
	}
}

func TestHandleTasks_OldestFirst(t *testing.T) {
	b := &Broker{}
	b.registerTask("a", "r", "i", nil)
	time.Sleep(2 * time.Millisecond)
	b.registerTask("b", "r", "i", nil)
	time.Sleep(2 * time.Millisecond)
	b.registerTask("c", "r", "i", nil)

	req := httptest.NewRequest("GET", "/admin/tasks", nil)
	rr := httptest.NewRecorder()
	b.HandleTasks(rr, req)

	var got []TaskState
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Errorf("not oldest-first: %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestHandlePending_EmptyReturnsEmptyArray(t *testing.T) {
	// Empty must be `[]`, not `null` — clients shouldn't have to handle both.
	b := &Broker{}
	req := httptest.NewRequest("GET", "/admin/pending", nil)
	rr := httptest.NewRecorder()
	b.HandlePending(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	body := strings.TrimSpace(rr.Body.String())
	if body != "[]" {
		t.Errorf("empty pending body = %q, want %q", body, "[]")
	}
}

func TestHandleHealth_BreakdownByStage(t *testing.T) {
	b := &Broker{}
	b.registerTask("a", "r", "i", nil)
	b.registerTask("b", "r", "i", nil)
	b.setStage("b", StagePending)
	b.registerTask("c", "r", "i", nil)
	b.setStage("c", StagePushing)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	b.HandleHealth(rr, req)

	var body struct {
		Running         int `json:"running"`
		PendingApproval int `json:"pending_approval"`
		Pushing         int `json:"pushing"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Running != 1 || body.PendingApproval != 1 || body.Pushing != 1 {
		t.Errorf("breakdown = %+v, want running=1 pending=1 pushing=1", body)
	}
}

func TestTaskModelFor(t *testing.T) {
	cases := []struct {
		taskModel, ocModel, vendor, want string
	}{
		{"gpt-x", "gemini-2.5-pro", "openai-compat", "gpt-x"},     // explicit --model wins
		{"", "gemini-2.5-pro", "openai-compat", "gemini-2.5-pro"}, // opencode falls back to configured model
		{"", "gemini-2.5-pro", "anthropic", ""},                   // other vendors don't use the oc model
		{"", "", "openai-compat", ""},                             // no model configured -> empty
		{"claude-x", "", "anthropic", "claude-x"},                 // claude explicit
	}
	for _, c := range cases {
		if got := taskModelFor(c.taskModel, c.ocModel, c.vendor); got != c.want {
			t.Errorf("taskModelFor(%q,%q,%q) = %q, want %q", c.taskModel, c.ocModel, c.vendor, got, c.want)
		}
	}
}

func TestModelEnv(t *testing.T) {
	cases := []struct {
		name string
		task string
		def  string
		want []string
	}{
		{"per-task wins over default", "claude-opus-4-8", "claude-sonnet-4-6", []string{"DRYDOCK_MODEL=claude-opus-4-8"}},
		{"falls back to default", "", "claude-sonnet-4-6", []string{"DRYDOCK_MODEL=claude-sonnet-4-6"}},
		{"both empty = no env (claude picks)", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := modelEnv(tc.task, tc.def)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDefaultModelNotLeakedToOpenAICompat verifies that the operator
// DefaultModel (a claude-oriented value) is never forwarded into the
// openai-compat lane. The blanking happens at broker.go:506-509; the
// observable effect is that modelEnv gets an empty defaultModel and therefore
// emits no DRYDOCK_MODEL entry for the operator default — only an explicit
// --model or the openai_compat.model config can resolve the model in that lane.
func TestDefaultModelNotLeakedToOpenAICompat(t *testing.T) {
	const operatorDefault = "claude-sonnet-4-6" // a claude-oriented default

	cases := []struct {
		name         string
		taskModel    string
		ocModel      string
		vendor       string
		wantContains string // non-empty means the env entry must appear
		wantAbsent   string // non-empty means this string must NOT appear in env
	}{
		{
			// openai-compat: no per-task model, no oc model → operator default
			// must be blanked; env is empty.
			name:       "compat/no explicit model: operator default absent",
			taskModel:  "",
			ocModel:    "",
			vendor:     "openai-compat",
			wantAbsent: operatorDefault,
		},
		{
			// openai-compat: openai_compat.model configured → that value wins;
			// operator default must still not appear.
			name:         "compat/oc model configured: oc model present, operator default absent",
			taskModel:    "",
			ocModel:      "gemini-2.5-pro",
			vendor:       "openai-compat",
			wantContains: "DRYDOCK_MODEL=gemini-2.5-pro",
			wantAbsent:   operatorDefault,
		},
		{
			// openai-compat: explicit --model overrides everything; operator
			// default still must not appear.
			name:         "compat/explicit --model: task model present, operator default absent",
			taskModel:    "gpt-4o",
			ocModel:      "gemini-2.5-pro",
			vendor:       "openai-compat",
			wantContains: "DRYDOCK_MODEL=gpt-4o",
			wantAbsent:   operatorDefault,
		},
		{
			// non-compat: no per-task model → operator default IS propagated.
			name:         "non-compat/no task model: operator default forwarded",
			taskModel:    "",
			ocModel:      "",
			vendor:       "anthropic",
			wantContains: "DRYDOCK_MODEL=" + operatorDefault,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror broker.go:506-510: blank defaultModel for the compat lane.
			defaultModel := operatorDefault
			if tc.vendor == "openai-compat" {
				defaultModel = ""
			}
			env := modelEnv(taskModelFor(tc.taskModel, tc.ocModel, tc.vendor), defaultModel)
			joined := strings.Join(env, "\n")

			if tc.wantContains != "" {
				found := false
				for _, e := range env {
					if e == tc.wantContains {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("env %v does not contain %q", env, tc.wantContains)
				}
			}
			if tc.wantAbsent != "" && strings.Contains(joined, tc.wantAbsent) {
				t.Errorf("env %v must not contain %q (operator default leaked into %s lane)", env, tc.wantAbsent, tc.vendor)
			}
		})
	}
}

// TestGatePush_AutoDeniesOnApprovalTimeout verifies that a task waiting at the
// approval gate is auto-denied (and its slot freed) once ApprovalTimeout passes,
// instead of blocking forever.
func TestGatePush_AutoDeniesOnApprovalTimeout(t *testing.T) {
	b := &Broker{AuditRoot: t.TempDir(), ApprovalTimeout: 40 * time.Millisecond}
	done := make(chan bool, 1)
	go func() { done <- b.gatePush(context.Background(), "task-to", "a diff", false) }()
	select {
	case approved := <-done:
		if approved {
			t.Error("expected auto-deny (false) on approval_timeout, got approved")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gatePush did not return; approval_timeout not enforced")
	}
}

func TestPRContent(t *testing.T) {
	title, body := prContent("Add a retry to the uploader\nmore detail here", "abc")
	if title != "Add a retry to the uploader" {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(body, "more detail here") || !strings.Contains(body, "task abc") {
		t.Errorf("body missing instruction or provenance: %q", body)
	}
	if tl, _ := prContent("", "abc"); tl != "" {
		t.Errorf("empty instruction must yield empty title, got %q", tl)
	}
	long := strings.Repeat("x", 100)
	if tl, _ := prContent(long, "abc"); len([]rune(tl)) > 72 {
		t.Errorf("title not clipped: %d runes", len([]rune(tl)))
	}
	if _, b := prContent(strings.Repeat("y", 5000), "abc"); !strings.Contains(b, "truncated") {
		t.Error("oversized body must be truncated")
	}
	// Multibyte title: 100 Chinese characters (3 bytes each). The clip must
	// not split a rune — the result must be valid UTF-8 and ≤72 runes.
	mbTitle, _ := prContent(strings.Repeat("你", 100), "abc")
	if !utf8.ValidString(mbTitle) {
		t.Errorf("multibyte title is not valid UTF-8: %q", mbTitle)
	}
	if len([]rune(mbTitle)) > 72 {
		t.Errorf("multibyte title exceeds 72 runes: %d", len([]rune(mbTitle)))
	}
}
