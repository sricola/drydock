package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirstRunBeforeInit(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if !firstRunBeforeInit(cfg) {
		t.Fatal("absent config must read as first run")
	}
	if err := os.WriteFile(cfg, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if firstRunBeforeInit(cfg) {
		t.Fatal("present config must NOT read as first run")
	}
}
