package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A misspelled security control must fail startup, not silently no-op to a
// weaker default (an operator who typos aggregate_budget_usd would otherwise run
// unattended with no cap and no error).
func TestLoad_RejectsUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("aggregate_budget_uds: 100.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("Load accepted a misspelled field (aggregate_budget_uds); want a parse error")
	}
}

func TestLoad_AcceptsValidConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("aggregate_budget_usd: 100.0\ntask_budget_usd: 5.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load rejected a valid config: %v", err)
	}
}

// The shipped config must load under strict decoding: this fails if the on-disk
// config/config.yaml ever drifts a key the struct does not have.
func TestLoad_ShippedConfigLoadsStrict(t *testing.T) {
	if _, err := Load("../../config/config.yaml"); err != nil {
		t.Fatalf("shipped config/config.yaml rejected under KnownFields(true): %v", err)
	}
}
