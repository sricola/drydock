package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"drydock/internal/config"
	"drydock/internal/provider"
	"drydock/internal/remote"
)

// runDoctor is the no-API-spend smoke. It catches the failure modes that
// only show up at task time today — stale image entrypoint, sandbox can't
// boot, nft pin doesn't enforce, anchor isn't up. None of these require
// brokerd to be running or a real ANTHROPIC_API_KEY; they just exercise
// the container artifacts the broker would lean on.
//
// Exit code 0 = all checks passed; 1 = at least one check failed.
func runDoctor() {
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
	out, err := exec.Command("container", "run", "--rm", "--entrypoint", "/bin/cat",
		cfg.SandboxImage, "/usr/local/bin/entrypoint.sh").CombinedOutput()
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
	// layer, gosu, claude-code install all worked).
	out, err = exec.Command("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "claude --version 2>&1").CombinedOutput()
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

	// 2b. Codex CLI must also be installed (the image hosts both agents). A
	// "not found" here almost always means cfg.SandboxImage predates the v0.1.5
	// rename (claude-sandbox -> drydock-sandbox, which added Codex), so point
	// the operator at the fix instead of dumping a raw shell error.
	out, err = exec.Command("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "codex --version 2>&1").CombinedOutput()
	if codexPresent(string(out), err) {
		step("codex present", true, strings.TrimSpace(lastLine(string(out))))
	} else {
		step("codex present", false, "not found in "+cfg.SandboxImage)
		fmt.Println("    → that image likely predates Codex (pre-v0.1.5). Fix: run `drydock init`")
		fmt.Println("      to rebuild, or set `sandbox_image: drydock-sandbox:latest` in ~/.drydock/config.yaml")
		failed = true
	}

	// 3. The nft egress pin must default-deny output. We install the pin
	// pointing at an unreachable gateway IP, then confirm a non-allowlisted
	// host fails to resolve (DNS dropped) or fails to connect (no route).
	// Passing means the central isolation claim holds; failing means the
	// sandbox would leak egress if `drydock submit` were invoked.
	out, err = exec.Command("container", "run", "--rm", "--user", "root",
		"--entrypoint", "/bin/bash", "--cap-add", "CAP_NET_ADMIN",
		cfg.SandboxImage, "-lc",
		`/usr/local/bin/init-firewall.sh 192.168.66.1 8088 3128 &&
		 curl -sS -m 5 https://example.com/ -o /dev/null -w '%{http_code}\n' 2>/dev/null || echo blocked`,
	).CombinedOutput()
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

	// 4. For each provider: when subscription auth is configured, validate the
	// stored OAuth token by calling Current() once. This also refreshes the
	// token if it is near expiry — no API budget spend beyond the refresh.
	// Skipped entirely in api_key mode (api-key source is reported instead).
	fileKeys := config.LoadAPIKeys(config.APIKeysPath())
	for _, p := range provider.Registry {
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
		src := apiKeySource(cfg.OpenAICompat.APIKeyEnv, fileKeys)
		step("openai-compat ("+cfg.OpenAICompat.Model+")", src != "none", "key from "+src)
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

	fmt.Println()
	if failed {
		fmt.Println("one or more checks failed — see above")
		os.Exit(1)
	}
	fmt.Println("all checks passed — your sandbox is ready for `drydock submit`")
}

// codexPresent reports whether `codex --version` indicates a working Codex
// CLI. A missing binary surfaces as a non-zero exit and/or a "not found"
// message (the shell can't resolve `codex` on PATH) — almost always a
// sandbox_image that predates Codex.
func codexPresent(out string, runErr error) bool {
	return runErr == nil && !strings.Contains(out, "not found")
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
