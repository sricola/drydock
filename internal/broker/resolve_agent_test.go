package broker

import (
	"strings"
	"testing"

	"drydock/internal/creds"
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
