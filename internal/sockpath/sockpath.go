// Package sockpath computes drydock's default Unix socket path. brokerd and
// the CLI must agree on this value or `drydock approve`/`status`/`tasks` all
// break. Per-uid pathing prevents one local user from colliding with another's
// socket, and the parent directory is created at 0700 so the socket itself
// can't be opened by another local process during the (small) window between
// bind() and chmod() in serve().
package sockpath

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// Default returns the per-uid default socket path. The parent directory is
// not created here — the caller (brokerd in serve(), the CLI in dial())
// decides whether to create or just compute. Callers should respect a
// BROKER_SOCKET env override.
func Default() string {
	uid := strconv.Itoa(os.Getuid())
	if u, err := user.Current(); err == nil && u.Uid != "" {
		uid = u.Uid
	}
	return filepath.Join(os.TempDir(), "drydock-"+uid, "drydock.sock")
}

// EnsureParent makes the socket's parent directory at mode 0700. brokerd
// calls this before listen(); the CLI doesn't need to.
//
// For drydock's OWN per-uid directory (the parent of Default()) it also
// enforces what MkdirAll does not: an existing directory is tightened to
// 0700 (MkdirAll leaves an existing mode alone), and a symlink in its place
// is refused, since whoever planted it would otherwise choose where the
// admin socket is created. An operator-chosen path (config `broker.socket`
// / BROKER_SOCKET) may sit in a directory drydock does not own, such as
// /tmp; its mode is the operator's business and is left alone.
func EnsureParent(sock string) error {
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if dir != filepath.Dir(Default()) {
		return nil
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sockpath: %s is a symlink; refusing to create the socket under it", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("sockpath: %s is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
