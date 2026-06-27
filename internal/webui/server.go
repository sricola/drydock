package webui

import (
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strings"
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
	api("GET /api/tasks", stub)
	api("GET /api/pending", stub)
	api("POST /api/approve/{id}", stub)
	api("POST /api/deny/{id}", stub)
	api("POST /api/kill/{id}", stub)
	api("POST /api/submit", stub)
	api("GET /api/diff/{id}", stub)
	api("GET /api/logs/{id}", stub)
	api("GET /api/widen/{id}", stub)
	api("GET /api/history", stub)
	return mux
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
