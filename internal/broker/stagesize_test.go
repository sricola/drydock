package broker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStageOverLimit_ByteCap(t *testing.T) {
	dir := t.TempDir()
	writeN(t, filepath.Join(dir, "a"), 2000)
	if stageOverLimit(dir, 5000, 1000) {
		t.Error("2000 bytes reported over a 5000 cap")
	}
	writeN(t, filepath.Join(dir, "b"), 4000)
	if !stageOverLimit(dir, 5000, 1000) {
		t.Error("6000 bytes should exceed the 5000 byte cap")
	}
}

func TestStageOverLimit_FileCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		writeN(t, filepath.Join(dir, fmt.Sprintf("f%d", i)), 1)
	}
	if stageOverLimit(dir, 1<<30, 20) {
		t.Error("10 files reported over a 20-file cap")
	}
	if !stageOverLimit(dir, 1<<30, 5) {
		t.Error("10 files should exceed the 5-file cap")
	}
}

func TestWatchStageSize_FiresOnOverLimit(t *testing.T) {
	dir := t.TempDir()
	writeN(t, filepath.Join(dir, "big"), 8000)
	orig := maxStageBytes
	maxStageBytes = 4000 // below the pre-written file
	t.Cleanup(func() { maxStageBytes = orig })

	fired := make(chan struct{}, 1)
	g := watchStageSize(dir, 10*time.Millisecond, func() { fired <- struct{}{} })
	defer g.stop()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("watchStageSize did not fire on an over-limit stage")
	}
	if !g.exceeded() {
		t.Error("exceeded() = false after firing")
	}
}

func TestFreeBytes_Positive(t *testing.T) {
	free, err := freeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("freeBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("freeBytes = %d, want > 0", free)
	}
}

func writeN(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}
