package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"drydock/internal/config"
)

// agentCredentialAvailable reports whether brokerd will have at least one
// usable agent credential, so `drydock start` can fail fast with a clear
// message instead of exec'ing a brokerd that just dies. Mirrors brokerd's own
// boot guard: the Anthropic side is satisfied by either an API key OR
// subscription auth; OpenAI by its key or subscription auth.
func agentCredentialAvailable(anthropicAuth, openaiAuth, anthropicKey, openaiKey string) bool {
	anthropicReady := anthropicAuth == "subscription" || anthropicKey != ""
	openaiReady := openaiAuth == "subscription" || openaiKey != ""
	return anthropicReady || openaiReady
}

// runStart finds brokerd on PATH (or alongside drydock if installed via
// `make install`) and execs it. drydock and brokerd are intentionally
// separate binaries so the CLI can talk to a long-running brokerd; `start`
// is a convenience for the foreground case.
func runStart() {
	anthropicAuth, openaiAuth := "api_key", "api_key"
	if cfg, err := config.Load(config.DefaultPath()); err == nil {
		anthropicAuth, openaiAuth = cfg.AnthropicAuth, cfg.OpenAIAuth
	}
	if !agentCredentialAvailable(anthropicAuth, openaiAuth, os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("OPENAI_API_KEY")) {
		fmt.Fprintln(os.Stderr, "drydock start: no usable agent credential.")
		fmt.Fprintln(os.Stderr, "  export ANTHROPIC_API_KEY=sk-ant-...        # Claude Code (API key)")
		fmt.Fprintln(os.Stderr, "  export OPENAI_API_KEY=sk-...               # OpenAI Codex")
		fmt.Fprintln(os.Stderr, "  or set anthropic_auth: subscription        # use your Claude subscription (run `drydock auth claude` first)")
		fmt.Fprintln(os.Stderr, "  or set openai_auth: subscription           # use your ChatGPT/Codex subscription (run `drydock auth codex` first)")
		os.Exit(1)
	}
	path, err := findBrokerd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock start: %v\n", err)
		fmt.Fprintln(os.Stderr, "  build it: make build  (or `go build -o brokerd ./cmd/brokerd`)")
		os.Exit(1)
	}
	// Exec rather than spawn so signals (SIGTERM/SIGINT) reach brokerd
	// directly and `drydock start` is fully transparent.
	if err := syscall.Exec(path, []string{path}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "drydock start: exec brokerd: %v\n", err)
		os.Exit(1)
	}
}

// findBrokerd looks for brokerd next to the drydock binary first (the
// `make install` layout), then falls back to PATH.
func findBrokerd() (string, error) {
	if self, err := os.Executable(); err == nil {
		sibling := siblingOf(self, "brokerd")
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}
	if p, err := exec.LookPath("brokerd"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("brokerd not found alongside drydock or on PATH")
}

func siblingOf(self, name string) string {
	for i := len(self) - 1; i >= 0; i-- {
		if self[i] == '/' {
			return self[:i+1] + name
		}
	}
	return name
}
