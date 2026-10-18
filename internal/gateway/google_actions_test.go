package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Google's inference routes are `/v1beta/models/{model}:{action}`. A bare
// directory prefix would also admit `:batchGenerateContent`, an asynchronous
// batch job whose spend the response-usage meter never sees (the same hole
// F-03 closed for Anthropic's /v1/messages/batches). Only the actions the
// pinned Gemini CLI needs are allowed, and the model segment must be a single
// path element.
func TestRouteAllowed_GoogleActionsAreExplicit(t *testing.T) {
	v := GoogleVendor()
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1beta/models/gemini-2.5-pro:generateContent", true},
		{"POST", "/v1beta/models/gemini-2.5-pro:streamGenerateContent", true},
		{"POST", "/v1beta/models/gemini-2.5-pro:countTokens", true},
		{"POST", "/v1beta/models/gemini-embedding-001:embedContent", true},
		{"POST", "/v1beta/models/gemini-2.5-pro:batchGenerateContent", false},
		{"POST", "/v1beta/models/gemini-2.5-pro:asyncBatchGenerateContent", false},
		{"POST", "/v1beta/models/gemini-2.5-pro:generateContentX", false},
		{"POST", "/v1beta/models/gemini-2.5-pro", false},                // no action
		{"POST", "/v1beta/models/:generateContent", false},              // empty model
		{"POST", "/v1beta/models/a/b:generateContent", false},           // model is one segment
		{"POST", "/v1beta/models/gemini:generateContent/x", false},      // nothing after the action
		{"GET", "/v1beta/models/gemini-2.5-pro", true},                  // retrieve stays open
		{"GET", "/v1beta/models/gemini-2.5-pro:generateContent", false}, // an action is never a retrieve
		{"GET", "/v1beta/models/gemini-2.5-pro:batchGenerateContent", false},
		{"GET", "/v1beta/models", true},
	}
	for _, c := range cases {
		if got := v.routeAllowed(c.method, c.path); got != c.want {
			t.Errorf("routeAllowed(%q, %q) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// The gateway must answer the batch route locally (403) and never forward it.
func TestGateway_GoogleBatchGenerateBlockedLocally(t *testing.T) {
	g := routedGateway(t, GoogleVendor())
	tok, _ := g.Mint("google", 10, 0, 0, 0, time.Minute)
	req := httptest.NewRequest("POST", "http://gw/v1beta/models/gemini-2.5-pro:batchGenerateContent", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", tok)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("batchGenerateContent status = %d, want 403", rec.Code)
	}
}
