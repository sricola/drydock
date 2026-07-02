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
		wantStatus  int
		wantHasProv bool
		wantMsgSub  string
	}{
		{"empty falls back to claude", "", "", map[string]creds.Provider{"anthropic": fake}, "claude", 0, true, ""},
		{"default agent used", "", "codex", map[string]creds.Provider{"openai": fake}, "codex", 0, true, ""},
		{"task overrides default", "claude", "codex", map[string]creds.Provider{"anthropic": fake}, "claude", 0, true, ""},
		{"unknown agent rejected", "gpt5", "", map[string]creds.Provider{"anthropic": fake}, "gpt5", 400, false, "want claude|codex"},
		{"missing provider rejected", "claude", "", map[string]creds.Provider{}, "claude", 400, false, "no API key configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &Broker{Providers: c.providers, DefaultAgent: c.defaultAg}
			name, prov, status, msg := b.resolveAgent(c.taskAgent)
			if name != c.wantName || status != c.wantStatus || (prov != nil) != c.wantHasProv {
				t.Fatalf("got (name=%q status=%d hasProv=%v msg=%q)", name, status, prov != nil, msg)
			}
			if c.wantMsgSub != "" && !strings.Contains(msg, c.wantMsgSub) {
				t.Fatalf("msg %q missing %q", msg, c.wantMsgSub)
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
	_, _, status, msg := b.resolveAgent("gpt5")
	if status != 400 {
		t.Fatalf("want status 400, got %d", status)
	}
	for _, a := range provider.Agents() {
		if !strings.Contains(msg, a) {
			t.Errorf("unknown-agent error %q missing registered agent %q", msg, a)
		}
	}
}
