package provider

import "testing"

func TestRegistry_AgentsAndLabels(t *testing.T) {
	if got := Agents(); len(got) != 3 || got[0] != "claude" || got[1] != "codex" || got[2] != "opencode" {
		t.Errorf("Agents() = %v, want [claude codex opencode]", got)
	}
	labels := Labels()
	if len(labels) != 3 || labels[0] != "Claude Code (Anthropic)" || labels[1] != "OpenAI Codex" || labels[2] != "OpenAI-compatible (bring your own)" {
		t.Errorf("Labels() = %v", labels)
	}
}

func TestRegistry_Lookups(t *testing.T) {
	if p, ok := ByAgent("codex"); !ok || p.Vendor != "openai" {
		t.Errorf("ByAgent(codex) = %+v,%v", p, ok)
	}
	if _, ok := ByAgent("nope"); ok {
		t.Error("ByAgent(nope) should miss")
	}
	if p, ok := ByVendor("anthropic"); !ok || p.Agent != "claude" {
		t.Errorf("ByVendor(anthropic) = %+v,%v", p, ok)
	}
	if _, ok := ByVendor("nope"); ok {
		t.Error("ByVendor(nope) should miss")
	}
}

// Guards a future malformed row: every entry must carry the fields the
// CLI/config layer relies on.
func TestRegistry_EntriesComplete(t *testing.T) {
	for _, p := range Registry {
		if p.Agent == "" || p.Vendor == "" || p.Label == "" || p.BaseURLEnv == "" || p.TokenEnv == "" {
			t.Errorf("incomplete registry entry: %+v", p)
		}
		if !p.ConfigBuilt && (p.APIKeyEnv == "" || p.AuthCmd == "" || p.APIVendor == nil) {
			t.Errorf("static provider missing APIKeyEnv/AuthCmd/APIVendor: %+v", p)
		}
	}
}

func TestRegistry_OpenAICompatRow(t *testing.T) {
	p, ok := ByAgent("opencode")
	if !ok || p.Vendor != "openai-compat" {
		t.Fatalf("ByAgent(opencode) = %+v,%v", p, ok)
	}
	if !p.ConfigBuilt {
		t.Error("opencode row must be ConfigBuilt (brokerd builds it from config)")
	}
	if p.APIVendor != nil || p.OAuthBackend != nil {
		t.Error("config-built provider must have nil APIVendor/OAuthBackend")
	}
	if p.BaseURLEnv != "OPENAI_BASE_URL" || p.TokenEnv != "OPENAI_API_KEY" {
		t.Errorf("opencode env names = %q/%q", p.BaseURLEnv, p.TokenEnv)
	}
}
