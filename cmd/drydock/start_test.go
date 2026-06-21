package main

import "testing"

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
			if got := agentCredentialAvailable(c.anthropicAuth, c.openaiAuth, c.anthropicKey, c.openaiKey); got != c.want {
				t.Errorf("agentCredentialAvailable(%q,%q,%q,%q) = %v, want %v",
					c.anthropicAuth, c.openaiAuth, c.anthropicKey, c.openaiKey, got, c.want)
			}
		})
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
		if got := agentCredentialAvailable(c.anthropicAuth, c.openaiAuth, c.aKey, c.oKey); got != c.want {
			t.Errorf("agentCredentialAvailable(%q,%q,%q,%q)=%v want %v", c.anthropicAuth, c.openaiAuth, c.aKey, c.oKey, got, c.want)
		}
	}
}
