package main

import (
	"strings"
	"testing"

	"drydock/internal/config"
	"drydock/internal/gateway"
)

// T1: openai_compat fully configured — backend built, vendor + cred correct.
func TestBuildBackends_OpenAICompat(t *testing.T) {
	cfg := config.Defaults()
	cfg.OpenAICompat.BaseURL = "https://up.test"
	cfg.OpenAICompat.BasePath = "/v1beta/openai"
	cfg.OpenAICompat.APIKeyEnv = "TEST_OC_KEY"
	cfg.OpenAICompat.Model = "m"

	fileKeys := map[string]string{"TEST_OC_KEY": "sk-real"}

	// Clear real env vars so the test doesn't accidentally pick up a live key
	// and build additional backends alongside the compat one.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	backends, err := buildBackends(cfg, fileKeys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(backends) != 1 {
		t.Fatalf("want 1 backend, got %d", len(backends))
	}
	b := backends[0]
	if b.Vendor.Name != "openai-compat" {
		t.Errorf("Vendor.Name = %q, want %q", b.Vendor.Name, "openai-compat")
	}
	if b.Vendor.BaseURL != "https://up.test" {
		t.Errorf("Vendor.BaseURL = %q, want %q", b.Vendor.BaseURL, "https://up.test")
	}
	if b.Vendor.BasePath != "/v1beta/openai" {
		t.Errorf("Vendor.BasePath = %q, want %q", b.Vendor.BasePath, "/v1beta/openai")
	}
	key, err := b.Cred.Current()
	if err != nil {
		t.Fatalf("Cred.Current() error: %v", err)
	}
	if key != "sk-real" {
		t.Errorf("Cred.Current() = %q, want %q", key, "sk-real")
	}
}

// T1b: prices flow through to the vendor's Prices map.
func TestBuildBackends_OpenAICompat_Prices(t *testing.T) {
	cfg := config.Defaults()
	cfg.OpenAICompat.BaseURL = "https://up.test"
	cfg.OpenAICompat.BasePath = "/v1beta/openai"
	cfg.OpenAICompat.APIKeyEnv = "TEST_OC_KEY"
	cfg.OpenAICompat.Model = "m"
	cfg.OpenAICompat.Prices = map[string]struct {
		Input  float64 `yaml:"input"`
		Output float64 `yaml:"output"`
	}{
		"m": {Input: 1.25, Output: 5.0},
	}

	fileKeys := map[string]string{"TEST_OC_KEY": "sk-real"}
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	backends, err := buildBackends(cfg, fileKeys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(backends) != 1 {
		t.Fatalf("want 1 backend, got %d", len(backends))
	}
	p, ok := backends[0].Vendor.Prices["m"]
	if !ok {
		t.Fatal("Prices[\"m\"] not set")
	}
	if p.InputPer1M != 1.25 {
		t.Errorf("InputPer1M = %v, want 1.25", p.InputPer1M)
	}
	if p.OutputPer1M != 5.0 {
		t.Errorf("OutputPer1M = %v, want 5.0", p.OutputPer1M)
	}
}

// T2: openai_compat base_url set but key missing → error mentioning the env var name.
func TestBuildBackends_OpenAICompat_MissingKey(t *testing.T) {
	cfg := config.Defaults()
	cfg.OpenAICompat.BaseURL = "https://up.test"
	cfg.OpenAICompat.APIKeyEnv = "TEST_OC_KEY"
	cfg.OpenAICompat.Model = "m"

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("TEST_OC_KEY", "") // ensure env doesn't supply the key either

	_, err := buildBackends(cfg, map[string]string{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "TEST_OC_KEY") {
		t.Errorf("error %q should mention TEST_OC_KEY", err.Error())
	}
}

// T3: anthropic API key present → at least one backend with anthropic vendor.
func TestBuildBackends_AnthropicAPIKey(t *testing.T) {
	cfg := config.Defaults()
	fileKeys := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	backends, err := buildBackends(cfg, fileKeys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, b := range backends {
		if strings.Contains(b.Vendor.Name, "anthropic") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no anthropic backend found among %d backends", len(backends))
	}
}

// T4: nothing configured → error containing "set at least one provider".
func TestBuildBackends_Empty(t *testing.T) {
	cfg := config.Defaults()

	// Clear all known API key env vars so the test environment doesn't
	// accidentally supply a live key.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	_, err := buildBackends(cfg, map[string]string{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "set at least one provider") {
		t.Errorf("error %q should contain 'set at least one provider'", err.Error())
	}
}

// Compile-time check: StaticKey implements gateway.Credential.
var _ gateway.Credential = gateway.StaticKey("")

// ocPrice is a convenience alias for the anonymous price struct used in
// config.OpenAICompat.Prices so test literals stay readable.
type ocPrice = struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

func TestOpenAICompatWarnings(t *testing.T) {
	mk := func(base, basePath string, prices map[string]ocPrice) config.Config {
		var c config.Config
		c.OpenAICompat.BaseURL = base
		c.OpenAICompat.BasePath = basePath
		c.OpenAICompat.APIKeyEnv = "OC_KEY"
		c.OpenAICompat.Model = "m"
		c.OpenAICompat.Prices = prices
		return c
	}
	price := func(in, out float64) ocPrice {
		return ocPrice{in, out}
	}

	t.Run("disabled lane yields no warnings", func(t *testing.T) {
		var c config.Config // BaseURL == ""
		if w := openAICompatWarnings(c.OpenAICompat); len(w) != 0 {
			t.Errorf("want none, got %v", w)
		}
	})
	t.Run("clean config yields no warnings", func(t *testing.T) {
		c := mk("https://up.test", "/api/v1", map[string]ocPrice{"default": price(1, 2)})
		if w := openAICompatWarnings(c.OpenAICompat); len(w) != 0 {
			t.Errorf("want none, got %v", w)
		}
	})
	t.Run("negative price warns", func(t *testing.T) {
		c := mk("https://up.test", "", map[string]ocPrice{"gpt-x": price(-1, 2), "default": price(1, 2)})
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "gpt-x") || !strings.Contains(w, "negative") {
			t.Errorf("expected negative-price warning naming gpt-x; got %q", w)
		}
	})
	t.Run("partial prices without default warns", func(t *testing.T) {
		c := mk("https://up.test", "", map[string]ocPrice{"gpt-x": price(1, 2)})
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "default") {
			t.Errorf("expected no-default warning; got %q", w)
		}
	})
	t.Run("base_url with path warns", func(t *testing.T) {
		c := mk("https://openrouter.ai/api/v1", "", nil)
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "/api/v1") || !strings.Contains(w, "base_path") {
			t.Errorf("expected base_url-path warning; got %q", w)
		}
	})
	t.Run("base_path without leading slash warns", func(t *testing.T) {
		c := mk("https://up.test", "v1", nil)
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "base_path") || !strings.Contains(w, "/") {
			t.Errorf("expected base_path slash warning; got %q", w)
		}
	})
}
