package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"drydock/internal/trustbrief"
)

// runInspect renders a task's trust brief: the broker-observed evidence a
// reviewer triages before opening the diff. --json prints the raw artifact.
//
// Args are scanned by hand (no flag.FlagSet) so --json/-json is accepted in
// either position relative to the task id — `inspect <id> --json` and
// `inspect --json <id>` both work. A stdlib FlagSet stops parsing flags at
// the first positional argument, which would silently ignore --json when it
// follows the id.
func runInspect(args []string) {
	var jsonOut bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--json", "-json":
			jsonOut = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		die("usage: drydock inspect <id> [--json]")
	}
	id := rest[0]
	b, err := trustbrief.Read(auditDir(), id)
	if err != nil {
		die("no trust brief for task %s: %v", id, err)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(b)
		return
	}
	printBrief(b)
}

// printBrief renders the human summary. Every value here is broker-observed;
// file paths originate from the (attacker-influenceable) work tree, so they
// pass through safeCell before reaching the terminal.
func printBrief(b trustbrief.Brief) {
	labels := ""
	if b.Task.Sensitive {
		labels += " · sensitive"
	}
	if b.Task.AutoApprove {
		labels += " · auto-approve"
	}
	fmt.Printf("task     %s%s\n", safeCell(b.TaskID), labels)
	base := b.Task.BaseCommit
	if len(base) > 12 {
		base = base[:12]
	}
	repoLine := safeCell(b.Task.RepoRef)
	if base != "" {
		repoLine += " @ " + base
	}
	fmt.Printf("repo     %s\n", repoLine)
	fmt.Printf("runtime  agent=%s vendor=%s model=%s image=%s\n",
		safeCell(b.Runtime.Agent), safeCell(b.Runtime.Vendor),
		orDash(safeCell(b.Runtime.Model)), safeCell(b.Runtime.ImageRef))
	budget := fmt.Sprintf("$%.2f (soft)", b.Policy.BudgetUSD)
	if b.Policy.BudgetHard {
		budget = fmt.Sprintf("$%.2f (hard)", b.Policy.BudgetUSD)
	}
	if b.Policy.BudgetUnbounded {
		budget = "uncapped (no USD metering on this lane)"
	}
	fmt.Printf("policy   budget %s · timeout %ds · policy sha %.12s\n",
		budget, b.Policy.TimeoutSeconds, b.Policy.SnapshotSHA256)
	fmt.Printf("egress   default: %s · widened: %s\n",
		orDash(strings.Join(b.Policy.EgressDefault, " ")),
		orDash(strings.Join(b.Policy.EgressWidened, " ")))
	fmt.Printf("spend    $%.4f · %s\n", b.Spend.USDBrokerMetered, shortDur(b.Spend.DurationMs))
	adds, dels := 0, 0
	for _, f := range b.Diff.Files {
		adds += f.Adds
		dels += f.Dels
	}
	trunc := ""
	if b.Diff.Truncated {
		trunc = " · TRUNCATED"
	}
	fmt.Printf("diff     sha %.12s · %d bytes · %d files (+%d -%d)%s\n",
		b.Diff.SHA256, b.Diff.Bytes, len(b.Diff.Files)+b.Diff.FilesOmitted, adds, dels, trunc)
	for _, fl := range b.Diff.Flags {
		paths := make([]string, 0, len(fl.Paths))
		for _, p := range fl.Paths {
			paths = append(paths, safeCell(p))
		}
		fmt.Printf("FLAG     %s: %s\n", safeCell(fl.Kind), strings.Join(paths, ", "))
	}
	fmt.Printf("verify   %s\n", safeCell(b.Verification.Status))
	for _, m := range b.MissingEvidence {
		fmt.Printf("gap      %s\n", safeCell(m))
	}
}

func orDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// safeCell strips control characters (terminal-escape defense: paths come
// from the untrusted work tree) and caps length for column sanity.
func safeCell(s string) string {
	var sb strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		sb.WriteRune(r)
		n++
		if n >= 200 {
			sb.WriteString("…")
			break
		}
	}
	return sb.String()
}

// briefFlagKinds returns the comma-joined flag kinds from a task's brief,
// or "" when no brief exists (older task) or it doesn't parse. Used by the
// pending list's FLAGS column — advisory triage, so absence is silent.
func briefFlagKinds(dir, id string) string {
	b, err := trustbrief.Read(dir, id)
	if err != nil {
		return ""
	}
	kinds := make([]string, 0, len(b.Diff.Flags))
	for _, f := range b.Diff.Flags {
		kinds = append(kinds, f.Kind)
	}
	return strings.Join(kinds, ",")
}
