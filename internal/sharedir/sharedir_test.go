package sharedir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocate_CWD verifies that Locate finds a file via the CWD / repo-relative
// candidate (the last fallback). We create a temp file at a relative path and
// set the working directory to the temp dir.
func TestLocate_CWD(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "config")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sub, "egress.yaml")
	if err := os.WriteFile(want, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Change working directory so the CWD candidate resolves.
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint:errcheck

	got, err := Locate("config/egress.yaml")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	// Use os.SameFile to compare: the returned path may be relative and on
	// macOS the temp dir path may contain symlinks (/var → /private/var),
	// so string comparison of absolute paths can fail when Getwd resolves them.
	gotFi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("Stat(%q): %v", got, err)
	}
	wantFi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("Stat(%q): %v", want, err)
	}
	if !os.SameFile(gotFi, wantFi) {
		t.Errorf("Locate returned %q, which is not the same file as %q", got, want)
	}
}

// TestLocate_HomebrewPrefix verifies that Locate finds a file via
// $HOMEBREW_PREFIX/share/drydock/<rel> when that path exists.
func TestLocate_HomebrewPrefix(t *testing.T) {
	dir := t.TempDir()
	sharePath := filepath.Join(dir, "share", "drydock", "config")
	if err := os.MkdirAll(sharePath, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sharePath, "egress.yaml")
	if err := os.WriteFile(want, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOMEBREW_PREFIX", dir)

	// Make sure the CWD candidate doesn't accidentally exist by switching to a
	// temp dir that has no config/ subdirectory.
	other := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint:errcheck

	got, err := Locate("config/egress.yaml")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != want {
		t.Errorf("Locate returned %q, want %q", got, want)
	}
}

// TestLocate_MissingReturnsError verifies that Locate returns a descriptive
// error when no candidate exists, and that the error lists the tried paths.
func TestLocate_MissingReturnsError(t *testing.T) {
	// Clear HOMEBREW_PREFIX so no prefix candidate is generated.
	t.Setenv("HOMEBREW_PREFIX", "")

	other := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint:errcheck

	_, err := Locate("config/egress.yaml")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "config/egress.yaml") {
		t.Errorf("error should mention rel path, got: %v", err)
	}
}

// TestCandidates_Order verifies that Candidates returns paths in binary →
// HOMEBREW → CWD order and that the binary-relative path is first.
func TestCandidates_Order(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "/fake/homebrew")

	cs := Candidates("config/egress.yaml")
	if len(cs) < 2 {
		t.Fatalf("expected ≥2 candidates, got %d: %v", len(cs), cs)
	}

	// CWD candidate is always last.
	last := cs[len(cs)-1]
	if last != "config/egress.yaml" {
		t.Errorf("last candidate should be CWD-relative, got %q", last)
	}

	// HOMEBREW candidate must be present (before CWD).
	hbIdx := -1
	for i, c := range cs {
		if strings.Contains(c, "/fake/homebrew/") {
			hbIdx = i
		}
	}
	if hbIdx < 0 {
		t.Errorf("HOMEBREW_PREFIX candidate not found in %v", cs)
	}
	if hbIdx >= len(cs)-1 {
		t.Errorf("HOMEBREW candidate should come before CWD, indices: hb=%d last=%d", hbIdx, len(cs)-1)
	}
}
