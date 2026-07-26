//go:build darwin

// Quota-backed stages (F-04). A plain bind mount lets a hostile in-VM agent
// fill the host filesystem through /work; the polling guard in
// internal/broker bounds that only softly (fill_rate * poll interval). On
// macOS the broker instead mounts a size-capped APFS sparse image at the
// stage root before cloning, so writes past the cap fail with ENOSPC inside
// the image: a filesystem hard wall the agent cannot poll-race. The image is
// sparse, so host disk is consumed only as bytes are actually written.
package stage

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// hdiutilTimeout bounds every hdiutil shell-out: a hung diskimages-helper
// must fail the task, not wedge the broker (same policy as the bounded
// container-CLI shell-outs).
const hdiutilTimeout = 60 * time.Second

func runHdiutil(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hdiutilTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "hdiutil", args...)
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("hdiutil %v: %w\n%s", args, err, out)
	}
	return string(out), nil
}

// QuotaImagePath returns the sparse-image file backing root's quota. It is a
// sibling of root (inside StageRoot, outside the mount) so the image file
// itself can never be reached from /work.
func QuotaImagePath(root string) string { return root + ".sparseimage" }

// AttachQuota creates a sizeBytes-capped APFS sparse image next to root and
// mounts it at root. root must already exist (empty). Fail closed: any error
// leaves no image file behind.
func AttachQuota(root string, sizeBytes int64) error {
	img := QuotaImagePath(root)
	if _, err := runHdiutil("create",
		"-size", fmt.Sprintf("%dm", sizeBytes>>20),
		"-type", "SPARSE", "-fs", "APFS",
		"-volname", "drydock-"+filepath.Base(root), img); err != nil {
		_ = os.Remove(img)
		return err
	}
	if _, err := runHdiutil("attach", img,
		"-mountpoint", root, "-nobrowse", "-noautoopen"); err != nil {
		// A failed attach can still have attached the device (a timeout
		// between kernel attach and CLI exit); tear it down before the
		// backing file is removed, or the mount becomes invisible to
		// every later cleanup (they gate on the image file existing).
		if isMounted(root) {
			_, _ = runHdiutil("detach", root, "-force")
		}
		_ = os.Remove(img)
		return err
	}
	return nil
}

// isMounted reports whether root is a mountpoint (its device differs from
// its parent's). Stat errors read as "not mounted": callers then skip the
// detach and fall through to the file removal, which is the safe direction.
func isMounted(root string) bool {
	var a, b syscall.Stat_t
	if syscall.Stat(root, &a) != nil {
		return false
	}
	if syscall.Stat(filepath.Dir(root), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}

// teardownQuota detaches root's quota image if mounted and deletes the
// backing file. A stage with no image (quota disabled, or non-darwin
// leftovers) is a no-op, so every cleanup path can call it unconditionally.
func teardownQuota(root string) error {
	img := QuotaImagePath(root)
	if _, err := os.Lstat(img); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if isMounted(root) {
		if _, err := runHdiutil("detach", root); err != nil {
			// A straggling fd (dying VM, Spotlight) can hold the mount;
			// -force revokes it. The VM is gone by every caller's point.
			if _, ferr := runHdiutil("detach", root, "-force"); ferr != nil {
				// Doubly degraded: even the forced detach failed, so the
				// image is still mounted and we must NOT delete its backing
				// file (that would strand a live, untracked mount). Leave
				// both in place and alert loudly with the exact paths: a
				// caller may swallow the returned error, but this line is not
				// silent. The next brokerd boot re-runs ReapOrphans, which
				// calls teardownQuota again and usually succeeds once the
				// straggling fd is gone; a persistently stuck mount needs a
				// manual `diskutil eject -force` then `rm` of the image.
				slog.Error("stage quota image stuck mounted; manual cleanup needed",
					"mount", root, "image", img, "err", ferr,
					"hint", "diskutil eject -force "+root+" && rm "+img)
				return ferr
			}
		}
	}
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// reattachQuota re-mounts an existing quota image at root when it is not
// mounted (an awaiting-approval stage after a host reboot). No image means
// a plain stage: no-op.
func reattachQuota(root string) error {
	img := QuotaImagePath(root)
	if _, err := os.Lstat(img); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if isMounted(root) {
		return nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	_, err := runHdiutil("attach", img, "-mountpoint", root, "-nobrowse", "-noautoopen")
	return err
}
