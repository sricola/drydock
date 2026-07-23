// Package atomicfile writes a file atomically so a crash mid-write can never
// leave a truncated or partially-written file where a whole one is expected,
// which matters for the host-only credential and key files drydock persists.
package atomicfile

import "os"

// Write atomically writes data to path with perm. It writes to a sibling
// ".tmp" file first, then os.Rename over the target (atomic on the same
// filesystem), so a failed or interrupted write leaves the existing target
// untouched. The temp file inherits perm too, so it is never briefly readable
// more widely than the final file.
func Write(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
