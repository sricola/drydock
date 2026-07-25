# Red-Team Residuals Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three residual red-team findings (F-04 hard disk quota, F-10 generated security-claims table, F-09 dated apt/npm snapshots) and commit the audit report with a closure addendum.

**Architecture:** F-04 backs each task's stage dir with a size-capped APFS sparse image (hdiutil) mounted at the stage root, so the VM's `/work` writes hit a filesystem hard wall; the existing polling guard stays as the early-cancel layer. F-10 adds a code-sourced generator in cmd/docs-build that renders `site/docs/security-defaults.md` from `config.Defaults()` and exported constants, with a drift test and a verified-by-test-exists test. F-09 pins apt to a dated snapshot.debian.org archive and npm resolution to a dated `--before` cutoff, both bumped deliberately by the existing cli-bump lane.

**Tech Stack:** Go 1.x (module `drydock`), hdiutil (macOS), Dockerfile (Apple `container` build), GitHub Actions.

## Global Constraints

- No em dashes anywhere: docs, comments, commit messages, generated output. Use commas, colons, or parentheses. (Maintainer rule; `cmd/docs-build/meta_test.go:75` enforces it for llms.txt.)
- Update `site/docs/*.md` and root docs alongside any behavior change (repo rule). Generated `.html` is gitignored; never commit it.
- Commit messages end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. No "Generated with Claude Code" banner in the PR body.
- TDD: write the failing test first for every code change. Run `go test -race` on the touched package before committing.
- Production platform is macOS (Apple `container`); Linux builds exist only for CI unit tests. Quota code is darwin-only behind build tags with silent no-op stubs elsewhere.
- Work on a feature branch `redteam-residuals` off `main` (create via the using-git-worktrees skill or `git checkout -b`), one PR at the end.
- Comment style: match the codebase (explanatory paragraphs on exported symbols, security reasoning inline). Wrap at ~80 cols.

---

## Part A: F-04, hard per-task disk quota for /work

### Task 1: config knob `stage_quota_gb`

**Files:**
- Modify: `internal/config/config.go` (struct ~line 94 area, `Defaults()` ~line 145, `applyEnvOverrides` ~line 327, `validate()` ~line 416, `SeedTemplate` ~line 489)
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Produces: `Config.StageQuotaGB int` (yaml `stage_quota_gb`, env `DRYDOCK_STAGE_QUOTA_GB`, default 8, 0 disables). Task 3 consumes it as `int64(cfg.StageQuotaGB) << 30`.

- [ ] **Step 1: Write the failing test** (append to `internal/config/config_test.go`, mirroring the existing default/env/validation test style in that file):

```go
func TestStageQuotaGB_DefaultEnvValidate(t *testing.T) {
	if got := Defaults().StageQuotaGB; got != 8 {
		t.Errorf("Defaults().StageQuotaGB = %d, want 8", got)
	}
	t.Setenv("DRYDOCK_STAGE_QUOTA_GB", "16")
	c := Defaults()
	applyEnvOverrides(c)
	if c.StageQuotaGB != 16 {
		t.Errorf("env override: StageQuotaGB = %d, want 16", c.StageQuotaGB)
	}
	c.StageQuotaGB = -1
	if err := c.validate(); err == nil {
		t.Error("validate accepted stage_quota_gb: -1, want error")
	}
}
```

Adjust the helper names to whatever `config_test.go` actually calls (`applyEnvOverrides`/`validate` are unexported; if existing tests go through `Load`, follow that pattern instead).

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./internal/config/ -run TestStageQuotaGB -v`
Expected: FAIL (`c.StageQuotaGB undefined`).

- [ ] **Step 3: Implement.** Struct field (next to `TaskMaxInFlight`):

```go
	// StageQuotaGB is the hard per-task disk bound in GiB. On macOS each
	// task's stage dir is an APFS sparse image of this size mounted at the
	// stage root, so a hostile in-VM agent writing to /work hits a
	// filesystem wall instead of the host disk (F-04). The polling stage
	// guard (4 GiB soft) fires first in normal operation; this is the
	// backstop it cannot provide. 0 disables the image (plain host dir,
	// polling guard only). Ignored off macOS.
	StageQuotaGB int `yaml:"stage_quota_gb"`
```

`Defaults()`: add `StageQuotaGB: 8,`. Env override (next to the `DRYDOCK_TASK_MAX_INFLIGHT` block):

```go
	if v := os.Getenv("DRYDOCK_STAGE_QUOTA_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.StageQuotaGB = n
		}
	}
```

`validate()`:

```go
	if c.StageQuotaGB < 0 {
		return fmt.Errorf("config: stage_quota_gb must be >= 0, got %d", c.StageQuotaGB)
	}
```

`SeedTemplate` (near the `task_max_requests` line, matching its comment style):

```
stage_quota_gb:         8              # hard per-task disk bound (GiB): /work lives in an APFS sparse image this big (macOS). 0 = plain host dir, polling guard only
```

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/config/ -v -run 'TestStageQuota|TestSeed|TestDefaults'`
Expected: PASS (also run the full package once: `go test -race ./internal/config/`).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): stage_quota_gb, the hard per-task disk bound (F-04)"
```

### Task 2: stage quota backend (hdiutil) + lifecycle integration

**Files:**
- Create: `internal/stage/quota_darwin.go`
- Create: `internal/stage/quota_other.go`
- Create: `internal/stage/quota_darwin_test.go`
- Modify: `internal/stage/stage.go` (`Cleanup` line ~332, `Reopen` line ~343, `ReapOrphans` line ~362)

**Interfaces:**
- Produces: `stage.AttachQuota(root string, sizeBytes int64) error`, `stage.QuotaImagePath(root string) string` (exported; Task 3 and Task 4 consume them). Unexported `teardownQuota(root string) error` and `reattachQuota(root string) error` used by `Cleanup`/`ReapOrphans`/`Reopen`.
- Consumes: `gitWaitDelay` (existing const in stage.go) for subprocess WaitDelay.

- [ ] **Step 1: Write the failing tests** (`internal/stage/quota_darwin_test.go`):

```go
//go:build darwin

package stage

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireHdiutil(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("no hdiutil on PATH")
	}
}

