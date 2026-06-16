package sockpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault_PerUid(t *testing.T) {
	got := Default()
	// Must live under TMPDIR.
	tmp := os.TempDir()
	if !strings.HasPrefix(got, tmp) {
		t.Errorf("default %q must live under TMPDIR %q", got, tmp)
	}
	// Per-uid parent — defends against another local user colliding.
	if !strings.Contains(filepath.Dir(got), "drydock-") {
		t.Errorf("default %q must have a drydock-<uid> parent", got)
	}
	if filepath.Base(got) != "drydock.sock" {
		t.Errorf("default basename = %q", filepath.Base(got))
	}
}

func TestEnsureParent_MakesDirMode0700(t *testing.T) {
	tmp := t.TempDir()
	sock := filepath.Join(tmp, "child", "drydock.sock")
	if err := EnsureParent(sock); err != nil {
		t.Fatalf("EnsureParent: %v", err)
	}
	info, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("parent mode = %o, want 0700", mode)
	}
}

func TestEnsureParent_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	sock := filepath.Join(tmp, "child", "drydock.sock")
	if err := EnsureParent(sock); err != nil {
		t.Fatal(err)
	}
	if err := EnsureParent(sock); err != nil {
		t.Errorf("second call must succeed: %v", err)
	}
}
