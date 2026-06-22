package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runReview pipes the persisted diff through $PAGER and prompts y/N to
// approve/deny. The whole reason this exists: "drydock pending" → "open the
// diff in another shell" is two steps; this is one.
func runReview(id string) {
	path := diffPath(id)
	if _, err := os.Stat(path); err != nil {
		die("no diff for task %s (looked for %s)", id, path)
	}

	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less -R"
	}
	cmd := pagerCommand(pager, path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		// less exits 0 on q; non-zero usually means user broke out hard.
		// Either way they've seen the diff; proceed to the prompt.
		fmt.Fprintf(os.Stderr, "drydock: pager exited %v; prompting anyway\n", err)
	}

	fmt.Printf("approve task %s? [y/N] ", id)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "y" || line == "yes" {
		signal("approve", id)
		return
	}
	signal("deny", id)
}

// pagerCommand builds `sh -c '<PAGER> "$1"' sh <path>`. PAGER is the (trusted,
// flag-bearing) script; the diff path is passed as the positional arg $1 rather
// than interpolated into the script string, so a path containing spaces or shell
// metacharacters can neither break the command nor inject into it.
func pagerCommand(pager, path string) *exec.Cmd {
	return exec.Command("sh", "-c", pager+` "$1"`, "sh", path)
}
