package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/config"
)

// ---------------------------------------------------------------------------
// TestSetNestedYAMLKey
// ---------------------------------------------------------------------------

func TestSetNestedYAMLKey(t *testing.T) {
	body := `openai_compat:
  base_url:    ""        # e.g. https://generativelanguage.googleapis.com
  base_path:   ""        # e.g. /v1beta/openai
  model:       ""        # model id
other_key: somevalue
`
	got := setNestedYAMLKey(body, "base_url", "https://example.com")
	if !strings.Contains(got, `base_url:    "https://example.com"`) {
		t.Errorf("expected base_url to be set; got:\n%s", got)
	}
	// other lines unchanged
	if !strings.Contains(got, `base_path:   ""`) {
		t.Errorf("base_path should be unchanged; got:\n%s", got)
	}
	if !strings.Contains(got, "other_key: somevalue") {
		t.Errorf("other_key should be unchanged; got:\n%s", got)
	}
	// trailing comment preserved
	if !strings.Contains(got, "# e.g. https://generativelanguage.googleapis.com") {
		t.Errorf("trailing comment should be preserved; got:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// TestPromptText
// ---------------------------------------------------------------------------

func TestPromptText(t *testing.T) {
	cases := []struct {
		in   string
		dflt string
		want string
	}{
		{"hello\n", "", "hello"},
		{"\n", "fallback", "fallback"}, // empty input → default
		{"  trimmed  \n", "", "trimmed"},
		{"\n", "", ""}, // empty input, no default → empty string
	}
	for _, c := range cases {
		var out strings.Builder
		got := promptText(strings.NewReader(c.in), &out, "Enter something", c.dflt)
		if got != c.want {
			t.Errorf("in=%q dflt=%q → %q, want %q", c.in, c.dflt, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TestRenderConfig_OpenAICompat
// ---------------------------------------------------------------------------

func TestRenderConfig_OpenAICompat(t *testing.T) {
	c := wizardChoices{
		DefaultAgent:  "claude",
		AnthropicAuth: "api_key",
		OpenAIAuth:    "api_key",
		OCBaseURL:     "https://generativelanguage.googleapis.com",
		OCBasePath:    "/v1beta/openai",
		OCModel:       "gemini-2.5-pro",
		OCKeyEnv:      "GEMINI_API_KEY",
	}
	body := renderConfig(c)

	if !strings.Contains(body, `"https://generativelanguage.googleapis.com"`) {
		t.Errorf("rendered config missing base_url value:\n%s", body)
	}
	if !strings.Contains(body, `"gemini-2.5-pro"`) {
		t.Errorf("rendered config missing model value:\n%s", body)
	}
	if !strings.Contains(body, `"GEMINI_API_KEY"`) {
		t.Errorf("rendered config missing api_key_env value:\n%s", body)
	}
	if !strings.Contains(body, `"/v1beta/openai"`) {
		t.Errorf("rendered config missing base_path value:\n%s", body)
	}

	// Round-trip: written config must parse without error.
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("rendered openai_compat config does not parse: %v", err)
	}
	if cfg.OpenAICompat.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Errorf("round-trip base_url = %q", cfg.OpenAICompat.BaseURL)
	}
	if cfg.OpenAICompat.Model != "gemini-2.5-pro" {
		t.Errorf("round-trip model = %q", cfg.OpenAICompat.Model)
	}
	if cfg.OpenAICompat.APIKeyEnv != "GEMINI_API_KEY" {
		t.Errorf("round-trip api_key_env = %q", cfg.OpenAICompat.APIKeyEnv)
	}
}

// ---------------------------------------------------------------------------
// TestRunWizard_OpenAICompat — full flow test
// ---------------------------------------------------------------------------

func TestRunWizard_OpenAICompat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Input: agent=1(Claude), auth=1(subscription), OC=yes,
	//        base URL, base path, model, key env.
	input := "1\n1\ny\nhttps://generativelanguage.googleapis.com\n/v1beta/openai\ngemini-2.5-pro\nGEMINI_API_KEY\n"
	deps := &wizardDeps{
		in:              strings.NewReader(input),
		out:             &strings.Builder{},
		bootstrapClaude: func() error { return nil },
		bootstrapCodex:  func() error { return nil },
		configPath:      filepath.Join(dir, ".drydock", "config.yaml"),
	}
	got := runWizard(deps)
	if got.OCBaseURL != "https://generativelanguage.googleapis.com" {
		t.Errorf("OCBaseURL = %q", got.OCBaseURL)
	}
	if got.OCModel != "gemini-2.5-pro" {
		t.Errorf("OCModel = %q", got.OCModel)
	}
	if got.OCKeyEnv != "GEMINI_API_KEY" {
		t.Errorf("OCKeyEnv = %q", got.OCKeyEnv)
	}

	// Written config must parse and contain OC values.
	cfg, err := config.Load(deps.configPath)
	if err != nil {
		t.Fatalf("config not parseable: %v", err)
	}
	if cfg.OpenAICompat.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Errorf("config base_url = %q", cfg.OpenAICompat.BaseURL)
	}
	if cfg.OpenAICompat.Model != "gemini-2.5-pro" {
		t.Errorf("config model = %q", cfg.OpenAICompat.Model)
	}
	if cfg.OpenAICompat.APIKeyEnv != "GEMINI_API_KEY" {
		t.Errorf("config api_key_env = %q", cfg.OpenAICompat.APIKeyEnv)
	}
}

// TestTTYEchoCmd_UsesTerminalStdin guards the no-echo regression: stty must run
// against the controlling terminal (os.Stdin), not exec's default /dev/null —
// otherwise it errors and a pasted secret echoes on screen.
func TestTTYEchoCmd_UsesTerminalStdin(t *testing.T) {
	for _, on := range []bool{false, true} {
		c := ttyEchoCmd(on)
		if c.Stdin != os.Stdin {
			t.Errorf("ttyEchoCmd(%v): Stdin must be os.Stdin (the tty), got %v", on, c.Stdin)
		}
		want := "-echo"
		if on {
			want = "echo"
		}
		if got := c.Args[len(c.Args)-1]; got != want {
			t.Errorf("ttyEchoCmd(%v): last arg = %q, want %q", on, got, want)
		}
	}
}

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
	keys, _ := config.LoadAPIKeys(config.APIKeysPath())
	if keys["OPENAI_API_KEY"] != "sk-from-env" {
		t.Error("consented API key not persisted to api-keys.env")
	}
}

// TestRunWizard_WriteFailureSurfacesError verifies that when the config file
// cannot be written, the wizard reports an error (not "wrote") so the operator
// knows the config is stale.
func TestRunWizard_WriteFailureSurfacesError(t *testing.T) {
	parent := t.TempDir()
	// Create the sub-directory but make it read-only so WriteFile fails.
	sub := filepath.Join(parent, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) }) // restore for cleanup

	configPath := filepath.Join(sub, "config.yaml")
	var out strings.Builder
	deps := &wizardDeps{
		in:              strings.NewReader("1\n1\n"), // agent: Claude, auth: subscription
		out:             &out,
		bootstrapClaude: func() error { return nil },
		bootstrapCodex:  func() error { return nil },
		configPath:      configPath,
	}
	runWizard(deps)
	got := out.String()
	if strings.Contains(got, "wrote") {
		t.Errorf("output must not say 'wrote' on write failure; got: %s", got)
	}
	if !strings.Contains(got, "error") {
		t.Errorf("output must report an error on write failure; got: %s", got)
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
