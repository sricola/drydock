package gateway

import (
	"context"
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
	tok, _ := g.Mint("anthropic", 100, 0, time.Minute)

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
	tok, _ := g.Mint("anthropic", 100, 0, time.Minute)
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
	tok, _ := g.Mint("anthropic", 100, 0, -time.Second) // already expired
	if rec := do(g, tok); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGateway_OverBudget402(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	tok, _ := g.Mint("anthropic", 1.0, 0, time.Minute) // budget 1.0; one call spends ~18 → next call 402
	do(g, tok)
	if rec := do(g, tok); rec.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", rec.Code)
	}
}

func TestGateway_RevokeInvalidates(t *testing.T) {
	g := newGW(t, "http://unused")
	tok, _ := g.Mint("anthropic", 100, 0, time.Minute)
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

	atok, _ := g.Mint("anthropic", 100, 0, time.Minute)
	otok, _ := g.Mint("openai", 100, 0, time.Minute)

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

func TestRequestCap_RejectsOverLimit(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	tok, _ := g.Mint("anthropic", 100, 2, time.Hour) // maxRequests = 2
	if _, s := g.check(tok); s != 0 {
		t.Fatalf("req1 rejected: %d", s)
	}
	if _, s := g.check(tok); s != 0 {
		t.Fatalf("req2 rejected: %d", s)
	}
	if _, s := g.check(tok); s != http.StatusTooManyRequests {
		t.Fatalf("req3 status = %d, want 429", s)
	}
}

func TestRequestCap_ZeroMeansUnlimited(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	tok, _ := g.Mint("anthropic", 100, 0, time.Hour) // 0 = unlimited
	for i := 0; i < 50; i++ {
		if _, s := g.check(tok); s != 0 {
			t.Fatalf("req %d rejected: %d", i, s)
		}
	}
}

func TestNew_RejectsNilCred(t *testing.T) {
	if _, err := New(Backend{Vendor: AnthropicVendor()}); err == nil {
		t.Fatal("New with nil Cred should error")
	}
}

func TestGateway_MintUnknownVendorErrors(t *testing.T) {
	g := newGW(t, "http://unused")
	if _, err := g.Mint("nope", 100, 0, time.Minute); err == nil {
		t.Error("Mint for a vendor with no backend should error")
	}
}

func TestGrant_SpentReflectsMeteredCost(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	p := &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://192.168.64.1:8088",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN", TTL: time.Minute, Budget: 100}

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

func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"/backend-api/codex", "/responses", "/backend-api/codex/responses"},
		{"/backend-api/codex", "responses", "/backend-api/codex/responses"},
		{"/backend-api/codex/", "/responses", "/backend-api/codex/responses"},
		{"/backend-api/codex/", "responses", "/backend-api/codex/responses"}, // a-trailing, b-no-leading
		{"", "/v1/messages", "/v1/messages"},                                 // non-codex vendors unaffected
	}
	for _, c := range cases {
		if got := singleJoiningSlash(c.a, c.b); got != c.want {
			t.Errorf("singleJoiningSlash(%q,%q)=%q want %q", c.a, c.b, got, c.want)
		}
	}
}

// The Codex BasePath remap rewrites req.URL.Path; the query string must survive
// untouched (the startup `GET /models?client_version=…` probe carries one).
func TestDirector_CodexRemapPreservesQuery(t *testing.T) {
	g, err := New(Backend{Vendor: OpenAIOAuthVendor("acct-1"), Cred: StaticKey("x")})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ inURL, wantPath, wantQuery string }{
		{"http://gw/responses?foo=bar&baz=1", "/backend-api/codex/responses", "foo=bar&baz=1"},
		{"http://gw/models?client_version=0.141.0", "/backend-api/codex/models", "client_version=0.141.0"},
		{"http://gw/v1/responses?x=1", "/backend-api/codex/responses", "x=1"}, // /v1 form maps the same
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", c.inURL, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, &reqCtx{lease: &Lease{Vendor: "openai"}, secret: "x"}))
		g.director(req)
		if req.URL.Path != c.wantPath {
			t.Errorf("%s: Path = %q, want %q", c.inURL, req.URL.Path, c.wantPath)
		}
		if req.URL.RawQuery != c.wantQuery {
			t.Errorf("%s: RawQuery = %q, want %q (query must survive the path remap)", c.inURL, req.URL.RawQuery, c.wantQuery)
		}
		if req.URL.Host != "chatgpt.com" {
			t.Errorf("%s: Host = %q, want chatgpt.com", c.inURL, req.URL.Host)
		}
	}
}

