package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"drydock/internal/gwcreds"
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
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)

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
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)
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
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, -time.Second) // already expired
	if rec := do(g, tok); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGateway_OverBudget402(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	tok, _ := g.Mint("anthropic", 1.0, 0, 0, 0, time.Minute) // budget 1.0; one call spends ~18 -> next call 402
	do(g, tok)
	if rec := do(g, tok); rec.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", rec.Code)
	}
}

func TestGateway_RevokeInvalidates(t *testing.T) {
	g := newGW(t, "http://unused")
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)
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

	atok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)
	otok, _ := g.Mint("openai", 100, 0, 0, 0, time.Minute)

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
	tok, _ := g.Mint("anthropic", 100, 2, 0, 0, time.Hour) // maxRequests = 2
	if _, s := g.admit(tok); s != 0 {
		t.Fatalf("req1 rejected: %d", s)
	}
	if _, s := g.admit(tok); s != 0 {
		t.Fatalf("req2 rejected: %d", s)
	}
	if _, s := g.admit(tok); s != http.StatusTooManyRequests {
		t.Fatalf("req3 status = %d, want 429", s)
	}
}

func TestRequestCap_ZeroMeansUnlimited(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Hour) // 0 = unlimited
	for i := 0; i < 50; i++ {
		if _, s := g.admit(tok); s != 0 {
			t.Fatalf("req %d rejected: %d", i, s)
		}
	}
}

// A lease with MaxInFlight=1 admits a second concurrent request only after the
// first completes. Spend is metered at response completion, so every
// concurrently admitted request can overshoot the budget by its own cost;
// this cap bounds that overshoot (F-02/F-05).
func TestAdmit_InFlightLimit(t *testing.T) {
	g, err := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := g.Mint("anthropic", 5, 0, 0, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := g.admit(tok); code != 0 {
		t.Fatalf("first admit: code %d", code)
	}
	if _, code := g.admit(tok); code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent admit: code %d, want 429", code)
	}
	g.release(tok)
	if _, code := g.admit(tok); code != 0 {
		t.Fatalf("admit after release: want admitted")
	}
}

// End to end: while one proxied request is still streaming, a second request
// gets a local 429 with Retry-After, and succeeds after the first completes.
func TestServeHTTP_InFlightLimit(t *testing.T) {
	unblock := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	v := AnthropicVendor()
	v.BaseURL = up.URL
	g, err := New(Backend{Vendor: v, Cred: StaticKey("k")})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := g.Mint("anthropic", 100, 0, 0, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		return w
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- do() }()
	// Wait until the first request is admitted and blocked in the upstream.
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		inflight := g.leases[tok].InFlight
		g.mu.Unlock()
		if inflight == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first request never became in-flight")
		}
		time.Sleep(time.Millisecond)
	}
	if w := do(); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("concurrent request: code %d retry-after %q, want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	close(unblock)
	if w := <-first; w.Code != http.StatusOK {
		t.Fatalf("first request: code %d", w.Code)
	}
	if w := do(); w.Code != http.StatusOK {
		t.Fatalf("request after completion: code %d, want 200", w.Code)
	}
}

func TestNew_RejectsNilCred(t *testing.T) {
	if _, err := New(Backend{Vendor: AnthropicVendor()}); err == nil {
		t.Fatal("New with nil Cred should error")
	}
}

