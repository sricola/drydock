package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func testServer() *Server { return &Server{AuditRoot: "/tmp", Token: "secret"} }

func do(t *testing.T, s *Server, method, target, host, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if host != "" {
		req.Host = host
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAuth(t *testing.T) {
	s := testServer()
	// No token header → 403.
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", ""); rec.Code != http.StatusForbidden {
		t.Errorf("no-token = %d, want 403", rec.Code)
	}
	// Wrong token → 403.
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer nope"); rec.Code != http.StatusForbidden {
		t.Errorf("bad-token = %d, want 403", rec.Code)
	}
	// Right token reaches the (stub) handler → not 403 (501 stub is fine here).
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer secret"); rec.Code == http.StatusForbidden {
		t.Errorf("good-token still 403")
	}
}

func TestHostCheck(t *testing.T) {
	s := testServer()
	if rec := do(t, s, "GET", "/api/tasks", "evil.example.com", "Bearer secret"); rec.Code != http.StatusForbidden {
		t.Errorf("rebinding host = %d, want 403", rec.Code)
	}
}

func TestNoTokenModeSkipsGate(t *testing.T) {
	s := &Server{AuditRoot: "/tmp", Token: ""} // --no-token
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", ""); rec.Code == http.StatusForbidden {
		t.Errorf("--no-token must not 403")
	}
}

func TestValidID(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef"
	for _, bad := range []string{"", "../etc", "ABC", good + "x", good[:31], "g" + good[1:]} {
		if validID(bad) {
			t.Errorf("validID(%q) = true, want false", bad)
		}
	}
	if !validID(good) {
		t.Errorf("validID(%q) = false, want true", good)
	}
}

func TestServesIndex(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:7878"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
}

// TestServesStaticAssets checks the other embedded assets (app.js, style.css)
// are reachable the same unauthenticated way index.html is.
func TestServesStaticAssets(t *testing.T) {
	s := testServer()
	for _, path := range []string{"/app.js", "/style.css"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "127.0.0.1:7878"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestEmptyHostRejected(t *testing.T) {
	s := testServer()
	// An empty/missing Host must not be treated as loopback.
	req := httptest.NewRequest("GET", "/api/tasks", nil)
	req.Host = ""
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("empty host = %d, want 403", rec.Code)
	}
}

// TestConstantTimeTokenCompare documents that the auth boundary uses
// crypto/subtle.ConstantTimeCompare, exercising both the accept and reject paths.
func TestConstantTimeTokenCompare(t *testing.T) {
	s := &Server{AuditRoot: "/tmp", Token: "secrettoken"}
	// Correct bearer must be accepted.
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer secrettoken"); rec.Code == http.StatusForbidden {
		t.Errorf("correct token rejected by constant-time compare")
	}
	// Token with same prefix but extra chars must be rejected — ConstantTimeCompare
	// returns 0 when lengths differ, so length extension is never silently accepted.
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer secrettokenXXX"); rec.Code != http.StatusForbidden {
		t.Errorf("length-extended token accepted — constant-time compare must reject mismatched tokens")
	}
	// Prefix-only token must be rejected.
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer secret"); rec.Code != http.StatusForbidden {
		t.Errorf("prefix-only token accepted — must reject")
	}
}

// TestServesFavicon checks the embedded favicon is reachable at the path
// index.html's <link rel="icon"> points at, with an image content type, and
// that it is served the same way as index.html/app.js: no bearer token
// required (GET / is not wrapped in s.authed(), unlike /api/*).
func TestServesFavicon(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest("GET", "/favicon-32.png", nil)
	req.Host = "127.0.0.1:7878"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /favicon-32.png = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("favicon body is empty")
	}
}

// TestSecurityHeaders checks every response — static assets, /api/* (including
// its 403 path), and 404s — carries the CSP + nosniff + frame-deny headers.
// Defense-in-depth behind the loopback bind and bearer token; the headers are
// set before the inner handler runs so no path can skip them.
func TestSecurityHeaders(t *testing.T) {
	s := testServer()
	const wantCSP = "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
	cases := []struct {
		name, target, auth string
	}{
		{"index", "/", ""},
		{"api-authed", "/api/tasks", "Bearer secret"},
		{"api-forbidden", "/api/tasks", ""},
		{"not-found", "/nope.txt", ""},
	}
	for _, c := range cases {
		rec := do(t, s, "GET", c.target, "127.0.0.1:7878", c.auth)
		if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s: Content-Security-Policy = %q, want %q", c.name, got, wantCSP)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", c.name, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", c.name, got)
		}
	}
}

func TestNonLoopbackOriginRejected(t *testing.T) {
	s := testServer()
	// A cross-origin request (browser-set Origin) must be rejected even with a
	// valid token and loopback Host — CSRF defense-in-depth.
	req := httptest.NewRequest("GET", "/api/tasks", nil)
	req.Host = "127.0.0.1:7878"
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin = %d, want 403", rec.Code)
	}
}
