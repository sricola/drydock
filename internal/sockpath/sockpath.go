// Package sockpath computes drydock's default Unix socket path. brokerd and
// the CLI must agree on this value or `drydock approve`/`status`/`tasks` all
// break. Per-uid pathing prevents one local user from colliding with another's
// socket, and the parent directory is created at 0700 so the socket itself
// can't be opened by another local process during the (small) window between
// bind() and chmod() in serve().
package sockpath

import (
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
func EnsureParent(sock string) error {
	return os.MkdirAll(filepath.Dir(sock), 0o700)
}
