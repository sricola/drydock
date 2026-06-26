package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A fresh install has no ~/.drydock yet — acquireLock must create the parent
// rather than failing on ENOENT.
func TestAcquireLock_CreatesParentAndLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "brokerd.lock") // parent missing
	f, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquireLock on fresh path: %v", err)
	}
	defer f.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
}

// flock attaches to the open file description, so a second open+lock of the
// same path — even in the same process — contends and returns errLockHeld,
// distinguishable from a generic open/mkdir failure.
func TestAcquireLock_SecondAcquireIsContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brokerd.lock")
	f1, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	defer f1.Close()
	if _, err := acquireLock(path); !errors.Is(err, errLockHeld) {
		t.Errorf("second acquireLock err = %v, want errLockHeld", err)
	}
}
