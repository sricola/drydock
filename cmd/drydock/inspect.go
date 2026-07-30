package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"drydock/internal/audit"
	"drydock/internal/stage"
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
	if b.Task.IssueURL != "" {
		fmt.Printf("issue    %s\n", safeCell(b.Task.IssueURL))
	}
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
	printSetup(b)
	printVerification(b)
	printPlan(b)
	for _, m := range b.MissingEvidence {
		fmt.Printf("gap      %s\n", safeCell(m))
	}
}

// printPlan renders the plan-mode block for a PlanOnly brief: the "planned"
// indicator with the artifact path, then the captured plan inline. The plan
// is untrusted agent output, so the read mirrors the other audit-artifact
// defenses — O_NOFOLLOW (audit.OpenRead refuses a planted symlink), capped
// at the same bound the broker enforced when persisting (stage.MaxPlanBytes)
// — and every line passes through safeCell (control-strip + cap) before
// reaching the terminal. A missing artifact leaves the indicator line alone:
// the run was still planned, the agent just wrote no plan (has_plan:false).
func printPlan(b trustbrief.Brief) {
	if !b.Task.PlanOnly {
		return
	}
	path := filepath.Join(auditDir(), b.TaskID+".plan.md")
	fmt.Printf("plan     planned — review %s\n", path)
	f, err := audit.OpenRead(path)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, stage.MaxPlanBytes))
	if err != nil || len(data) == 0 {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		fmt.Printf("         %s\n", safeCell(line))
	}
}

// printSetup renders the setting_up-stage evidence block, mirroring
// printVerification: a not_configured brief gets the plain status line only
// (a brief from before the setup stage existed — empty status — renders the
// same way); a populated block gets the setup VMs' egress posture, one line
// per command with the broker-observed verdict, and the display-only log
// path. Rendered before the verification block because setup runs first.
//
// Argv elements are operator config, not VM output, but they still pass
// through safeCell (strip + cap) like every other rendered string; the log
// path is host-constructed from auditDir and the validated task id.
func printSetup(b trustbrief.Brief) {
	s := b.Setup
	if s.Status == "" || s.Status == trustbrief.SetupNotConfigured {
		fmt.Printf("setup    %s\n", trustbrief.SetupNotConfigured)
		return
	}
	line := safeCell(s.Status)
	if s.Network != "" {
		line += " · network " + safeCell(s.Network)
	}
	fmt.Printf("setup    %s\n", line)
	for _, c := range s.Commands {
		argv := safeCell(strings.Join(c.Argv, " "))
		switch c.Status {
		case trustbrief.VerifyCmdPassed, trustbrief.VerifyCmdFailed:
			fmt.Printf("         %s → exit %d (%s)\n", argv, c.ExitCode, shortDur(c.DurationMs))
		case trustbrief.VerifyCmdSkipped:
			fmt.Printf("         %s → skipped\n", argv)
		default: // timed_out, error: no meaningful exit code
			fmt.Printf("         %s → %s (%s)\n", argv, safeCell(c.Status), shortDur(c.DurationMs))
		}
	}
	if len(s.Commands) > 0 {
		fmt.Printf("         log %s\n", filepath.Join(auditDir(), b.TaskID+".setup.log"))
	}
}

// printVerification renders the independent-verifier evidence block. A
// not_configured brief renders exactly as before this stage existed: the
// plain status line (the gap list explains it). A populated block gets the
// capability posture, the sealed-tree hash, one line per command with the
// broker-observed verdict, and the display-only log path.
//
// Argv elements are operator config, not VM output, but they still pass
// through safeCell (strip + cap) like every other rendered string; the log
// path is host-constructed from auditDir and the validated task id.
func printVerification(b trustbrief.Brief) {
	v := b.Verification
	if v.Status == trustbrief.VerificationNotConfigured {
		fmt.Printf("verify   %s\n", safeCell(v.Status))
		return
	}
	line := safeCell(v.Status)
	if v.Network != "" {
		line += " · network " + safeCell(v.Network)
	}
	switch v.Credentials {
	case "":
	case "none":
		line += " · no credentials"
	default:
		line += " · credentials " + safeCell(v.Credentials)
	}
	if v.TreeSHA != "" {
		line += fmt.Sprintf(" · tree %.12s", safeCell(v.TreeSHA))
	}
	fmt.Printf("verify   %s\n", line)
	for _, c := range v.Commands {
		argv := safeCell(strings.Join(c.Argv, " "))
		switch c.Status {
		case trustbrief.VerifyCmdPassed, trustbrief.VerifyCmdFailed:
			fmt.Printf("         %s → exit %d (%s)\n", argv, c.ExitCode, shortDur(c.DurationMs))
		case trustbrief.VerifyCmdSkipped:
			fmt.Printf("         %s → skipped\n", argv)
		default: // timed_out, error: no meaningful exit code
			fmt.Printf("         %s → %s (%s)\n", argv, safeCell(c.Status), shortDur(c.DurationMs))
		}
	}
	if len(v.Commands) > 0 {
		fmt.Printf("         log %s\n", filepath.Join(auditDir(), b.TaskID+".verify.log"))
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
//
// C0 controls (< 0x20) and DEL are the classic terminal-escape vector, but
// C1 controls (0x80-0x9F, e.g. CSI at U+009B) are just as live a threat over
// UTF-8-aware terminals, and Unicode formatting characters — bidi overrides
// like U+202E RIGHT-TO-LEFT OVERRIDE, embeddings, and zero-width chars —
// let a hostile filename visually spoof its extension in the evidence line
// a reviewer reads (e.g. "cod‮fdp.exe" renders as "cod<reversed>"). Both
// are stripped here; ordinary non-ASCII letters (é, ©, …) are untouched.
func safeCell(s string) string {
	var sb strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if r >= 0x80 && r <= 0x9f {
			continue
		}
		if unicode.Is(unicode.Cf, r) {
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