// The quota is a hard wall: writes past the image size fail with ENOSPC and
// the backing sparse file never grows past quota plus APFS overhead.
func TestQuota_HardBoundENOSPC(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const quota = int64(64 << 20)
	if err := AttachQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = teardownQuota(root) })

	chunk := make([]byte, 1<<20)
	var wrote int64
	var werr error
	for i := 0; i < 96; i++ { // try to write 96 MiB into a 64 MiB image
		werr = os.WriteFile(filepath.Join(root, "fill"+string(rune('a'+i%26))+string(rune('a'+i/26))), chunk, 0o600)
		if werr != nil {
			break
		}
		wrote += int64(len(chunk))
	}
	if werr == nil {
		t.Fatalf("wrote %d MiB into a 64 MiB quota with no error; the bound is not hard", wrote>>20)
	}
	if fi, err := os.Stat(QuotaImagePath(root)); err == nil && fi.Size() > quota+(32<<20) {
		t.Errorf("backing image grew to %d MiB, want <= quota + 32 MiB slack", fi.Size()>>20)
	}
}

// teardownQuota detaches and deletes the image; calling it again, or on a
// plain quota-less dir, is a no-op.
func TestQuota_TeardownIdempotent(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AttachQuota(root, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := teardownQuota(root); err != nil {
		t.Fatalf("first teardown: %v", err)
	}
	if _, err := os.Lstat(QuotaImagePath(root)); !os.IsNotExist(err) {
		t.Error("backing image survived teardown")
	}
	if isMounted(root) {
		t.Error("root still mounted after teardown")
	}
	if err := teardownQuota(root); err != nil {
		t.Errorf("second teardown: %v, want no-op nil", err)
	}
	plain := t.TempDir()
	if err := teardownQuota(plain); err != nil {
		t.Errorf("teardown of a plain dir: %v, want no-op nil", err)
	}
}

// Cleanup on a quota-backed stage detaches the image before removing the
// root, and removes the backing file too.
func TestQuota_CleanupRemovesImage(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AttachQuota(root, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Stage{Root: root}).Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Error("stage root survived Cleanup")
	}
	if _, err := os.Lstat(QuotaImagePath(root)); !os.IsNotExist(err) {
		t.Error("quota image survived Cleanup")
	}
}

// A detached image (host reboot) is re-attached by Reopen so an
// awaiting-approval task resumes with its work tree intact.
func TestQuota_ReopenReattaches(t *testing.T) {
	requireHdiutil(t)
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AttachQuota(root, 64<<20); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"work", "git"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "work", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runHdiutil("detach", root); err != nil { // simulate reboot
		t.Fatal(err)
	}
	st, err := Reopen(root)
	if err != nil {
		t.Fatalf("Reopen after detach: %v", err)
	}
	t.Cleanup(func() { _ = st.Cleanup() })
	if _, err := os.Stat(filepath.Join(root, "work", "f")); err != nil {
		t.Errorf("work tree content missing after reattach: %v", err)
	}
}

// ReapOrphans detaches and deletes orphaned quota stages, and preserves
// (still mounted) the ones named in keep.
func TestQuota_ReapOrphansDetaches(t *testing.T) {
	requireHdiutil(t)
	parent := t.TempDir()
	orphan := filepath.Join(parent, "aaaa")
	kept := filepath.Join(parent, "bbbb")
	for _, r := range []string{orphan, kept} {
		if err := os.MkdirAll(r, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := AttachQuota(r, 64<<20); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = teardownQuota(kept); _ = teardownQuota(orphan) })
	if _, err := ReapOrphans(parent, map[string]bool{"bbbb": true}); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Error("orphan stage dir survived the reap")
	}
	if _, err := os.Lstat(QuotaImagePath(orphan)); !os.IsNotExist(err) {
		t.Error("orphan quota image survived the reap")
	}
	if !isMounted(kept) {
		t.Error("kept stage was unmounted by the reap; resume would find it empty")
	}
}
```

- [ ] **Step 2: Run them, verify failure**

Run: `go test ./internal/stage/ -run TestQuota_ -v`
Expected: FAIL to compile (`AttachQuota` undefined).

- [ ] **Step 3: Implement `internal/stage/quota_darwin.go`:**

```go
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
		_ = os.Remove(img)
		return err
	}
	return nil
}

// isMounted reports whether root is a mountpoint (its device differs from
// its parent's). Statfs errors read as "not mounted": callers then skip the
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
		return nil
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
```

And `internal/stage/quota_other.go`:

```go
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

func teardownQuota(root string) error  { return nil }
func reattachQuota(root string) error  { return nil }
```

Integrate into `stage.go`. `Cleanup` (line ~332) becomes:

```go
func (s *Stage) Cleanup() error {
	clean := filepath.Clean(s.Root)
	if clean == "" || clean == "/" || clean == "." || !filepath.IsAbs(clean) {
		return fmt.Errorf("stage: refusing to clean unsafe path %q", s.Root)
	}
	// Quota-backed stage (F-04): detach the image and delete its backing
	// file first; RemoveAll on a live mountpoint would delete inside the
	// image and then fail on the mountpoint itself.
	if err := teardownQuota(clean); err != nil {
		return err
	}
	return os.RemoveAll(clean)
}
```

`Reopen` (line ~343): insert before the stat loop:

```go
	// A quota-backed stage that survived a host reboot has its image
	// detached; remount it so the work tree is visible again (F-04).
	if err := reattachQuota(root); err != nil {
		return nil, fmt.Errorf("stage: cannot reattach quota image for %q: %w", root, err)
	}
```

`ReapOrphans` (line ~362): inside the entry loop, replace the body with:

```go
	for _, e := range entries {
		if !e.IsDir() {
			// An orphan quota image whose mountpoint dir is already gone
			// (F-04). Mounted images always have their dir, so a bare
			// .sparseimage here is safe to delete unless its task is kept.
			if id, ok := strings.CutSuffix(e.Name(), ".sparseimage"); ok && !keep[id] {
				if rerr := os.Remove(filepath.Join(root, e.Name())); rerr != nil && !os.IsNotExist(rerr) && firstErr == nil {
					firstErr = rerr
				}
			}
			continue
		}
		if keep[e.Name()] {
			continue // awaiting-approval stage: preserved for resume
		}
		child := filepath.Join(root, e.Name())
		// Detach + delete the quota image (no-op for plain stages) before
		// removing the dir, same ordering reason as Cleanup (F-04).
		if terr := teardownQuota(child); terr != nil {
			if firstErr == nil {
				firstErr = terr
			}
			continue
		}
		if rerr := os.RemoveAll(child); rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		n++
	}
