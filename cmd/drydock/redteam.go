package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"drydock/internal/config"
	"drydock/internal/gateway"
)

// runRedteam runs drydock's live containment attacks against the operator's
// OWN sandbox — on their own Mac, on their actual image — and prints a
// pass/fail table. These are the same VM-backed attacks the THREAT_MODEL
// red-team suite asserts (A1, A2, A7); running them here means you don't have
// to trust the threat model, you can watch containment hold on your machine.
//
// No API spend: the attacks inspect the VM's environment, egress, and
// filesystem — they never call a model. The host-side claims (A3-A6) are pure
// logic asserted on every commit by `go test` (`make redteam` from source).
//
// Exit 0 = every live attack contained; 1 = a breach (or setup failure).
func runRedteam() {
	fmt.Println("drydock red-team — live containment attacks on your own Mac (no API spend)")
	fmt.Println()
	checkPlatform()

	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		step("config", false, err.Error())
		os.Exit(1)
	}
	img := cfg.SandboxImage

	failed := false
	failed = !redteamA1(img) || failed
	failed = !redteamA2(img) || failed
	failed = !redteamA7(img) || failed

	fmt.Println()
	fmt.Println("  host-side claims A3-A6 (git-hook exec, diff leak, push gate,")
	fmt.Println("  egress widening) are asserted every commit: `make redteam` from source.")
	fmt.Println()
	if failed {
		fmt.Println("CONTAINMENT BREACH — at least one attack was not contained (see above)")
		os.Exit(1)
	}
	fmt.Println("all live attacks contained — A1/A2/A7 verified against your sandbox")
}

// redteamA1 — the real vendor key never enters the VM. Build the exact env the
// broker injects (a real gateway + provider with a sentinel real key, minted
// to a bearer), then read it back from inside the VM.
func redteamA1(img string) bool {
	const label = "A1  real key never enters VM"
	const sentinel = "sk-ant-SENTINEL-REDTEAM-DO-NOT-LEAK"

	gw, err := gateway.New(gateway.Backend{Vendor: gateway.AnthropicVendor(), RealKey: sentinel})
	if err != nil {
		step(label, false, "gateway init: "+err.Error())
		return false
	}
	prov := &gateway.Provider{GW: gw, Vendor: "anthropic", BaseURL: "http://10.0.0.1:8088", Budget: 1, TTL: time.Minute}
	grant, err := prov.Mint(1)
	if err != nil {
		step(label, false, "mint: "+err.Error())
		return false
	}
	defer grant.Revoke()

	args := []string{"run", "--rm", "--entrypoint", "/bin/bash"}
	for _, e := range grant.EnvVars() {
		args = append(args, "--env", e)
	}
	args = append(args, img, "-lc", "env; echo ---; tr '\\0' '\\n' < /proc/self/environ")
	out, err := exec.Command("container", args...).CombinedOutput()
	if err != nil {
		step(label, false, "container run failed: "+strings.TrimSpace(lastLine(string(out))))
		return false
	}
	ok, detail := a1Verdict(string(out), sentinel)
	step(label, ok, detail)
	return ok
}

// a1Verdict — the sentinel real key must be absent from the VM env; a gateway
// bearer (tok_) must be present.
func a1Verdict(vmEnv, sentinel string) (bool, string) {
	if strings.Contains(vmEnv, sentinel) {
		return false, "BREACH: the real key is visible inside the VM"
	}
	if !strings.Contains(vmEnv, "tok_") {
		return false, "no gateway bearer (tok_) in the VM env — setup incomplete"
	}
	return true, "only a budget-capped tok_ bearer present"
}

// redteamA2 — the agent cannot reach a hostile or unintended host. Pin the
// in-VM firewall, then attempt three escapes: HTTPS to a non-allowlisted host,
// raw DNS, and a direct-IP connect. All must be blocked.
func redteamA2(img string) bool {
	const label = "A2  egress to hostile hosts blocked"
	script := `
/usr/local/bin/init-firewall.sh 192.168.66.1 8088 3128 >/dev/null 2>&1
echo "HTTPS:$(curl -sS -m 4 https://example.com/ -o /dev/null 2>/dev/null && echo reachable || echo blocked)"
echo "DNS:$(nslookup evil.example.com 1.1.1.1 >/dev/null 2>&1 && echo resolved || echo blocked)"
echo "DIRECTIP:$(curl -sS -m 4 https://1.1.1.1/ -o /dev/null 2>/dev/null && echo reachable || echo blocked)"
`
	out, err := exec.Command("container", "run", "--rm", "--user", "root", "--cap-add", "CAP_NET_ADMIN",
		"--entrypoint", "/bin/bash", img, "-lc", script).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "HTTPS:") {
		step(label, false, "container run failed: "+strings.TrimSpace(lastLine(string(out))))
		return false
	}
	ok, detail := a2Verdict(string(out))
	step(label, ok, detail)
	return ok
}

// a2Verdict — every probed vector must report "<TAG>:blocked".
func a2Verdict(probe string) (bool, string) {
	vectors := []struct{ tag, name string }{
		{"HTTPS:", "HTTPS"}, {"DNS:", "DNS"}, {"DIRECTIP:", "direct-IP"},
	}
	var leaked []string
	for _, v := range vectors {
		if !strings.Contains(probe, v.tag+"blocked") {
			leaked = append(leaked, v.name)
		}
	}
	if len(leaked) > 0 {
		return false, "BREACH: egress reachable via " + strings.Join(leaked, ", ")
	}
	return true, "HTTPS, DNS, and direct-IP all blocked"
}

// redteamA7 — no task state persists. Task 1 writes a secret and exits (--rm);
// a fresh task 2 must not see it.
func redteamA7(img string) bool {
	const label = "A7  no state persists between tasks"
	const secret = "REDTEAM-A7-SECRET-DO-NOT-CARRY-OVER"

	if err := exec.Command("container", "run", "--rm", "--entrypoint", "/bin/bash",
		img, "-lc", "echo "+secret+" > /tmp/leak").Run(); err != nil {
		step(label, false, "first VM run failed: "+err.Error())
		return false
	}
	out, err := exec.Command("container", "run", "--rm", "--entrypoint", "/bin/bash",
		img, "-lc", "cat /tmp/leak 2>/dev/null || echo absent").CombinedOutput()
	if err != nil {
		step(label, false, "second VM run failed: "+strings.TrimSpace(lastLine(string(out))))
		return false
	}
	ok, detail := a7Verdict(string(out), secret)
	step(label, ok, detail)
	return ok
}

// a7Verdict — a fresh VM must not contain the prior task's secret.
func a7Verdict(probe, secret string) (bool, string) {
	if strings.Contains(probe, secret) {
		return false, "BREACH: a fresh VM saw the previous task's file"
	}
	if !strings.Contains(probe, "absent") {
		return false, "unexpected probe output — expected the marker to be absent"
	}
	return true, "fresh VM per task — the prior task's state is gone"
}
