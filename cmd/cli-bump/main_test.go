// Tests for cli-bump: newer and planBumps.
package main

import (
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.1.177", "2.1.178", true},
		{"2.1.177", "2.1.177", false},
		{"2.2.0", "2.1.9", false}, // b=2.1.9 is NOT newer than a=2.2.0
		{"1.0", "1.0.1", true},
	}
	for _, c := range cases {
		got := newer(c.a, c.b)
		if got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPlanBumps(t *testing.T) {
	dockerfile := `ARG CLAUDE_CODE_VERSION=2.1.177
ARG CODEX_VERSION=0.140.0
ARG OPENCODE_VERSION=1.17.11
ARG GEMINI_CLI_VERSION=0.49.0
`
	latest := map[string]string{
		"@anthropic-ai/claude-code": "2.1.178",
		"@openai/codex":             "0.141.0",
		"opencode-ai":               "1.17.11", // same version, no bump
		"@google/gemini-cli":        "0.49.0",  // same version, no bump
	}

	out, bumps := planBumps(dockerfile, latest)

	if len(bumps) != 2 {
		t.Fatalf("expected 2 bumps, got %d: %+v", len(bumps), bumps)
	}

	// Verify CLAUDE_CODE_VERSION bump
	var ccBump, codexBump bump
	for _, b := range bumps {
		switch b.Arg {
		case "CLAUDE_CODE_VERSION":
			ccBump = b
		case "CODEX_VERSION":
			codexBump = b
		}
	}
	if ccBump.From != "2.1.177" || ccBump.To != "2.1.178" {
		t.Errorf("claude-code bump: got From=%q To=%q, want From=2.1.177 To=2.1.178", ccBump.From, ccBump.To)
	}
	if codexBump.From != "0.140.0" || codexBump.To != "0.141.0" {
		t.Errorf("codex bump: got From=%q To=%q, want From=0.140.0 To=0.141.0", codexBump.From, codexBump.To)
	}

	// Rewritten content must have new versions
	if !strings.Contains(out, "ARG CLAUDE_CODE_VERSION=2.1.178") {
		t.Error("rewritten dockerfile missing ARG CLAUDE_CODE_VERSION=2.1.178")
	}
	if !strings.Contains(out, "ARG CODEX_VERSION=0.141.0") {
		t.Error("rewritten dockerfile missing ARG CODEX_VERSION=0.141.0")
	}

	// Unchanged versions must remain
	if !strings.Contains(out, "ARG OPENCODE_VERSION=1.17.11") {
		t.Error("rewritten dockerfile should still have ARG OPENCODE_VERSION=1.17.11")
	}
	if !strings.Contains(out, "ARG GEMINI_CLI_VERSION=0.49.0") {
		t.Error("rewritten dockerfile should still have ARG GEMINI_CLI_VERSION=0.49.0")
	}
}
