//go:build !darwin

package stage

// Quota images are hdiutil/APFS-backed, so they exist only on macOS, the
// only platform Apple `container` (and therefore drydock) runs on. These
// stubs keep non-darwin builds (CI unit tests) compiling; there the polling
// guard in internal/broker remains the only stage bound.

// AttachQuota is a no-op off macOS.
func AttachQuota(root string, sizeBytes int64) error { return nil }

// QuotaImagePath mirrors the darwin naming so cross-platform tests can
// reference it.
func QuotaImagePath(root string) string { return root + ".sparseimage" }

func teardownQuota(root string) error { return nil }
func reattachQuota(root string) error { return nil }
