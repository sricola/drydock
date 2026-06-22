package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/config"
)

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
