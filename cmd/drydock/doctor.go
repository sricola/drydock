package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"drydock/internal/config"
	"drydock/internal/defaults"
	"drydock/internal/egress"
	"drydock/internal/provider"
	"drydock/internal/remote"
)

// pushCredsAvailable is the pure heart of doctor's git-credential check: a
// heuristic (doctor knows no target repo; the per-repo preflight at submit
// is the real gate). Passes when an HTTPS credential helper is configured
// or SSH looks usable (agent socket or a private key on disk).
func pushCredsAvailable(credHelper, sshAuthSock string, sshKeys []string) (bool, string) {
	switch {
	case credHelper != "":
		return true, "https credential helper: " + credHelper
	case sshAuthSock != "":
		return true, "ssh agent socket present"
	case len(sshKeys) > 0:
		return true, "ssh key on disk: " + filepath.Base(sshKeys[0])
	default:
		return false, "no https credential helper and no ssh key/agent; pushes will fail at the submit preflight. Fix: `gh auth setup-git` (https) or create/load an ssh key"
	}
}

// runDoctor is the no-API-spend smoke. It catches the failure modes that
// only show up at task time today — stale image entrypoint, sandbox can't
// boot, nft pin doesn't enforce, anchor isn't up. None of these require
// brokerd to be running or a real ANTHROPIC_API_KEY; they just exercise
// the container artifacts the broker would lean on.
//
// Exit code 0 = all checks passed; 1 = at least one check failed.
func runDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	repo := fs.String("repo", "", "diagnose a repo path for drydock readiness (no API spend, no container)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: drydock doctor [--repo <path>]\n\n%s\n\nFlags:\n", subHelp["doctor"])
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError: Parse exits on bad flags / handles -h
	if *repo != "" {
		runRepoDoctor(*repo)
		return
	}

	fmt.Println("drydock doctor — sandbox smoke test (no API spend)")
	fmt.Println()
	failed := false

	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		step("config", false, err.Error())
		os.Exit(1)
	}

	// 1. The image entrypoint must read DRYDOCK_GW_IP, not the pre-rename
	// MACAGENT_GW_IP — otherwise every task aborts at boot. Same property
	// `drydock init` guards on rebuild, just here as a runtime check too.
	out, err := runCmd("container", "run", "--rm", "--entrypoint", "/bin/cat",
		cfg.SandboxImage, "/usr/local/bin/entrypoint.sh")
	switch {
	case err != nil:
		step("sandbox entrypoint", false, "could not read: "+strings.TrimSpace(string(out)))
		failed = true
	case !strings.Contains(string(out), "DRYDOCK_GW_IP"):
		step("sandbox entrypoint", false, "stale — reads MACAGENT_GW_IP; run `drydock init` to rebuild")
		failed = true
	default:
		step("sandbox entrypoint", true, "fresh (reads DRYDOCK_GW_IP)")
	}

	// 2. Sandbox must actually boot and report a working `claude --version`.
	// This is the cheap proof that the image is healthy end-to-end (apt
	// layer, setpriv drop, claude-code install all worked).
	out, err = runCmd("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "claude --version 2>&1")
	switch {
	case err != nil:
		step("sandbox boot", false, "container run failed: "+strings.TrimSpace(string(out)))
		failed = true
	case !strings.Contains(string(out), "Claude Code"):
		step("sandbox boot", false, "claude --version did not return Claude Code: "+strings.TrimSpace(string(out)))
		failed = true
	default:
		// `container run` prints [0/6]…[6/6] progress lines before the
		// real stdout. Strip them so the doctor line stays one line.
		step("sandbox boot", true, claudeVersionLine(string(out)))
	}

	// 2a. The dropped agent user must end up with a writable HOME. Claude
	// Code creates ~/.claude/session-env/<id> on every Bash tool call, so a
	// HOME leaked from root (=/root, unwritable after the setpriv drop)
	// breaks every shell command inside the sandbox while trivial
	// read/edit tasks still pass (issue #198). Two assertions in one boot:
	// the entrypoint's claude exec sets HOME=/home/agent (staleness, like
	// the DRYDOCK_GW_IP check above), and that home is actually writable
	// through the real setpriv drop, so the check fails exactly when
	// tasks would.
	out, err = runCmd("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c",
		`grep -q 'env HOME=/home/agent claude' /usr/local/bin/entrypoint.sh || { echo stale-entrypoint-no-agent-home; exit 1; }; `+
			`/usr/local/bin/drop-agent env HOME=/home/agent /bin/sh -c 'mkdir -p "$HOME/.claude/session-env/doctor-probe" && echo agent-home-writable'`)
	switch {
	case strings.Contains(string(out), "stale-entrypoint-no-agent-home"):
		step("agent HOME writable", false, "stale entrypoint — claude runs with root's HOME; run `drydock init` to rebuild")
		failed = true
	case err != nil:
		step("agent HOME writable", false, "probe failed: "+strings.TrimSpace(string(out)))
		failed = true
	case !strings.Contains(string(out), "agent-home-writable"):
		step("agent HOME writable", false, strings.TrimSpace(lastLine(string(out))))
		failed = true
	default:
		step("agent HOME writable", true, "agent can create ~/.claude/session-env")
	}

	// 2b. Codex CLI must also be installed (the image hosts both agents). A
	// "not found" here almost always means cfg.SandboxImage predates the v0.1.5
	// rename (claude-sandbox -> drydock-sandbox, which added Codex), so point
	// the operator at the fix instead of dumping a raw shell error.
	out, err = runCmd("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "codex --version 2>&1")
	if codexPresent(string(out), err) {
		step("codex present", true, strings.TrimSpace(lastLine(string(out))))
	} else {
		step("codex present", false, "not found in "+cfg.SandboxImage)
		fmt.Println("    → that image likely predates Codex (pre-v0.1.5). Fix: run `drydock init`")
		fmt.Println("      to rebuild, or set `sandbox_image: drydock-sandbox:latest` in ~/.drydock/config.yaml")
		failed = true
	}

	// 2c. Gemini CLI presence (native google vendor). Absence usually means the
	// image predates native Gemini — point at `drydock init` rather than a raw
	// shell error.
	out, err = runCmd("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "gemini --version 2>&1")
	if cliVersionPresent(string(out), err) {
		step("gemini present", true, strings.TrimSpace(lastLine(string(out))))
	} else {
		step("gemini present", false, "not found in "+cfg.SandboxImage)
		fmt.Println("    → that image likely predates native Gemini. Fix: run `drydock init` to rebuild")
		failed = true
	}

	// 2d. opencode CLI presence (the openai-compat / bring-your-own-model lane).
	// Without this check an image missing opencode passes doctor green, then
	// every `--agent opencode` task dies at the entrypoint.
	out, err = runCmd("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "opencode --version 2>&1")
	if cliVersionPresent(string(out), err) { // same predicate: bare version, zero exit, no "not found"
		step("opencode present", true, strings.TrimSpace(lastLine(string(out))))
	} else {
		step("opencode present", false, "not found in "+cfg.SandboxImage)
		fmt.Println("    → that image likely predates the opencode lane. Fix: run `drydock init` to rebuild")
		failed = true
	}

	// 3. The nft egress pin must default-deny output. We install the pin
	// pointing at an unreachable gateway IP, then confirm a non-allowlisted
	// host fails to resolve (DNS dropped) or fails to connect (no route).
	// Passing means the central isolation claim holds; failing means the
	// sandbox would leak egress if `drydock submit` were invoked.
	out, err = runCmd("container", "run", "--rm", "--user", "root",
		"--entrypoint", "/bin/bash", "--cap-add", "CAP_NET_ADMIN",
		cfg.SandboxImage, "-lc",
		`/usr/local/bin/init-firewall.sh 192.168.66.1 8088 3128 &&
		 curl -sS -m 5 https://example.com/ -o /dev/null -w '%{http_code}\n' 2>/dev/null || echo blocked`)
	got := strings.TrimSpace(string(out))
	switch {
	case err != nil && !strings.Contains(got, "blocked"):
		step("egress pin enforces", false, "smoke failed: "+got)
		failed = true
	case got == "blocked", strings.HasSuffix(got, "blocked"):
		step("egress pin enforces", true, "non-allowlisted host blocked")
	default:
		step("egress pin enforces", false, "non-allowlisted host reachable: "+got)
		failed = true
	}

	// In-VM DNS advisory: a loopback-only host resolver (WARP/dnscrypt/VPN)
	// breaks DNS inside every `container` VM, so image (re)builds fail at
	// apt/npm even though everything already built keeps working. Advisory,
	// not a failure — nothing is broken until the next build.
	if out, err := runCmd("scutil", "--dns"); err == nil && loopbackOnlyDNS(string(out)) {
		stepWarn("vm dns", "host resolvers are loopback-only (WARP/VPN?) — image (re)builds will fail; fix: container builder start --dns 1.1.1.1")
	}

	// 4. For each provider: when subscription auth is configured, validate the
	// stored OAuth token by calling Current() once. This also refreshes the
	// token if it is near expiry — no API budget spend beyond the refresh.
	// Skipped entirely in api_key mode (api-key source is reported instead).
	fileKeys, _ := config.LoadAPIKeys(config.APIKeysPath())
	for _, p := range provider.Registry {
		if p.ConfigBuilt {
			continue
		}
		if cfg.AuthMode(p.Vendor) == "subscription" {
			backend, err := p.OAuthBackend(config.Dir())
			if err != nil {
				step(p.Agent+" subscription", false, "load creds: "+err.Error())
				failed = true
			} else {
				if _, err := backend.Cred.Current(); err != nil {
					step(p.Agent+" subscription", false, err.Error())
					failed = true
				} else {
					step(p.Agent+" subscription", true, "token valid")
				}
			}
		} else {
			step(p.Vendor+" api key", true, "source: "+apiKeySource(p.APIKeyEnv, fileKeys))
		}
	}

	// openai-compat: optional bring-your-own endpoint — report key source but
	// never mark doctor failed (the provider is opt-in).
	if cfg.OpenAICompat.BaseURL != "" {
		label := "openai-compat (" + cfg.OpenAICompat.Model + ")"
		if src := apiKeySource(cfg.OpenAICompat.APIKeyEnv, fileKeys); src == "none" {
			// Opt-in lane, not configured yet: advise, don't fail. A red ✗ here
			// would contradict the "all checks passed" line printed below.
			stepWarn(label, "no key in "+cfg.OpenAICompat.APIKeyEnv+" — set it before submitting opencode tasks")
		} else {
			step(label, true, "key from "+src)
		}
	}

	// PR tooling: report which platform CLI (if any) is authenticated. Not a
	// failure — push-only is a legitimate mode, and doctor is repo-agnostic.
	anyAuthed := false
	for _, a := range []remote.Adapter{remote.GitHubAdapter{}, remote.GitLabAdapter{}, remote.GiteaAdapter{}} {
		if err := a.Available(); err == nil {
			step("PR tooling: "+a.Name(), true, "authenticated")
			anyAuthed = true
		}
	}
	if !anyAuthed {
		fmt.Println("note: no PR CLI (gh/glab/tea) is authenticated — tasks will push a branch but not open a PR until you authenticate one.")
	}

	// Git push credentials (heuristic; the per-repo submit preflight is
	// the enforced gate). Non-interactive: prompts are disabled everywhere.
	// --get-regexp (not --get credential.helper) so a URL-scoped helper
	// entry counts too: `gh auth setup-git` (doctor's own https remedy)
	// writes credential.https://github.com.helper, which --get on the bare
	// "credential.helper" key never matches, so doctor would otherwise
	// report no helper right after following its own fix.
	helperOut, _ := runCmd("git", "config", "--get-regexp", `^credential\.`)
	credHelper := firstNonEmptyLine(string(helperOut))
	keys, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".ssh", "id_*"))
	// Public keys don't count; keep only files without .pub.
	priv := keys[:0]
	for _, k := range keys {
		if !strings.HasSuffix(k, ".pub") {
			priv = append(priv, k)
		}
	}
	ok, detail := pushCredsAvailable(credHelper, os.Getenv("SSH_AUTH_SOCK"), priv)
	step("git push credentials", ok, detail)
	if !ok {
		failed = true
	}

	fmt.Println()
	if failed {
		fmt.Println("one or more checks failed — see above")
		os.Exit(1)
	}
	fmt.Println("all checks passed — your sandbox is ready for `drydock submit`")
}

// runRepoDoctor is `doctor --repo <path>`: a pure, host-side preflight of one
// local repo (size vs the stage cap, toolchain vs what the image ships, egress
// registry allowlisting, diff-policy collisions). It never boots a container,
// never spends API budget, and never shells out — the whole diagnosis is
// diagnoseRepo's bounded walk. Exit 0 = no blockers (warnings are advisory);
// 1 = at least one blocker; 2 = the path isn't a directory.
func runRepoDoctor(path string) {
	dir := filepath.Clean(path)
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "drydock doctor: --repo %s: %v\n", dir, err)
		os.Exit(2)
	case !info.IsDir():
		fmt.Fprintf(os.Stderr, "drydock doctor: --repo %s is not a directory\n", dir)
		os.Exit(2)
	}

	fmt.Printf("drydock doctor — repo preflight for %s (no API spend, no container)\n\n", dir)

	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		cfg = config.Defaults()
		stepWarn("config", "could not load ~/.drydock/config.yaml ("+err.Error()+") — using built-in defaults")
	}
	eg := loadEgressForRepoDoctor()

	blockers := renderRepoDoctor(diagnoseRepo(dir, cfg, eg, productionStageLimits(cfg)))
	if blockers > 0 {
		os.Exit(1)
	}
}

