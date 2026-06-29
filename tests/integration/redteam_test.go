//go:build integration

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"drydock/internal/gateway"
	"drydock/internal/provider"
)

// gwEnvNames returns the injected env-var names for a vendor, sourced from the
// provider registry so a hand-built gateway.Provider here can't drift from the
// real wiring (and satisfies Mint's non-empty BaseURLEnv/TokenEnv guard).
func gwEnvNames(vendor string) (baseURLEnv, tokenEnv string) {
	p, _ := provider.ByVendor(vendor)
	return p.BaseURLEnv, p.TokenEnv
}

// VM-backed red-team tests (THREAT_MODEL A1, A2, A7): each runs an actual
// attack inside the sandbox VM and asserts containment. They need the Apple
// `container` runtime + the drydock-sandbox image, so they live in the
// integration suite. Run: `make redteam-vm` (or `make test-integration`).

func sandboxImage() string {
	if v := os.Getenv("SANDBOX_IMAGE"); v != "" {
		return v
	}
	return "drydock-sandbox:latest"
}

func requireContainer(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("no `container` CLI on PATH; skipping VM red-team test")
	}
}

// containerRun runs the container CLI and returns combined output. The in-VM
// command is often expected to fail (a blocked curl), so we assert on output,
// not on the process exit code.
func containerRun(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := exec.Command("container", args...).CombinedOutput()
	t.Logf("container %s ->\n%s", strings.Join(args, " "), out)
	return string(out)
}

// A1 (openai-compat variant) — the real vendor key never enters the VM for the
// openai-compat (opencode) lane. We build the EXACT env the broker injects,
// using an OpenAICompatVendor gateway with a sentinel real key, then inspect
// that env from inside the VM. The sentinel must be absent; only the per-task
// bearer token may be present.
func TestRedteam_A1_OpenAICompatRealKeyNeverInVM(t *testing.T) {
	requireContainer(t)
	const sentinel = "sk-oc-SENTINEL-DO-NOT-LEAK-7a2b"

	gw, err := gateway.New(gateway.Backend{Vendor: gateway.OpenAICompatVendor("openai-compat", "https://up.invalid", "", nil), Cred: gateway.StaticKey(sentinel)})
	if err != nil {
		t.Fatal(err)
	}
	oBase, oTok := gwEnvNames("openai-compat")
	if oBase == "" || oTok == "" {
		t.Fatalf("gwEnvNames(\"openai-compat\") returned empty names (oBase=%q oTok=%q); registry gap — cannot proceed", oBase, oTok)
	}
	prov := &gateway.Provider{GW: gw, Vendor: "openai-compat", BaseURL: "http://10.0.0.1:8088", BaseURLEnv: oBase, TokenEnv: oTok, Budget: 1, TTL: time.Minute}
	grant, err := prov.Mint(1)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Revoke()

	args := []string{"run", "--rm", "--entrypoint", "/bin/bash"}
	for _, e := range grant.EnvVars() {
		args = append(args, "--env", e)
	}
	args = append(args, sandboxImage(), "-lc",
		"env; echo '---PROC---'; tr '\\0' '\\n' < /proc/self/environ")

	out := containerRun(t, args...)
	if strings.Contains(out, sentinel) {
		t.Fatalf("A1 BREACH: the real openai-compat key leaked into the VM environment:\n%s", out)
	}
	if !strings.Contains(out, "tok_") {
		t.Errorf("expected the per-task bearer token (tok_) in the VM env; got:\n%s", out)
	}
}

// A1 — the real vendor key never enters the VM. We build the EXACT env the
// broker injects, using the real gateway + credential provider with a sentinel
// real key, then inspect that env from inside the VM. The sentinel must be
// absent; only the per-task bearer token may be present.
func TestRedteam_A1_RealKeyNeverInVM(t *testing.T) {
	requireContainer(t)
	const sentinel = "sk-ant-SENTINEL-DO-NOT-LEAK-9f3c"

	gw, err := gateway.New(gateway.Backend{Vendor: gateway.AnthropicVendor(), Cred: gateway.StaticKey(sentinel)})
	if err != nil {
		t.Fatal(err)
	}
	aBase, aTok := gwEnvNames("anthropic")
	prov := &gateway.Provider{GW: gw, Vendor: "anthropic", BaseURL: "http://10.0.0.1:8088", BaseURLEnv: aBase, TokenEnv: aTok, Budget: 1, TTL: time.Minute}
	grant, err := prov.Mint(1)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Revoke()

	args := []string{"run", "--rm", "--entrypoint", "/bin/bash"}
	for _, e := range grant.EnvVars() {
		args = append(args, "--env", e)
	}
	args = append(args, sandboxImage(), "-lc",
		"env; echo '---PROC---'; tr '\\0' '\\n' < /proc/self/environ")

	out := containerRun(t, args...)
	if strings.Contains(out, sentinel) {
		t.Fatalf("A1 BREACH: the real key leaked into the VM environment:\n%s", out)
	}
	if !strings.Contains(out, "tok_") {
		t.Errorf("expected the per-task bearer token (tok_) in the VM env; got:\n%s", out)
	}
}

// noopStore satisfies gateway.CredStore without persisting anything.
// The far-future Expiry on the OAuthCred means Current() never triggers a
// refresh, so Load/Save are never called — the type exists only to satisfy
// the interface.
type noopStore struct{}

func (noopStore) Load() (gateway.CredSnapshot, error) { return gateway.CredSnapshot{}, nil }
func (noopStore) Save(gateway.CredSnapshot) error     { return nil }