```

Add `"strings"` to stage.go imports if absent. Update the `ReapOrphans` doc comment: non-directory entries are no longer untouched; orphan `.sparseimage` files are removed unless kept.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/stage/ -v -run 'TestQuota_'` then the full package `go test -race ./internal/stage/`.
Expected: all PASS on macOS (hdiutil present). Also `GOOS=linux go build ./...` to prove the stub compiles.

- [ ] **Step 5: Commit**

```bash
git add internal/stage/quota_darwin.go internal/stage/quota_other.go internal/stage/quota_darwin_test.go internal/stage/stage.go
git commit -m "feat(stage): APFS sparse-image quota backing for the stage dir (F-04)"
```

### Task 3: broker + brokerd wiring, host free-floor fix

**Files:**
- Modify: `internal/broker/broker.go` (struct ~line 139 seams, HandleTask ~line 395-421, runSandbox ~line 641)
- Modify: `internal/broker/stagesize.go` (`watchStageSize` signature, line 83)
- Modify: `internal/broker/stagesize_test.go` (call sites of `watchStageSize`)
- Modify: `cmd/brokerd/main.go` (Broker literal ~line 406)
- Test: `internal/broker/stagesize_wiring_test.go` (append)

**Interfaces:**
- Consumes: `stage.AttachQuota`, `stage.Stage{Root: ...}.Cleanup()` from Task 2; `Config.StageQuotaGB` from Task 1.
- Produces: `Broker.StageQuotaBytes int64` field; `Broker.attachQuota func(root string, sizeBytes int64) error` test seam (nil falls back to `stage.AttachQuota`).

