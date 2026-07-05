package provider

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistry_Agents(t *testing.T) {
	if got := Agents(); len(got) != 4 || got[0] != "claude" || got[1] != "codex" || got[2] != "gemini" || got[3] != "opencode" {
		t.Errorf("Agents() = %v, want [claude codex gemini opencode]", got)
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
		if !p.ConfigBuilt && (p.APIKeyEnv == "" || p.APIVendor == nil) {
			t.Errorf("static provider missing APIKeyEnv/APIVendor: %+v", p)
		}
		// AuthCmd is required only for providers that support subscription mode
		// (OAuthBackend != nil); api-key-only providers (e.g. gemini) may omit it.
		if !p.ConfigBuilt && p.OAuthBackend != nil && p.AuthCmd == "" {
			t.Errorf("provider with subscription mode missing AuthCmd (remediation hint): %+v", p)
		}
	}
}

// TestOAuthProviders_FilenameConsistency pins that OAuthFile, OAuthBackend, and
// LoadOAuthSnap all use the same credential filename. It writes a valid
// credential file at filepath.Join(dir, p.OAuthFile) and verifies that
// LoadOAuthSnap reads from exactly that path.
//
// Both FileCredStore (claude) and CodexStore (codex) use the same on-disk JSON
// shape: {"access_token":..., "refresh_token":..., "expiry":...}. Codex also
// has "account_id" but that field is optional for Load().
func TestOAuthProviders_FilenameConsistency(t *testing.T) {
	// JSON written by gateway.FileCredStore.Save / CodexStore.Put.
	validCredJSON := `{"access_token":"test-tok","refresh_token":"test-ref","expiry":"2999-01-01T00:00:00Z"}`

	for _, p := range Registry {
		if p.OAuthBackend == nil {
			continue // ConfigBuilt providers have no OAuth file
		}
		if p.OAuthFile == "" {
			t.Errorf("%s: has OAuthBackend but empty OAuthFile", p.Agent)
		}
		if p.LoadOAuthSnap == nil {
			t.Errorf("%s: has OAuthBackend but nil LoadOAuthSnap", p.Agent)
		}
		if p.AuthLabel == "" {
			t.Errorf("%s: has OAuthBackend but empty AuthLabel", p.Agent)
		}

		// Verify that LoadOAuthSnap reads from p.OAuthFile by placing a valid
		// credential file at exactly that path inside a temp dir.
		dir := t.TempDir()
		credPath := filepath.Join(dir, p.OAuthFile)
		if err := os.WriteFile(credPath, []byte(validCredJSON), 0o600); err != nil {
			t.Fatalf("%s: write cred file: %v", p.Agent, err)
		}

		snap, err := p.LoadOAuthSnap(dir)
		if err != nil {
			t.Errorf("%s: LoadOAuthSnap could not read from OAuthFile path %q: %v", p.Agent, p.OAuthFile, err)
			continue
		}
		if snap.Access != "test-tok" {
			t.Errorf("%s: LoadOAuthSnap returned access=%q, want %q", p.Agent, snap.Access, "test-tok")
		}
	}
}

// TestOAuthFile_BackwardCompat pins the on-disk credential filenames to their
// exact historical values. These names are a compatibility contract: existing
// installs have ~/.drydock/<name> and brokerd/doctor resolve the same path, so
// a rename would silently orphan users' stored credentials. The consistency
// test above only proves write==read within one run; this pins the literal.
func TestOAuthFile_BackwardCompat(t *testing.T) {
	want := map[string]string{
		"claude": "claude-oauth.json",
		"codex":  "codex-oauth.json",
	}
	for agent, name := range want {
		p, ok := ByAgent(agent)
		if !ok {
			t.Fatalf("ByAgent(%q) not found", agent)
		}
		if p.OAuthFile != name {
			t.Errorf("%s: OAuthFile = %q, want %q (renaming breaks existing credential files)", agent, p.OAuthFile, name)
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
	if !p.NeedsModel {
		t.Error("opencode row must have NeedsModel=true (no built-in model)")
	}
	if !p.NoOperatorDefault {
		t.Error("opencode row must have NoOperatorDefault=true (operator default is claude/codex-oriented)")
	}
	if p.APIVendor != nil || p.OAuthBackend != nil {
		t.Error("config-built provider must have nil APIVendor/OAuthBackend")
	}
	if p.BaseURLEnv != "OPENAI_BASE_URL" || p.TokenEnv != "OPENAI_API_KEY" {
		t.Errorf("opencode env names = %q/%q", p.BaseURLEnv, p.TokenEnv)
	}
}

func TestVendorForAgent(t *testing.T) {
	cases := []struct {
		name   string
		vendor string
		ok     bool
	}{
		{"", "anthropic", true},       // empty defaults to claude
		{"claude", "anthropic", true}, // explicit claude
		{"codex", "openai", true},     // codex maps to openai
		{"opencode", "openai-compat", true},
		{"bogus", "", false},
	}
	for _, c := range cases {
		got, ok := VendorForAgent(c.name)
		if got != c.vendor || ok != c.ok {
			t.Errorf("VendorForAgent(%q) = (%q,%v), want (%q,%v)", c.name, got, ok, c.vendor, c.ok)
		}
	}
}

func TestGatewayHosts(t *testing.T) {
	hosts := GatewayHosts()
	// The static registry must produce exactly the gateway-fronted API hosts.
	want := map[string]bool{
		"api.anthropic.com":                 true,
		"api.openai.com":                    true,
		"generativelanguage.googleapis.com": true,
	}
	if len(hosts) != len(want) {
		t.Fatalf("GatewayHosts() len=%d, want %d: got %v", len(hosts), len(want), hosts)
	}
	for h := range want {
		if !hosts[h] {
			t.Errorf("GatewayHosts() missing %q", h)
		}
	}
	// Only providers with a static APIVendor contribute a host; config-built
	// providers (nil APIVendor, e.g. openai-compat) must contribute nothing, so
	// the host count must equal the number of static-vendor providers.
	staticVendors := 0
	for _, p := range Registry {
		if p.APIVendor != nil {
			staticVendors++
		}
	}
	if len(hosts) != staticVendors {
		t.Errorf("GatewayHosts() len=%d, want %d (one per static-APIVendor provider)", len(hosts), staticVendors)
	}
}

func TestRegistry_GeminiRow(t *testing.T) {
	p, ok := ByAgent("gemini")
	if !ok {
		t.Fatal("gemini agent not registered")
	}
	if p.Vendor != "google" || p.APIKeyEnv != "GEMINI_API_KEY" ||
		p.BaseURLEnv != "GOOGLE_GEMINI_BASE_URL" || p.TokenEnv != "GEMINI_API_KEY" {
		t.Errorf("gemini row fields wrong: %+v", p)
	}
	if p.APIVendor == nil {
		t.Error("gemini must have a static APIVendor (native, not config-built)")
	}
	if p.ConfigBuilt || p.OAuthBackend != nil {
		t.Error("gemini is api-key-only native: ConfigBuilt=false, OAuthBackend=nil")
	}
	if !p.NoOperatorDefault {
		t.Error("gemini must set NoOperatorDefault (operator default_model is claude/codex-oriented)")
	}
	if v := p.APIVendor(); v.Name != "google" {
		t.Errorf("APIVendor().Name = %q, want google", v.Name)
	}
}
