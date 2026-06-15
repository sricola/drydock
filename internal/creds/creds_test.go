package creds

import (
	"slices"
	"testing"
)

func TestStaticProvider_GrantEnvAndRevoke(t *testing.T) {
	var p Provider = StaticProvider{Key: "sk-static"}
	g, err := p.Mint(5.0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !slices.Contains(g.EnvVars(), "ANTHROPIC_API_KEY=sk-static") {
		t.Errorf("EnvVars = %v", g.EnvVars())
	}
	if err := g.Revoke(); err != nil {
		t.Errorf("Revoke: %v", err)
	}
}

func TestStaticProvider_EmptyKeyErrors(t *testing.T) {
	if _, err := (StaticProvider{Key: ""}).Mint(1.0); err == nil {
		t.Errorf("want error for empty key")
	}
}
