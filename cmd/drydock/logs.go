package main

import (
	"errors"
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
		die("%v", err) // err already includes the path
	}
	defer f.Close()

	if _, err := io.Copy(stdout(), f); err != nil {
		die("%v", err)
	}
	if !follow {
		return
	}

	// Poll for appended bytes. brokerd writes via os.File which closes/
	// reopens are not needed — the file stays the same inode, just grows.
	for {
		n, err := io.Copy(stdout(), f)
		if errors.Is(err, io.EOF) || err == nil {
			if n > 0 {
				continue
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		fmt.Fprintf(stderr(), "drydock logs: %v\n", err)
		return
	}
}

// Indirected through helpers so other subcommands can override (none today,
// but it keeps test-friendliness cheap).
func stdout() io.Writer { return os.Stdout }
func stderr() io.Writer { return os.Stderr }
