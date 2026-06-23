//go:build squide2e

// Full VM-level end-to-end test for per-task egress widening. Unlike
// squid_live_test.go (host curl through a loopback squid), this drives the REAL
// path: a real sandbox container on the drydock-egress vmnet, the real
// init-firewall.sh nft default-deny pin, the real vmnet gateway IP, and the
// real squid (started from CompileSquidConf, registered via SquidController,
// authed by brokerd __squid-authhelper). The in-VM curl is the representative
// tool-egress client (the agent reaches the model via the gateway, not squid).
//
// It does NOT run an agent and spends no model credits.
//
// Requires: `container system start` up; drydock-sandbox:latest + drydock-anchor
// :latest images; the drydock-egress network (192.168.66.0/24); squid; the host
// able to bind 192.168.66.1 once the anchor is up; outbound net to api.github.com.
//
// Run with:
//
//	go test -tags squide2e ./internal/netfw/ -run TestEgressWidening_E2E -v
package netfw

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	e2eGatewayIP = "192.168.66.1"
	e2eProxyAddr = e2eGatewayIP + ":3128"
	e2eNetwork   = "drydock-egress"
	e2eSandbox   = "drydock-sandbox:latest"
	e2eAnchor    = "drydock-anchor:latest"
	e2eWidened   = "api.github.com" // widened-only: NOT in the default allowlist
)

