package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func useBrokerServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("BROKER_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	// Keep a developer's real config from influencing fallback/error behavior.
	t.Setenv("HOME", t.TempDir())
	return srv
}

func TestFetchTasksHTTPContract(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/admin/tasks" {
				t.Errorf("request = %s %s, want GET /admin/tasks", r.Method, r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode([]taskState{{ID: "task-1", Stage: "running", StartedAt: started}})
		}))

		got, err := fetchTasks()
		if err != nil {
			t.Fatalf("fetchTasks: %v", err)
		}
		if len(got) != 1 || got[0].ID != "task-1" || got[0].Stage != "running" || !got[0].StartedAt.Equal(started) {
			t.Fatalf("fetchTasks = %+v, want decoded task", got)
		}
	})

	t.Run("non-2xx-is-an-error-even-with-valid-json", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "[]")
		}))
		if _, err := fetchTasks(); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("fetchTasks error = %v, want HTTP 503", err)
		}
	})

	t.Run("malformed-json", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "[")
		}))
		if _, err := fetchTasks(); err == nil || !strings.Contains(err.Error(), "parse /admin/tasks") {
			t.Fatalf("fetchTasks error = %v, want parse context", err)
		}
	})
}

func TestHealthHTTPContract(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
				t.Errorf("request = %s %s, want GET /healthz", r.Method, r.URL.Path)
			}
			_, _ = io.WriteString(w, `{"ok":true,"running":2,"awaiting_egress":1,"verifying":4,"pending_approval":3,"pushing":1}`)
		}))
		got, err := health()
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		if !got.OK || got.Running != 2 || got.AwaitingEgress != 1 || got.Verifying != 4 || got.PendingApproval != 3 || got.Pushing != 1 {
			t.Fatalf("health = %+v, want decoded stage counts", got)
		}
	})

	t.Run("non-2xx-is-an-error-even-with-valid-json", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"ok":false}`)
		}))
		if _, err := health(); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("health error = %v, want HTTP 503", err)
		}
	})

	t.Run("malformed-json", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "{")
		}))
		if _, err := health(); err == nil || !strings.Contains(err.Error(), "parse health") {
			t.Fatalf("health error = %v, want parse context", err)
		}
	})
}

func TestListPendingRendersBothGateTypes(t *testing.T) {
	started := time.Now().Add(-time.Minute).UTC()
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]taskState{
			{ID: "diff-task", Repo: "git@github.com:owner/repo", Instruction: "first line\nsecond line", Stage: "awaiting_approval", StartedAt: started},
			{ID: "egress-task", Repo: "https://gitlab.com/group/repo", Stage: "awaiting_egress", StartedAt: started, EgressExtra: []domain{{Host: "api.example.com", Ports: []int{443, 8443}}}},
			{ID: "running-task", Stage: "running", StartedAt: started},
		})
	}))

	out := captureStdout(t, listPending)
	for _, want := range []string{"diff-task", "diff", "first line …", "egress-task", "egress", "api.example.com:443,8443"} {
		if !strings.Contains(out, want) {
			t.Errorf("pending output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "running-task") {
		t.Errorf("pending output included a running task:\n%s", out)
	}
}

// TestListPendingShowsSecondLookMarker: a pending diff-gate task carrying
// second_look categories gets a SECOND-LOOK annotation naming them; a task
// without any renders exactly as before.
func TestListPendingShowsSecondLookMarker(t *testing.T) {
	started := time.Now().Add(-time.Minute).UTC()
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]taskState{
			{ID: "acked-task", Repo: "git@github.com:owner/repo", Instruction: "touch ci", Stage: "awaiting_approval", StartedAt: started, SecondLook: []string{"ci-workflow", "lockfile"}},
			{ID: "plain-task", Repo: "git@github.com:owner/repo", Instruction: "plain change", Stage: "awaiting_approval", StartedAt: started},
		})
	}))

	out := captureStdout(t, listPending)
	if !strings.Contains(out, "SECOND-LOOK[ci-workflow,lockfile]") {
		t.Errorf("pending output missing SECOND-LOOK marker:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "plain-task") && strings.Contains(line, "SECOND-LOOK") {
			t.Errorf("plain task must not carry the marker:\n%s", out)
		}
	}
}

// TestRunTasksRendersPolicyBlocked: a task the broker failed closed under
// diff_policy (result subtype policy_blocked) renders as "policy blocked".
func TestRunTasksRendersPolicyBlocked(t *testing.T) {
	auditRoot := t.TempDir()
	t.Setenv("AUDIT_ROOT", auditRoot)
	rows := `{"type":"result","subtype":"policy_blocked","is_error":false,"duration_ms":9000,"total_cost_usd":0.05,"num_turns":0,"src":"broker"}` + "\n" +
		`{"type":"metrics","src":"broker","task_id":"blocked","outcome":"policy_blocked","cost_usd":0.05}` + "\n"
	if err := os.WriteFile(filepath.Join(auditRoot, "blocked.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, runTasks)
	if !strings.Contains(out, "policy blocked") {
		t.Errorf("tasks output missing %q:\n%s", "policy blocked", out)
	}
}

func TestRunStatusCombinesBrokerAndAuditState(t *testing.T) {
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"running":2,"awaiting_egress":1,"verifying":1,"pending_approval":3,"pushing":1}`)
	}))
	auditRoot := t.TempDir()
	t.Setenv("AUDIT_ROOT", auditRoot)
	recent := filepath.Join(auditRoot, "recent.jsonl")
	old := filepath.Join(auditRoot, "old.jsonl")
	ignored := filepath.Join(auditRoot, "recent.diff")
	for _, path := range []string{recent, old, ignored} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, runStatus)
	for _, want := range []string{
		"brokerd     up",
		"2 running · 1 verifying · 1 awaiting egress · 3 awaiting diff · 1 pushing",
		"2 total · 1 in last 24h",
		auditRoot,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestRunTasksListsNewestFirstAndIgnoresOtherArtifacts(t *testing.T) {
	auditRoot := t.TempDir()
	t.Setenv("AUDIT_ROOT", auditRoot)
	newID, oldID := "new-task", "old-task"
	result := `{"type":"result","subtype":"success","duration_ms":1200,"total_cost_usd":0.125,"num_turns":2}` + "\n"
	for _, id := range []string{newID, oldID} {
		if err := os.WriteFile(filepath.Join(auditRoot, id+".jsonl"), []byte(result), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	oldPath := filepath.Join(auditRoot, oldID+".jsonl")
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auditRoot, "ignored.diff"), []byte("diff"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, runTasks)
	newPos, oldPos := strings.Index(out, newID), strings.Index(out, oldID)
	if newPos < 0 || oldPos < 0 || newPos >= oldPos {
		t.Fatalf("tasks not listed newest-first:\n%s", out)
	}
	for _, want := range []string{"1.2s", "$0.1250", "ok (2 turns)"} {
		if !strings.Contains(out, want) {
			t.Errorf("tasks output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ignored") {
		t.Errorf("tasks output included non-jsonl artifact:\n%s", out)
	}
}

func TestRunLogsWithoutFollowCopiesAudit(t *testing.T) {
	auditRoot := t.TempDir()
	t.Setenv("AUDIT_ROOT", auditRoot)
	want := "first line\nsecond line\n"
	if err := os.WriteFile(filepath.Join(auditRoot, "abc.jsonl"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := captureStdout(t, func() { runLogs("abc", false) }); got != want {
		t.Errorf("runLogs output = %q, want %q", got, want)
	}
}

func TestRunKillUsesBrokerBeforeContainerFallback(t *testing.T) {
	origRunCmd := runCmd
	t.Cleanup(func() { runCmd = origRunCmd })

	t.Run("broker-cancels-live-task", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/admin/kill/task-1" {
				t.Errorf("request = %s %s, want POST /admin/kill/task-1", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		containerCalled := false
		runCmd = func(string, ...string) ([]byte, error) {
			containerCalled = true
			return nil, errors.New("must not run")
		}

		out := captureStdout(t, func() { runKill("task-1") })
		if containerCalled {
			t.Error("container fallback ran after broker returned 204")
		}
		if !strings.Contains(out, "task task-1 cancelled") {
			t.Errorf("kill output = %q", out)
		}
	})

	t.Run("broker-404-cleans-up-orphan-vm", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		containerCalled := false
		runCmd = func(name string, args ...string) ([]byte, error) {
			containerCalled = true
			if name != "container" || strings.Join(args, " ") != "delete --force task-task-2" {
				t.Errorf("fallback command = %q %q", name, args)
			}
			return nil, nil
		}

		out := captureStdout(t, func() { runKill("task-2") })
		if !containerCalled {
			t.Error("container fallback did not run after broker returned 404")
		}
		if !strings.Contains(out, "VM removed") {
			t.Errorf("kill fallback output = %q", out)
		}
	})

	// Regression test: an unknown task id (brokerd doesn't track it AND the
	// container CLI reports no such container) used to fall off the end of
	// runKill with an implicit exit 0, the same shape of failure that
	// approve/deny (signal in client.go) already reports as exit 1.
	t.Run("unknown-id-exits-1", func(t *testing.T) {
		useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		runCmd = func(string, ...string) ([]byte, error) {
			return []byte("Error: no such container: task-does-not-exist"), errors.New("exit status 1")
		}

		var code int
		captureStdout(t, func() { code = runKill("does-not-exist") })
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
	})
}

func TestPostSubmitSendsContractAndConsumesStream(t *testing.T) {
	var got taskRequest
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/tasks" {
			t.Errorf("request = %s %s, want POST /tasks", r.Method, r.URL.Path)
		}
		if gotType := r.Header.Get("Content-Type"); gotType != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", gotType)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"event":"accepted","task_id":"abc123"}`+"\n")
		_, _ = io.WriteString(w, `{"event":"result","outcome":"pushed","branch":"agent/abc123","platform":"github"}`+"\n")
	}))
	want := taskRequest{
		RepoRef:     "git@github.com:owner/repo",
		Instruction: "fix the race",
		EgressExtra: []domain{{Host: "api.example.com", Ports: []int{443}}},
		Sensitive:   true,
		AutoApprove: true,
		Platform:    "github",
		Model:       "model-x",
		Agent:       "claude",
		Draft:       true,
	}
	origTTY := tty
	tty = false
	t.Cleanup(func() { tty = origTTY })

	var submitErr error
	out := captureStdout(t, func() { submitErr = postSubmit(want, false, false) })
	if submitErr != nil {
		t.Fatalf("postSubmit: %v", submitErr)
	}
	if got.RepoRef != want.RepoRef || got.Instruction != want.Instruction || got.Agent != want.Agent || got.Model != want.Model ||
		!got.Sensitive || !got.AutoApprove || !got.Draft || len(got.EgressExtra) != 1 {
		t.Errorf("submitted request = %+v, want %+v", got, want)
	}
	for _, text := range []string{"accepted", "abc123", "pushed", "agent/abc123"} {
		if !strings.Contains(out, text) {
			t.Errorf("submit output missing %q:\n%s", text, out)
		}
	}
}
