package main

import (
	"errors"
	"testing"
)

func TestCodexPresent(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		err    error
		wantOK bool
	}{
		{"healthy version", "codex-cli 0.140.0", nil, true},
		{"healthy with progress preamble", "[6/6] Starting container [0s]\ncodex-cli 0.140.0\n", nil, true},
		{"not found (nonzero exit)", "/bin/sh: 1: codex: not found", errors.New("exit status 127"), false},
		{"not found (zero exit, defensive)", "/bin/sh: 1: codex: not found", nil, false},
		{"container run error", "", errors.New("container run failed"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexPresent(c.out, c.err); got != c.wantOK {
				t.Errorf("codexPresent(%q, %v) = %v, want %v", c.out, c.err, got, c.wantOK)
			}
		})
	}
}

func TestClaudeVersionLine_StripsContainerProgress(t *testing.T) {
	noisy := `[0/6] [0s]
[1/6] Fetching image [0s]
[2/6] Unpacking image [0s]
[3/6] Fetching kernel [0s]
[4/6] Fetching init image [0s]
[5/6] Unpacking init image [0s]
[6/6] Starting container [0s]
[6/6] Starting container [0s]
2.1.177 (Claude Code)
`
	got := claudeVersionLine(noisy)
	want := "2.1.177 (Claude Code)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestClaudeVersionLine_PassesThroughCleanOutput(t *testing.T) {
	got := claudeVersionLine("2.1.177 (Claude Code)\n")
	if got != "2.1.177 (Claude Code)" {
		t.Errorf("got %q", got)
	}
}

func TestClaudeVersionLine_NoMatchFallsBackToTrimmed(t *testing.T) {
	got := claudeVersionLine("  some other output  \n")
	if got != "some other output" {
		t.Errorf("got %q", got)
	}
}

func TestGeminiPresent(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		err    error
		wantOK bool
	}{
		{"healthy version", "0.49.0", nil, true},
		{"healthy with progress preamble", "[6/6] Starting container [0s]\n0.49.0\n", nil, true},
		{"not found (nonzero exit)", "", errors.New("exit status 127"), false},
		{"empty output zero exit", "", nil, false},
		{"container run error", "", errors.New("container run failed"), false},
		{"not found with zero exit (defensive)", "/bin/sh: 1: gemini: not found", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := geminiPresent(c.out, c.err); got != c.wantOK {
				t.Errorf("geminiPresent(%q, %v) = %v, want %v", c.out, c.err, got, c.wantOK)
			}
		})
	}
}

func TestAPIKeySource(t *testing.T) {
	file := map[string]string{"OPENAI_API_KEY": "sk-o"}

	t.Run("env", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
		if got := apiKeySource("ANTHROPIC_API_KEY", file); got != "env" {
			t.Errorf("got %q, want env", got)
		}
	})
	t.Run("file", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "")
		if got := apiKeySource("OPENAI_API_KEY", file); got != "~/.drydock/api-keys.env" {
			t.Errorf("got %q, want file path", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := apiKeySource("ANTHROPIC_API_KEY", file); got != "none" {
			t.Errorf("got %q, want none", got)
		}
	})
}
