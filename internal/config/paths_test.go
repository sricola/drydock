package config

import (
	"path/filepath"
	"testing"
)

// APIKeysPath and EgressPath both hang off Dir() (~/.drydock). These lock the
// exact filenames callers depend on (the host-only key store and the egress
// allowlist) and the "" contract when the home directory is unresolvable.

func TestAPIKeysPath_And_EgressPath_UnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	wantDir := filepath.Join(home, ".drydock")
	if got, want := APIKeysPath(), filepath.Join(wantDir, "api-keys.env"); got != want {
		t.Errorf("APIKeysPath() = %q, want %q", got, want)
	}
	if got, want := EgressPath(), filepath.Join(wantDir, "egress.yaml"); got != want {
		t.Errorf("EgressPath() = %q, want %q", got, want)
	}
}

func TestPaths_EmptyWhenHomeUnresolvable(t *testing.T) {
	t.Setenv("HOME", "")
	if Dir() != "" {
		t.Skip("platform resolves a home dir without $HOME; the empty-home branch is unreachable here")
	}
	if got := APIKeysPath(); got != "" {
		t.Errorf("APIKeysPath() = %q, want empty when home is unresolvable", got)
	}
	if got := EgressPath(); got != "" {
		t.Errorf("EgressPath() = %q, want empty when home is unresolvable", got)
	}
}
