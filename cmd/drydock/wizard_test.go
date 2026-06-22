package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/config"
)

func TestRunWizard_ClaudeSubscription(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // APIKeysPath/config.Dir resolve under here
	var bootstrapped bool
	deps := &wizardDeps{
		in:              strings.NewReader("1\n1\n"), // agent: Claude, auth: subscription
		out:             &strings.Builder{},
		bootstrapClaude: func() error { bootstrapped = true; return nil },
		bootstrapCodex:  func() error { return nil },
		configPath:      filepath.Join(dir, ".drydock", "config.yaml"),
	}
	got := runWizard(deps)
	if got.DefaultAgent != "claude" || got.AnthropicAuth != "subscription" {
		t.Fatalf("choices = %+v", got)
	}
	if !bootstrapped {
		t.Error("subscription path should call the claude bootstrap core")
	}
	if _, err := config.Load(deps.configPath); err != nil {
		t.Errorf("config not written/parseable: %v", err)
	}
}

func TestRunWizard_CodexApiKeyConsentStores(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("OPENAI_API_KEY", "sk-from-env")
	deps := &wizardDeps{
		in:              strings.NewReader("2\n2\ny\n"), // agent: Codex, auth: API key, persist? yes
		out:             &strings.Builder{},
		bootstrapClaude: func() error { return nil },
		bootstrapCodex:  func() error { return nil },
		configPath:      filepath.Join(dir, ".drydock", "config.yaml"),
	}
	got := runWizard(deps)
	if got.DefaultAgent != "codex" || got.OpenAIAuth != "api_key" {
		t.Fatalf("choices = %+v", got)
	}
	// Consented persist of an already-exported env key → stored, no re-prompt.
	if config.LoadAPIKeys(config.APIKeysPath())["OPENAI_API_KEY"] != "sk-from-env" {
		t.Error("consented API key not persisted to api-keys.env")
	}
}

func TestPromptChoice(t *testing.T) {
	opts := []string{"Claude Code", "OpenAI Codex", "both"}
	cases := []struct {
		in   string
		want int
	}{
		{"2\n", 2},       // explicit
		{"\n", 1},        // empty → default
		{"x\n5\n3\n", 3}, // invalid, out-of-range, then valid
	}
	for _, c := range cases {
		var out strings.Builder
		if got := promptChoice(strings.NewReader(c.in), &out, "Which agent?", opts, 1); got != c.want {
			t.Errorf("in=%q → %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPromptYesNo(t *testing.T) {
	cases := []struct {
		in   string
		dflt bool
		want bool
	}{
		{"y\n", false, true},
		{"n\n", true, false},
		{"\n", true, true}, // empty → default
		{"\n", false, false},
	}
	for _, c := range cases {
		var out strings.Builder
		if got := promptYesNo(strings.NewReader(c.in), &out, "ok?", c.dflt); got != c.want {
			t.Errorf("in=%q dflt=%v → %v, want %v", c.in, c.dflt, got, c.want)
		}
	}
}

func TestRenderConfig_SetsChosenKeys(t *testing.T) {
	cases := []wizardChoices{
		{DefaultAgent: "claude", AnthropicAuth: "subscription", OpenAIAuth: "api_key"},
		{DefaultAgent: "codex", AnthropicAuth: "api_key", OpenAIAuth: "subscription"},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(renderConfig(c)), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatalf("rendered config does not parse: %v", err)
		}
		if cfg.DefaultAgent != c.DefaultAgent || cfg.AnthropicAuth != c.AnthropicAuth || cfg.OpenAIAuth != c.OpenAIAuth {
			t.Errorf("choices=%+v → cfg default=%q anthropic=%q openai=%q", c, cfg.DefaultAgent, cfg.AnthropicAuth, cfg.OpenAIAuth)
		}
	}
}
