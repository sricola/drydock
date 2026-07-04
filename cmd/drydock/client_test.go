package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSocketPath_HonorsConfig verifies that socketPath reads broker.socket from
// config.yaml when BROKER_SOCKET is not set, matching the resolution order in
// brokerd (cfg.Broker.Socket before sockpath.Default()).
func TestSocketPath_HonorsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BROKER_SOCKET", "") // ensure env override is off

	cfgDir := filepath.Join(home, ".drydock")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"),
		[]byte("broker:\n  socket: /tmp/custom-test.sock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := socketPath()
	if got != "/tmp/custom-test.sock" {
		t.Errorf("socketPath() = %q, want /tmp/custom-test.sock (from config broker.socket)", got)
	}
}

// TestBrokerdDown_ConfigOnlyTCP verifies that a TCP broker configured only via
// config.yaml (broker.addr, no BROKER_ADDR env) is treated as TCP mode — so a
// dial failure does NOT get the misleading "brokerd not running, run drydock
// start" socket hint (which only applies to the unix-socket transport).
func TestBrokerdDown_ConfigOnlyTCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BROKER_ADDR", "") // config is the only source of the TCP addr

	cfgDir := filepath.Join(home, ".drydock")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"),
		[]byte("broker:\n  addr: 127.0.0.1:19999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if brokerdDown(errors.New("dial tcp 127.0.0.1:19999: connect: connection refused")) {
		t.Error("brokerdDown must be false in config-only TCP mode (no socket hint)")
	}
}

// TestPrintClientErr_BrokerDown_PrintsFriendlyHint verifies that when the
// broker socket is absent (brokerdDown=true), printClientErr substitutes the
// human-readable hint instead of the raw Go dial error. This is the primary
// UX gate: a first-time operator should see "start brokerd", not an opaque
// transport error.
func TestPrintClientErr_BrokerDown_PrintsFriendlyHint(t *testing.T) {
	// Point BROKER_SOCKET at a file that does not exist — os.Stat returns
	// IsNotExist, which is the signal brokerdDown uses.
	missing := filepath.Join(t.TempDir(), "no-such.sock")
	t.Setenv("BROKER_SOCKET", missing)
	t.Setenv("BROKER_ADDR", "")
	// Isolate from any real ~/.drydock/config.yaml that sets broker.addr.
	t.Setenv("HOME", t.TempDir())

	var buf bytes.Buffer
	orig := errOut
	t.Cleanup(func() { errOut = orig })
	errOut = &buf

	printClientErr(errors.New("dial unix " + missing + ": no such file or directory"))

	got := buf.String()
	if !strings.Contains(got, brokerDownHint) {
		t.Errorf("expected brokerDownHint %q in output; got: %q", brokerDownHint, got)
	}
}

// TestPrintClientErr_OtherError_PrintsRawError verifies that when brokerd IS
// reachable (or in TCP mode), printClientErr does not substitute the socket
// hint — it prints the raw error so the operator sees the actual problem.
func TestPrintClientErr_OtherError_PrintsRawError(t *testing.T) {
	// TCP mode (BROKER_ADDR set) — brokerdDown returns false immediately.
	t.Setenv("BROKER_ADDR", "127.0.0.1:19999")

	var buf bytes.Buffer
	orig := errOut
	t.Cleanup(func() { errOut = orig })
	errOut = &buf

	myErr := errors.New("unexpected HTTP 503")
	printClientErr(myErr)

	got := buf.String()
	if strings.Contains(got, brokerDownHint) {
		t.Errorf("must NOT print brokerDownHint for non-down TCP error; got: %q", got)
	}
	if !strings.Contains(got, myErr.Error()) {
		t.Errorf("raw error %q missing from output; got: %q", myErr.Error(), got)
	}
}
