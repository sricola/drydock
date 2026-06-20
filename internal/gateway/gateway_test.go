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
	g, err := New(Backend{Vendor: v, Cred: StaticKey("REAL")})
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

// The usageReader must meter a streaming SSE response correctly WITHOUT
// buffering the whole body: input from message_start, output from message_delta,
// while the hundreds of usage-less content_block_delta events (the bulk, often
// split across read boundaries) are discarded as they flow.
func TestGateway_MetersStreamingWithoutBufferingBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\n")
		io.WriteString(w, `data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":1000000,"output_tokens":0}}}`+"\n\n")
		for i := 0; i < 500; i++ { // ~45KB of usage-less events → spans read chunks
			io.WriteString(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"streamed output chunk"}}`+"\n\n")
		}
		io.WriteString(w, `data: {"type":"message_delta","usage":{"output_tokens":1000000}}`+"\n\n")
	}))
	defer up.Close()

	g := newGW(t, up.URL)
	tok, _ := g.Mint("anthropic", 100, time.Minute)
	if rec := do(g, tok); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 1M in @3 + 1M out @15 = 18.0
	if s := g.spent(tok); s < 17.9 || s > 18.1 {
		t.Errorf("spent=%v, want ~18 (metered input from message_start + output from message_delta across a chunked stream)", s)
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

// TestGateway_MultiVendorRouting drives one gateway holding BOTH an anthropic
// and an openai backend, and asserts each minted token routes to the right
// upstream with the right credential injection (X-Api-Key vs Authorization:
// Bearer) — the property the vendor registry exists to guarantee.
func TestGateway_MultiVendorRouting(t *testing.T) {
	var anthropicHit, openaiHit bool
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHit = true
		if r.Header.Get("X-Api-Key") != "REAL_ANTHROPIC" {
			t.Errorf("anthropic upstream: X-Api-Key=%q, want REAL_ANTHROPIC", r.Header.Get("X-Api-Key"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("anthropic upstream: Authorization not stripped: %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"model":"claude-x","usage":{"input_tokens":10,"output_tokens":10}}`)
	}))
	defer anthropicUp.Close()
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHit = true
		if r.Header.Get("Authorization") != "Bearer REAL_OPENAI" {
			t.Errorf("openai upstream: Authorization=%q, want Bearer REAL_OPENAI", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Api-Key") != "" {
			t.Errorf("openai upstream: X-Api-Key not stripped: %q", r.Header.Get("X-Api-Key"))
		}
		io.WriteString(w, `{"model":"gpt-5-codex","usage":{"input_tokens":10,"output_tokens":10}}`)
	}))
	defer openaiUp.Close()

	av := Vendor{Name: "anthropic", BaseURL: anthropicUp.URL, Inject: AnthropicVendor().Inject, ParseUsage: parseAnthropicUsage, Prices: AnthropicPrices()}
	ov := Vendor{Name: "openai", BaseURL: openaiUp.URL, Inject: OpenAIVendor().Inject, ParseUsage: parseOpenAIUsage, Prices: OpenAIPrices()}
	g, err := New(Backend{Vendor: av, Cred: StaticKey("REAL_ANTHROPIC")}, Backend{Vendor: ov, Cred: StaticKey("REAL_OPENAI")})
	if err != nil {
		t.Fatal(err)
	}

	atok, _ := g.Mint("anthropic", 100, time.Minute)
	otok, _ := g.Mint("openai", 100, time.Minute)

	if rec := do(g, atok); rec.Code != http.StatusOK {
		t.Fatalf("anthropic token: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(g, otok); rec.Code != http.StatusOK {
		t.Fatalf("openai token: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !anthropicHit || !openaiHit {
		t.Errorf("upstreams hit: anthropic=%v openai=%v (each token must reach its own vendor)", anthropicHit, openaiHit)
	}
	if g.spent(atok) <= 0 || g.spent(otok) <= 0 {
		t.Errorf("per-vendor metering: anthropic spent=%v openai spent=%v", g.spent(atok), g.spent(otok))
	}
}

func TestNew_RejectsNilCred(t *testing.T) {
	if _, err := New(Backend{Vendor: AnthropicVendor()}); err == nil {
		t.Fatal("New with nil Cred should error")
	}
}

func TestGateway_MintUnknownVendorErrors(t *testing.T) {
	g := newGW(t, "http://unused")
	if _, err := g.Mint("nope", 100, time.Minute); err == nil {
		t.Error("Mint for a vendor with no backend should error")
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
