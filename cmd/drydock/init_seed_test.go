package main

import (
	"os"
	"path/filepath"
	"testing"
)

// `drydock init` creates ~/.drydock (0700) and seeds config.yaml and
// egress.yaml, and a second run never overwrites the operator's edits
// (configuration.md: "never overwrites existing copies").
func TestEnsureUserConfig_SeedsOnceAndPreservesEdits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	ensureUserConfig()
	dir := filepath.Join(home, ".drydock")
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("~/.drydock not created: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("~/.drydock mode %o, want 0700", fi.Mode().Perm())
	}
	for _, name := range []string{"config.yaml", "egress.yaml"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || len(b) == 0 {
			t.Fatalf("%s not seeded (err %v, %d bytes)", name, err, len(b))
		}
	}
	// Operator edits, then a second init.
	for _, name := range []string{"config.yaml", "egress.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# operator edit: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ensureUserConfig()
	for _, name := range []string{"config.yaml", "egress.yaml"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if string(b) != "# operator edit: "+name+"\n" {
			t.Errorf("%s was overwritten by a second init: %q", name, b)
		}
	}
}
