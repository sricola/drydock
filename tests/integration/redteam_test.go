//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"drydock/internal/gateway"
	"drydock/internal/gwcreds"
	"drydock/internal/provider"
	"drydock/internal/runner"
	"drydock/internal/stage"
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

func (noopStore) Load() (gwcreds.CredSnapshot, error) { return gwcreds.CredSnapshot{}, nil }
func (noopStore) Save(gwcreds.CredSnapshot) error     { return nil }

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
		Cred: gwcreds.NewOAuthCred(
			gwcreds.CredSnapshot{
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
		Cred: gwcreds.NewOAuthCredCodex(
			gwcreds.CredSnapshot{
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

// A2 (second leg) — the agent CANNOT rewrite the egress pin. The other A2 test
// proves the ruleset denies egress, but it runs as root+CAP_NET_ADMIN, so it
// never exercises the load-bearing half of the claim: "the agent user cannot
// flush nft." Here root installs the firewall, then drops to the real agent via
// the production drop-agent script, and *as the agent* we assert nft flush is
// denied and egress stays blocked afterward. Without setpriv's no-new-privs +
// emptied bounding set, `gosu` would leave CAP_NET_ADMIN reachable and this
// would BREACH.
func TestRedteam_A2_AgentCannotRewriteFirewall(t *testing.T) {
	requireContainer(t)
	script := `
/usr/local/bin/init-firewall.sh 192.168.66.1 8088 3128
/usr/local/bin/drop-agent bash -c '
  echo "WHOAMI: $(id -un)"
  echo "FLUSH: $(nft flush ruleset >/dev/null 2>&1 && echo succeeded || echo denied)"
  echo "HTTPS-AFTER: $(curl -sS -m 4 https://example.com/ -o /dev/null 2>/dev/null && echo reachable || echo blocked)"
'
`
	out := containerRun(t, "run", "--rm", "--user", "root", "--cap-add", "CAP_NET_ADMIN",
		"--entrypoint", "/bin/bash", sandboxImage(), "-lc", script)
	if !strings.Contains(out, "WHOAMI: agent") {
		t.Fatalf("expected the payload to run as the dropped agent user:\n%s", out)
	}
	if !strings.Contains(out, "FLUSH: denied") {
		t.Errorf("A2 BREACH: the agent was able to flush the nft ruleset:\n%s", out)
	}
	if !strings.Contains(out, "HTTPS-AFTER: blocked") {
		t.Errorf("A2 BREACH: egress was reachable after the agent's flush attempt:\n%s", out)
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

// A8: /work is quota-backed (F-04). An in-VM write flood hits the APFS
// image's hard byte wall (ENOSPC through virtiofs), and the sparse backing
// file on the host never grows meaningfully past the quota. This is the
// hard bound the polling stage guard cannot provide.
func TestRedteam_A8_WorkQuotaHardBound(t *testing.T) {
	requireContainer(t)
	if runtime.GOOS != "darwin" {
		t.Skip("quota images are hdiutil-backed; darwin only")
	}
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const quota = int64(256 << 20)
	if err := stage.AttachQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = (&stage.Stage{Root: root}).Cleanup() })

	out := containerRun(t, "run", "--rm", "--entrypoint", "/bin/bash",
		"--mount", "type=bind,source="+root+",target=/work",
		sandboxImage(), "-lc",
		"dd if=/dev/zero of=/work/fill bs=1M count=1024 2>&1; df -h /work")

	if !strings.Contains(out, "No space left") {
		t.Errorf("A8: expected the in-VM flood to hit ENOSPC; got:\n%s", out)
	}
	fi, err := os.Stat(stage.QuotaImagePath(root))
	if err != nil {
		t.Fatalf("stat quota image: %v", err)
	}
	if fi.Size() > quota+(64<<20) {
		t.Errorf("A8 BREACH: backing image grew to %d MiB, want <= quota (256 MiB) + 64 MiB slack", fi.Size()>>20)
	}
}

// V1 — the verifier VM's containment is strictly tighter than the agent VM's.
// runner.BuildVerifyArgs never injects credential or proxy env (unlike the
// agent VM, which carries a per-task bearer token) and installs a loopback-only
// deny-all nft pin — no gateway, no squid, nothing the agent allowlist permits.
// We build the EXACT argv the broker uses for a verification command (see
// internal/broker/verify.go) and run a probe script through it that checks,
// as the privilege-dropped agent user: (a) /proc/self/environ carries no
// credential/proxy material, (b) HTTPS to a public host, raw DNS, and even a
// direct connect to the network gateway all fail, (c) nft flush ruleset is
// denied (privilege drop holds), and (d) $HOME is writable. Each check prints
// a distinct "LABEL: verdict" token the assertions grep for.
func TestRedteam_V1_VerifierVMHasNoNetworkAndNoCredentials(t *testing.T) {
	requireContainer(t)

	const probe = `
echo "WHOAMI: $(id -un)"
echo "HOME-WRITABLE: $(test -w "$HOME" && echo yes || echo no)"
leak=$(tr '\0' '\n' < /proc/self/environ | grep -Ei 'tok_|sk-|PROXY|AUTH_TOKEN|API_KEY' || true)
if [ -z "$leak" ]; then echo "ENVIRON: clean"; else echo "ENVIRON: LEAK:$leak"; fi
echo "HTTPS: $(curl -sS -m 4 https://example.com/ -o /dev/null 2>/dev/null && echo reachable || echo blocked)"
echo "DNS: $(timeout 5 nslookup evil.example.com 1.1.1.1 >/dev/null 2>&1 && echo resolved || echo blocked)"
echo "GATEWAY: $(curl -sS -m 4 http://192.168.66.1:8088/ -o /dev/null 2>/dev/null && echo reachable || echo blocked)"
echo "FLUSH: $(nft flush ruleset >/dev/null 2>&1 && echo succeeded || echo denied)"
`

	args := runner.BuildVerifyArgs(runner.VerifySpec{
		TaskID:    "redteam-v1",
		Network:   "drydock-egress",
		ImageRef:  sandboxImage(),
		VerifyDir: t.TempDir(),
		Argv:      []string{"bash", "-c", probe},
		MemoryGB:  4,
		CPUs:      4,
	})

	out := containerRun(t, args...)

	if !strings.Contains(out, "WHOAMI: agent") {
		t.Fatalf("expected the probe to run as the dropped agent user:\n%s", out)
	}
	if strings.Contains(out, "LEAK:") {
		t.Fatalf("V1 BREACH: credential/proxy material present in the verifier VM's environment:\n%s", out)
	}
	for _, want := range []string{
		"ENVIRON: clean",
		"HTTPS: blocked",
		"DNS: blocked",
		"GATEWAY: blocked",
		"FLUSH: denied",
		"HOME-WRITABLE: yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("V1 BREACH: expected %q in probe output:\n%s", want, out)
		}
	}
}
