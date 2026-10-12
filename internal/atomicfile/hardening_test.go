package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

// TempPath is per process: two processes writing the same credential file
// (brokerd and `drydock auth`) can never share a temp, so neither can rename
// the other's half-written inode into place.
func TestTempPath_IsPerProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cred.json")
	got := TempPath(p)
	if filepath.Dir(got) != filepath.Dir(p) {
		t.Errorf("temp must be a sibling (same filesystem for rename): %q", got)
	}
	if filepath.Ext(got) != ".tmp" || got == p+".tmp" {
		t.Errorf("temp %q must end in .tmp and carry a per-process component", got)
	}
}

// A symlink planted at this process's temp path is replaced, never followed:
// the credential bytes would otherwise land wherever the link points, and the
// rename would then move the LINK over the target.
func TestWrite_ReplacesSymlinkAtTempPathWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cred.json")
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, TempPath(p)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := Write(p, []byte("secret"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := os.ReadFile(elsewhere)
	if string(got) != "untouched" {
		t.Errorf("symlink target was written through: %q", got)
	}
	got, _ = os.ReadFile(p)
	if string(got) != "secret" {
		t.Errorf("target content = %q, want secret", got)
	}
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("target must be a regular file, got %v (%v)", fi, err)
	}
}

// A leftover at this process's own temp name can only be a crashed earlier
// process that happened to get the same pid (or a planted file): nothing
// live owns it, so it is replaced.
func TestWrite_ReplacesLeftoverAtOwnTempPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cred.json")
	if err := os.WriteFile(TempPath(p), []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, []byte("whole"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "whole" {
		t.Errorf("content = %q, want whole", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600 even though the leftover was 0644", fi.Mode().Perm())
	}
	if _, err := os.Stat(TempPath(p)); !os.IsNotExist(err) {
		t.Errorf("temp should not survive: %v", err)
	}
}

// Another process's temp (a different pid, or a crashed one) is not ours to
// touch and must not block us: Write succeeds and leaves it alone. Boot-time
// sweeps (RemoveStaleTemps) are where leftovers go.
func TestWrite_IgnoresAnotherProcessesTemp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cred.json")
	other := p + ".99999999.tmp"
	if err := os.WriteFile(other, []byte("in-flight elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, []byte("mine"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "mine" {
		t.Errorf("content = %q, want mine", got)
	}
	got, _ = os.ReadFile(other)
	if string(got) != "in-flight elsewhere" {
		t.Errorf("another process's temp was disturbed: %q", got)
	}
}

// RemoveStaleTemps clears every leftover for a path: the legacy fixed
// "<path>.tmp" an older drydock used and the per-process "<path>.<pid>.tmp"
// forms, without touching the target or unrelated siblings.
func TestRemoveStaleTemps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "item.json")
	for _, f := range []string{p, p + ".tmp", p + ".123.tmp", p + ".456.tmp", filepath.Join(dir, "item.json2")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveStaleTemps(p); err != nil {
		t.Fatalf("RemoveStaleTemps: %v", err)
	}
	for _, gone := range []string{p + ".tmp", p + ".123.tmp", p + ".456.tmp"} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should be removed (%v)", gone, err)
		}
	}
	for _, kept := range []string{p, filepath.Join(dir, "item.json2")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should be kept: %v", kept, err)
		}
	}
	if err := RemoveStaleTemps(p); err != nil {
		t.Errorf("second call must be a no-op, got %v", err)
	}
}
