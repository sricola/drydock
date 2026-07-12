package egress

import (
	"os"
	"path/filepath"
	"testing"
)

// An unknown egress key must be a hard error, not silently ignored.
func TestEgressLoad_RejectsUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "egress.yaml")
	if err := os.WriteFile(p, []byte("bogus_unknown_key: 1\ndefault:\n  domains: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("egress Load accepted an unknown key; want a parse error")
	}
}

// The shipped egress config must load under strict decoding.
func TestEgressLoad_ShippedConfigLoadsStrict(t *testing.T) {
	if _, err := Load("../../config/egress.yaml"); err != nil {
		t.Fatalf("shipped config/egress.yaml rejected under KnownFields(true): %v", err)
	}
}
