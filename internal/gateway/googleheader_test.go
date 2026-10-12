package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The per-task token is accepted from X-Goog-Api-Key for every vendor (the
// Gemini CLI presents it there), but only the Google injector removes that
// header. For any other vendor the task token must still be stripped before
// the request leaves for the upstream: a gateway credential is not something
// a vendor should ever see.
func TestGateway_TaskTokenInGoogleHeaderNeverReachesNonGoogleUpstream(t *testing.T) {
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Goog-Api-Key")
		io.WriteString(w, `{"model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	av := Vendor{Name: "anthropic", BaseURL: up.URL, Inject: AnthropicVendor().Inject,
		ParseUsage: parseAnthropicUsage, Prices: AnthropicPrices(), AllowedRoutes: AnthropicVendor().AllowedRoutes}
	g, err := New(Backend{Vendor: av, Cred: StaticKey("REAL")})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Goog-Api-Key", tok) // no Authorization header at all
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s (the token must still authenticate)", rec.Code, rec.Body.String())
	}
	if seen != "" {
		t.Errorf("upstream received X-Goog-Api-Key=%q; the task token must be stripped", seen)
	}
}
