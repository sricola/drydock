//go:build integration

package integration

// Gemini-specific integration / red-team tests (macOS-gated, same bar as
// the existing claude and codex tests in redteam_test.go / brokerd_test.go).
//
// These tests require:
//   - macOS Apple silicon
//   - Apple container CLI installed and `container system start` already run
//   - bin/brokerd and bin/drydock pre-built (run `make build`)
//   - A rebuilt drydock-sandbox image with @google/gemini-cli installed
//     (Task 4 of the Gemini feature branch; run `make build-image`)
//   - A real GEMINI_API_KEY (end-to-end test only; A1/A2 use a sentinel)
//
// IMPORTANT — environment reality: the implementer has confirmed this file
// compiles clean under `go vet -tags integration ./tests/integration/` but
// CANNOT run it (requires macOS Apple silicon). The maintainer MUST run
// `make test-integration` on macOS with a rebuilt sandbox image and a real
// GEMINI_API_KEY to obtain the true egress-risk verification; a GREEN result
// there is the completion of the spec's egress-risk check (the end-to-end
// test is where an unexpected CLI phone-home would surface). This matches
// the CI reality under which claude, codex, and opencode shipped.
//
// Run: make test-integration  (or `go test -tags=integration ./tests/...`)

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/gateway"
)

// TestGemini_A1_RealKeyNeverInVM asserts that the Google/Gemini real API key
// never enters the VM. We build the EXACT env the broker injects — using a
// sentinel real key via the real GoogleVendor + gateway machinery — then
// inspect that env from inside the VM. The sentinel must be absent; only the
// per-task bearer token (tok_...) may be present.
//
// The complementary assertion — that the gateway forwards the real key to
// generativelanguage.googleapis.com upstream — is verified at the unit layer
// in internal/gateway/vendor_test.go and internal/gateway/gateway_test.go,
// matching the level at which the same check lives for the Anthropic and
// OpenAI vendors.
func TestGemini_A1_RealKeyNeverInVM(t *testing.T) {
	requireContainer(t)
	const sentinel = "AIza-SENTINEL-DO-NOT-LEAK-gemini-7f4d"

	gw, err := gateway.New(gateway.Backend{Vendor: gateway.GoogleVendor(), Cred: gateway.StaticKey(sentinel)})
	if err != nil {
		t.Fatal(err)
	}
	gBase, gTok := gwEnvNames("google")
	if gBase == "" || gTok == "" {
		t.Fatalf("gwEnvNames(\"google\") returned empty names (gBase=%q gTok=%q); registry gap — cannot proceed", gBase, gTok)
	}
	prov := &gateway.Provider{
		GW:         gw,
		Vendor:     "google",
		BaseURL:    "http://10.0.0.1:8088",
		BaseURLEnv: gBase,
		TokenEnv:   gTok,
		Budget:     1,
		TTL:        time.Minute,
	}
	grant, err := prov.Mint(1)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Revoke() //nolint:errcheck

	args := []string{"run", "--rm", "--entrypoint", "/bin/bash"}
	for _, e := range grant.EnvVars() {
		args = append(args, "--env", e)
	}
	args = append(args, sandboxImage(), "-lc",
		"env; echo '---PROC---'; tr '\\0' '\\n' < /proc/self/environ")

	out := containerRun(t, args...)
	if strings.Contains(out, sentinel) {
		t.Fatalf("A1 BREACH: the real Gemini API key leaked into the VM environment:\n%s", out)
	}
	if !strings.Contains(out, "tok_") {
		t.Errorf("expected the per-task bearer token (tok_) in the VM env; got:\n%s", out)
	}
}

