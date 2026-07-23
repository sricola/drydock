package stage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A hostile task can stage a giant/binary diff; CaptureDiff must bound the bytes
// it holds in broker memory rather than buffering the whole thing.
func TestCaptureDiff_TruncatesOversizeDiff(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	big := strings.Repeat("some added line of content\n", 2000) // ~54 KiB
	if err := os.WriteFile(filepath.Join(s.WorkDir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.stageAll(); err != nil {
		t.Fatal(err)
	}

	out, err := s.gitDiffCapped(512) // cap far below the diff size
	if !errors.Is(err, ErrDiffTooLarge) {
		t.Fatalf("gitDiffCapped over cap: got (%q, %v), want ErrDiffTooLarge", out, err)
	}
	if out != "" {
		t.Errorf("oversize diff must return no partial content, got %d bytes", len(out))
	}
}

func TestCaptureDiff_SmallDiffNotTruncated(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	if err := os.WriteFile(filepath.Join(s.WorkDir, "small.txt"), []byte("a small change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := s.CaptureDiff()
	if err != nil {
		t.Fatalf("CaptureDiff: %v", err)
	}
	if strings.Contains(out, "truncated") {
		t.Error("small diff must not be truncated")
	}
	if !strings.Contains(out, "small.txt") {
		t.Errorf("diff missing the change: %q", out)
	}
}
