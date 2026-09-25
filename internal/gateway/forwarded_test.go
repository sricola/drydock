package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The proxy runs in Rewrite mode, so the upstream vendor learns nothing about
// the in-VM client: no X-Forwarded-For is synthesized from RemoteAddr, and any
// Forwarded/X-Forwarded-* header the agent sends itself is dropped rather than
// passed through. Both are checked at the upstream, not on the rewritten
// request, because ReverseProxy applies its own header edits around Rewrite.
func TestGateway_NoForwardedHeadersReachUpstream(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer up.Close()
	g := newGW(t, up.URL)
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)

	req := httptest.NewRequest("POST", "http://gw/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Forwarded-For", "10.9.8.7")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "gopher")
	req.Header.Set("Forwarded", "for=10.9.8.7")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if v := seen.Values(h); len(v) != 0 {
			t.Errorf("upstream saw %s = %q; the gateway must neither synthesize nor pass through forwarding headers", h, v)
		}
	}
}