// loadEgressForRepoDoctor loads the operator's egress allowlist, falling back
// to the embedded default (what `drydock init` would seed) when egress.yaml is
// missing or unreadable, and to an empty config as the last resort — the
// allowlist check then warns on every registry host, which is the honest
// answer when no allowlist can be read.
func loadEgressForRepoDoctor() egress.Config {
	eg, err := egress.Load(config.EgressPath())
	if err == nil {
		return eg
	}
	var fallback egress.Config
	if yerr := yaml.Unmarshal(defaults.EgressYAML, &fallback); yerr != nil {
		stepWarn("egress config", "no readable egress.yaml and the embedded default failed to parse — treating the allowlist as empty")
		return egress.Config{}
	}
	stepWarn("egress config", "could not load ~/.drydock/egress.yaml ("+err.Error()+") — using the built-in default allowlist")
	return fallback
}

// renderRepoDoctor prints one line per check (pass → ok, Warn → WARN,
// blocker → FAIL) plus a summary line, and returns the blocker count. The
// os.Exit decision stays in runRepoDoctor so this seam is testable via
// captureStdout.
func renderRepoDoctor(checks []repoCheck) (blockers int) {
	for _, c := range checks {
		switch {
		case !c.OK:
			blockers++
			step(c.Label, false, c.Detail)
		case c.Warn:
			stepWarn(c.Label, c.Detail)
		default:
			step(c.Label, true, c.Detail)
		}
	}
	fmt.Println()
	if blockers > 0 {
		fmt.Printf("%d blocker(s) — a task on this repo would fail; fix before `drydock submit`\n", blockers)
	} else {
		fmt.Println("repo preflight passed — no blockers (warnings above, if any, are advisory)")
	}
	return blockers
}

