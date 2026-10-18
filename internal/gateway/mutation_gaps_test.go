package gateway

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Mutation testing (gremlins) showed these behaviours survived every mutant:
// each test here was written to kill a specific surviving mutant and was
// checked against it.

// Metered spend must feed the aggregate ledger; seeding alone is not enough.
// (Survivor: gateway.go meter `delta > 0` negated.)
func TestGateway_MeteredSpendTripsTheAggregateCap(t *testing.T) {
	up := upstream(t) // 1M in + 1M out at 3/15 per 1M = $18 per call
	defer up.Close()
	g := newGW(t, up.URL)
	g.SetAggregateCap(10, time.Hour, []string{"anthropic"})
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Hour)
	if g.AggregateExceeded("anthropic") {
		t.Fatal("cap exceeded before any spend")
	}
	req := httptest.NewRequest("POST", "http://gw/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}
	if !g.AggregateExceeded("anthropic") {
		t.Fatal("$18 of metered spend did not trip a $10 aggregate cap")
	}
	if _, code, _ := g.admit(tok); code != http.StatusPaymentRequired {
		t.Errorf("admit after the cap tripped = %d, want 402", code)
	}
}

// A reservation that exactly fits the remaining budget is admitted; one more
// is not. (Survivor: admit `> l.BudgetUSD` boundary.)
func TestAdmit_ReservationExactFitIsAdmitted(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	tok, _ := g.Mint("anthropic", 1.0, 0, 0.5, 0, time.Hour)
	for i := 1; i <= 2; i++ {
		if _, code, _ := g.admit(tok); code != 0 {
			t.Fatalf("admit %d (reserved %.1f of 1.0) = %d, want admitted", i, 0.5*float64(i), code)
		}
	}
	if _, code, _ := g.admit(tok); code != http.StatusPaymentRequired {
		t.Errorf("third admit (would reserve 1.5 of 1.0) = %d, want 402", code)
	}
}

// Inject defaults anthropic-version only when the client sent none.
// (Survivors: vendor.go anthropic-version == "" negated, both vendors.)
func TestAnthropicInject_VersionHeaderDefaultedNotOverwritten(t *testing.T) {
	for _, v := range []Vendor{AnthropicVendor(), AnthropicOAuthVendor()} {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		v.Inject(r, "secret")
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("%s: absent version defaulted to %q, want 2023-06-01", v.Name, got)
		}
		r = httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("anthropic-version", "2024-10-22")
		v.Inject(r, "secret")
		if got := r.Header.Get("anthropic-version"); got != "2024-10-22" {
			t.Errorf("%s: client version overwritten to %q", v.Name, got)
		}
	}
}

// Any single non-zero token field makes a usage block parseable; all-zero does
// not. (Survivors: Usage.any per-field comparisons.)
func TestParseAnthropicUsage_EachFieldAloneCounts(t *testing.T) {
	for field, want := range map[string]Usage{
		"input_tokens":                {Input: 7},
		"output_tokens":               {Output: 7},
		"cache_creation_input_tokens": {CacheWrite: 7},
		"cache_read_input_tokens":     {CacheRead: 7},
	} {
		body := `{"model":"m","usage":{"` + field + `":7}}`
		u, ok := parseAnthropicUsage([]byte(body), "application/json")
		if !ok {
			t.Errorf("%s alone: ok=false, want parseable", field)
			continue
		}
		want.Model = "m"
		if u != want {
			t.Errorf("%s alone: usage %+v, want %+v", field, u, want)
		}
	}
	if _, ok := parseAnthropicUsage([]byte(`{"model":"m","usage":{"input_tokens":0,"output_tokens":0}}`), "application/json"); ok {
		t.Error("all-zero usage parsed as ok; a zero-usage success must be reported as unparseable so the gateway warns")
	}
	// A streamed message_delta with zero output must not erase the provisional
	// output from message_start. (Survivor: usage.go OutputTokens > 0 boundary.)
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n\n"
	u, ok := parseAnthropicUsage([]byte(sse), "text/event-stream")
	if !ok || u.Output != 3 {
		t.Errorf("zero-output message_delta: usage %+v ok=%v, want Output 3 kept", u, ok)
	}
}

// chunkReader hands out b in pseudo-random slices so a line can be split at
// any byte, including inside "usage" and across the "\n".
type chunkReader struct {
	b   []byte
	rnd *rand.Rand
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := 1 + c.rnd.Intn(len(p))
	if n > len(c.b) {
		n = len(c.b)
	}
	copy(p, c.b[:n])
	c.b = c.b[n:]
	return n, nil
}

