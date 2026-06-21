package main

import "testing"

func TestAgentCredentialAvailable(t *testing.T) {
	cases := []struct {
		name          string
		anthropicAuth string
		anthropicKey  string
		openaiKey     string
		want          bool
	}{
		{"api_key, no keys", "api_key", "", "", false},
		{"api_key + anthropic key", "api_key", "sk-ant", "", true},
		{"api_key + openai key", "api_key", "", "sk-oai", true},
		{"subscription, no keys", "subscription", "", "", true}, // the gap the e2e caught
		{"subscription + openai key", "subscription", "", "sk-oai", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentCredentialAvailable(c.anthropicAuth, c.anthropicKey, c.openaiKey); got != c.want {
				t.Errorf("agentCredentialAvailable(%q,%q,%q) = %v, want %v",
					c.anthropicAuth, c.anthropicKey, c.openaiKey, got, c.want)
			}
		})
	}
}
