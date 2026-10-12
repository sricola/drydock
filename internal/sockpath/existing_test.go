package sockpath

import (
	"os"
	"path/filepath"
	"testing"
)

// defaultParentUnder points Default() at a parent inside tmp by overriding
// TMPDIR (os.TempDir honors it), so the tests can exercise drydock's OWN
// per-uid directory without touching the real one.
func defaultParentUnder(t *testing.T, tmp string) (sock, parent string) {
	t.Helper()
	t.Setenv("TMPDIR", tmp)
	sock = Default()
	return sock, filepath.Dir(sock)
}

// The per-uid directory is the only thing standing between the admin socket
// and another local process during the bind-to-chmod window. If it already
// exists wider than 0700 (a stale one from an older build, or one pre-created
// by something else) it must be tightened, not silently accepted:
// os.MkdirAll leaves an existing directory's mode alone.
func TestEnsureParent_TightensExistingWideDefaultDir(t *testing.T) {
	sock, parent := defaultParentUnder(t, t.TempDir())
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureParent(sock); err != nil {
		t.Fatalf("EnsureParent: %v", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("existing parent mode = %o, want tightened to 0700", mode)
	}
}

// A symlink where drydock's own directory should be would let whoever planted
// it choose where the socket is created. Refuse it.
func TestEnsureParent_RefusesSymlinkedDefaultDir(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	sock, parent := defaultParentUnder(t, tmp)
	if err := os.Symlink(target, parent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := EnsureParent(sock); err == nil {
		t.Fatal("EnsureParent accepted a symlinked per-uid dir; want an error")
	}
}

// An operator-chosen socket path (broker.socket / BROKER_SOCKET) can live in
// a directory drydock does not own, such as /tmp or a shared ~/run. Its mode
// is the operator's business: never chmod it, never refuse it.
func TestEnsureParent_LeavesOperatorChosenParentAlone(t *testing.T) {
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "run")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureParent(filepath.Join(parent, "custom.sock")); err != nil {
		t.Fatalf("EnsureParent: %v", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Errorf("operator-owned parent mode = %o, want left at 0755", mode)
	}
}
