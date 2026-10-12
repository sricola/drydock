package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_CreatesWithPermAndContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cred.json")
	if err := Write(p, []byte("secret"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Errorf("content = %q, want %q", got, "secret")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	// No sibling temp is left behind after a successful write.
	if left, _ := filepath.Glob(p + ".*tmp"); len(left) != 0 {
		t.Errorf("temp files survived a successful write: %v", left)
	}
}

func TestWrite_ReplacesExistingAtomically(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cred.json")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, []byte("new"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "new" {
		t.Errorf("content = %q, want overwritten to %q", got, "new")
	}
}

func TestWrite_ErrorsWhenDirMissing_LeavesNoPartial(t *testing.T) {
	// A path under a non-existent directory: creating the .tmp fails, so
	// Write returns the error and never renames a partial file into place.
	p := filepath.Join(t.TempDir(), "nope", "cred.json")
	if err := Write(p, []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error writing under a missing directory")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("target must not exist after a failed write: %v", err)
	}
}