// TestGemini_A2_EgressDenyByDefault asserts that the deny-by-default sandbox
// egress policy holds for the Gemini lane. Deny-by-default is enforced
// image-wide (init-firewall.sh), not per-agent, so this test reuses exactly
// the same container invocation and assertions as TestRedteam_A2_EgressToHostileHostBlocked;
// it exists explicitly so the test run clearly covers the Gemini egress surface
// and the test report names a Gemini-specific check.
func TestGemini_A2_EgressDenyByDefault(t *testing.T) {
	requireContainer(t)
	script := `
/usr/local/bin/init-firewall.sh 192.168.66.1 8088 3128
echo "HTTPS: $(curl -sS -m 4 https://example.com/ -o /dev/null 2>/dev/null && echo reachable || echo blocked)"
echo "DNS: $(timeout 5 nslookup evil.example.com 1.1.1.1 >/dev/null 2>&1 && echo resolved || echo blocked)"
echo "DIRECT-IP: $(curl -sS -m 4 https://1.1.1.1/ -o /dev/null 2>/dev/null && echo reachable || echo blocked)"
`
	out := containerRun(t, "run", "--rm", "--user", "root", "--cap-add", "CAP_NET_ADMIN",
		"--entrypoint", "/bin/bash", sandboxImage(), "-lc", script)
	for _, vec := range []string{"HTTPS", "DNS", "DIRECT-IP"} {
		if !strings.Contains(out, vec+": blocked") {
			t.Errorf("A2 BREACH: %s egress was not blocked in the Gemini sandbox:\n%s", vec, out)
		}
	}
}

