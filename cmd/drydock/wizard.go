package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	ossignal "os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"drydock/internal/config"
)

// promptChoice prints a numbered menu and returns the chosen 1-based index.
// Empty input returns dflt; invalid input re-prompts.
func promptChoice(in io.Reader, out io.Writer, q string, opts []string, dflt int) int {
	r := bufio.NewReader(in)
	for {
		fmt.Fprintln(out, q)
		for i, o := range opts {
			tag := ""
			if i+1 == dflt {
				tag = "  (default)"
			}
			fmt.Fprintf(out, "  [%d] %s%s\n", i+1, o, tag)
		}
		fmt.Fprint(out, "> ")
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return dflt
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(opts) {
			return n
		}
		fmt.Fprintf(out, "  please enter 1–%d\n", len(opts))
	}
}

// promptYesNo returns the y/n answer; empty input returns dflt.
func promptYesNo(in io.Reader, out io.Writer, q string, dflt bool) bool {
	suffix := " [y/N] "
	if dflt {
		suffix = " [Y/n] "
	}
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, q+suffix)
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return dflt
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
}

// promptSecret reads one line from stdin with terminal echo disabled, so a
// pasted API key doesn't render on screen. Uses the system `stty` (no new
// dependency); echo is restored even on error.
func promptSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	_ = exec.Command("stty", "-echo").Run()
	restore := func() { _ = exec.Command("stty", "echo").Run() }
	// A bare defer won't run if a signal kills the process mid-read, which
	// would leave the terminal echo-off. Restore on SIGINT/SIGTERM too.
	sigCh := make(chan os.Signal, 1)
	ossignal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			restore()
			fmt.Fprintln(os.Stdout)
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		ossignal.Stop(sigCh)
		close(done)
		restore()
		fmt.Fprintln(os.Stdout)
	}()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

type wizardChoices struct {
	DefaultAgent  string // "claude" | "codex"
	AnthropicAuth string // "api_key" | "subscription"
	OpenAIAuth    string // "api_key" | "subscription"
}

// renderConfig returns a complete config.yaml body: the seeded template with
// default_agent / anthropic_auth / openai_auth set to the wizard's choices;
// every other key keeps its template default.
func renderConfig(c wizardChoices) string {
	if c.DefaultAgent == "" {
		c.DefaultAgent = "claude"
	}
	if c.AnthropicAuth == "" {
		c.AnthropicAuth = "api_key"
	}
	if c.OpenAIAuth == "" {
		c.OpenAIAuth = "api_key"
	}
	body := config.SeedTemplate
	body = setYAMLKey(body, "default_agent", c.DefaultAgent)
	body = setYAMLKey(body, "anthropic_auth", c.AnthropicAuth)
	body = setYAMLKey(body, "openai_auth", c.OpenAIAuth)
	return body
}

// setYAMLKey rewrites the value of a top-level `key:` line, preserving the rest
// of the line's trailing comment alignment as written in the template.
func setYAMLKey(body, key, value string) string {
	re := regexp.MustCompile(`(?m)^(` + regexp.QuoteMeta(key) + `:\s*)\S+`)
	return re.ReplaceAllString(body, "${1}"+value)
}

type wizardDeps struct {
	in              io.Reader
	out             io.Writer
	bootstrapClaude func() error
	bootstrapCodex  func() error
	configPath      string
}

// runWizard drives the interactive config flow and writes config.yaml.
func runWizard(d *wizardDeps) wizardChoices {
	// Wrap d.in in a single bufio.Reader so all prompt helpers share the same
	// buffer. bufio.NewReader is a no-op if d.in is already a *bufio.Reader.
	d.in = bufio.NewReader(d.in)
	var c wizardChoices

	agent := promptChoice(d.in, d.out, "Which coding agent?",
		[]string{"Claude Code (Anthropic)", "OpenAI Codex", "both"}, 1)
	wantClaude := agent == 1 || agent == 3
	wantCodex := agent == 2 || agent == 3
	if agent == 2 {
		c.DefaultAgent = "codex"
	} else {
		c.DefaultAgent = "claude" // 1 or 3 ("both" defaults to claude)
	}

	if wantClaude {
		c.AnthropicAuth = authStep(d, "Claude Code", "claude login", "ANTHROPIC_API_KEY", d.bootstrapClaude)
	}
	if wantCodex {
		c.OpenAIAuth = authStep(d, "OpenAI Codex", "codex login", "OPENAI_API_KEY", d.bootstrapCodex)
	}

	if err := os.MkdirAll(filepath.Dir(d.configPath), 0o700); err == nil {
		_ = os.WriteFile(d.configPath, []byte(renderConfig(c)), 0o644)
	}
	fmt.Fprintf(d.out, "\nwrote %s · default_agent: %s\n", d.configPath, c.DefaultAgent)
	fmt.Fprintln(d.out, "start:  drydock start      first task:  drydock submit --repo <url> --instruction \"…\"")
	return c
}

// authStep asks the auth mode for one agent and bootstraps the credential.
// Returns "subscription" or "api_key". All credential failures are non-fatal.
func authStep(d *wizardDeps, label, loginCmd, envName string, bootstrap func() error) string {
	mode := promptChoice(d.in, d.out, "How will "+label+" authenticate?",
		[]string{"subscription — no API key", "API key (" + envName + ")"}, 1)
	if mode == 1 {
		if err := bootstrap(); err != nil {
			fmt.Fprintf(d.out, "  ! %v — run `%s`, then re-run `drydock setup`\n", err, loginCmd)
		} else {
			fmt.Fprintf(d.out, "  ✓ %s credential stored\n", label)
		}
		return "subscription"
	}
	// API key: consented persistence; env-only preserved.
	if promptYesNo(d.in, d.out, "Store the API key at ~/.drydock/api-keys.env (0600) so the broker finds it across shells?", true) {
		val := os.Getenv(envName)
		if val == "" {
			v, err := promptSecret("  paste " + envName + ": ")
			if err == nil {
				val = v
			}
		}
		if val != "" {
			if err := config.WriteAPIKey(config.APIKeysPath(), envName, val); err != nil {
				fmt.Fprintf(d.out, "  ! could not store key: %v\n", err)
			} else {
				fmt.Fprintf(d.out, "  ✓ stored %s\n", envName)
			}
		}
	} else if os.Getenv(envName) == "" {
		fmt.Fprintf(d.out, "  ! before `drydock start`: export %s=…\n", envName)
	}
	return "api_key"
}
