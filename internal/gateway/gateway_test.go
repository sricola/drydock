package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upstream stands in for api.anthropic.com; it asserts the gateway swapped creds.
func upstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization should be stripped, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Api-Key") != "REAL" {
			t.Errorf("X-Api-Key = %q, want REAL", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-x","usage":{"input_tokens":1000000,"output_tokens":1000000}}`)
	}))
}

func newGW(t *testing.T, up string) *Gateway {
	t.Helper()
	v := Vendor{
		Name:       "anthropic",
		BaseURL:    up,
		Inject:     AnthropicVendor().Inject,
		ParseUsage: parseAnthropicUsage,
		Prices:     map[string]Price{"claude-x": {InputPer1M: 3, OutputPer1M: 15}},
	}
	g, err := New(Backend{Vendor: v, RealKey: "REAL"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func do(g *Gateway, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://gw/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

func TestGateway_ValidTokenProxiesAndMeters(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	tok, _ := g.Mint("anthropic", 100, time.Minute)

	rec := do(g, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	// 1M in @3 + 1M out @15 = 18.0 spent
	if got := g.spent(tok); got < 17.9 || got > 18.1 {
		t.Errorf("spent = %v, want ~18", got)
	}
}

func TestGateway_UnknownToken401(t *testing.T) {
	g := newGW(t, "http://unused")
	if rec := do(g, "nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGateway_ExpiredToken401(t *testing.T) {
	g := newGW(t, "http://unused")
	tok, _ := g.Mint("anthropic", 100, -time.Second) // already expired
	if rec := do(g, tok); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGateway_OverBudget402(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	tok, _ := g.Mint("anthropic", 1.0, time.Minute) // budget 1.0; one call spends ~18 → next call 402
	do(g, tok)
	if rec := do(g, tok); rec.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", rec.Code)
	}
}

func TestGateway_RevokeInvalidates(t *testing.T) {
	g := newGW(t, "http://unused")
	tok, _ := g.Mint("anthropic", 100, time.Minute)
	g.Revoke(tok)
	if rec := do(g, tok); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d after revoke, want 401", rec.Code)
	}
}

func TestGrant_SpentReflectsMeteredCost(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	p := &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://192.168.64.1:8088", TTL: time.Minute, Budget: 100}

	grant, err := p.Mint(100)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	defer grant.Revoke()

	// Before any request, spend should be zero.
	if got := grant.Spent(); got != 0 {
		t.Errorf("Spent before request = %v, want 0", got)
	}

	// Drive a request through the gateway to accumulate spend.
	// The grant's token is in its env vars; extract it.
	var tok string
	for _, e := range grant.EnvVars() {
		if len(e) > len("ANTHROPIC_AUTH_TOKEN=") && e[:len("ANTHROPIC_AUTH_TOKEN=")] == "ANTHROPIC_AUTH_TOKEN=" {
			tok = e[len("ANTHROPIC_AUTH_TOKEN="):]
		}
	}
	if tok == "" {
		t.Fatal("could not extract token from EnvVars")
	}
	rec := do(g, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy request failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 1M in @3 + 1M out @15 = 18.0 spent; Spent() must reflect that.
	got := grant.Spent()
	if got < 17.9 || got > 18.1 {
		t.Errorf("Spent() = %v, want ~18", got)
	}
	// Also verify it matches the underlying gateway lease.
	if lease := g.spent(tok); got != lease {
		t.Errorf("Spent() = %v, gateway.spent() = %v, want equal", got, lease)
	}
}