func TestEgressWidening_E2E(t *testing.T) {
	squidBin, err := FindSquid()
	if err != nil {
		t.Skipf("squid not installed: %v", err)
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Skipf("container CLI not available: %v", err)
	}

	runDir := t.TempDir()

	// Real auth helper: the brokerd binary.
	helperBin := filepath.Join(runDir, "brokerd")
	if out, err := exec.Command("go", "build", "-o", helperBin, "drydock/cmd/brokerd").CombinedOutput(); err != nil {
		t.Fatalf("build brokerd helper: %v\n%s", err, out)
	}
	tokenPath := filepath.Join(runDir, "task-tokens")
	helperCmd := fmt.Sprintf("%s __squid-authhelper %s", helperBin, tokenPath)

	// Default allowlist deliberately does NOT contain the widened host, so the
	// only way the VM reaches it is via the per-task widening credential.
	allowPath := filepath.Join(runDir, "squid-allow.txt")
	if err := os.WriteFile(allowPath, []byte("registry.npmjs.org\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(runDir, "squid.conf")
	if err := os.WriteFile(confPath, []byte(CompileSquidConf(e2eProxyAddr, allowPath, runDir, helperCmd)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ResetTaskState(runDir); err != nil {
		t.Fatal(err)
	}

	// 1) Anchor brings up the vmnet gateway IP (192.168.66.1) so squid can bind it.
	run(t, "container", "rm", "-f", "drydock-anchor")
	if out, err := exec.Command("container", "run", "-d", "--name", "drydock-anchor",
		"--network", e2eNetwork, e2eAnchor).CombinedOutput(); err != nil {
		t.Fatalf("start anchor: %v\n%s", err, out)
	}
	t.Cleanup(func() { run(t, "container", "rm", "-f", "drydock-anchor") })
	if !waitBindableTCP(e2eProxyAddr, 20*time.Second) {
		t.Fatalf("gateway %s never became bindable (anchor up?)", e2eProxyAddr)
	}

	// 2) Start the real squid on the gateway IP.
	squid := exec.Command(squidBin, "-N", "-f", confPath)
	squid.Stdout, squid.Stderr = os.Stdout, os.Stderr
	if err := squid.Start(); err != nil {
		t.Fatalf("start squid: %v", err)
	}
	t.Cleanup(func() { _ = squid.Process.Kill(); _ = squid.Wait() })
	// The vmnet gateway IP is reachable from containers, NOT from the host, so a
	// host-side TCP dial to it never succeeds even once squid is listening.
	// Detect readiness via lsof (listener presence) instead. Binding a
	// non-loopback IP also takes squid several seconds longer than loopback.
	if !waitSquidListening(e2eProxyAddr, 30*time.Second) {
		t.Fatalf("squid did not start listening on %s", e2eProxyAddr)
	}

	// 3) Register the per-task widening credential via the real controller.
	const user, secret = "task-e2e", "e2e-secret-xyz"
	ctl := NewSquidController(squidBin, confPath, runDir)
	if err := ctl.AddTask(user, secret, []string{e2eWidened}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	t.Cleanup(func() { _ = ctl.RemoveTask(user) })

	// 4) Run a REAL sandbox container: pin the in-VM firewall, then probe egress
	//    through the proxy exactly as a task's tool would.
	authedProxy := fmt.Sprintf("http://%s:%s@%s", user, secret, e2eProxyAddr)
	plainProxy := "http://" + e2eProxyAddr
	script := fmt.Sprintf(`
/usr/local/bin/init-firewall.sh %s 8088 3128 >/dev/null 2>&1
probe() { curl -sS -m 10 -x "$1" -o /dev/null -w '%%{http_code}' "https://$2/" 2>/dev/null || echo BLOCKED; }
echo "WIDENED:$(probe '%s' %s)"
echo "NOTALLOWED:$(probe '%s' example.com)"
echo "NOCREDS:$(probe '%s' %s)"
`, e2eGatewayIP, authedProxy, e2eWidened, authedProxy, plainProxy, e2eWidened)

	out, err := exec.Command("container", "run", "--rm", "--user", "root",
		"--cap-add", "CAP_NET_ADMIN", "--network", e2eNetwork,
		"--entrypoint", "/bin/bash", e2eSandbox, "-lc", script).CombinedOutput()
	probe := string(out)
	t.Logf("VM probe output:\n%s", probe)
	if err != nil && !strings.Contains(probe, "WIDENED:") {
		t.Fatalf("container run failed: %v\n%s", err, probe)
	}

	// 5) Assertions.
	widened := grepTag(probe, "WIDENED:")
	if !isReachable(widened) {
		t.Errorf("[E2E] widened host %s NOT reachable through authed proxy (got %q) — widening failed end to end", e2eWidened, widened)
	} else {
		t.Logf("[E2E] widened host %s reachable via per-task cred (%s) ✓", e2eWidened, widened)
	}
	if notAllowed := grepTag(probe, "NOTALLOWED:"); isReachable(notAllowed) {
		t.Errorf("[E2E] non-allowlisted example.com was reachable (%q) — should be blocked", notAllowed)
	} else {
		t.Logf("[E2E] non-allowlisted example.com blocked (%s) ✓", notAllowed)
	}
	if noCreds := grepTag(probe, "NOCREDS:"); isReachable(noCreds) {
		t.Errorf("[E2E] widened host reachable WITHOUT proxy creds (%q) — auth not enforced", noCreds)
	} else {
		t.Logf("[E2E] widened host without creds blocked (%s) ✓", noCreds)
	}
}

// isReachable reports whether a probe tag value is a 2xx/3xx HTTP code.
func isReachable(v string) bool {
	if len(v) != 3 {
		return false
	}
	return v[0] == '2' || v[0] == '3'
}

func grepTag(out, tag string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, tag) {
			return strings.TrimSpace(strings.TrimPrefix(ln, tag))
		}
	}
	return ""
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	_ = exec.Command(name, args...).Run()
}

func waitBindableTCP(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			_ = l.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// waitSquidListening polls lsof until a squid process is LISTENing on addr.
// Used instead of a host-side dial because the vmnet gateway IP is only
// reachable from inside containers on the network, not from the host.
func waitSquidListening(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("lsof", "-nP", "-iTCP:3128", "-sTCP:LISTEN").CombinedOutput()
		s := string(out)
		if strings.Contains(s, "squid") && strings.Contains(s, addr) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
