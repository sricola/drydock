package gateway

import (
	"slices"
	"strings"
	"testing"
	"time"

	"drydock/internal/creds"
)

func TestProvider_GrantCarriesBaseURLAndToken(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("REAL")})
	var p creds.Provider = &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://192.168.64.1:8088",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN", TTL: time.Minute}

	grant, err := p.Mint(2.5)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	env := grant.EnvVars()
	if !slices.Contains(env, "ANTHROPIC_BASE_URL=http://192.168.64.1:8088") {
		t.Errorf("env missing base url: %v", env)
	}
	hasToken := false
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=tok_") {
			hasToken = true
		}
	}
	if !hasToken {
		t.Errorf("env missing auth token: %v", env)
	}
	// Revoke must invalidate the underlying gateway lease.
	if err := grant.Revoke(); err != nil {
		t.Fatal(err)
	}
}

func TestGrantEnvVars(t *testing.T) {
	cases := []struct {
		vendor, baseURLEnv, tokenEnv string
		wantBase, wantTok            string
	}{
		{"anthropic", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL=http://gw", "ANTHROPIC_AUTH_TOKEN=tok_x"},
		{"openai", "OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL=http://gw", "OPENAI_API_KEY=tok_x"},
	}
	for _, tc := range cases {
		g := &grant{token: "tok_x", baseURL: "http://gw", baseURLEnv: tc.baseURLEnv, tokenEnv: tc.tokenEnv}
		env := g.EnvVars()
		if len(env) != 2 || env[0] != tc.wantBase || env[1] != tc.wantTok {
			t.Errorf("%s: EnvVars() = %v", tc.vendor, env)
		}
	}
}

// TestRedteam_A1_OpenAICompatRealKeyNeverInGrantEnv is the host-side A1
// red-team assertion for the openai-compat (opencode) lane: the real upstream
// API key must never appear in the grant's env vars — only the gateway base
// URL and a minted tok_-style lease token may reach the VM. The TestRedteam_A1
// name puts it in the `make redteam` containment report alongside the other
// lanes, so the newest credential path is not dark there.
func TestRedteam_A1_OpenAICompatRealKeyNeverInGrantEnv(t *testing.T) {
	const realKey = "sk-REAL-SENTINEL"
	const gwBaseURL = "http://192.168.64.1:8088"

	vendor := OpenAICompatVendor("openai-compat", "https://openrouter.ai", "/api/v0", nil)
	g, err := New(Backend{Vendor: vendor, Cred: StaticKey(realKey)})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}

	p := &Provider{
		GW:         g,
		Vendor:     "openai-compat",
		BaseURL:    gwBaseURL,
		BaseURLEnv: "OPENAI_BASE_URL",
		TokenEnv:   "OPENAI_API_KEY",
		TTL:        time.Minute,
	}

	grant, err := p.Mint(2.5)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	defer grant.Revoke() //nolint:errcheck

	env := grant.EnvVars()

	// Gateway base URL must be present so the VM can reach the proxy.
	if !slices.Contains(env, "OPENAI_BASE_URL="+gwBaseURL) {
		t.Errorf("grant env missing gateway base URL; got %v", env)
	}

	// A minted lease token (tok_-prefix) must be present.
	hasToken := false
	for _, e := range env {
		if strings.HasPrefix(e, "OPENAI_API_KEY=tok_") {
			hasToken = true
		}
	}
	if !hasToken {
		t.Errorf("grant env missing tok_-style token; got %v", env)
	}

	// The real upstream key must NEVER appear anywhere in the grant env.
	for _, e := range env {
		if strings.Contains(e, realKey) {
			t.Errorf("grant env leaks real upstream key %q: %v", realKey, env)
		}
	}
}

func TestProvider_MintGuardEmptyEnvNames(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("REAL")})

	// Empty BaseURLEnv → error
	p := &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://gw", TokenEnv: "ANTHROPIC_AUTH_TOKEN", TTL: time.Minute}
	if _, err := p.Mint(1); err == nil {
		t.Error("Mint with empty BaseURLEnv should return error")
	}

	// Empty TokenEnv → error
	p2 := &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://gw", BaseURLEnv: "ANTHROPIC_BASE_URL", TTL: time.Minute}
	if _, err := p2.Mint(1); err == nil {
		t.Error("Mint with empty TokenEnv should return error")
	}

	// Both set → success
	p3 := &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://gw",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN", TTL: time.Minute}
	grant, err := p3.Mint(1)
	if err != nil {
		t.Fatalf("Mint with both env names set should succeed: %v", err)
	}
	grant.Revoke()
}
