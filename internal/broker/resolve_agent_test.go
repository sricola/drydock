package broker

import (
	"strings"
	"testing"

	"drydock/internal/creds"
	"drydock/internal/provider"
)

// fakeProvider satisfies creds.Provider for unit tests that do not exercise
// credential minting itself.
type fakeProvider struct{}

func (fakeProvider) Mint(float64) (creds.Grant, error) { return nil, nil }

func TestResolveAgent(t *testing.T) {
	fake := fakeProvider{}
	cases := []struct {
		name        string
		taskAgent   string
		defaultAg   string
		providers   map[string]creds.Provider
		wantName    string
		wantErr     bool
		wantHasProv bool
		wantMsgSub  string
	}{
		{"empty falls back to claude", "", "", map[string]creds.Provider{"anthropic": fake}, "claude", false, true, ""},
		{"default agent used", "", "codex", map[string]creds.Provider{"openai": fake}, "codex", false, true, ""},
		{"task overrides default", "claude", "codex", map[string]creds.Provider{"anthropic": fake}, "claude", false, true, ""},
		{"unknown agent rejected", "gpt5", "", map[string]creds.Provider{"anthropic": fake}, "gpt5", true, false, "want claude|codex"},
		{"missing provider rejected", "claude", "", map[string]creds.Provider{}, "claude", true, false, "no API key configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &Broker{Providers: c.providers, DefaultAgent: c.defaultAg}
			name, prov, err := b.resolveAgent(c.taskAgent)
			gotErr := err != nil
			if name != c.wantName || gotErr != c.wantErr || (prov != nil) != c.wantHasProv {
				t.Fatalf("got (name=%q err=%v hasProv=%v)", name, err, prov != nil)
			}
			if c.wantMsgSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", c.wantMsgSub)
				}
				if !strings.Contains(err.Error(), c.wantMsgSub) {
					t.Fatalf("err %q missing %q", err.Error(), c.wantMsgSub)
				}
			}
		})
	}
}

// TestResolveAgent_UnknownNameAllProviders asserts that the unknown-agent error
// message names every agent in the registry. This test drives the requirement
// that the error string is derived from provider.Agents() rather than a
// hardcoded subset.
func TestResolveAgent_UnknownNameAllProviders(t *testing.T) {
	fake := fakeProvider{}
	b := &Broker{
		Providers:    map[string]creds.Provider{"anthropic": fake},
		DefaultAgent: "",
	}
	_, _, err := b.resolveAgent("gpt5")
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	for _, a := range provider.Agents() {
		if !strings.Contains(err.Error(), a) {
			t.Errorf("unknown-agent error %q missing registered agent %q", err.Error(), a)
		}
	}
}
