package main

import (
	"errors"
	"os"
	"path/filepath"
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