func TestGateway_MintUnknownVendorErrors(t *testing.T) {
	g := newGW(t, "http://unused")
	if _, err := g.Mint("nope", 100, 0, 0, 0, time.Minute); err == nil {
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
	tok, _ := g.Mint("openai-compat", 1.0, 0, 0, 0, time.Minute)

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

// memCredStore is an in-memory gwcreds.CredStore for tests. It satisfies the
// interface via structural typing; no Save is expected here because the
// snapshot is far from expiry and no token refresh is triggered.
type memCredStore struct{ snap gwcreds.CredSnapshot }

func (m *memCredStore) Load() (gwcreds.CredSnapshot, error) { return m.snap, nil }
func (m *memCredStore) Save(s gwcreds.CredSnapshot) error   { m.snap = s; return nil }

// TestServeHTTP_OAuthVendor_StripRequestFields verifies the wired path through
// ServeHTTP for AnthropicOAuthVendor: the gateway must
//
//  1. Strip "context_management" from the JSON body (Vendor.StripFields) and
//     update Content-Length accordingly.
//  2. Inject OAuth headers (Authorization: Bearer, anthropic-beta) and remove
//     X-Api-Key before forwarding.
//
// It uses a real gwcreds.OAuthCred (the "real OAuth constructor path") with a
// far-future expiry so no network refresh is attempted.
func TestServeHTTP_OAuthVendor_StripRequestFields(t *testing.T) {
	// Upstream captures the rewritten request.
	var (
		gotBody          []byte
		gotAuth          string
		gotBeta          string
		gotXApiKey       string
		gotContentLength string
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotXApiKey = r.Header.Get("X-Api-Key")
		gotContentLength = r.Header.Get("Content-Length")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-opus-4","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	// Build an OAuthCred via gwcreds (the real constructor). Expiry is far in
	// the future so Current() returns the canned token without hitting the
	// network.
	snap := gwcreds.CredSnapshot{
		Access:  "real-bearer-token",
		Refresh: "dummy-refresh",
		Expiry:  time.Now().Add(time.Hour),
	}
	store := &memCredStore{snap: snap}
	cred := gwcreds.NewOAuthCred(snap, store)

	// AnthropicOAuthVendor is the live vendor constructor; redirect its
	// BaseURL to the in-process test server.
	v := AnthropicOAuthVendor()
	v.BaseURL = up.URL

	g, err := New(Backend{Vendor: v, Cred: cred})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)

	// Body contains context_management, which AnthropicOAuthVendor must strip.
	const body = `{"model":"claude-opus-4","messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[1]}}`
	req := httptest.NewRequest("POST", "http://gw/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "should-be-removed")
	req.ContentLength = int64(len(body))

	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	// 1. context_management must be absent from the forwarded body.
	if strings.Contains(string(gotBody), "context_management") {
		t.Errorf("context_management must be stripped; upstream received: %s", gotBody)
	}
	// 2. Other fields must survive.
	if !strings.Contains(string(gotBody), `"model"`) || !strings.Contains(string(gotBody), `"messages"`) {
		t.Errorf("model/messages must be preserved; upstream received: %s", gotBody)
	}
	// 3. Content-Length must be updated to the stripped body length.
	wantLen := len(gotBody)
	if gotContentLength != strconv.Itoa(wantLen) {
		t.Errorf("Content-Length = %q, want %d (must reflect stripped body)", gotContentLength, wantLen)
	}
	if wantLen >= len(body) {
		t.Errorf("stripped body (%d bytes) must be shorter than original (%d bytes)", wantLen, len(body))
	}
	// 4. OAuth headers injected, X-Api-Key removed.
	if gotAuth != "Bearer real-bearer-token" {
		t.Errorf("Authorization = %q, want Bearer real-bearer-token", gotAuth)
	}
	if gotXApiKey != "" {
		t.Errorf("X-Api-Key must be absent upstream, got %q", gotXApiKey)
	}
	if gotBeta != anthropicOAuthBeta {
		t.Errorf("anthropic-beta = %q, want %q", gotBeta, anthropicOAuthBeta)
	}
}

func TestServeHTTP_BearerWinsOverGoogleKeyHeader(t *testing.T) {
	// A valid Authorization: Bearer must admit even when a (bogus) x-goog-api-key
	// is also present — Authorization takes priority and the key header is not
	// consulted for admission.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-2.5-pro"}`))
	}))
	defer up.Close()
	v := GoogleVendor()
	v.BaseURL = up.URL
	g, err := New(Backend{Vendor: v, Cred: StaticKey("REAL-KEY")})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := g.Mint("google", 1.0, 0, 0, 0, time.Minute)

	r := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("X-Goog-Api-Key", "not-a-real-token")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (valid Bearer must admit regardless of x-goog-api-key)", w.Code)
	}
}

func TestServeHTTP_MalformedAuthDoesNotFallBack(t *testing.T) {
	// A present-but-malformed Authorization must NOT fall back to x-goog-api-key;
	// a valid token in the key header alongside a bad Authorization is rejected.
	g, err := New(Backend{Vendor: GoogleVendor(), Cred: StaticKey("REAL-KEY")})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := g.Mint("google", 1.0, 0, 0, 0, time.Minute)
	r := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	r.Header.Set("Authorization", "Basic Zm9vOmJhcg==") // malformed (not Bearer)
	r.Header.Set("X-Goog-Api-Key", tok)                 // valid token, but must be ignored
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("status = %d, want 401 (malformed Authorization must not fall back to x-goog-api-key)", w.Code)
	}
}

func TestServeHTTP_AdmitsGoogleKeyHeader(t *testing.T) {
	// Fake upstream records the injected key.
	var gotKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Goog-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-2.5-pro"}`))
	}))
	defer up.Close()

	v := GoogleVendor()
	v.BaseURL = up.URL // redirect upstream to the fake
	g, err := New(Backend{Vendor: v, Cred: StaticKey("REAL-KEY")})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := g.Mint("google", 1.0, 0, 0, 0, time.Minute)

	// Present the bearer ONLY in x-goog-api-key (how the Gemini CLI sends it).
	r := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	r.Header.Set("X-Goog-Api-Key", tok)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (token in x-goog-api-key must admit)", w.Code)
	}
	if gotKey != "REAL-KEY" {
		t.Errorf("upstream x-goog-api-key = %q, want REAL-KEY (real key injected, bearer swapped)", gotKey)
	}
}

