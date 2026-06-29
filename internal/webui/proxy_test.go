package webui

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

func TestProxyRejectsBadID(t *testing.T) {
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.NotFoundHandler())}
	rec := do(t, s, "POST", "/api/approve/NOTHEX", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", rec.Code)
	}
}