// The usage lines the meter keeps must not depend on how the upstream body
// happened to be chunked.
func TestUsageReader_KeptLinesAreChunkingInvariant(t *testing.T) {
	var body bytes.Buffer
	for i := 0; i < 200; i++ {
		body.WriteString("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"streamed chunk number " + strings.Repeat("x", i%37) + "\"}}\n\n")
		if i%50 == 0 {
			body.WriteString("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":" + strings.Repeat("9", 1+i/50) + "}}\n\n")
		}
	}
	body.WriteString("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}") // no trailing newline
	want := ""
	for _, ln := range strings.Split(body.String(), "\n") {
		if strings.Contains(ln, "usage") {
			want += ln + "\n"
		}
	}
	for seed := int64(1); seed <= 40; seed++ {
		var kept []byte
		complete := false
		u := &usageReader{
			rc:     io.NopCloser(&chunkReader{b: body.Bytes(), rnd: rand.New(rand.NewSource(seed))}),
			onDone: func(b []byte, c bool) { kept = append([]byte(nil), b...); complete = c },
		}
		buf := make([]byte, 1+int(seed*7)%1500)
		if _, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{u}, buf); err != nil {
			t.Fatalf("seed %d: copy: %v", seed, err)
		}
		if string(kept) != want {
			t.Fatalf("seed %d: kept usage lines differ from the chunking-independent expectation\n got: %q\nwant: %q", seed, kept, want)
		}
		if !complete {
			t.Fatalf("seed %d: EOF reached but onDone reported complete=false", seed)
		}
	}
}

// The per-line buffer is capped at exactly maxUsageBufBytes: a longer
// newline-free body is truncated to the cap, not grown. (Survivors: appendLine
// arithmetic and boundary.)
func TestUsageReader_LineCapIsExact(t *testing.T) {
	orig := maxUsageBufBytes
	maxUsageBufBytes = 64
	t.Cleanup(func() { maxUsageBufBytes = orig })
	line := []byte(`{"usage":` + strings.Repeat("1", 500) + `}`) // "usage" inside the first 64 bytes
	var kept []byte
	u := &usageReader{rc: io.NopCloser(bytes.NewReader(line)), onDone: func(b []byte, _ bool) { kept = append([]byte(nil), b...) }}
	if _, err := io.Copy(io.Discard, struct{ io.Reader }{u}); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 64+1 { // the truncated line plus the newline flushLine adds
		t.Fatalf("kept %d bytes, want exactly the 64-byte cap plus newline", len(kept))
	}
	if !bytes.Equal(kept[:64], line[:64]) {
		t.Errorf("kept prefix differs from the line's first 64 bytes")
	}
}

// Provider.Mint falls back to the configured budget and a one-hour TTL when
// the caller passes zero. (Survivors: provider.go b == 0 / ttl == 0.)
func TestProvider_MintZeroBudgetAndTTLFallBack(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	p := &Provider{GW: g, Vendor: "anthropic", Budget: 2.5, BaseURL: "http://gw", BaseURLEnv: "B", TokenEnv: "T"}
	gr, err := p.Mint(0)
	if err != nil {
		t.Fatal(err)
	}
	tok := gr.(*grant).token
	g.mu.Lock()
	l := g.leases[tok]
	g.mu.Unlock()
	if l.BudgetUSD != 2.5 {
		t.Errorf("budget = %v, want the provider default 2.5 when Mint gets 0", l.BudgetUSD)
	}
	if ttl := time.Until(l.Expiry); ttl < 59*time.Minute || ttl > 61*time.Minute {
		t.Errorf("ttl = %v, want about one hour when the provider TTL is zero", ttl)
	}
}

// After Revoke, the grant reports 0 spent and 0 requests, never the internal
// "lease gone" sentinel. (Survivors: provider.go s < 0 / n < 0.)
func TestGrant_SpentAndRequestsAreZeroAfterRevoke(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	p := &Provider{GW: g, Vendor: "anthropic", Budget: 1, BaseURL: "http://gw", BaseURLEnv: "B", TokenEnv: "T"}
	gr, _ := p.Mint(1)
	if err := gr.Revoke(); err != nil {
		t.Fatal(err)
	}
	if s := gr.Spent(); s != 0 {
		t.Errorf("Spent after revoke = %v, want 0", s)
	}
	if n := gr.(*grant).Requests(); n != 0 {
		t.Errorf("Requests after revoke = %d, want 0", n)
	}
}

// A Gemini usage block with only prompt tokens, or only candidate tokens, is
// still usage; an all-zero block is not. (Survivor: usage.go in == 0 && out == 0.)
func TestParseGoogleUsage_EitherSideAloneCounts(t *testing.T) {
	in := []byte(`{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":5}}`)
	out := []byte(`{"modelVersion":"gemini-2.5-pro","usageMetadata":{"candidatesTokenCount":5}}`)
	none := []byte(`{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}}`)
	if u, ok := parseGoogleUsage(in, "application/json"); !ok || u.Input != 5 || u.Output != 0 {
		t.Errorf("prompt-only: %+v ok=%v", u, ok)
	}
	if u, ok := parseGoogleUsage(out, "application/json"); !ok || u.Output != 5 || u.Input != 0 {
		t.Errorf("candidates-only: %+v ok=%v", u, ok)
	}
	if _, ok := parseGoogleUsage(none, "application/json"); ok {
		t.Error("all-zero usageMetadata parsed as usage")
	}
}
