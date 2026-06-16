package broker

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// HandleTask is exercised by the host-integration end-to-end test (Task 10);
// its pure helpers now live in the gateway and creds packages.

// TestMain silences the operator-facing macOS notification that the approval
// gate would otherwise pop up on every test run on a developer's Mac.
func TestMain(m *testing.M) {
	os.Setenv("DRYDOCK_NO_NOTIFY", "1")
	os.Exit(m.Run())
}

func TestGithubRepoRef(t *testing.T) {
	cases := []struct {
		in    string
		valid bool
	}{
		// Accept the three github.com forms gh can resolve.
		{"https://github.com/sricola/drydock", true},
		{"https://github.com/sricola/drydock.git", true},
		{"git@github.com:sricola/drydock", true},
		{"git@github.com:sricola/drydock.git", true},
		{"ssh://git@github.com/sricola/drydock.git", true},
		// Reject local paths (the bug we just hit: gh pr create fails on these).
		{"/Users/sray/gits/drydock", false},
		{"./drydock", false},
		// Reject other hosts.
		{"https://gitlab.com/x/y", false},
		{"git@gitlab.com:x/y", false},
		// Reject malformed inputs.
		{"", false},
		{"https://github.com/", false},
		{"https://github.com/onlyowner", false},
		{"github.com/x/y", false},
	}
	for _, tc := range cases {
		got := githubRepoRef.MatchString(tc.in)
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
