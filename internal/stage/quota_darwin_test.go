//go:build darwin

package stage

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireHdiutil(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("no hdiutil on PATH")
	}
}

// The quota is a hard wall: writes past the image size fail with ENOSPC and
// the backing sparse file never grows past quota plus APFS overhead.
func TestQuota_HardBoundENOSPC(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const quota = int64(64 << 20)
	if err := AttachQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = teardownQuota(root) })

	chunk := make([]byte, 1<<20)
	var wrote int64
	var werr error
	for i := 0; i < 96; i++ { // try to write 96 MiB into a 64 MiB image
		werr = os.WriteFile(filepath.Join(root, "fill"+string(rune('a'+i%26))+string(rune('a'+i/26))), chunk, 0o600)
		if werr != nil {
			break
		}
		wrote += int64(len(chunk))
	}
	if werr == nil {
		t.Fatalf("wrote %d MiB into a 64 MiB quota with no error; the bound is not hard", wrote>>20)
	}
	if fi, err := os.Stat(QuotaImagePath(root)); err == nil && fi.Size() > quota+(32<<20) {
		t.Errorf("backing image grew to %d MiB, want <= quota + 32 MiB slack", fi.Size()>>20)
	}
}

// teardownQuota detaches and deletes the image; calling it again, or on a
// plain quota-less dir, is a no-op.
func TestQuota_TeardownIdempotent(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AttachQuota(root, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := teardownQuota(root); err != nil {
		t.Fatalf("first teardown: %v", err)
	}
	if _, err := os.Lstat(QuotaImagePath(root)); !os.IsNotExist(err) {
		t.Error("backing image survived teardown")
	}
	if isMounted(root) {
		t.Error("root still mounted after teardown")
	}
	if err := teardownQuota(root); err != nil {
		t.Errorf("second teardown: %v, want no-op nil", err)
	}
	plain := t.TempDir()
	if err := teardownQuota(plain); err != nil {
		t.Errorf("teardown of a plain dir: %v, want no-op nil", err)
	}
}

// Cleanup on a quota-backed stage detaches the image before removing the
// root, and removes the backing file too.
func TestQuota_CleanupRemovesImage(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AttachQuota(root, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Stage{Root: root}).Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Error("stage root survived Cleanup")
	}
	if _, err := os.Lstat(QuotaImagePath(root)); !os.IsNotExist(err) {
		t.Error("quota image survived Cleanup")
	}
}

// A detached image (host reboot) is re-attached by Reopen so an
// awaiting-approval task resumes with its work tree intact.
func TestQuota_ReopenReattaches(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AttachQuota(root, 64<<20); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"work", "git"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "work", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runHdiutil("detach", root); err != nil { // simulate reboot
		t.Fatal(err)
	}
	st, err := Reopen(root)
	if err != nil {
		t.Fatalf("Reopen after detach: %v", err)
	}
	t.Cleanup(func() { _ = st.Cleanup() })
	if _, err := os.Stat(filepath.Join(root, "work", "f")); err != nil {
		t.Errorf("work tree content missing after reattach: %v", err)
	}
}

// ReapOrphans detaches and deletes orphaned quota stages, and preserves
// (still mounted) the ones named in keep.
func TestQuota_ReapOrphansDetaches(t *testing.T) {
	requireHdiutil(t)
	parent := t.TempDir()
	orphan := filepath.Join(parent, "aaaa")
	kept := filepath.Join(parent, "bbbb")
	for _, r := range []string{orphan, kept} {
		if err := os.MkdirAll(r, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := AttachQuota(r, 64<<20); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = teardownQuota(kept); _ = teardownQuota(orphan) })
	if _, err := ReapOrphans(parent, map[string]bool{"bbbb": true}); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Error("orphan stage dir survived the reap")
	}
	if _, err := os.Lstat(QuotaImagePath(orphan)); !os.IsNotExist(err) {
		t.Error("orphan quota image survived the reap")
	}
	if !isMounted(kept) {
		t.Error("kept stage was unmounted by the reap; resume would find it empty")
	}
}
