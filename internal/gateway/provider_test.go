package gateway

import (
	"slices"
	"strings"
	"testing"
	"time"

	"macagent/internal/creds"
)

func TestProvider_GrantCarriesBaseURLAndToken(t *testing.T) {
	g, _ := New("REAL", "http://unused", DefaultPrices())
	var p creds.Provider = &Provider{GW: g, BaseURL: "http://192.168.64.1:8088", TTL: time.Minute}

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
