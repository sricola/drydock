package webui

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Server struct {
	AuditRoot  string
	Token      string
	BrokerDial func() (net.Conn, error)
}

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validID(id string) bool { return idRe.MatchString(id) }

// Handler wires the SPA and the /api surface. Proxy/audit/submit handlers are
// added in later tasks; here they are stubs so the mux + middleware are testable.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(assetsFS, "assets")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authed(h)) }
	stub := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
	api("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, "GET", "/admin/tasks") })
	api("GET /api/pending", func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, "GET", "/admin/pending") })
	api("POST /api/approve/{id}", s.signalHandler("approve"))
	api("POST /api/deny/{id}", s.signalHandler("deny"))
	api("POST /api/kill/{id}", s.signalHandler("kill"))
	api("POST /api/submit", stub)
	api("GET /api/diff/{id}", stub)
	api("GET /api/logs/{id}", stub)
	api("GET /api/widen/{id}", stub)
	api("GET /api/history", stub)
	return mux
}

// brokerClient returns an http.Client that dials brokerd's unix socket. Used
// for the short admin pokes (5s timeout). base is the dummy host the dialer
// ignores. Submit (Task 6) uses a separate no-timeout client.
func (s *Server) brokerClient() (*http.Client, string) {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return s.BrokerDial() },
		},
	}, "http://brokerd"
}

// proxy forwards the request to brokerd at adminPath and copies status+body back.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, method, adminPath string) {
	if s.BrokerDial == nil {
		http.Error(w, "brokerd not running — run `drydock start`", http.StatusBadGateway)
		return
	}
	c, base := s.brokerClient()
	req, err := http.NewRequestWithContext(r.Context(), method, base+adminPath, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp, err := c.Do(req)
	if err != nil {
		http.Error(w, "brokerd not running — run `drydock start`", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

// signalHandler builds an approve/deny/kill handler for the given verb.
func (s *Server) signalHandler(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID(id) {
			http.Error(w, "bad task id", http.StatusBadRequest)
			return
		}
		s.proxy(w, r, "POST", "/admin/"+verb+"/"+id)
	}
}

// authed enforces the loopback Host allowlist, an optional cross-origin Origin
// check, and the bearer token (unless --no-token). It is the only auth boundary
// for /api/*; the token is never read from a cookie (cookies are CSRF-forgeable).
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !loopbackOrigin(o) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if s.Token != "" {
			if r.Header.Get("Authorization") != "Bearer "+s.Token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func loopbackHost(hostport string) bool {
	h := hostname(hostport)
	return h == "127.0.0.1" || h == "localhost" || h == "[::1]" || h == "::1"
}

func loopbackOrigin(origin string) bool {
	// Origin is scheme://host[:port]; check the host component is loopback.
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	return loopbackHost(origin[i+3:])
}
