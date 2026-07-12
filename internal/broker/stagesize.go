package broker

import (
	"io/fs"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// A task's /work is a plain host bind mount, so a hostile in-VM agent can fill
// the host filesystem (dd of=/work/x) or exhaust inodes (millions of small
// files). These are soft (polling) bounds that cancel such a task; worst-case
// overshoot is about fill_rate * stageSizeInterval. Vars (not consts) only so
// tests can lower them; nothing in production writes them.
var (
	maxStageBytes     int64 = 4 << 30 // total file bytes under the stage
	maxStageFiles           = 200_000 // file-count (inode) bound
	stageSizeInterval       = 2 * time.Second
)

// minFreeStageBytes is the host free space below which a task is refused at
// submit (preflight) or cancelled mid-run (monitor): fail closed rather than
// exhaust the host disk.
var minFreeStageBytes int64 = 2 << 30 // 2 GiB

// freeBytes returns the bytes available to an unprivileged user on the
// filesystem containing path.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func belowFreeFloor(path string) bool {
	free, err := freeBytes(path)
	return err == nil && free < minFreeStageBytes
}

// stageOverLimit walks root, returning true as soon as the total regular-file
// size exceeds maxBytes or the file count exceeds maxFiles, stopping early so it
// never fully walks a pathological tree.
func stageOverLimit(root string, maxBytes int64, maxFiles int) bool {
	var total int64
	files := 0
	over := false
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best effort; ignore transient read races
		}
		if d.IsDir() {
			return nil
		}
		files++
		if info, e := d.Info(); e == nil {
			total += info.Size()
		}
		if total > maxBytes || files > maxFiles {
			over = true
			return filepath.SkipAll
		}
		return nil
	})
	return over
}

// stageSizeGuard watches a stage dir and cancels the task if it grows past the
// byte/file bounds or the host runs low on free space. Call stop() after the run
// and exceeded() to check whether it tripped.
type stageSizeGuard struct {
	fired atomic.Bool
	stop  func()
}

func (g *stageSizeGuard) exceeded() bool { return g.fired.Load() }

// watchStageSize polls root every interval until stop() is called, invoking
// onExceed once if the stage crosses its bounds or host free space drops below
// the floor. Cross-platform (no runtime dependency), so it is CI-testable.
func watchStageSize(root string, interval time.Duration, onExceed func()) *stageSizeGuard {
	g := &stageSizeGuard{}
	done := make(chan struct{})
	var once sync.Once
	g.stop = func() { once.Do(func() { close(done) }) }
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if stageOverLimit(root, maxStageBytes, maxStageFiles) || belowFreeFloor(root) {
					if g.fired.CompareAndSwap(false, true) {
						onExceed()
					}
					return
				}
			}
		}
	}()
	return g
}