// codexPresent reports whether `codex --version` indicates a working Codex
// CLI. A missing binary surfaces as a non-zero exit and/or a "not found"
// message (the shell can't resolve `codex` on PATH) — almost always a
// sandbox_image that predates Codex.
func codexPresent(out string, runErr error) bool {
	return runErr == nil && !strings.Contains(out, "not found")
}

// cliVersionPresent reports whether an agent CLI's `--version` returned a
// usable version — a bare version string, zero exit, no "not found". Used for
// both gemini and opencode presence checks (was geminiPresent).
func cliVersionPresent(out string, err error) bool {
	// Also reject a shell that exits 0 while printing "not found" (defensive;
	// mirrors codexPresent) — otherwise a pathological image would report a
	// spurious "gemini present".
	return err == nil && strings.TrimSpace(out) != "" && !strings.Contains(out, "not found")
}

// lastLine returns the last non-empty line of s, trimmed. Used for version
// output where the real version string is the final line after any preamble.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// claudeVersionLine extracts the last non-progress line from `container
// run`'s combined output. `container run` prints [0/6]…[6/6] image-pull
// progress before the real command stdout, so the last line is what claude
// actually printed.
func claudeVersionLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if strings.Contains(ln, "Claude Code") {
			return ln
		}
	}
	return strings.TrimSpace(s)
}

