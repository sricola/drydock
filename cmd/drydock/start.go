package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"drydock/internal/config"
	"drydock/internal/provider"
)

// agentCredentialAvailable reports whether brokerd will have at least one
// usable agent credential, so `drydock start` can fail fast with a clear
// message instead of exec'ing a brokerd that just dies. Registry-driven:
// any provider satisfied by subscription auth OR a non-empty API key env var
// is sufficient.
func agentCredentialAvailable(cfg *config.Config) bool {
	for _, p := range provider.Registry {
		if p.ConfigBuilt {
			if cfg.OpenAICompat.BaseURL != "" && os.Getenv(cfg.OpenAICompat.APIKeyEnv) != "" {
				return true
			}
			continue
		}
		if cfg.AuthMode(p.Vendor) == "subscription" || os.Getenv(p.APIKeyEnv) != "" {
			return true
		}
	}
	return false
}

// runStart finds brokerd on PATH (or alongside drydock if installed via
// `make install`) and execs it. drydock and brokerd are intentionally
// separate binaries so the CLI can talk to a long-running brokerd; `start`
// is a convenience for the foreground case.
func runStart() {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		// best-effort — use defaults if config is missing
		cfg, _ = config.Load("")
	}
	if cfg == nil {
		cfg = config.Defaults()
	}
	if !agentCredentialAvailable(cfg) {
		fmt.Fprintln(os.Stderr, "drydock start: no usable agent credential.")
		for _, p := range provider.Registry {
			if p.ConfigBuilt {
				continue
			}
			fmt.Fprintf(os.Stderr, "  export %s=...\t\t# %s (API key)\n", p.APIKeyEnv, p.Label)
		}
		for _, p := range provider.Registry {
			if p.ConfigBuilt {
				continue
			}
			fmt.Fprintf(os.Stderr, "  or set %s_auth: subscription\t# run `%s` first\n", p.Vendor, p.AuthCmd)
		}
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
