package brokerclient_test

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/brokerclient"
	"drydock/internal/sockpath"
)

// serveUnix starts a minimal HTTP server on a temp unix socket and returns
// a BrokerDial function for it plus a cleanup.
func serveUnix(t *testing.T) func() (net.Conn, error) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "broker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "unix-ok") //nolint:errcheck
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return func() (net.Conn, error) { return net.Dial("unix", sock) }
}

// TestNewInjecteddialFn verifies that when a dialFn is supplied, New returns a
// unix-socket client ("http://brokerd" base) that actually reaches the server,
// regardless of BROKER_ADDR being set.
func TestNewInjectedDialFn(t *testing.T) {
	t.Setenv("BROKER_ADDR", "") // ensure env does not interfere
	dial := serveUnix(t)
	c, base := brokerclient.New(dial, 3*time.Second)
	if base != "http://brokerd" {
		t.Fatalf("base = %q, want %q", base, "http://brokerd")
	}
	resp, err := c.Get(base + "/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "unix-ok" {
		t.Fatalf("body = %q, want %q", string(body), "unix-ok")
	}
}

// TestNewBROKER_ADDR verifies that when BROKER_ADDR is set and dialFn is nil,
// New returns a plain TCP client with the env-derived base URL.
func TestNewBROKER_ADDR(t *testing.T) {
	t.Setenv("BROKER_ADDR", "127.0.0.1:19999")
	c, base := brokerclient.New(nil, 5*time.Second)
	if base != "http://127.0.0.1:19999" {
		t.Fatalf("base = %q, want %q", base, "http://127.0.0.1:19999")
	}
	// The client must be a plain TCP client: its Transport must NOT have a
	// custom DialContext (if BROKER_ADDR is set we use default http.Transport).
	if c.Transport != nil {
		t.Errorf("BROKER_ADDR branch must use default transport (nil Transport), got %T", c.Transport)
	}
}

// TestNewUnixFallback verifies that when neither BROKER_ADDR nor a dialFn is
// set, New returns a unix-socket client with a custom Transport.
func TestNewUnixFallback(t *testing.T) {
	t.Setenv("BROKER_ADDR", "")
	t.Setenv("BROKER_SOCKET", filepath.Join(t.TempDir(), "nosuchsocket.sock"))
	c, base := brokerclient.New(nil, 5*time.Second)
	if base != "http://brokerd" {
		t.Fatalf("base = %q, want %q", base, "http://brokerd")
	}
	if c.Transport == nil {
		t.Fatal("unix fallback must have a custom Transport")
	}
}

// TestNewZeroTimeout verifies that timeout=0 leaves client.Timeout at zero
// (no deadline) on both the TCP and unix-socket paths.
func TestNewZeroTimeout(t *testing.T) {
	// TCP path.
	t.Setenv("BROKER_ADDR", "127.0.0.1:19999")
	c, _ := brokerclient.New(nil, 0)
	if c.Timeout != 0 {
		t.Errorf("TCP zero-timeout: got %v, want 0", c.Timeout)
	}
	// Unix path via injected dialFn.
	t.Setenv("BROKER_ADDR", "")
	dial := func() (net.Conn, error) { return nil, os.ErrNotExist }
	c2, _ := brokerclient.New(dial, 0)
	if c2.Timeout != 0 {
		t.Errorf("unix zero-timeout: got %v, want 0", c2.Timeout)
	}
}

// TestResolveAddr covers the BROKER_ADDR env and the absent case.
func TestResolveAddr(t *testing.T) {
	t.Setenv("BROKER_ADDR", "localhost:1234")
	if got := brokerclient.ResolveAddr(); got != "localhost:1234" {
		t.Errorf("ResolveAddr = %q, want %q", got, "localhost:1234")
	}
	t.Setenv("BROKER_ADDR", "")
	// No config file → should return "".
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // ensure clean config lookup
	if got := brokerclient.ResolveAddr(); got != "" {
		t.Errorf("empty env: ResolveAddr = %q, want empty", got)
	}
}

// TestResolveSocketPath covers the BROKER_SOCKET env override.
func TestResolveSocketPath(t *testing.T) {
	want := "/tmp/custom-drydock.sock"
	t.Setenv("BROKER_SOCKET", want)
	if got := brokerclient.ResolveSocketPath(); got != want {
		t.Errorf("ResolveSocketPath = %q, want %q", got, want)
	}
}

func TestResolversReadConfigWhenEnvironmentIsUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BROKER_ADDR", "")
	t.Setenv("BROKER_SOCKET", "")

	configDir := filepath.Join(home, ".drydock")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := "broker:\n  addr: 127.0.0.1:8765\n  socket: /tmp/from-config.sock\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := brokerclient.ResolveAddr(); got != "127.0.0.1:8765" {
		t.Errorf("ResolveAddr() = %q, want config value", got)
	}
	if got := brokerclient.ResolveSocketPath(); got != "/tmp/from-config.sock" {
		t.Errorf("ResolveSocketPath() = %q, want config value", got)
	}
}

func TestResolveSocketPathFallsBackToPerUserDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BROKER_ADDR", "")
	t.Setenv("BROKER_SOCKET", "")

	if got, want := brokerclient.ResolveSocketPath(), sockpath.Default(); got != want {
		t.Errorf("ResolveSocketPath() = %q, want default %q", got, want)
	}
}

func TestNewUsesConfigAddr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BROKER_ADDR", "")
	t.Setenv("BROKER_SOCKET", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tcp-config-ok")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	configDir := filepath.Join(home, ".drydock")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := "broker:\n  addr: " + ln.Addr().String() + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	c, base := brokerclient.New(nil, 2*time.Second)
	if base != "http://"+ln.Addr().String() {
		t.Fatalf("base = %q, want config TCP address", base)
	}
	resp, err := c.Get(base + "/ping")
	if err != nil {
		t.Fatalf("GET through config address: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tcp-config-ok" {
		t.Errorf("body = %q, want tcp-config-ok", body)
	}
}