- [ ] **Step 1: Write the failing tests** (append to `internal/broker/stagesize_wiring_test.go`; the file's existing tests show the `testBroker`/`submit` harness):

```go
// Quota attach failure fails the task closed: no clone happens onto an
// unbounded plain dir when the operator configured a hard bound (F-04).
func TestHandleTask_QuotaAttachFailureFailsClosed(t *testing.T) {
	staged := false
	st := &fakeStage{workDir: t.TempDir()}
	b := testBroker(t, "anthropic", st, &fakeGrant{},
		func(context.Context, []string, io.Writer, io.Writer) error { return nil })
	b.StageQuotaBytes = 1 << 30
	b.attachQuota = func(string, int64) error { return errors.New("hdiutil boom") }
	b.prepareStage = func(context.Context, string, string) (taskStage, error) { staged = true; return st, nil }

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] != "error" {
		t.Fatalf("terminal = %+v, want an error event", terminal)
	}
	if reason, _ := terminal["reason"].(string); !strings.Contains(reason, "quota") {
		t.Errorf("reason = %q, want it to mention the stage quota", reason)
	}
	if staged {
		t.Error("stage was prepared despite the quota failure; want fail closed")
	}
}

// With a quota configured, HandleTask attaches it at the task's stage dir
// with the configured size before preparing the stage.
func TestHandleTask_QuotaAttachedBeforePrepare(t *testing.T) {
	var gotRoot string
	var gotSize int64
	attached := false
	st := &fakeStage{workDir: t.TempDir()}
	b := testBroker(t, "anthropic", st, &fakeGrant{},
		func(context.Context, []string, io.Writer, io.Writer) error { return nil })
	b.StageQuotaBytes = 2 << 30
	b.attachQuota = func(root string, size int64) error {
		attached, gotRoot, gotSize = true, root, size
		return nil
	}
	b.prepareStage = func(_ context.Context, root string, _ string) (taskStage, error) {
		if !attached {
			t.Error("prepareStage ran before the quota was attached")
		}
		if root != gotRoot {
			t.Errorf("prepare root %q != quota root %q", root, gotRoot)
		}
		return st, nil
	}

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] == "error" {
		t.Fatalf("unexpected error terminal: %+v", terminal)
	}
	if gotSize != 2<<30 {
		t.Errorf("quota size = %d, want %d", gotSize, int64(2<<30))
	}
}
```

Add `"errors"` to the file's imports.

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/broker/ -run 'TestHandleTask_Quota' -v`
Expected: FAIL to compile (`b.StageQuotaBytes`, `b.attachQuota` undefined).

- [ ] **Step 3: Implement.** In `broker.go`:

(a) Struct: next to `StageRoot` (line ~92) add:

```go
	// StageQuotaBytes, when > 0, hard-bounds each task's stage dir with an
	// APFS sparse image of this size (F-04). 0 = plain host dir.
	StageQuotaBytes int64
```

Next to the `prepareStage` seam (line ~139) add:

```go
	// attachQuota mounts the hard per-task disk bound at a stage root. nil
	// in production falls back to stage.AttachQuota (a no-op off macOS).
	// Tests inject fakes.
	attachQuota func(root string, sizeBytes int64) error
```

(b) HandleTask: after the low-disk preflight block (line ~408) and the "preparing" emit, immediately before `st, err := prepare(...)`, insert:

```go
	if b.StageQuotaBytes > 0 {
		attach := b.attachQuota
		if attach == nil {
			attach = stage.AttachQuota
		}
		qerr := os.MkdirAll(stageDir, 0o700)
		if qerr == nil {
			qerr = attach(stageDir, b.StageQuotaBytes)
		}
		if qerr != nil {
			// Fail closed: never fall back to an unbounded plain dir when
			// the operator configured a hard bound (F-04).
			slog.Warn("task stage quota failed", "task_id", taskID, "err", qerr)
			sw.emit(errorEvent(taskID, "stage quota setup failed",
				"hdiutil could not create or mount the stage image; see the broker log"))
			_ = (&stage.Stage{Root: stageDir}).Cleanup()
			return
		}
		// Until tr.st exists, no defer tears the mount down; cover the
		// clone/prompt-write failure exits in between.
		defer func() {
			if tr.st == nil {
				_ = (&stage.Stage{Root: stageDir}).Cleanup()
			}
		}()
	}
```

Confirm `internal/stage` is already imported in broker.go (it is, for `defaultPrepareStage`); confirm `errorEvent`'s second string parameter is the hint field by reading a neighboring call.

(c) `stagesize.go`: give the monitor a host-filesystem path for the free floor, since with a quota mounted, statfs on the stage root reports the image's own free space, not the host's:

```go
// watchStageSize polls root every interval until stop() is called, invoking
// onExceed once if the stage crosses its bounds or host free space drops
// below the floor. hostRoot is where the free floor is measured: with a
// quota image mounted at root, statfs(root) sees the image filesystem, while
// the sparse backing file grows on the host filesystem that contains it.
// Cross-platform (no runtime dependency), so it is CI-testable.
func watchStageSize(root, hostRoot string, interval time.Duration, onExceed func()) *stageSizeGuard {
```

and change line 96 to `if stageOverLimit(root, maxStageBytes, maxStageFiles) || belowFreeFloor(hostRoot) {`.

(d) runSandbox call site (broker.go line ~641):

```go
	sizeGuard := watchStageSize(stageRoot, filepath.Dir(stageRoot), stageSizeInterval, runCancel)
```

(e) Update every `watchStageSize(` call in `internal/broker/stagesize_test.go` to pass the same dir twice (`watchStageSize(dir, dir, ...)`), preserving each test's intent.

(f) `cmd/brokerd/main.go` Broker literal (~line 406): add `StageQuotaBytes: int64(cfg.StageQuotaGB) << 30,` next to `StageRoot`.

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/broker/ ./cmd/brokerd/`
Expected: PASS, including the two new tests and all pre-existing stage-size tests.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/broker.go internal/broker/stagesize.go internal/broker/stagesize_test.go internal/broker/stagesize_wiring_test.go cmd/brokerd/main.go
git commit -m "feat(broker): mount the stage quota image per task; measure the free floor on the host fs (F-04)"
```

### Task 4: VM red-team test A8 + F-04 docs

**Files:**
- Modify: `tests/integration/redteam_test.go` (append)
- Modify: `internal/broker/stagesize.go` (top comment, lines 12-16)
- Modify: `THREAT_MODEL.md`, `SECURITY.md` (disk-exhaustion residual wording; locate with `grep -n "disk\|/work" THREAT_MODEL.md SECURITY.md`)
- Modify: `docs/ROADMAP.md` (F-04 status), `CHANGELOG.md` (new Unreleased section)

**Interfaces:**
- Consumes: `stage.AttachQuota`, `stage.QuotaImagePath`, `stage.Stage.Cleanup` (Task 2); existing helpers `requireContainer`, `containerRun`, `sandboxImage` in redteam_test.go.

- [ ] **Step 1: Write the VM test** (append to `tests/integration/redteam_test.go`; add `"runtime"`, `"os"`, `"path/filepath"`, and `"drydock/internal/stage"` imports as needed):

```go
// A8: /work is quota-backed (F-04). An in-VM write flood hits the APFS
// image's hard byte wall (ENOSPC through virtiofs), and the sparse backing
// file on the host never grows meaningfully past the quota. This is the
// hard bound the polling stage guard cannot provide.
func TestRedteam_A8_WorkQuotaHardBound(t *testing.T) {
	requireContainer(t)
	if runtime.GOOS != "darwin" {
		t.Skip("quota images are hdiutil-backed; darwin only")
	}
	root := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const quota = int64(256 << 20)
	if err := stage.AttachQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = (&stage.Stage{Root: root}).Cleanup() })

	out := containerRun(t, "run", "--rm", "--entrypoint", "/bin/bash",
		"--mount", "type=bind,source="+root+",target=/work",
		sandboxImage(), "-lc",
		"dd if=/dev/zero of=/work/fill bs=1M count=1024 2>&1; df -h /work")

	if !strings.Contains(out, "No space left") {
		t.Errorf("A8: expected the in-VM flood to hit ENOSPC; got:\n%s", out)
	}
	fi, err := os.Stat(stage.QuotaImagePath(root))
	if err != nil {
		t.Fatalf("stat quota image: %v", err)
	}
	if fi.Size() > quota+(64<<20) {
		t.Errorf("A8 BREACH: backing image grew to %d MiB, want <= quota (256 MiB) + 64 MiB slack", fi.Size()>>20)
	}
}
```

- [ ] **Step 2: Run it** (needs the sandbox image built: `make image` if stale)

Run: `go test -tags=integration -count=1 -timeout=10m -run 'TestRedteam_A8' ./tests/... -v`
Expected: PASS (dd reports "No space left on device", image stays near 256 MiB). If the mount of an attached-image path fails under `container`, STOP and report: that invalidates the whole F-04 approach and the human partner must weigh in.

- [ ] **Step 3: Update docs.**
  - `internal/broker/stagesize.go` lines 12-16, replace the first paragraph of the comment with:

```go
// A task's /work is bounded twice (F-04). On macOS the stage root is a
// size-capped APFS sparse image (see internal/stage AttachQuota): the hard
// wall. These polling bounds are the early-cancel layer on top: they stop a
// task cleanly before it slams into ENOSPC, and they are the only layer on
// non-darwin builds (CI). Worst-case soft overshoot is about
// fill_rate * stageSizeInterval, now capped by the image size. Vars (not
// consts) only so tests can lower them; nothing in production writes them.
```

  - `THREAT_MODEL.md` and `SECURITY.md`: find the paragraphs describing the /work disk-exhaustion bound (grep `disk`); rewrite them to state: hard bound = `stage_quota_gb` APFS image (default 8 GiB, macOS), soft early-cancel = 4 GiB / 200k files / 2 GiB host free floor polling guard, and that `Verified by:` `TestRedteam_A8_WorkQuotaHardBound` plus `TestQuota_HardBoundENOSPC`. Follow the existing "Verified by:" link style used by other claims.
  - `docs/ROADMAP.md`: in the v0.6.x notes where F-04 partial status is implied, note the hard quota landed.
  - `CHANGELOG.md`: add at the top:

```markdown
## Unreleased

### Security

- **Each task's stage dir is now a size-capped APFS sparse image, making the
  /work disk bound hard (F-04).** A hostile in-VM agent writing through
  /work previously raced a 2-second polling guard and could overshoot by
  its write rate; it now hits the image's filesystem wall (ENOSPC) at
  `stage_quota_gb` (default 8 GiB). The polling guard remains as the
  early-cancel layer. VM-verified by the new A8 red-team test.
```

- [ ] **Step 4: Regenerate the site and run the doc guards**

Run: `go run ./cmd/docs-build && go test ./cmd/docs-build/`
Expected: PASS (no stale-claim phrases reintroduced, no em dashes in llms.txt).

- [ ] **Step 5: Commit**

```bash
git add tests/integration/redteam_test.go internal/broker/stagesize.go THREAT_MODEL.md SECURITY.md docs/ROADMAP.md CHANGELOG.md site/
git commit -m "test(redteam): A8 proves the /work quota hard bound; document the two-layer disk bound (F-04)"
```

---

## Part B: F-10, generated security-defaults table

### Task 5: export the stage-bound defaults

**Files:**
- Modify: `internal/broker/stagesize.go` (lines 17-26)

**Interfaces:**
- Produces: `broker.DefaultMaxStageBytes int64`, `broker.DefaultMaxStageFiles int`, `broker.DefaultMinFreeStageBytes int64` (exported consts; Task 6 imports them). Existing `broker.DefaultUncappedRequestCap` already exported.

- [ ] **Step 1: Refactor** (no behavior change; the existing stage-size tests are the safety net):

```go
// Defaults for the polling stage bounds, exported so the generated
// security-defaults table (cmd/docs-build) renders them from code instead
// of restating them (F-10).
const (
	DefaultMaxStageBytes     int64 = 4 << 30 // total file bytes under the stage
	DefaultMaxStageFiles           = 200_000 // file-count (inode) bound
	DefaultMinFreeStageBytes int64 = 2 << 30 // host free-space floor
)

// Vars (not consts) only so tests can lower them; nothing in production
// writes them.
var (
	maxStageBytes     = DefaultMaxStageBytes
	maxStageFiles     = DefaultMaxStageFiles
	stageSizeInterval = 2 * time.Second
)

// minFreeStageBytes is the host free space below which a task is refused at
// submit (preflight) or cancelled mid-run (monitor): fail closed rather
// than exhaust the host disk.
var minFreeStageBytes = DefaultMinFreeStageBytes
```

- [ ] **Step 2: Run tests**

Run: `go test -race ./internal/broker/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/broker/stagesize.go
git commit -m "chore(broker): export the stage-bound defaults for the claims table (F-10)"
```

### Task 6: claims generator, generated page, drift + existence tests

**Files:**
- Create: `cmd/docs-build/claims.go`
- Modify: `cmd/docs-build/main.go` (generate before glob, ~line 40; add slug to `order`, line 19)
- Modify: `cmd/docs-build/claims_test.go` (comment + two new tests)
- Create (generated, committed): `site/docs/security-defaults.md`
- Modify: `THREAT_MODEL.md`, `README.md` (link the page)

**Interfaces:**
- Consumes: `config.Defaults()` (Task 1 field included), `broker.DefaultUncappedRequestCap`, `broker.DefaultMaxStageBytes/Files/MinFreeStageBytes` (Task 5), `stage.MaxDiffBytes`.
- Produces: `securityClaims() []claim` and `renderClaims([]claim) string` (used by main.go and the tests).

- [ ] **Step 1: Write the failing tests** (append to `cmd/docs-build/claims_test.go`):

```go
// The security-defaults page is generated from code (config.Defaults() and
// exported constants). A committed copy that no longer matches a fresh
// render means a default changed without regenerating docs: exactly the
// drift F-10 is about.
func TestSecurityDefaultsPageCurrent(t *testing.T) {
	root := repoRoot(t)
	want := renderClaims(securityClaims())
	got, err := os.ReadFile(filepath.Join(root, "site", "docs", "security-defaults.md"))
	if err != nil {
		t.Fatalf("read generated page: %v (run `go run ./cmd/docs-build` from the repo root)", err)
	}
	if string(got) != want {
		t.Error("site/docs/security-defaults.md is stale; run `go run ./cmd/docs-build` and commit the result")
	}
}

// Every claim row must cite a real enforcing test: an affirmative check, not
// just a forbidden-phrase blacklist. A renamed or deleted test surfaces here.
func TestSecurityDefaultsVerifiedByTestsExist(t *testing.T) {
	root := repoRoot(t)
	var testSrc strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "site" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			testSrc.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all := testSrc.String()
	for _, c := range securityClaims() {
		if c.Test == "" {
			t.Errorf("claim %q cites no enforcing test; every row must", c.Setting)
			continue
		}
		if !strings.Contains(all, "func "+c.Test+"(") {
			t.Errorf("claim %q cites test %q, which no longer exists", c.Setting, c.Test)
		}
	}
}
```

Add `"io/fs"` and `"strings"` to imports.

- [ ] **Step 2: Run, verify failure**

Run: `go test ./cmd/docs-build/ -run TestSecurityDefaults -v`
Expected: FAIL to compile (`renderClaims` undefined).

- [ ] **Step 3: Implement `cmd/docs-build/claims.go`:**

```go
// The security-defaults table (F-10): one generated source of truth for the
// operator-facing financial and containment defaults. Values come from
// config.Defaults() and exported constants, never from prose, so the table
// cannot drift from the code; TestSecurityDefaultsPageCurrent pins the
// committed copy and TestSecurityDefaultsVerifiedByTestsExist pins each
// row's enforcing test.
package main

import (
	"fmt"
	"strings"

	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/stage"
)

type claim struct {
	Setting string // config key, or (built-in) for compiled constants
	Default string // rendered from code
	Bounds  string // what it bounds, and whether the bound is hard or soft
	Test    string // the Go test enforcing it (existence is itself tested)
}

func securityClaims() []claim {
	d := config.Defaults()
	return []claim{
		{"task_budget_usd", fmt.Sprintf("%.2f", d.TaskBudgetUSD),
			"Per-task USD spend through the gateway. Soft: metering is post-hoc, so spend can overshoot by up to task_max_inflight in-flight requests.",
			"PLACEHOLDER"},
		{"task_max_inflight", fmt.Sprintf("%d", d.TaskMaxInFlight),
			"Concurrent gateway requests per task lease. Hard at admission; bounds the budget overshoot.",
			"PLACEHOLDER"},
		{"task_max_requests", fmt.Sprintf("%d (0 falls closed to %d)", d.TaskMaxRequests, broker.DefaultUncappedRequestCap),
			"Total gateway requests per task, every auth mode. Hard at admission.",
			"PLACEHOLDER"},
		{"max_request_cost_usd", fmt.Sprintf("%.2f (0 = reservation off)", d.MaxRequestCostUSD),
			"Per-request USD reservation taken at admission. Off by default; setting it makes the per-task budget reservation-backed.",
			"PLACEHOLDER"},
		{"aggregate_budget_usd", fmt.Sprintf("%.2f (0 = off)", d.AggregateBudgetUSD),
			"Cross-task USD ceiling per api_key vendor over aggregate_window. Soft in the same post-hoc sense as task_budget_usd.",
			"PLACEHOLDER"},
		{"task_timeout", d.TaskTimeout.String(),
			"Wall-clock bound per task; the VM is killed at expiry. Hard.",
			"PLACEHOLDER"},
		{"stage_quota_gb", fmt.Sprintf("%d", d.StageQuotaGB),
			"Per-task disk bound: the stage dir is an APFS sparse image of this size (macOS). Hard (filesystem ENOSPC).",
			"PLACEHOLDER"},
		{"(built-in) stage soft bounds", fmt.Sprintf("%d GiB, %d files, %d GiB host free floor",
			broker.DefaultMaxStageBytes>>30, broker.DefaultMaxStageFiles, broker.DefaultMinFreeStageBytes>>30),
			"Polling guard (2s) cancels a task growing past these, before the hard quota wall. Soft by design; the quota is the wall.",
			"PLACEHOLDER"},
		{"(built-in) review diff cap", fmt.Sprintf("%d MiB", int64(stage.MaxDiffBytes)>>20),
			"A staged diff over the cap fails the task closed; a diff is never truncated for review. Hard.",
			"PLACEHOLDER"},
	}
}

// renderClaims renders the table as the full Markdown page. Deterministic
// output: the committed copy is byte-compared by the drift test.
func renderClaims(claims []claim) string {
	var b strings.Builder
	b.WriteString("# Security defaults\n\n")
	b.WriteString("<!-- GENERATED by cmd/docs-build (claims.go); do not edit by hand. Regenerate with `go run ./cmd/docs-build`. -->\n\n")
	b.WriteString("What the shipped defaults bound, how hard each bound is, and the test that enforces it. ")
	b.WriteString("Generated from `config.Defaults()` and the exported constants, so this page cannot drift from the code.\n\n")
	b.WriteString("| Setting | Default | What it bounds | Verified by |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, c := range claims {
		fmt.Fprintf(&b, "| `%s` | %s | %s | `%s` |\n", c.Setting, c.Default, c.Bounds, c.Test)
	}
	b.WriteString("\nSoft means enforcement is post-hoc or polling-based with a stated overshoot bound; hard means the mechanism cannot be raced. ")
	b.WriteString("The full adversarial context is in the [threat model](threat-model.html).\n")
	return b.String()
}
```

Replace every `"PLACEHOLDER"` with the real enforcing test name before committing: find each with `grep -rn "func Test" internal/gateway/*_test.go internal/broker/*_test.go internal/stage/*_test.go cmd/brokerd/*_test.go` (budget/inflight/request-cap tests live in internal/gateway; the timeout test near the broker; quota tests are `TestQuota_HardBoundENOSPC` and A8 from Part A; diff cap test is the V-01 regression in internal/stage). `TestSecurityDefaultsVerifiedByTestsExist` fails listing any name that does not exist, which is the feedback loop for this step.

In `main.go`:
  - line 19, add `"security-defaults"` to `order` after `"configuration"`.
  - at the top of `run()` (before the glob at line 41):

```go
	// Generate the security-defaults page from code first, so this run
	// renders it like any other doc page (F-10).
	if err := os.WriteFile(filepath.Join(docsDir, "security-defaults.md"),
		[]byte(renderClaims(securityClaims())), 0o644); err != nil {
		return err
	}
```

Update the stale comment in `claims_test.go` (lines 13-16): replace "This is the lightweight half of ... remains a follow-up." with "The blacklist below is the regression half; the generated `site/docs/security-defaults.md` (claims.go) is the affirmative single source of truth."

- [ ] **Step 4: Generate, link, and test**

Run: `go run ./cmd/docs-build` (creates `site/docs/security-defaults.md`), then link the page: in `README.md` next to the threat-model link add a "[security defaults](https://sricola.github.io/drydock/docs/security-defaults.html)" reference (match however README links other doc pages, check with `grep -n "docs/" README.md`), and in `THREAT_MODEL.md` reference it where defaults are discussed. Then:

Run: `go test ./cmd/docs-build/ -v`
Expected: PASS (drift test, existence test, em-dash guard, claims blacklist).

- [ ] **Step 5: Commit**

```bash
git add cmd/docs-build/claims.go cmd/docs-build/main.go cmd/docs-build/claims_test.go site/docs/security-defaults.md README.md THREAT_MODEL.md site/sitemap.xml site/llms.txt
git commit -m "feat(docs): generated security-defaults table, the F-10 single source of truth"
```

---

## Part C: F-09, dated apt and npm snapshots

### Task 7: Dockerfile snapshot pinning

**Files:**
- Modify: `image/Dockerfile` (apt block lines 10-21; add ARGs; npm installs lines 57, 73, 79, 86, 91)

- [ ] **Step 1: Pin apt to snapshot.debian.org.** Replace the comment + RUN at lines 10-21 with:

```dockerfile
# F-09: apt resolves against a dated snapshot.debian.org archive, so the same
# Dockerfile yields the same Debian package set on every build. Security
# currency comes from bumping DEBIAN_SNAPSHOT deliberately (the weekly bump
# lane and the daily CVE scan flag when a bump is due), not from whatever
# Debian shipped the minute the build ran. Snapshot Release files carry
# expired Valid-Until stamps by design, hence Check-Valid-Until off; retries
# because snapshot.debian.org rate-limits.
ARG DEBIAN_SNAPSHOT=20260725T000000Z
RUN set -eu; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s/\nSuites: bookworm bookworm-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s/\nSuites: bookworm-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' \
        "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list.d/debian.sources; \
    printf 'Acquire::Check-Valid-Until "false";\nAcquire::Retries "3";\n' > /etc/apt/apt.conf.d/80snapshot
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends \
      git ca-certificates curl jq nftables dnsutils ipset \
      python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*
```

- [ ] **Step 2: Pin npm resolution with a date cutoff.** After the `NPM_VERSION` ARG (line 56) add:

```dockerfile
# F-09: --before restricts every npm resolution (including transitives) to
# versions published before this UTC date, the npm analogue of the Debian
# snapshot above. An exact pin published after the date fails the build
# (fail closed), so the bump lane moves this date whenever it bumps a pin.
ARG NPM_BEFORE=2026-07-25
```

Append `--before=${NPM_BEFORE}` to all five `npm install -g` commands (npm itself, claude-code, codex, opencode, gemini-cli), e.g.:

```dockerfile
RUN npm install -g --ignore-scripts --no-audit --no-fund --before=${NPM_BEFORE} npm@${NPM_VERSION} \
```

```dockerfile
RUN npm install -g --before=${NPM_BEFORE} @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && claude --version
```

(same pattern for the other three, keeping each line's existing flags).

- [ ] **Step 3: Smoke-build the image** (long: full no-cache pull through snapshot.debian.org)

Run: `container build --no-cache -t drydock-sandbox:f09 image/`
Expected: SUCCESS, including the Go checksum `OK` line and all five CLI `--version` smoke checks. If snapshot.debian.org 503s persistently, rerun once; if the bookworm-updates suite is empty on the snapshot mirror, drop `bookworm-updates` from the Suites line and note it in the commit message.

- [ ] **Step 4: Commit**

```bash
git add image/Dockerfile
git commit -m "build(image): date-pin apt (snapshot.debian.org) and npm (--before) resolution (F-09)"
```

### Task 8: cli-bump moves the npm date; docs reconciled

**Files:**
- Modify: `cmd/cli-bump/main.go`
- Test: `cmd/cli-bump/main_test.go` (append)
- Modify: `.github/workflows/agent-cli-bump.yml` (rewrite step)
- Modify: `docs/ROADMAP.md` (2.5 wording), `CHANGELOG.md` (Unreleased)

**Interfaces:**
- Produces: `cli-bump -before YYYY-MM-DD` flag; when at least one version bump is written, `ARG NPM_BEFORE=` is rewritten to that date.

- [ ] **Step 1: Write the failing test** (append to `cmd/cli-bump/main_test.go`, mirroring the existing `planBumps` test style):

```go
// -before: when a bump lands, ARG NPM_BEFORE moves with it, or the freshly
// bumped pin (published after the old date) would fail the image build.
// With no bumps, the date must not move (a date-only weekly PR is noise).
func TestApplyBeforeDate(t *testing.T) {
	df := "ARG NPM_BEFORE=2026-07-25\nARG CLAUDE_CODE_VERSION=2.1.207\n"

	out, bumps := planBumps(df, map[string]string{"@anthropic-ai/claude-code": "2.2.0"})
	if len(bumps) != 1 {
		t.Fatalf("bumps = %v, want one", bumps)
	}
	out = applyBeforeDate(out, "2026-08-01", len(bumps) > 0)
	if !strings.Contains(out, "ARG NPM_BEFORE=2026-08-01") {
		t.Errorf("NPM_BEFORE not moved with the bump:\n%s", out)
	}

	same := applyBeforeDate(df, "2026-08-01", false)
	if !strings.Contains(same, "ARG NPM_BEFORE=2026-07-25") {
		t.Errorf("NPM_BEFORE moved with no bumps:\n%s", same)
	}

	bad := applyBeforeDate(df, "8/1/2026; rm -rf /", true)
	if !strings.Contains(bad, "ARG NPM_BEFORE=2026-07-25") {
		t.Errorf("malformed date was written into the Dockerfile:\n%s", bad)
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./cmd/cli-bump/ -run TestApplyBeforeDate -v`
Expected: FAIL to compile (`applyBeforeDate` undefined).

- [ ] **Step 3: Implement.** In `cmd/cli-bump/main.go`:

```go
// beforeDateRE guards -before against injection: strict ISO date only.
var beforeDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

var npmBeforeArgRE = regexp.MustCompile(`(?m)^(ARG NPM_BEFORE=)([^\s]+)`)

// applyBeforeDate rewrites ARG NPM_BEFORE to date when a bump was written
// (bumped). A fresh pin may be published after the old cutoff, so the date
// must move with the pins; with no bumps it stays put so the weekly lane
// does not open date-only PRs. Malformed dates are rejected with a warning.
func applyBeforeDate(dockerfile, date string, bumped bool) string {
	if !bumped || date == "" {
		return dockerfile
	}
	if !beforeDateRE.MatchString(date) {
		fmt.Fprintf(os.Stderr, "cli-bump: warning: rejected malformed -before date %q\n", date)
		return dockerfile
	}
	return npmBeforeArgRE.ReplaceAllString(dockerfile, "${1}"+date)
}
```

In `main()`: add `beforeFlag := flag.String("before", "", "when bumps are written, also move ARG NPM_BEFORE to this UTC date (YYYY-MM-DD)")`, and after `out, bumps := planBumps(...)` insert `out = applyBeforeDate(out, *beforeFlag, len(bumps) > 0)`. Update the doc comment at the top of the file (usage line) to include `[-before YYYY-MM-DD]`.

In `.github/workflows/agent-cli-bump.yml`, rewrite step: change `go run ./cmd/cli-bump` to `go run ./cmd/cli-bump -before "$(date -u +%F)"`.

- [ ] **Step 4: Reconcile docs.** `docs/ROADMAP.md` section 2.5: rewrite the residual note (the "what still floats ... tracked as F-09 follow-up" area, lines ~106-121) to state: apt resolves against a dated snapshot.debian.org archive (`DEBIAN_SNAPSHOT`) and npm resolution is date-cut (`NPM_BEFORE`), both moved deliberately by the bump lane; remaining accepted residuals are registry/mirror trust (a snapshot is reproducibility, not provenance) and the two CLIs whose install scripts must run (claude-code, opencode). Do NOT use the phrase "every external input is pinned" (forbidden by `TestSecurityClaimsNoDrift`). Also update the Dockerfile-referencing comment at the top of the Dockerfile (lines 1-3) if it still describes the old policy, and `CHANGELOG.md` Unreleased:

```markdown
- **The sandbox image's apt and npm dependency graphs are date-pinned
  (F-09).** apt installs resolve against a dated snapshot.debian.org
  archive and every npm resolution is cut at a pinned --before date, so
  two builds of the same commit produce the same dependency set; both
  dates move deliberately via the weekly bump lane.
```

- [ ] **Step 5: Run tests + doc guards**

Run: `go test -race ./cmd/cli-bump/ ./cmd/docs-build/ && go run ./cmd/docs-build`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/cli-bump/main.go cmd/cli-bump/main_test.go .github/workflows/agent-cli-bump.yml docs/ROADMAP.md CHANGELOG.md image/Dockerfile site/
git commit -m "build(cli-bump): move the npm --before cutoff with version bumps; reconcile the F-09 pinning docs"
```

---

## Part D: audit report

### Task 9: commit findings.md with a closure addendum

**Files:**
- Create: `docs/audits/2026-07-12-red-team-review.md` (moved from untracked `/findings.md`)
- Delete: `findings.md` (root, untracked)
- Modify: `SECURITY.md` (link the audit), `CHANGELOG.md` (Unreleased docs note)

- [ ] **Step 1: Move and amend.** `mkdir -p docs/audits && git mv` does not apply (untracked); `mv findings.md docs/audits/2026-07-12-red-team-review.md`. Insert a new section directly under the round-2 addendum header block (after line 30's release recommendation, before the `### V-01` heading), dated today:

```markdown
## Closure addendum, 2026-07-25

All ten findings and both verification-pass gaps are now closed or explicitly
risk-accepted:

- **v0.6.3** closed V-01, F-02, F-03, F-05, F-07, F-08, and V-02 (see the
  CHANGELOG entry for the per-fix regression tests). F-01 and F-06 were
  verified fixed in the round-2 pass above.
- **F-04 closed:** each task's stage dir is now a size-capped APFS sparse
  image (`stage_quota_gb`, default 8 GiB), a filesystem hard wall behind
  the existing polling early-cancel guard. VM-verified by
  `TestRedteam_A8_WorkQuotaHardBound`.
- **F-09 closed with stated residuals:** apt resolves against a dated
  snapshot.debian.org archive and npm resolution is cut at a pinned
  `--before` date, both bumped deliberately. Accepted residuals: registry
  and mirror trust (snapshots give reproducibility, not provenance), and
  the claude-code/opencode install scripts that their packaging requires.
- **F-10 closed:** the security-defaults table is now generated from
  `config.Defaults()` and exported constants
  (`site/docs/security-defaults.md`), with a drift test and a
  verified-by-test-exists test alongside the forbidden-phrase guard.

The report below is retained for audit history. Where its wording and this
addendum disagree, the addendum and the generated security-defaults table
are current.
```

Then normalize punctuation across the whole file: replace every em dash with a comma, colon, or parenthetical that preserves the sentence's meaning (repo prose rule; content stays otherwise verbatim). Check none remain: `grep -c '—' docs/audits/2026-07-12-red-team-review.md` must print 0.

- [ ] **Step 2: Link it.** In `SECURITY.md`, add near the disclosure/verification material (find with `grep -n "## " SECURITY.md`):

```markdown
Past security reviews are kept in-tree: the July 2026 red-team review and
its remediation history live at
[docs/audits/2026-07-12-red-team-review.md](docs/audits/2026-07-12-red-team-review.md).
```

`CHANGELOG.md` Unreleased, under a `### Docs` heading: `- The July 2026 red-team review is committed at docs/audits/, with a closure addendum mapping every finding to its fix.`

- [ ] **Step 3: Run the doc guards**

Run: `go test ./cmd/docs-build/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add docs/audits/2026-07-12-red-team-review.md SECURITY.md CHANGELOG.md
git commit -m "docs(audits): commit the red-team review with a closure addendum"
```

---

## Task 10: full verification and PR

- [ ] **Step 1: Full suite**

Run, expecting every one to pass:

```bash
go test -race -count=1 ./...
go vet ./...
make lint
make redteam
```

- [ ] **Step 2: VM suite** (macOS, needs images: `make image network` first if stale)

Run: `make redteam-vm`
Expected: PASS including the new A8.

- [ ] **Step 3: Push and open the PR** (no banner in the body):

```bash
git push -u origin redteam-residuals
gh pr create --title "fix: close the residual red-team findings (F-04 hard quota, F-09 date pins, F-10 generated claims)" --body "..."
```

PR body: summarize the three closures + the committed audit report, list the verification commands run and their results, and note the two operator-visible changes (`stage_quota_gb` default 8, npm/apt snapshot dates in the image build).

- [ ] **Step 4: Request review** via the requesting-code-review skill (review agents must be read-only or worktree-isolated).

---

## Self-review notes

- Spec coverage: F-04 (Tasks 1-4), F-10 (Tasks 5-6), F-09 (Tasks 7-8), findings.md (Task 9), verification (Task 10). The findings addendum depends on Parts A-C landing first; keep Task 9 after them.
- The A8 mount-an-attached-image-path check (Task 4 Step 2) is the plan's one real unknown; it is front-loaded with an explicit STOP instruction if `container` cannot bind-mount a path backed by an attached disk image.
- Test names in Task 6 are deliberately `PLACEHOLDER` tokens with a closing feedback loop (`TestSecurityDefaultsVerifiedByTestsExist` fails until each cites a real test); the implementer must resolve them by grep, not invent names.
