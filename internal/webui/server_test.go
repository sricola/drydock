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
