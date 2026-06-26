package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// errLockHeld is returned by acquireLock when another process already holds the
// single-instance lock (flock returned EWOULDBLOCK).
var errLockHeld = errors.New("brokerd lock held by another process")

// acquireLock takes an exclusive, non-blocking flock on path so only one
// brokerd runs per host. The returned *os.File MUST be kept open for the
// process's whole life — closing it drops the lock. The parent dir is created
// if absent (~/.drydock may not exist on a fresh start; the broker otherwise
// creates its state dirs lazily on the first task). Returns errLockHeld on
// contention, or the underlying error on any other failure.
func acquireLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLockHeld
		}
		return nil, err
	}
	return f, nil
}
