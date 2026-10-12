// Package atomicfile writes a file atomically and durably so a crash mid-write
// can never leave a truncated or partially-written file where a whole one is
// expected. drydock uses it for the host-only credential files and for the
// durable markers and ledgers brokerd resumes from after a restart.
package atomicfile

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// TempPath is the temp file Write stages path through: a sibling (so the
// rename stays on one filesystem) named per PROCESS, "<path>.<pid>.tmp". Two
// processes writing the same file (brokerd and `drydock auth` share the
// credential files) therefore never share a temp, so neither can rename the
// other's half-written inode into place; the last rename wins with its own
// whole, fsynced contents. Within one process callers serialize writes to a
// path with their own locks. Exported so tests can plant a blocker at the
// exact name.
func TempPath(path string) string {
	return path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
}

// Write writes data to path with perm: into TempPath(path) first, fsynced,
// then os.Rename over the target (atomic on the same filesystem), so a failed
// or interrupted write leaves the existing target untouched and a rename that
// landed is backed by data already on disk.
//
// The temp is opened O_EXCL|O_NOFOLLOW at perm, so it is never briefly
// readable more widely than the final file. Anything already at that name
// cannot be a live write of ours (callers hold a lock per path), so a regular
// file (a crashed earlier process that drew the same pid) or a symlink
// (planted; never followed) is removed first. Anything else, such as a
// directory, was not left by a writer and is never removed: the write fails.
// On any failure the temp this call created is removed and the error
// returned. The directory fsync after the rename is best effort: the data is
// already durable and the rename already happened, so a filesystem that
// refuses it does not turn a successful write into a reported failure.
func Write(path string, data []byte, perm os.FileMode) error {
	tmp := TempPath(path)
	if fi, err := os.Lstat(tmp); err == nil {
		if !fi.Mode().IsRegular() && fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("atomicfile: %s: refusing to remove %s, which is not a temp file", path, tmp)
		}
		if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		if err := d.Sync(); err != nil {
			slog.Debug("atomicfile: could not fsync the directory after rename", "path", path, "err", err)
		}
		d.Close()
	}
	return nil
}

// RemoveStaleTemps removes every temp a crashed writer may have left for
// path: the per-process "<path>.<pid>.tmp" forms and the fixed "<path>.tmp"
// an older drydock used. The target itself is untouched. A missing file is
// success. Callers run it where no live writer can exist for the path: at
// boot, or when removing the target.
func RemoveStaleTemps(path string) error {
	matches, err := filepath.Glob(path + ".*.tmp")
	if err != nil {
		return err
	}
	var first error
	for _, m := range append(matches, path+".tmp") {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}
