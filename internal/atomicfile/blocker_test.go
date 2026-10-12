package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

// Write clears a leftover FILE or symlink at its own temp path, but it never
// removes a directory: a directory there was not left by any writer, and
// deleting things it did not create is not atomicfile's business. The write
// fails and the target is left exactly as it was. (internal/broker's tests
// rely on this to make one marker's write fail deterministically.)
func TestWrite_DoesNotRemoveDirectoryAtTempPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "item.json")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(TempPath(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, []byte("new"), 0o600); err == nil {
		t.Fatal("Write succeeded past a directory at the temp path; want an error")
	}
	got, _ := os.ReadFile(p)
	if string(got) != "old" {
		t.Errorf("target changed to %q after a failed write", got)
	}
	if fi, err := os.Stat(TempPath(p)); err != nil || !fi.IsDir() {
		t.Errorf("the directory at the temp path was removed (%v, %v)", fi, err)
	}
}
