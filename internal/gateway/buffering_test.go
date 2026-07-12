package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A giant newline-free response body must not grow the current-line buffer
// unbounded (metering memory stays bounded regardless of upstream behaviour).
func TestUsageReader_BoundsLine(t *testing.T) {
	orig := maxUsageBufBytes
	maxUsageBufBytes = 1024
	t.Cleanup(func() { maxUsageBufBytes = orig })

	u := &usageReader{}
	u.appendLine(bytes.Repeat([]byte("x"), 100_000))
	if u.line.Len() > maxUsageBufBytes {
		t.Errorf("u.line = %d bytes, want <= %d", u.line.Len(), maxUsageBufBytes)
	}
}

// An endless stream of "usage"-bearing lines must not grow the retained buffer
// unbounded.
func TestUsageReader_BoundsKept(t *testing.T) {
	orig := maxUsageBufBytes
	maxUsageBufBytes = 1024
	t.Cleanup(func() { maxUsageBufBytes = orig })

	var many bytes.Buffer
	for i := 0; i < 5000; i++ {
		many.WriteString(`{"usage":1}` + "\n")
	}
	u := &usageReader{rc: io.NopCloser(bytes.NewReader(many.Bytes())), onDone: func([]byte) {}}
	if _, err := io.ReadAll(u); err != nil {
		t.Fatal(err)
	}
	if u.kept.Len() > maxUsageBufBytes+1024 { // + at most one line past the cap
		t.Errorf("u.kept = %d bytes, want <= ~%d", u.kept.Len(), maxUsageBufBytes)
	}
}

// An oversized request body is rejected rather than buffered/forwarded whole.
func TestGateway_RequestBodyCapped(t *testing.T) {
	orig := maxProxyRequestBytes
	maxProxyRequestBytes = 1024
	t.Cleanup(func() { maxProxyRequestBytes = orig })

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	v := AnthropicVendor()
	v.BaseURL = up.URL
	v.Prices = map[string]Price{"claude-x": {InputPer1M: 3, OutputPer1M: 15}}
	g, err := New(Backend{Vendor: v, Cred: StaticKey("REAL")})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := g.Mint("anthropic", 100, 0, 0, time.Minute)

	req := httptest.NewRequest("POST", "http://gw/v1/messages", strings.NewReader(strings.Repeat("x", 8192)))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("oversized request (8 KiB over a 1 KiB cap) returned 200; want rejected")
	}
}