func TestOpenAICompatVendor(t *testing.T) {
	v := OpenAICompatVendor("openai-compat", "https://example.test", "/v1beta/openai", nil)
	if v.Name != "openai-compat" || v.BaseURL != "https://example.test" || v.BasePath != "/v1beta/openai" {
		t.Fatalf("vendor fields = %+v", v)
	}
	// Inject must set bearer + clear X-Api-Key (identical to OpenAI).
	r, _ := http.NewRequest("POST", "http://gw/v1/chat/completions", nil)
	r.Header.Set("X-Api-Key", "should-be-removed")
	v.Inject(r, "real-key")
	if r.Header.Get("Authorization") != "Bearer real-key" || r.Header.Get("X-Api-Key") != "" {
		t.Errorf("inject headers = %v", r.Header)
	}
	// Usage parser is the OpenAI one (non-nil).
	if v.ParseUsage == nil {
		t.Error("ParseUsage must be set (parseOpenAIUsage)")
	}
}

// TestDirector_OpenAICompatRoutes verifies that OpenAICompatVendor BasePath
// joins correctly onto the inbound path, and that empty BasePath forwards
// the path byte-identical while still rewriting scheme/host.
func TestDirector_OpenAICompatRoutes(t *testing.T) {
	cases := []struct {
		name     string
		basePath string
		inURL    string
		wantPath string
		wantHost string
	}{
		{
			name:     "basePath=/api/v1 strips leading /v1 and joins",
			basePath: "/api/v1",
			inURL:    "http://gw/v1/chat/completions",
			wantPath: "/api/v1/chat/completions",
			wantHost: "up.test",
		},
		{
			name:     "basePath=/api/v1 non-v1-prefixed path joins without extra strip",
			basePath: "/api/v1",
			inURL:    "http://gw/chat/completions",
			wantPath: "/api/v1/chat/completions",
			wantHost: "up.test",
		},
		{
			name:     "basePath= forwards path byte-identical",
			basePath: "",
			inURL:    "http://gw/v1/chat/completions",
			wantPath: "/v1/chat/completions",
			wantHost: "up.test",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := OpenAICompatVendor("openai-compat", "https://up.test", c.basePath, nil)
			g, err := New(Backend{Vendor: v, Cred: StaticKey("k")})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", c.inURL, nil)
			req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, &reqCtx{lease: &Lease{Vendor: "openai-compat"}, secret: "k"}))
			g.director(req)
			if req.URL.Scheme != "https" {
				t.Errorf("Scheme = %q, want https", req.URL.Scheme)
			}
			if req.URL.Host != c.wantHost {
				t.Errorf("Host = %q, want %q", req.URL.Host, c.wantHost)
			}
			if req.URL.Path != c.wantPath {
				t.Errorf("Path = %q, want %q", req.URL.Path, c.wantPath)
			}
		})
	}
}

