package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/config"
)

func TestChooseLogHandler(t *testing.T) {
	cases := []struct {
		name       string
		jsonForced bool
		isTTY      bool
		want       string // "*slog.JSONHandler" or "*slog.TextHandler"
	}{
		{"json forced on a tty wins", true, true, "*slog.JSONHandler"},
		{"non-tty defaults to json", false, false, "*slog.JSONHandler"},
		{"tty without force gets text", false, true, "*slog.TextHandler"},
		{"json forced off a tty", true, false, "*slog.JSONHandler"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := chooseLogHandler(&bytes.Buffer{}, tc.jsonForced, tc.isTTY)
			if got := typeName(h); got != tc.want {
				t.Errorf("chooseLogHandler(json=%v, tty=%v) = %s, want %s",
					tc.jsonForced, tc.isTTY, got, tc.want)
			}
		})
	}
}

func typeName(h slog.Handler) string {
	switch h.(type) {
	case *slog.JSONHandler:
		return "*slog.JSONHandler"
	case *slog.TextHandler:
		return "*slog.TextHandler"
	default:
		return "unknown"
	}
}

// TestListen_UnixSocketPermissions verifies that listen() creates a Unix socket
// with mode 0600 and ensures its parent directory is mode 0700. This pins the
// security boundary: no other local user can open the broker socket.
// The umask(0o077) + explicit chmod both defended here.
//
// Note: Unix socket path names are limited to 104 bytes on macOS (UNIX_PATH_MAX).
// We use /tmp directly (rather than os.TempDir() which expands to the long
// /var/folders/... form on macOS) to keep the path under that limit.
func TestListen_UnixSocketPermissions(t *testing.T) {
	// Use /tmp so the path stays under macOS's 104-byte UNIX_PATH_MAX limit.
	// EnsureParent creates "sub" at 0700, proving the wired call chain works.
	tmpDir, err := os.MkdirTemp("/tmp", "brd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	sockDir := filepath.Join(tmpDir, "sub")
	sock := filepath.Join(sockDir, "test.sock")

	cfg := config.Defaults()
	cfg.Broker.Addr = ""     // force Unix-socket path
	cfg.Broker.Socket = sock // override default per-uid path

	l, sockPath := listen(cfg, "127.0.0.1:18088", "127.0.0.1:13128")
	defer l.Close()

	if sockPath != sock {
		t.Fatalf("sockPath = %q, want %q", sockPath, sock)
	}

	// Socket must be mode 0600: no group/world read, write, or execute.
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm = %04o, want 0600 (owner-only r/w)", perm)
	}

	// Parent directory must be 0700 (EnsureParent guarantees this).
	dirFi, err := os.Stat(sockDir)
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if perm := dirFi.Mode().Perm(); perm != 0o700 {
		t.Errorf("parent dir perm = %04o, want 0700", perm)
	}
}

// TestPruneOrphanTasks_Ordering verifies that the exec calls inside
// pruneOrphanTasks happen in the documented order: container ls first, then
// container delete for each found task-* container, then pkill for squid.
// The comment in the source says "ORDER MATTERS — do not reorder"; this test
// pins that invariant so a refactor can't silently swap the steps.
func TestPruneOrphanTasks_Ordering(t *testing.T) {
	var calls []string
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })

	runCmd = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		// Respond to "container ls" with a JSON fragment that contains one
		// task-* name; the parser scans fields so it tolerates any JSON shape.
		if name == "container" && len(args) > 0 && args[0] == "ls" {
			return []byte(`{"Names":["task-dead1234"]}`), nil
		}
		return nil, nil
	}

	stageRoot := t.TempDir()
	auditRoot := t.TempDir()
	pruneOrphanTasks(stageRoot, auditRoot)

	// Must have at least three calls: ls, delete, pkill.
	if len(calls) < 3 {
		t.Fatalf("expected ≥3 runCmd calls, got %d: %v", len(calls), calls)
	}

	// First call must be the listing.
	if !strings.HasPrefix(calls[0], "container ls") {
		t.Errorf("call[0] = %q, want prefix %q", calls[0], "container ls")
	}

	// Locate delete and pkill positions.
	deleteIdx, pkillIdx := -1, -1
	for i, c := range calls {
		if strings.Contains(c, "container delete") && strings.Contains(c, "task-dead1234") {
			deleteIdx = i
		}
		if strings.HasPrefix(c, "pkill") {
			pkillIdx = i
		}
	}

	if deleteIdx < 0 {
		t.Error("container delete --force task-dead1234 was never called")
	}
	if pkillIdx < 0 {
		t.Error("pkill -f squid was never called")
	}
	// ORDER MATTERS: delete must precede pkill (and both precede stage reap,
	// which is not an exec call but follows in source order).
	if deleteIdx >= 0 && pkillIdx >= 0 && deleteIdx > pkillIdx {
		t.Errorf("container delete (pos %d) must happen before pkill (pos %d); "+
			"a live container may hold its stage dir open", deleteIdx, pkillIdx)
	}
}

func TestResolveAPIKey_Precedence(t *testing.T) {
	file := map[string]string{"ANTHROPIC_API_KEY": "from-file"}

	t.Run("non-empty env overrides file", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "from-env")
		if got := resolveAPIKey("ANTHROPIC_API_KEY", file); got != "from-env" {
			t.Errorf("got %q, want from-env", got)
		}
	})
	t.Run("empty env falls through to file", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := resolveAPIKey("ANTHROPIC_API_KEY", file); got != "from-file" {
			t.Errorf("got %q, want from-file", got)
		}
	})
	t.Run("unset env + no file entry yields empty", func(t *testing.T) {
		if got := resolveAPIKey("OPENAI_API_KEY", file); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
