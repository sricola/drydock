package netfw

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A stale pid file whose PID is dead must be removed so squid can start clean
// (a hard-killed broker leaves squid's pid file behind).
func TestReapStaleSquid_RemovesDeadPid(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "squid.pid")
	// PID 1 is init/launchd, never squid; use an almost-certainly-dead high pid.
	if err := os.WriteFile(pidPath, []byte("2147483646\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reapStaleSquid(dir); err != nil {
		t.Fatalf("reapStaleSquid returned error for a dead pid: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stale pid file should have been removed")
	}
}

// A garbage pid file is treated as stale and removed.
func TestReapStaleSquid_RemovesGarbagePid(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "squid.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reapStaleSquid(dir); err != nil {
		t.Fatalf("reapStaleSquid error: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("garbage pid file should have been removed")
	}
}

// No pid file → no-op, no error.
func TestReapStaleSquid_NoFile(t *testing.T) {
	if err := reapStaleSquid(t.TempDir()); err != nil {
		t.Fatalf("reapStaleSquid on empty dir: %v", err)
	}
}

// A pid file pointing at a LIVE process that is NOT squid (pid reuse) is stale
// and removed — we must not error or kill an unrelated process. The test's own
// pid is live and is not squid.
func TestReapStaleSquid_LiveNonSquidPidIsStale(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "squid.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reapStaleSquid(dir); err != nil {
		t.Fatalf("reapStaleSquid should treat a live non-squid pid as stale, got: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("pid file for a live non-squid process should have been removed as stale")
	}
}

// Stop removes the pid file so the next start isn't tripped by a leftover.
func TestSquidStop_RemovesPidFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "squid.pid")
	if err := os.WriteFile(pidPath, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Squid{runDir: dir} // no live process; Stop should still clean the pid file
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("Stop should remove the squid.pid file")
	}
}
