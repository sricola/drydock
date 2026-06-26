package provider

import "testing"

func TestRegistry_AgentsAndLabels(t *testing.T) {
	if got := Agents(); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Errorf("Agents() = %v, want [claude codex]", got)
	}
	labels := Labels()
	if len(labels) != 2 || labels[0] != "Claude Code (Anthropic)" || labels[1] != "OpenAI Codex" {
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
		if p.Agent == "" || p.Vendor == "" || p.Label == "" || p.APIKeyEnv == "" ||
			p.AuthCmd == "" || p.BaseURLEnv == "" || p.TokenEnv == "" || p.APIVendor == nil {
			t.Errorf("incomplete registry entry: %+v", p)
		}
	}
}
