package stage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// capCopy is the bounded-read core of gitDiffCapped. These tests exercise its
// branches directly (small, oversize, mid-stream read error) — the last is the
// fail path of the human diff-review gate that a real git subprocess can't be
// coaxed into on demand.
func TestCapCopy_UnderCapReturnsAll(t *testing.T) {
	out, trunc, err := capCopy(strings.NewReader("hello"), 100)
	if err != nil || trunc || out != "hello" {
		t.Fatalf("capCopy(small) = (%q, %v, %v), want (\"hello\", false, nil)", out, trunc, err)
	}
}

func TestCapCopy_OverCapTruncates(t *testing.T) {
	out, trunc, err := capCopy(strings.NewReader(strings.Repeat("x", 500)), 100)
	if err != nil || !trunc || len(out) != 100 {
		t.Fatalf("capCopy(oversize) = (len %d, trunc %v, %v), want (100, true, nil)", len(out), trunc, err)
	}
}

func TestCapCopy_ReadErrorSurfaces(t *testing.T) {
	boom := errors.New("stream read failed")
	out, trunc, err := capCopy(iotest.ErrReader(boom), 100)
	if !errors.Is(err, boom) {
		t.Fatalf("capCopy(err reader) err = %v, want %v", err, boom)
	}
	if out != "" || trunc {
		t.Errorf("on read error want empty/untruncated, got (%q, %v)", out, trunc)
	}
}

// cappedBuffer bounds git's stderr; a write past the cap must keep only the
// first max bytes yet report full consumption so the writer never blocks.
func TestCappedBuffer_BoundsButNeverBlocks(t *testing.T) {
	c := &cappedBuffer{max: 4}
	n, err := c.Write([]byte("abcdefgh")) // 8 bytes into a 4-byte cap
	if n != 8 || err != nil {
		t.Fatalf("Write returned (%d, %v), want (8, nil) so the writer isn't blocked", n, err)
	}
	if got := c.String(); got != "abcd" {
		t.Errorf("buffered %q, want first 4 bytes \"abcd\"", got)
	}
	// A further write once full is fully "consumed" but retained nowhere.
	if n, _ := c.Write([]byte("ij")); n != 2 {
		t.Errorf("post-full Write returned %d, want 2 (reported consumed)", n)
	}
	if got := c.String(); got != "abcd" {
		t.Errorf("buffer grew past cap to %q", got)
	}
}

// A hostile task can stage a giant/binary diff; CaptureDiff must fail closed
// rather than buffer or return a truncated (partially reviewable) diff.
func TestCaptureDiff_OversizeDiffFailsClosed(t *testing.T) {
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
