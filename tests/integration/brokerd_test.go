//go:build integration

// Integration tests for drydock. Require:
//   - macOS Apple silicon
//   - Apple container CLI installed and `container system start` already run
//   - bin/brokerd and bin/drydock pre-built (run `make build`)
//   - drydock-egress network created (`make network`)
//   - drydock-anchor image built (`make image-anchor`)
//
// These tests boot brokerd as a subprocess with a placeholder API key and
// exercise the HTTP+CLI surface that unit tests can't reach (the boot
// sequence, the unix socket, the JSON shapes of /admin/* end-to-end).
// No real Anthropic spend.
//
// Run with: make test-integration  (or `go test -tags=integration ./tests/...`)
package integration

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	brokerdBin = "../../bin/brokerd"
	drydockBin = "../../bin/drydock"
)

// TestMain checks prerequisites once so individual tests fail fast, and
// reaps any orphan drydock state (anchor container, squid process) left
// behind by a crashed previous run.
func TestMain(m *testing.M) {
	for _, p := range []string{brokerdBin, drydockBin} {
		if _, err := os.Stat(p); err != nil {
			println("integration tests require pre-built binaries; run `make build` first")
			println("  missing:", p)
			os.Exit(1)
		}
	}
	cleanupOrphans()
	code := m.Run()
	cleanupOrphans()
	os.Exit(code)
}

// cleanupOrphans removes any leftover drydock-anchor container and kills
// any orphan squid that's still binding the vmnet gateway port. Tests
// otherwise stall in waitBindable because squid persists past brokerd's
// signal cleanup when brokerd is killed non-gracefully.
func cleanupOrphans() {
	_ = exec.Command("container", "rm", "-f", "drydock-anchor").Run()
	_ = exec.Command("pkill", "-f", "squid -N -f").Run()
	time.Sleep(500 * time.Millisecond)
}

type brokerdHandle struct {
	t       *testing.T
	cmd     *exec.Cmd
	sock    string
	stopped bool
}

// startBrokerd boots brokerd with a placeholder key and notifications off,
// pointing AUDIT_ROOT/STAGE_ROOT/BROKER_SOCKET at a test-scoped tempdir
// so each integration test is isolated.
//
// We deliberately avoid t.TempDir() because macOS unix-socket paths are
// limited to 104 chars and t.TempDir() under /var/folders/... blows past
// that. Short /tmp paths it is.
func startBrokerd(t *testing.T) *brokerdHandle {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "drydk-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	// Point EGRESS_CONFIG at the repo-root file. The test process runs in
	// tests/integration/, but brokerd needs the egress YAML wherever we
	// build it from.
	egressPath, err := filepath.Abs("../../config/egress.yaml")
	if err != nil {
		t.Fatalf("locate egress.yaml: %v", err)
	}

	cmd := exec.Command(brokerdBin)
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_API_KEY=sk-ant-fake",
		"DRYDOCK_NO_NOTIFY=1",
		"BROKER_SOCKET="+sock,
		"EGRESS_CONFIG="+egressPath,
		"AUDIT_ROOT="+filepath.Join(dir, "audit"),
		"STAGE_ROOT="+filepath.Join(dir, "stage"),
		"SQUID_RUN_DIR="+filepath.Join(dir, "squid"),
	)
	logFile, err := os.Create(filepath.Join(dir, "brokerd.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start brokerd: %v", err)
	}
	h := &brokerdHandle{t: t, cmd: cmd, sock: sock}
	t.Cleanup(h.stop)

	if !h.waitReady(20 * time.Second) {
		logBytes, _ := os.ReadFile(filepath.Join(dir, "brokerd.log"))
		t.Fatalf("brokerd never became ready. log:\n%s", logBytes)
	}
	return h
}

func (h *brokerdHandle) stop() {
	if h.stopped || h.cmd == nil || h.cmd.Process == nil {
		return
	}
	h.stopped = true
	_ = h.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = h.cmd.Process.Kill()
	}
	// Even on a clean SIGINT shutdown, squid is a child process that
	// brokerd's signal handler races against — make sure it's gone
	// before the next test tries to bind the same port.
	cleanupOrphans()
}

func (h *brokerdHandle) waitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.health() == nil {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func (h *brokerdHandle) client() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", h.sock)
			},
		},
	}
}

func (h *brokerdHandle) get(path string) (*http.Response, error) {
	return h.client().Get("http://drydock" + path)
}

func (h *brokerdHandle) health() error {
	resp, err := h.get("/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &httpStatusError{code: resp.StatusCode}
	}
	return nil
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return http.StatusText(e.code) }

func TestBrokerd_BootHealthAndEmptyAdmin(t *testing.T) {
	h := startBrokerd(t)

	// /healthz returns the structured breakdown.
	resp, err := h.get("/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	var hb map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&hb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"ok", "running", "awaiting_egress", "pending_approval", "pushing"} {
		if _, ok := hb[k]; !ok {
			t.Errorf("healthz missing field %q: %+v", k, hb)
		}
	}

	// /admin/tasks and /admin/pending must be empty arrays, not null.
	for _, p := range []string{"/admin/tasks", "/admin/pending"} {
		resp, err := h.get(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		got := strings.TrimSpace(string(body[:n]))
		if got != "[]" {
			t.Errorf("%s = %q, want %q", p, got, "[]")
		}
	}
}

func TestBrokerd_TaskValidation(t *testing.T) {
	h := startBrokerd(t)

	// Local path repo_ref → 400.
	resp, err := h.client().Post("http://drydock/tasks",
		"application/json",
		strings.NewReader(`{"repo_ref":"/Users/x/repo","instruction":"x"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("local path repo_ref = %d, want 400", resp.StatusCode)
	}

	// Malformed JSON → 400.
	resp, err = h.client().Post("http://drydock/tasks",
		"application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("post bad json: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bad JSON = %d, want 400", resp.StatusCode)
	}
}

func TestDrydock_CLIAgainstLiveBroker(t *testing.T) {
	h := startBrokerd(t)

	for _, args := range [][]string{
		{"status"},
		{"pending"},
		{"tasks"},
		{"version"},
	} {
		cmd := exec.Command(drydockBin, args...)
		cmd.Env = append(os.Environ(), "BROKER_SOCKET="+h.sock)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("drydock %s: %v\n%s", strings.Join(args, " "), err, out)
			continue
		}
		if len(out) == 0 {
			t.Errorf("drydock %s produced no output", strings.Join(args, " "))
		}
	}
}