func TestGateway_AggregateCap(t *testing.T) {
	g, err := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")}, Backend{Vendor: OpenAIVendor(), Cred: StaticKey("k")})
	if err != nil {
		t.Fatal(err)
	}
	g.SetAggregateCap(5.0, time.Hour, []string{"anthropic", "openai"})
	now := time.Now()

	// anthropic at the cap: a fresh lease's request must be refused with 402.
	g.SeedAggregate("anthropic", 5.0, now)
	tok, _ := g.Mint("anthropic", 100.0, 0, 0, 0, time.Hour) // generous per-task budget
	if _, code := g.admit(tok); code != http.StatusPaymentRequired {
		t.Errorf("anthropic over aggregate cap: admit code = %d, want 402", code)
	}

	// openai is under its own cap: must still admit (per-vendor isolation).
	tok2, _ := g.Mint("openai", 100.0, 0, 0, 0, time.Hour)
	if _, code := g.admit(tok2); code != 0 {
		t.Errorf("openai under aggregate cap: admit code = %d, want 0 (admitted)", code)
	}

	if !g.AggregateExceeded("anthropic") {
		t.Error("AggregateExceeded(anthropic) = false, want true")
	}
	if g.AggregateExceeded("openai") {
		t.Error("AggregateExceeded(openai) = true, want false")
	}
}

func TestAdmit_InFlightReservationBounds(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	// Budget 1.0, per-request reservation 0.6: the first admit reserves 0.6,
	// a second concurrent admit (before any meter) would need 0.6+0.6=1.2 > 1.0
	// and must be rejected.
	tok, _ := g.Mint("anthropic", 1.0, 0, 0.6, 0, time.Hour)
	if _, code := g.admit(tok); code != 0 {
		t.Fatalf("first admit code = %d, want 0", code)
	}
	if _, code := g.admit(tok); code != http.StatusPaymentRequired {
		t.Errorf("second concurrent admit code = %d, want 402 (reservation bound)", code)
	}
}

func TestAdmit_NoReservationWhenDisabled(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	tok, _ := g.Mint("anthropic", 1.0, 0, 0, 0, time.Hour) // R=0 disables reservation
	for i := 0; i < 5; i++ {
		if _, code := g.admit(tok); code != 0 {
			t.Fatalf("admit %d code = %d, want 0 (R=0, no reservation)", i, code)
		}
	}
}

// TestMeter_ReleasesReservation verifies that meter's onDone closure releases
// the per-request reservation (Reserved returns to 0) whether or not the
// response body carries parseable usage, and that SpentUSD reflects the actual
// metered cost.
func TestMeter_ReleasesReservation(t *testing.T) {
	const R = 0.5 // per-request reservation

	// Case 1: upstream returns a valid usage body; Reserved must drop to 0 and
	// SpentUSD must equal the metered cost.
	t.Run("with_usage", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_start\n")
			io.WriteString(w, `data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":1000000,"output_tokens":0}}}`+"\n\n")
			io.WriteString(w, `data: {"type":"message_delta","usage":{"output_tokens":1000000}}`+"\n\n")
		}))
		defer up.Close()

		g := newGW(t, up.URL)
		// generous budget, R=0.5 per request
		tok, _ := g.Mint("anthropic", 100, 0, R, 0, time.Minute)

		if rec := do(g, tok); rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}

		g.mu.Lock()
		lease := g.leases[tok]
		reserved := lease.Reserved
		spent := lease.SpentUSD
		g.mu.Unlock()

		if reserved != 0 {
			t.Errorf("Reserved = %v after completion, want 0 (reservation must be released)", reserved)
		}
		// 1M in @3 + 1M out @15 = 18.0
		if spent < 17.9 || spent > 18.1 {
			t.Errorf("SpentUSD = %v, want ~18 (metered cost committed)", spent)
		}
	})

	// Case 2: upstream returns NO usage lines; Reserved must still drop to 0 and
	// SpentUSD must remain 0 (no delta committed).
	t.Run("no_usage", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// A truncated/empty stream: no usage-bearing events at all.
			io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")
		}))
		defer up.Close()

		g := newGW(t, up.URL)
		tok, _ := g.Mint("anthropic", 100, 0, R, 0, time.Minute)

		if rec := do(g, tok); rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}

		g.mu.Lock()
		lease := g.leases[tok]
		reserved := lease.Reserved
		spent := lease.SpentUSD
		g.mu.Unlock()

		if reserved != 0 {
			t.Errorf("Reserved = %v after usageless stream, want 0 (reservation must be released regardless)", reserved)
		}
		if spent != 0 {
			t.Errorf("SpentUSD = %v after usageless stream, want 0 (no delta to commit)", spent)
		}
	})
}

func TestGateway_AggregateCap_DisabledByDefault(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	// No SetAggregateCap call: cap disabled.
	tok, _ := g.Mint("anthropic", 100.0, 0, 0, 0, time.Hour)
	if _, code := g.admit(tok); code != 0 {
		t.Errorf("cap disabled: admit code = %d, want 0", code)
	}
	if g.AggregateExceeded("anthropic") {
		t.Error("AggregateExceeded with cap disabled = true, want false")
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