// firstNonEmptyLine returns the first non-blank line of s, trimmed. Used to
// turn `git config --get-regexp ^credential\.` output (one "key value" pair
// per line, in config-file order) into a single representative helper
// string for the doctor detail line: any matching line, global or
// URL-scoped, is proof a helper is configured.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// apiKeySource names where an api_key for envName would come from, so the
// operator can see whether a stored file or the shell env is in effect.
func apiKeySource(envName string, fileKeys map[string]string) string {
	if os.Getenv(envName) != "" {
		return "env"
	}
	if fileKeys[envName] != "" {
		return "~/.drydock/api-keys.env"
	}
	return "none"
}

// loopbackOnlyDNS reports whether the host's primary resolver (resolver #1 in
// `scutil --dns` output) lists only loopback nameservers. That's the shape
// Cloudflare WARP, dnscrypt, and some VPNs leave behind — and the vmnet DNS
// forwarder that Apple `container` VMs use cannot reach a host-loopback
// resolver, so every in-VM lookup fails (image builds die at apt/npm) while
// raw egress still works. A public nameserver alongside the loopback one is
// fine; the forwarder can use it.
func loopbackOnlyDNS(scutilOut string) bool {
	inFirst := false
	loop, total := 0, 0
	for _, line := range strings.Split(scutilOut, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "resolver #") {
			if inFirst {
				break // end of resolver #1
			}
			inFirst = l == "resolver #1"
			continue
		}
		if !inFirst || !strings.HasPrefix(l, "nameserver[") {
			continue
		}
		_, ip, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		ip = strings.TrimSpace(ip)
		total++
		if strings.HasPrefix(ip, "127.") || ip == "::1" {
			loop++
		}
	}
	return total > 0 && loop == total
}
