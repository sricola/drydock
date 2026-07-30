package webui

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBroker serves on a temp unix socket and records the last request.
func fakeBroker(t *testing.T, h http.Handler) func() (net.Conn, error) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.Remove(sock) })
	return func() (net.Conn, error) { return net.Dial("unix", sock) }
}

func TestProxyTasks(t *testing.T) {
	dial := fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tasks" {
			t.Errorf("brokerd path = %s, want /admin/tasks", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":"x"}]`)
	}))
	s := &Server{Token: "secret", BrokerDial: dial}
	rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusOK || rec.Body.String() != `[{"id":"x"}]` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestProxyApprovePostsToAdmin(t *testing.T) {
	var gotPath, gotMethod string
	dial := fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	s := &Server{Token: "secret", BrokerDial: dial}
	id := "0123456789abcdef0123456789abcdef"
	rec := do(t, s, "POST", "/api/approve/"+id, "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if gotMethod != "POST" || gotPath != "/admin/approve/"+id {
		t.Fatalf("brokerd got %s %s", gotMethod, gotPath)
	}
}

// TestProxyApproveForwardsAckBody: the web UI's acknowledged approve sends
// {"acknowledge":[...]} — the proxy must thread that body (and its
// Content-Type) through to brokerd's /admin/approve, where the second-look
// validation lives.
func TestProxyApproveForwardsAckBody(t *testing.T) {
	var gotBody []byte
	var gotCT string
	dial := fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	s := &Server{Token: "secret", BrokerDial: dial}
	id := "0123456789abcdef0123456789abcdef"
	ackJSON := `{"acknowledge":["ci-workflow","lockfile"]}`

	req := httptest.NewRequest("POST", "/api/approve/"+id, strings.NewReader(ackJSON))
	req.Host = "127.0.0.1:7878"
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if string(gotBody) != ackJSON {
		t.Errorf("brokerd body = %q, want %q", gotBody, ackJSON)
	}
	if gotCT != "application/json" {
		t.Errorf("brokerd Content-Type = %q, want application/json", gotCT)
	}
}

// TestProxyApprove422PassesThrough: brokerd's second-look refusal (422 +
// JSON naming the missing categories) must reach the browser unmodified so
// the UI can toast the category names.
func TestProxyApprove422PassesThrough(t *testing.T) {
	refusal := `{"error":"approval refused","missing":["ci-workflow"],"required":["ci-workflow"]}`
	dial := fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, refusal)
	}))
	s := &Server{Token: "secret", BrokerDial: dial}
	id := "0123456789abcdef0123456789abcdef"
	rec := do(t, s, "POST", "/api/approve/"+id, "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if rec.Body.String() != refusal {
		t.Errorf("body = %q, want the refusal passed through", rec.Body.String())
	}
}

// TestProxyBodyCap: the forwarded body is capped — an abusive oversized
// "ack list" is refused at the UI boundary, never proxied. (Short test name
// keeps t.TempDir()'s unix-socket path under the sockaddr length limit.)
func TestProxyBodyCap(t *testing.T) {
	brokerHit := false
	dial := fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokerHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	s := &Server{Token: "secret", BrokerDial: dial}
	id := "0123456789abcdef0123456789abcdef"
	big := strings.Repeat("x", 64<<10)

	req := httptest.NewRequest("POST", "/api/approve/"+id, strings.NewReader(big))
	req.Host = "127.0.0.1:7878"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if brokerHit {
		t.Error("oversize body must not reach brokerd")
	}
}

func TestProxyRejectsBadID(t *testing.T) {
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.NotFoundHandler())}
	rec := do(t, s, "POST", "/api/approve/NOTHEX", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", rec.Code)
	}
}
