package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// F-03: the gateway must forward only each vendor's inference routes. A task
// holding a valid bearer must not be able to drive the injected host credential
// to control-plane endpoints (file upload/download/delete, fine-tuning, model
// management) that response-usage metering does not see. Disallowed routes must
// be rejected gateway-local (403) without ever reaching upstream.
func TestGateway_RouteAllowlist(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	v := AnthropicVendor() // carries the real AllowedRoutes
	v.BaseURL = up.URL     // but point at the mock upstream
	v.Prices = map[string]Price{"claude-x": {InputPer1M: 3, OutputPer1M: 15}}
	g, err := New(Backend{Vendor: v, Cred: StaticKey("REAL")})
	if err != nil {
		t.Fatal(err)
	}

	send := func(method, path string) int {
		tok, _ := g.Mint("anthropic", 100, 0, 0, 0, time.Minute)
		req := httptest.NewRequest(method, "http://gw"+path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec.Code
	}

	// The inference route is allowed and reaches upstream.
	if code := send("POST", "/v1/messages"); code != http.StatusOK {
		t.Errorf("POST /v1/messages = %d, want 200 (allowed inference route)", code)
	}

	reached = false
	// Control-plane routes are rejected locally, never reaching upstream.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/files"},
		{"DELETE", "/v1/files/abc"},
		{"POST", "/v1/fine_tuning/jobs"},
		{"POST", "/v1/uploads"},
		{"GET", "/v1/organization/invites"},
	} {
		if code := send(tc.method, tc.path); code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 (control-plane route blocked)", tc.method, tc.path, code)
		}
	}
	if reached {
		t.Error("a disallowed route reached upstream with the real credential")
	}

	// A traversal that prefix-matches an allowed route must still be rejected.
	if code := send("POST", "/v1/messages/../files"); code == http.StatusOK {
		t.Errorf("POST /v1/messages/../files = %d, want rejected (path traversal)", code)
	}
}
