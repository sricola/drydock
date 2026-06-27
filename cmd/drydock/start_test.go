package main

import (
	"os"
	"testing"

	"drydock/internal/config"
)

// makeTestCfg returns a config with the given per-vendor auth modes and no
// other magic; env vars set by the caller override the key checks.
func makeTestCfg(anthropicAuth, openaiAuth string) *config.Config {
	c := config.Defaults()
	c.AnthropicAuth = anthropicAuth
	c.OpenAIAuth = openaiAuth
	return c
}

func TestAgentCredentialAvailable(t *testing.T) {
	cases := []struct {
		name          string
		anthropicAuth string
		openaiAuth    string
		anthropicKey  string
		openaiKey     string
		want          bool
	}{
		{"api_key, no keys", "api_key", "api_key", "", "", false},
		{"api_key + anthropic key", "api_key", "api_key", "sk-ant", "", true},
		{"api_key + openai key", "api_key", "api_key", "", "sk-oai", true},
		{"subscription, no keys", "subscription", "api_key", "", "", true}, // the gap the e2e caught
		{"subscription + openai key", "subscription", "api_key", "", "sk-oai", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.anthropicKey != "" {
				t.Setenv("ANTHROPIC_API_KEY", c.anthropicKey)
			} else {
				os.Unsetenv("ANTHROPIC_API_KEY")
			}
			if c.openaiKey != "" {
				t.Setenv("OPENAI_API_KEY", c.openaiKey)
			} else {
				os.Unsetenv("OPENAI_API_KEY")
			}
			cfg := makeTestCfg(c.anthropicAuth, c.openaiAuth)
			if got := agentCredentialAvailable(cfg); got != c.want {
				t.Errorf("agentCredentialAvailable(%q,%q,ANTHROPIC_API_KEY=%q,OPENAI_API_KEY=%q) = %v, want %v",
					c.anthropicAuth, c.openaiAuth, c.anthropicKey, c.openaiKey, got, c.want)
			}
		})
	}
}

func TestAgentCredentialAvailable_OpenAICompat(t *testing.T) {
	// Only openai_compat configured (BaseURL set + api key env var present)
	// should be sufficient for agentCredentialAvailable to return true.
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("OPENAI_API_KEY")
	t.Setenv("MY_COMPAT_KEY", "sk-compat")

	cfg := config.Defaults()
	cfg.OpenAICompat.BaseURL = "https://openrouter.ai"
	cfg.OpenAICompat.APIKeyEnv = "MY_COMPAT_KEY"
	cfg.OpenAICompat.Model = "some-model"

	if got := agentCredentialAvailable(cfg); !got {
		t.Error("agentCredentialAvailable with openai_compat BaseURL+APIKeyEnv set should return true")
	}
}

func TestAgentCredentialAvailable_OpenAICompatNoKey(t *testing.T) {
	// openai_compat BaseURL set but api key env var absent → false (no standard creds either).
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("MY_COMPAT_KEY")

	cfg := config.Defaults()
	cfg.OpenAICompat.BaseURL = "https://openrouter.ai"
	cfg.OpenAICompat.APIKeyEnv = "MY_COMPAT_KEY"
	cfg.OpenAICompat.Model = "some-model"

	if got := agentCredentialAvailable(cfg); got {
		t.Error("agentCredentialAvailable with openai_compat BaseURL but missing key should return false")
	}
}

func TestAgentCredentialAvailable_Codex(t *testing.T) {
	cases := []struct {
		anthropicAuth, openaiAuth, aKey, oKey string
		want                                  bool
	}{
		{"api_key", "subscription", "", "", true}, // codex subscription alone is enough
		{"api_key", "api_key", "", "", false},     // nothing configured
		{"subscription", "api_key", "", "", true}, // claude subscription alone
		{"api_key", "api_key", "", "sk-o", true},  // openai key
	}
	for _, c := range cases {
		if c.aKey != "" {
			t.Setenv("ANTHROPIC_API_KEY", c.aKey)
		} else {
			os.Unsetenv("ANTHROPIC_API_KEY")
		}
		if c.oKey != "" {
			t.Setenv("OPENAI_API_KEY", c.oKey)
		} else {
			os.Unsetenv("OPENAI_API_KEY")
		}
		cfg := makeTestCfg(c.anthropicAuth, c.openaiAuth)
		if got := agentCredentialAvailable(cfg); got != c.want {
			t.Errorf("agentCredentialAvailable(%q,%q,ANTHROPIC=%q,OPENAI=%q)=%v want %v",
				c.anthropicAuth, c.openaiAuth, c.aKey, c.oKey, got, c.want)
		}
	}
}
