package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"drydock/internal/config"
)

// In TCP mode a browser can reach the loopback listener via DNS rebinding, so
// the broker must reject a non-loopback Host and a non-loopback Origin (the
// browser-origin defense the web UI already applies). A legit CLI/curl over TCP
// sends a loopback Host and no Origin, so it must still pass.
func TestBrokerHandler_TCPModeRejectsRebinding(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	tcp := &config.Config{}
	tcp.Broker.Addr = "127.0.0.1:8799"
	h := brokerHandler(tcp, ok)

	cases := []struct {
		name, host, origin string
		want               int
	}{
		{"loopback host, no origin (legit CLI)", "127.0.0.1:8799", "", http.StatusOK},
		{"localhost host", "localhost:8799", "", http.StatusOK},
		{"loopback host, loopback origin", "127.0.0.1:8799", "http://127.0.0.1:8799", http.StatusOK},
		{"DNS-rebinding host", "attacker.example:8799", "", http.StatusForbidden},
		{"loopback host, hostile origin", "127.0.0.1:8799", "http://evil.example", http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "http://x/admin/approve/abc", nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: Host=%q Origin=%q -> %d, want %d", c.name, c.host, c.origin, rec.Code, c.want)
		}
	}
}

// In the default unix-socket mode the handler is unwrapped: the CLI addresses
// the socket with a non-loopback Host ("brokerd") that must pass, and a browser
// cannot reach a unix socket, so no Host/Origin guard is applied.
func TestBrokerHandler_UnixModeUnwrapped(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := brokerHandler(&config.Config{}, ok) // Broker.Addr == ""

	req := httptest.NewRequest("GET", "http://brokerd/admin/tasks", nil)
	req.Host = "brokerd"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("unix-mode handler rejected the CLI Host %q -> %d, want 200", req.Host, rec.Code)
	}
}