// A1 (OAuth variant) — OAuth access and refresh tokens never enter the VM.
// We build the EXACT env the broker injects using the OAuth backend with
// sentinel token values, then inspect that env from inside the VM. Both
// sentinels must be absent; only the per-task bearer token may be present.
func TestRedteam_A1_OAuthTokensNeverInVM(t *testing.T) {
	requireContainer(t)
	const (
		accessSentinel  = "sk-ant-oat-SENTINEL-ACCESS"
		refreshSentinel = "sk-ant-oat-SENTINEL-REFRESH"
	)

	gw, err := gateway.New(gateway.Backend{
		Vendor: gateway.AnthropicOAuthVendor(),
		Cred: gateway.NewOAuthCred(
			gateway.CredSnapshot{
				Access:  accessSentinel,
				Refresh: refreshSentinel,
				Expiry:  time.Now().Add(time.Hour),
			},
			noopStore{},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	aBase, aTok := gwEnvNames("anthropic")
	prov := &gateway.Provider{GW: gw, Vendor: "anthropic", BaseURL: "http://10.0.0.1:8088", BaseURLEnv: aBase, TokenEnv: aTok, Budget: 1, TTL: time.Minute}
	grant, err := prov.Mint(1)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Revoke()

	args := []string{"run", "--rm", "--entrypoint", "/bin/bash"}
	for _, e := range grant.EnvVars() {
		args = append(args, "--env", e)
	}
	args = append(args, sandboxImage(), "-lc",
		"env; echo '---PROC---'; tr '\\0' '\\n' < /proc/self/environ")

	out := containerRun(t, args...)
	if strings.Contains(out, accessSentinel) {
		t.Fatalf("A1 BREACH: OAuth access token leaked into the VM environment:\n%s", out)
	}
	if strings.Contains(out, refreshSentinel) {
		t.Fatalf("A1 BREACH: OAuth refresh token leaked into the VM environment:\n%s", out)
	}
	if !strings.Contains(out, "tok_") {
		t.Errorf("expected the per-task bearer token (tok_) in the VM env; got:\n%s", out)
	}
}

// A1 (Codex OAuth variant) — OAuth access token, refresh token, AND the
// chatgpt-account-id header value never enter the VM. We build the EXACT env
// the broker injects using the Codex OAuth backend with sentinel values, then
// inspect that env from inside the VM. All three sentinels must be absent;
// only the per-task bearer token may be present.
func TestRedteam_A1_CodexOAuthNeverInVM(t *testing.T) {
	requireContainer(t)
	const (
		access  = "sk-codex-SENTINEL-ACCESS"
		refresh = "sk-codex-SENTINEL-REFRESH"
		account = "acct-SENTINEL-ID"
	)

	gw, err := gateway.New(gateway.Backend{
		Vendor: gateway.OpenAIOAuthVendor(account),
		Cred: gateway.NewOAuthCredCodex(
			gateway.CredSnapshot{
				Access:  access,
				Refresh: refresh,
				Expiry:  time.Now().Add(time.Hour),
			},
			noopStore{},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	oBase, oTok := gwEnvNames("openai")
	prov := &gateway.Provider{GW: gw, Vendor: "openai", BaseURL: "http://10.0.0.1:8088", BaseURLEnv: oBase, TokenEnv: oTok, Budget: 1, TTL: time.Minute}
	grant, err := prov.Mint(1)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Revoke()

	args := []string{"run", "--rm", "--entrypoint", "/bin/bash"}
	for _, e := range grant.EnvVars() {
		args = append(args, "--env", e)
	}
	args = append(args, sandboxImage(), "-lc",
		"env; echo '---PROC---'; tr '\\0' '\\n' < /proc/self/environ")

	out := containerRun(t, args...)
	if strings.Contains(out, access) {
		t.Fatalf("A1 BREACH: Codex OAuth access token leaked into the VM environment:\n%s", out)
	}
	if strings.Contains(out, refresh) {
		t.Fatalf("A1 BREACH: Codex OAuth refresh token leaked into the VM environment:\n%s", out)
	}
	if strings.Contains(out, account) {
		t.Fatalf("A1 BREACH: chatgpt-account-id leaked into the VM environment:\n%s", out)
	}
	if !strings.Contains(out, "tok_") {
		t.Errorf("expected the per-task bearer token (tok_) in the VM env; got:\n%s", out)
	}
}

// A2 — the agent cannot reach a hostile or unintended host. Apply the in-VM
// firewall pin, then try three escapes: HTTPS to a non-allowlisted host, raw
// DNS, and a direct-IP connect. All must be blocked.
func TestRedteam_A2_EgressToHostileHostBlocked(t *testing.T) {
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
			t.Errorf("A2 BREACH: %s egress was not blocked:\n%s", vec, out)
		}
	}
}

// A7 — no task state persists between tasks. Task 1 writes a secret into the VM
// filesystem and exits (--rm); a fresh task 2 must not see it.
func TestRedteam_A7_NoStatePersistsBetweenTasks(t *testing.T) {
	requireContainer(t)
	const secret = "REDTEAM-A7-SECRET-DO-NOT-CARRY-OVER"

	containerRun(t, "run", "--rm", "--entrypoint", "/bin/bash",
		sandboxImage(), "-lc", "echo "+secret+" > /tmp/leak; echo wrote")

	out := containerRun(t, "run", "--rm", "--entrypoint", "/bin/bash",
		sandboxImage(), "-lc", "cat /tmp/leak 2>/dev/null || echo absent")
	if strings.Contains(out, secret) {
		t.Fatalf("A7 BREACH: task 2 saw task 1's filesystem state:\n%s", out)
	}
	if !strings.Contains(out, "absent") {
		t.Errorf("expected the marker to be absent in a fresh VM; got:\n%s", out)
	}
}