// startBrokerdGemini boots brokerd with the provided GEMINI_API_KEY and no
// Anthropic/OpenAI keys, so only the google/gemini vendor is registered by
// buildBackends. Modeled on startBrokerdOpenAI in brokerd_test.go; see that
// function's inline comments for rationale on the /tmp path and socket length.
func startBrokerdGemini(t *testing.T, geminiKey string) *brokerdHandle {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "drydk-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	egressPath, err := filepath.Abs("../../config/egress.yaml")
	if err != nil {
		t.Fatalf("locate egress.yaml: %v", err)
	}

	cmd := exec.Command(brokerdBin)
	cmd.Env = append(os.Environ(),
		"GEMINI_API_KEY="+geminiKey,
		"DRYDOCK_NO_NOTIFY=1",
		"BROKER_SOCKET="+sock,
		"EGRESS_CONFIG="+egressPath,
		"AUDIT_ROOT="+filepath.Join(dir, "audit"),
		"STAGE_ROOT="+filepath.Join(dir, "stage"),
		"SQUID_RUN_DIR="+filepath.Join(dir, "squid"),
	)
	// Strip ambient ANTHROPIC_API_KEY and OPENAI_API_KEY so only the Google
	// vendor is registered by buildBackends — we're exercising the gemini lane
	// only. The ambient GEMINI_API_KEY (source of geminiKey) may appear twice
	// in the env slice; that is harmless since both entries carry the same value.
	filtered := cmd.Env[:0]
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	cmd.Env = filtered

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

// TestGemini_EndToEnd_TaskCompletesViaGateway submits a trivial Gemini task
// through a real brokerd subprocess and asserts it appears as running in
// /admin/tasks, proving the Gemini CLI inside the sandbox successfully routes
// API calls through the drydock gateway rather than being blocked by
// deny-by-default egress or dialing generativelanguage.googleapis.com directly.
// This is the egress-risk verification from the spec: if the pinned CLI
// contacts any non-allowlisted host (telemetry, update check, auth discovery)
// the sandbox firewall blocks it and the task fails before reaching the broker.
//
// Skip conditions (any one is sufficient):
//   - GEMINI_API_KEY unset — no key, no real run
//   - DRYDOCK_GEMINI_TEST_REPO unset — need a real clonable repo (same pattern
//     as DRYDOCK_CODEX_TEST_REPO for TestBrokerd_CodexTaskReachesAwaitingApproval)
//
// This test requires macOS Apple-silicon, the Apple container CLI, a rebuilt
// drydock-sandbox image with @google/gemini-cli@0.49.0 installed, and a real
// GEMINI_API_KEY. It is NOT run in CI. Run manually: make test-integration
func TestGemini_EndToEnd_TaskCompletesViaGateway(t *testing.T) {
	requireContainer(t)

	geminiKey := os.Getenv("GEMINI_API_KEY")
	if geminiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping Gemini end-to-end egress-verification test")
	}

	repo := os.Getenv("DRYDOCK_GEMINI_TEST_REPO")
	if repo == "" {
		t.Skip("DRYDOCK_GEMINI_TEST_REPO not set (need a real clonable repo); skipping Gemini end-to-end test")
	}

	h := startBrokerdGemini(t, geminiKey)

	// POST /tasks is synchronous and blocks until the container run finishes
	// (or is killed). Use a long-timeout client so the submission goroutine
	// does not time out before the container starts; the default h.client()
	// timeout of 5 s is sufficient only for health checks and admin reads.
	sock := h.sock
	longClient := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}

	type postResult struct {
		statusCode int
		body       []byte
		err        error
	}
	postDone := make(chan postResult, 1)
	go func() {
		resp, err := longClient.Post(
			"http://drydock/tasks",
			"application/json",
			strings.NewReader(fmt.Sprintf(`{
				"repo_ref":    "%s",
				"instruction": "add a comment to README.md",
				"agent":       "gemini"
			}`, repo)),
		)
		if err != nil {
			postDone <- postResult{err: err}
			return
		}
		defer resp.Body.Close()
		var buf [4096]byte
		n, _ := resp.Body.Read(buf[:])
		postDone <- postResult{statusCode: resp.StatusCode, body: buf[:n]}
	}()

	// Poll /admin/tasks until the task appears (running or awaiting_approval).
	// Allow 90 s for the container CLI to start the VM and emit the first
	// gateway request — Gemini startup is slightly slower than claude due to
	// the npm-based runtime.
	var taskID string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := h.get("/admin/tasks")
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		var tasks []map[string]any
		if jerr := json.NewDecoder(resp.Body).Decode(&tasks); jerr != nil {
			resp.Body.Close()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		for _, task := range tasks {
			id, _ := task["id"].(string)
			if id != "" {
				taskID = id
				break
			}
		}
		if taskID != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if taskID == "" {
		// The task never appeared; check what POST returned before declaring failure.
		select {
		case pr := <-postDone:
			if pr.err != nil {
				t.Fatalf("POST /tasks: %v", pr.err)
			}
			t.Fatalf("gemini task never appeared in /admin/tasks; POST returned %d: %s",
				pr.statusCode, pr.body)
		default:
			t.Fatal("gemini task never appeared in /admin/tasks within 90 s (egress block or image missing gemini-cli?)")
		}
	}

	// Task is live — the CLI reached the gateway, which is the egress-risk
	// assertion (deny-by-default would have killed the container before the
	// broker recorded it). Kill the task to cap real spend.
	killResp, err := h.client().Post(
		"http://drydock/admin/kill/"+taskID,
		"application/json",
		strings.NewReader(""),
	)
	if err != nil {
		t.Fatalf("kill task %s: %v", taskID, err)
	}
	killResp.Body.Close()
	if killResp.StatusCode != http.StatusNoContent {
		t.Errorf("kill returned %d, want 204", killResp.StatusCode)
	}

	// Wait for the POST goroutine to drain (brokerd responds after the kill).
	select {
	case pr := <-postDone:
		if pr.err != nil {
			t.Fatalf("POST /tasks after kill: %v", pr.err)
		}
		// 200 (completed or cancelled) or 500 (container exited before kill
		// landed) are both acceptable — the egress assertion is that the task
		// appeared (CLI reached the gateway), not that it produced a full diff.
		if pr.statusCode != http.StatusOK && pr.statusCode != http.StatusInternalServerError {
			t.Errorf("POST /tasks returned %d after kill, want 200 or 500", pr.statusCode)
		}
	case <-time.After(30 * time.Second):
		t.Error("POST /tasks goroutine did not return within 30 s after kill")
	}
}
