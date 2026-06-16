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
	// `sh -c "$PAGER"` so PAGER can contain flags.
	cmd := exec.Command("sh", "-c", pager+" "+path)
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
