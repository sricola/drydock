package main

import (
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
