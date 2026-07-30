package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"drydock/internal/trustbrief"
)

// runReview pipes the persisted diff through $PAGER and prompts y/N to
// approve/deny. The whole reason this exists: "drydock pending" → "open the
// diff in another shell" is two steps; this is one.
func runReview(id string) {
	path := diffPath(id)
	if _, err := os.Stat(path); err != nil {
		die("no diff for task %s (looked for %s)", id, path)
	}

	// Evidence before content: show the broker-observed brief, then page the
	// diff. Older tasks (pre-brief) simply skip the header — a missing brief
	// is not a problem worth interrupting the review for. A brief that exists
	// but fails to parse is different: it means something wrote (or
	// corrupted) the artifact, and staying silent about that would let a
	// reviewer approve a diff thinking "no brief" when the truth is "brief
	// unreadable". Surface that case loudly, on stderr, before paging.
	switch b, err := trustbrief.Read(auditDir(), id); {
	case err == nil:
		printBrief(b)
		fmt.Println()
	case errors.Is(err, fs.ErrNotExist):
		// absent brief — silent, as before.
	default:
		fmt.Fprintf(os.Stderr, "drydock: trust brief for %s unreadable: %v\n", id, err)
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

	r := bufio.NewReader(os.Stdin)

	// Second-look acknowledgments: the broker computed which flagged
	// categories this diff requires (diff_policy.second_look_paths); each must
	// be explicitly acknowledged before the approve, or brokerd refuses it
	// with 422. Prompt per category AFTER the diff has been seen and BEFORE
	// the approve question; any refusal denies (consistent with answering N
	// to the approve prompt — the diff stays in the audit dir, nothing
	// pushes).
	required := secondLookRequired(id)
	acks, acked := promptAcks(r, required)
	if !acked {
		fmt.Println("second-look category not acknowledged — denying")
		signal("deny", id, nil)
		return
	}

	fmt.Printf("approve task %s? [y/N] ", id)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "y" || line == "yes" {
		signal("approve", id, acks)
		return
	}
	signal("deny", id, nil)
}

// promptAcks asks the reviewer to acknowledge each required second-look
// category, in order. All must be answered y/yes for ok=true with the full
// acknowledgment list; the first other answer aborts with ok=false. Category
// kinds are broker-authored stable identifiers but pass through safeCell
// before reaching the terminal, like every other rendered string.
func promptAcks(r *bufio.Reader, required []string) (acks []string, ok bool) {
	acks = make([]string, 0, len(required))
	for _, cat := range required {
		fmt.Printf("acknowledge %s change? [y/N] ", safeCell(cat))
		line, _ := r.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "y" && line != "yes" {
			return nil, false
		}
		acks = append(acks, cat)
	}
	return acks, true
}

// secondLookRequired fetches the pending task's second-look categories from
// brokerd (/admin/tasks). Best-effort by design: on any error it warns and
// returns nil so the review can proceed — the enforcement boundary is
// brokerd itself, which refuses an under-acknowledged approve with a 422
// that signal renders with the --acknowledge re-run hint. There is no path
// on which this fallback approves without the required acks.
func secondLookRequired(id string) []string {
	tasks, err := fetchTasks()
	if err != nil {
		fmt.Fprintf(errOut, "drydock: could not fetch second-look requirements: %v (brokerd will still refuse an under-acknowledged approve)\n", err)
		return nil
	}
	for _, t := range tasks {
		if t.ID == id {
			return t.SecondLook
		}
	}
	return nil
}

// pagerCommand builds `sh -c '<PAGER> "$1"' sh <path>`. PAGER is the (trusted,
// flag-bearing) script; the diff path is passed as the positional arg $1 rather
// than interpolated into the script string, so a path containing spaces or shell
// metacharacters can neither break the command nor inject into it.
func pagerCommand(pager, path string) *exec.Cmd {
	return exec.Command("sh", "-c", pager+` "$1"`, "sh", path)
}
