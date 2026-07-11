//go:build integration

// Regression test for the shutdown race that orphaned squid (fixed in #146).
//
// brokerd's SIGTERM handler used to call srv.Shutdown, which unblocked main's
// Serve; main then returned and exited the process before the handler ran
// cleanup()/squid.Stop(). squid was left holding gateway_ip:3128, so the next
// start failed to bind. The fix runs cleanup in main after Serve returns.
//
// This asserts brokerd's OWN shutdown reaps its squid child, WITHOUT the
// harness cleanupOrphans() pkill safety net. Prereqs are the same as the other
// integration tests (see brokerd_test.go header). Run:
//
//	go test -tags=integration -run TestBrokerd_ShutdownReapsSquid ./tests/integration/
package integration

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBrokerd_ShutdownReapsSquid(t *testing.T) {
	h := startBrokerd(t)

	// brokerd is ready (waitReady passed), so squid has started as its child.
	squidPid := squidChildOf(h.cmd.Process.Pid)
	if squidPid == 0 {
		t.Fatal("no squid child found for brokerd; cannot test shutdown reaping")
	}
	if !processAlive(squidPid) {
		t.Fatalf("squid %d not alive before shutdown", squidPid)
	}

	// Graceful shutdown, the way `kill <brokerd>` does it.
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM brokerd: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("brokerd did not exit within 15s of SIGTERM")
	}
	h.stopped = true // we drove the shutdown; skip the t.Cleanup double-signal

	// The fix: brokerd's own cleanup (now in main, after Serve returns) must
	// have reaped squid. No harness pkill has run yet, so a surviving squid
	// means the shutdown teardown regressed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(squidPid) {
			return // squid reaped by brokerd itself
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(squidPid, syscall.SIGKILL) // don't leak the orphan
	t.Fatalf("squid %d survived brokerd shutdown: it was orphaned (cleanup did not run)", squidPid)
}

// squidChildOf returns the pid of the `squid -N` process parented by pid, or 0.
func squidChildOf(pid int) int {
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(pid), "-f", "squid -N").Output()
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// processAlive reports whether pid names a live process (signal 0 is an
// existence check). EPERM means alive but not ours to signal.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
