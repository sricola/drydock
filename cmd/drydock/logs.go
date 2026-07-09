package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

// runLogs prints the task's audit jsonl. With follow=true, behaves like
// `tail -f`: re-reads new bytes as brokerd writes them. We don't shell out
// to tail so it works the same on stock macOS as on a sandboxed runner.
func runLogs(id string, follow bool) {
	path := auditPath(id)
	f, err := os.Open(path)
	if err != nil {
		// Reattach race: `drydock submit` prints the `logs <id> -f` hint on ^C
		// before brokerd has created the trace. In follow mode, poll for the
		// file to appear instead of dying on the advice the tool just gave.
		if !follow || !os.IsNotExist(err) {
			die("%v", err) // err already includes the path
		}
		for os.IsNotExist(err) {
			time.Sleep(250 * time.Millisecond)
			f, err = os.Open(path)
		}
		if err != nil {
			die("%v", err)
		}
	}
	defer f.Close()

	if _, err := io.Copy(os.Stdout, f); err != nil {
		die("%v", err)
	}
	if !follow {
		return
	}

	// Poll for appended bytes. brokerd writes via os.File which closes/
	// reopens are not needed — the file stays the same inode, just grows.
	for {
		n, err := io.Copy(os.Stdout, f)
		if err == nil {
			if n > 0 {
				continue
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		fmt.Fprintf(os.Stderr, "drydock logs: %v\n", err)
		return
	}
}