// TestGateway_OpenAICompatMetersAndCaps verifies end-to-end that an
// OpenAICompatVendor: injects the real key as Bearer upstream, meters SpentUSD
// against the configured prices, and returns 402 once the budget is exhausted.
func TestGateway_OpenAICompatMetersAndCaps(t *testing.T) {
	const realKey = "sk-oc-real"
	// Upstream asserts correct auth header and returns a known usage response.
	// prompt_tokens=100000, completion_tokens=200000; at $1/1M in, $2/1M out
	// → cost = 0.1 + 0.4 = 0.5 USD.
	upstreamHit := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit++
		if got := r.Header.Get("Authorization"); got != "Bearer "+realKey {
			t.Errorf("Authorization = %q, want Bearer %s", got, realKey)
		}
		if r.Header.Get("X-Api-Key") != "" {
			t.Errorf("X-Api-Key should be unset for openai-compat, got %q", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"my-model","usage":{"prompt_tokens":100000,"completion_tokens":200000}}`)
	}))
	defer up.Close()

	prices := map[string]Price{
		"my-model": {InputPer1M: 1.0, OutputPer1M: 2.0},
		"default":  {InputPer1M: 1.0, OutputPer1M: 2.0},
	}
	v := OpenAICompatVendor("openai-compat", up.URL, "", prices)
	g, err := New(Backend{Vendor: v, Cred: StaticKey(realKey)})
	if err != nil {
		t.Fatal(err)
	}

	// Budget of 1.0 USD; first request costs 0.5 USD; second also 0.5 → budget
	// exactly exhausted, third should 402.
	tok, _ := g.Mint("openai-compat", 1.0, 0, time.Minute)

	req1 := httptest.NewRequest("POST", "http://gw/v1/chat/completions", strings.NewReader("{}"))
	req1.Header.Set("Authorization", "Bearer "+tok)
	rec1 := httptest.NewRecorder()
	g.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d body=%s", rec1.Code, rec1.Body.String())
	}
	// 100k in @1/1M + 200k out @2/1M = 0.1 + 0.4 = 0.5 USD
	if got := g.spent(tok); got < 0.49 || got > 0.51 {
		t.Errorf("SpentUSD after first request = %v, want ~0.5", got)
	}

	req2 := httptest.NewRequest("POST", "http://gw/v1/chat/completions", strings.NewReader("{}"))
	req2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	g.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request: status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	// 0.5 + 0.5 = 1.0 USD spent, equal to budget → next request should 402.
	if got := g.spent(tok); got < 0.99 || got > 1.01 {
		t.Errorf("SpentUSD after second request = %v, want ~1.0", got)
	}

	// Third request: budget exhausted → 402.
	req3 := httptest.NewRequest("POST", "http://gw/v1/chat/completions", strings.NewReader("{}"))
	req3.Header.Set("Authorization", "Bearer "+tok)
	rec3 := httptest.NewRecorder()
	g.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusPaymentRequired {
		t.Errorf("third request: status = %d, want 402 (StatusPaymentRequired)", rec3.Code)
	}
	if upstreamHit != 2 {
		t.Errorf("upstream hit %d times, want 2 (third request must be blocked before upstream)", upstreamHit)
	}
}

func TestStripJSONObjectFields(t *testing.T) {
	// Top-level field removed; every other field preserved verbatim. (This is the
	// real fix: the OAuth endpoint 400s on context_management Claude Code sends.)
	in := []byte(`{"model":"m","max_tokens":16,"context_management":{"edits":[1]},"messages":[{"role":"user","content":"hi"}]}`)
	out, changed := stripJSONObjectFields(in, []string{"context_management"})
	if !changed {
		t.Fatal("expected changed=true")
	}
	if strings.Contains(string(out), "context_management") {
		t.Errorf("context_management not removed: %s", out)
	}
	for _, must := range []string{`"model":"m"`, `"max_tokens":16`, `"content":"hi"`} {
		if !strings.Contains(string(out), must) {
			t.Errorf("expected %s preserved in %s", must, out)
		}
	}

	// Field absent -> unchanged, byte-identical.
	in2 := []byte(`{"model":"m","messages":[]}`)
	if out2, changed2 := stripJSONObjectFields(in2, []string{"context_management"}); changed2 || string(out2) != string(in2) {
		t.Errorf("expected unchanged; changed=%v out=%s", changed2, out2)
	}

	// Non-object JSON -> unchanged.
	if _, changed3 := stripJSONObjectFields([]byte(`[1,2,3]`), []string{"context_management"}); changed3 {
		t.Error("non-object body should be unchanged")
	}
}
