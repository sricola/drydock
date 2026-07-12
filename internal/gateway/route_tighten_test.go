package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Segment-boundary matching: a route prefix must not admit a same-prefix sibling
// like /v1/models_secret under /v1/models. Exact endpoints and their
// sub-resources still match.
func TestRouteAllowed_SegmentBoundary(t *testing.T) {
	v := AnthropicVendor() // POST /v1/messages, GET /v1/models
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/messages", true},         // exact endpoint
		{"POST", "/v1/messages/batches", true}, // sub-resource (accepted residual)
		{"POST", "/v1/messagesX", false},       // sibling prefix: now blocked
		{"GET", "/v1/models", true},            // exact list
		{"GET", "/v1/models/claude-x", true},   // sub-resource retrieve
		{"GET", "/v1/models_secret", false},    // sibling prefix: the latent gap, now closed
		{"GET", "/v1/messages", false},         // wrong method
	}
	for _, c := range cases {
		if got := v.routeAllowed(c.method, c.path); got != c.want {
			t.Errorf("routeAllowed(%q, %q) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// A directory prefix (trailing slash, e.g. Google's /v1beta/models/) matches any
// sub-path but still rejects a same-stem sibling and a different prefix.
func TestRouteAllowed_DirectoryPrefix(t *testing.T) {
	v := GoogleVendor() // POST /v1beta/models/, GET /v1beta/models, GET /v1/models
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1beta/models/gemini-2:generateContent", true},
		{"POST", "/v1beta/models/gemini-2:streamGenerateContent", true},
		{"GET", "/v1beta/models", true},          // exact list
		{"GET", "/v1beta/models_secret", false},  // sibling of the list route: blocked
		{"POST", "/v1beta/tunedModels/x", false}, // different prefix: blocked
	}
	for _, c := range cases {
		if got := v.routeAllowed(c.method, c.path); got != c.want {
			t.Errorf("routeAllowed(%q, %q) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// routedGateway builds a gateway for one vendor pointed at a mock upstream that
// fails the test if a request ever reaches it (used to prove local 403s).
func routedGateway(t *testing.T, v Vendor) *Gateway {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a blocked request reached upstream")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)
	v.BaseURL = up.URL
	g, err := New(Backend{Vendor: v, Cred: StaticKey("REAL")})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Encoded traversal (%2e%2e) is decoded by net/http into r.URL.Path, so the
// pathHasTraversal check catches it exactly like a literal "..".
func TestGateway_EncodedTraversalBlocked(t *testing.T) {
	g := routedGateway(t, AnthropicVendor())
	tok, _ := g.Mint("anthropic", 100, 0, 0, time.Minute)
	req := httptest.NewRequest("POST", "http://gw/v1/messages/%2e%2e/files", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("encoded-traversal path = %d, want 403", rec.Code)
	}
}

// The Google header-token path (x-goog-api-key, no Authorization) is route-checked
// too: leaseVendor resolves the header token's vendor, so a control-plane route
// is blocked locally.
func TestGateway_HeaderTokenRouteChecked(t *testing.T) {
	g := routedGateway(t, GoogleVendor())
	tok, _ := g.Mint("google", 100, 0, 0, time.Minute)
	req := httptest.NewRequest("GET", "http://gw/v1/files", nil)
	req.Header.Set("X-Goog-Api-Key", tok) // no Authorization: header-token path
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /v1/files via header token = %d, want 403", rec.Code)
	}
}
