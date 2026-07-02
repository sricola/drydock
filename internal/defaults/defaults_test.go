package defaults

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEgressYAMLMatchesSource verifies that the embedded egress.yaml is
// byte-equal to the canonical config/egress.yaml at the repo root. This test
// is the tripwire that replaces the old "Must stay in sync" comment: if you
// update config/egress.yaml without updating internal/defaults/egress.yaml,
// this test fails.
func TestEgressYAMLMatchesSource(t *testing.T) {
	// This file is at internal/defaults/defaults_test.go;
	// the repo root is two directories up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	source := filepath.Join(repoRoot, "config", "egress.yaml")

	sourceBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read config/egress.yaml: %v", err)
	}

	if !bytes.Equal(EgressYAML, sourceBytes) {
		t.Errorf("internal/defaults/egress.yaml differs from config/egress.yaml\n"+
			"Run: cp config/egress.yaml internal/defaults/egress.yaml\n"+
			"embedded len=%d source len=%d", len(EgressYAML), len(sourceBytes))
	}
}

// TestEgressYAMLNonEmpty verifies the embedded bytes are non-empty and start
// with "version:" so a botched embed is caught immediately.
func TestEgressYAMLNonEmpty(t *testing.T) {
	if len(EgressYAML) == 0 {
		t.Fatal("EgressYAML is empty")
	}
	if !bytes.HasPrefix(EgressYAML, []byte("version:")) {
		t.Errorf("EgressYAML does not start with 'version:'; first bytes: %q", EgressYAML[:min(20, len(EgressYAML))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
