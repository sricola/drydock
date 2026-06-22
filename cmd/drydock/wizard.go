package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

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
	defer func() { _ = exec.Command("stty", "echo").Run(); fmt.Fprintln(os.Stdout) }()
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
