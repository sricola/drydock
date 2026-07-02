package webui

import (
	"crypto/subtle"
	"io"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"drydock/internal/brokerclient"
)

type Server struct {
	AuditRoot  string
	Token      string
	BrokerDial func() (net.Conn, error)

	// broker is the cached 5s-timeout client for short admin polls.
	// brokerNoTimeout is used by handleSubmit (tasks can run for 30+ min).
	// Both are initialised once in Handler() from BrokerDial.
	broker          *http.Client
	brokerBase      string
	brokerNoTimeout *http.Client
}

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validID(id string) bool { return idRe.MatchString(id) }

// Handler wires the SPA and the /api surface. It also initialises the cached
// broker HTTP clients so that proxy calls reuse a single transport rather than
// allocating a fresh one per request.
func (s *Server) Handler() http.Handler {
	// Build the broker clients once. If BrokerDial is nil (e.g. in unit tests
	// that only exercise auth/routing, not proxying) we leave them nil and the
	// proxy helper's nil-check handles it gracefully.
	if s.BrokerDial != nil {
		s.broker, s.brokerBase = brokerclient.New(s.BrokerDial, 5*time.Second)
		s.brokerNoTimeout, _ = brokerclient.New(s.BrokerDial, 0)
	}

	mux := http.NewServeMux()

	sub, _ := fs.Sub(assetsFS, "assets")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authed(h)) }
	api("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, "GET", "/admin/tasks") })
	api("GET /api/pending", func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, "GET", "/admin/pending") })
	api("POST /api/approve/{id}", s.signalHandler("approve"))
	api("POST /api/deny/{id}", s.signalHandler("deny"))
	api("POST /api/kill/{id}", s.signalHandler("kill"))
	api("POST /api/submit", s.handleSubmit)
	api("GET /api/diff/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.serveAuditFile(w, r, ".diff", "text/plain; charset=utf-8")
	})
	api("GET /api/logs/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.serveAuditFile(w, r, ".jsonl", "text/plain; charset=utf-8")
	})
	api("GET /api/widen/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.serveAuditFile(w, r, ".widen.json", "application/json")
	})
	api("GET /api/history", s.handleHistory)
	return mux
}

// proxy forwards the request to brokerd at adminPath and copies status+body back.
// It uses the cached 5s-timeout broker client built once in Handler().
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, method, adminPath string) {
	if s.broker == nil {
		http.Error(w, "brokerd not running — run `drydock start`", http.StatusBadGateway)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), method, s.brokerBase+adminPath, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp, err := s.broker.Do(req)
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
			// Use constant-time comparison to prevent timing side-channels on
			// token validation; the UI token is a bearer secret and must not leak
			// its value through response latency differences.
			got := r.Header.Get("Authorization")
			want := "Bearer " + s.Token
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
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
