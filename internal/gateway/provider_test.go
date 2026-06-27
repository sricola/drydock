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
