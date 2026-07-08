//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Round-trips a THROWAWAY launchd job (its own label, TempDir plist, stub
// binary) through bootstrap → print → bootout using the real launchctl.
// Never touches the real so.sri.drydock.brokerd job.
func TestLaunchctlRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only")
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		t.Skip("launchctl not on PATH")
	}

	const testLabel = "so.sri.drydock.inttest"
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "stub.log")
	plist, err := renderPlist(testLabel, stub, logPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(dir, testLabel+".plist")
	if err := os.WriteFile(pp, plist, 0o644); err != nil {
		t.Fatal(err)
	}

	_ = launchctlBootout(testLabel) // stale job from a previous run
	if err := launchctlBootstrap(pp); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = launchctlBootout(testLabel) })

	// RunAtLoad starts the stub; poll until launchd reports running.
	deadline := time.Now().Add(10 * time.Second)
	var st launchdState
	for time.Now().Before(deadline) {
		if out, err := launchctlPrint(testLabel); err == nil {
			if st = parseLaunchdState(out); st.Running {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !st.Running || st.PID == "" {
		t.Fatalf("job never reported running: %+v", st)
	}

	if err := launchctlBootout(testLabel); err != nil {
		t.Fatalf("bootout: %v", err)
	}
	if _, err := launchctlPrint(testLabel); err == nil {
		t.Error("job still loaded after bootout")
	}
}
