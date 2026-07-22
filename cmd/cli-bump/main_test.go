// Tests for cli-bump: newer, versionRE, and planBumps.
package main

import (
	"strings"
	"testing"
)

func TestVersionRE(t *testing.T) {
	accept := []string{
		"2.1.178",
		"1.0.0-rc1",
		"1.0.0-beta.2",
		"0.49.0",
		"3",
		"1.2",
	}
	reject := []string{
		"2.1.178; rm -rf /",
		"2.1.178`id`",
		"2.1.178 extra",
		"$(evil)",
		"1.0.0_beta",
		"",
		"v1.0.0",
	}
	for _, v := range accept {
		if !versionRE.MatchString(v) {
			t.Errorf("versionRE: expected to accept %q", v)
		}
	}
	for _, v := range reject {
		if versionRE.MatchString(v) {
			t.Errorf("versionRE: expected to reject %q", v)
		}
	}
}

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
	dockerfile := `ARG NPM_VERSION=11.18.0
ARG CLAUDE_CODE_VERSION=2.1.177
ARG CODEX_VERSION=0.140.0
ARG OPENCODE_VERSION=1.17.11
ARG GEMINI_CLI_VERSION=0.49.0
`
	latest := map[string]string{
		"@anthropic-ai/claude-code": "2.1.178",
		"@openai/codex":             "0.141.0",
		"opencode-ai":               "1.17.11", // same version, no bump
		"@google/gemini-cli":        "0.49.0",  // same version, no bump
		"npm":                       "11.19.0",
	}

	out, bumps := planBumps(dockerfile, latest)

	if len(bumps) != 3 {
		t.Fatalf("expected 3 bumps, got %d: %+v", len(bumps), bumps)
	}

	// Verify CLAUDE_CODE_VERSION bump
	var ccBump, codexBump, npmBump bump
	for _, b := range bumps {
		switch b.Arg {
		case "CLAUDE_CODE_VERSION":
			ccBump = b
		case "CODEX_VERSION":
			codexBump = b
		case "NPM_VERSION":
			npmBump = b
		}
	}
	if ccBump.From != "2.1.177" || ccBump.To != "2.1.178" {
		t.Errorf("claude-code bump: got From=%q To=%q, want From=2.1.177 To=2.1.178", ccBump.From, ccBump.To)
	}
	if codexBump.From != "0.140.0" || codexBump.To != "0.141.0" {
		t.Errorf("codex bump: got From=%q To=%q, want From=0.140.0 To=0.141.0", codexBump.From, codexBump.To)
	}
	if npmBump.From != "11.18.0" || npmBump.To != "11.19.0" {
		t.Errorf("npm bump: got From=%q To=%q, want From=11.18.0 To=11.19.0", npmBump.From, npmBump.To)
	}

	// Rewritten content must have new versions
	if !strings.Contains(out, "ARG CLAUDE_CODE_VERSION=2.1.178") {
		t.Error("rewritten dockerfile missing ARG CLAUDE_CODE_VERSION=2.1.178")
	}
	if !strings.Contains(out, "ARG CODEX_VERSION=0.141.0") {
		t.Error("rewritten dockerfile missing ARG CODEX_VERSION=0.141.0")
	}
	if !strings.Contains(out, "ARG NPM_VERSION=11.19.0") {
		t.Error("rewritten dockerfile missing ARG NPM_VERSION=11.19.0")
	}

	// Unchanged versions must remain
	if !strings.Contains(out, "ARG OPENCODE_VERSION=1.17.11") {
		t.Error("rewritten dockerfile should still have ARG OPENCODE_VERSION=1.17.11")
	}
	if !strings.Contains(out, "ARG GEMINI_CLI_VERSION=0.49.0") {
		t.Error("rewritten dockerfile should still have ARG GEMINI_CLI_VERSION=0.49.0")
	}
}

// TestPlanBumps_RejectsMalformedVersion verifies that a candidate version
// containing shell metacharacters is silently dropped: no bump is recorded and
// the Dockerfile pin is left exactly as-is.
func TestPlanBumps_RejectsMalformedVersion(t *testing.T) {
	dockerfile := `ARG CLAUDE_CODE_VERSION=2.1.177
ARG CODEX_VERSION=0.140.0
ARG OPENCODE_VERSION=1.17.11
ARG GEMINI_CLI_VERSION=0.49.0
`
	// Supply a metacharacter-laden "version" that must be rejected entirely.
	poisoned := map[string]string{
		"@anthropic-ai/claude-code": "2.1.178; rm -rf /",
	}

	out, bumps := planBumps(dockerfile, poisoned)

	if len(bumps) != 0 {
		t.Fatalf("expected 0 bumps for malformed version, got %d: %+v", len(bumps), bumps)
	}

	// The original pin must be unchanged.
	if !strings.Contains(out, "ARG CLAUDE_CODE_VERSION=2.1.177") {
		t.Error("malformed version must not rewrite the Dockerfile pin")
	}

	// The poisoned string must never appear in the output.
	if strings.Contains(out, "rm -rf") {
		t.Error("poisoned string must not appear in output Dockerfile")
	}
}
