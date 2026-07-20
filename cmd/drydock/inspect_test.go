package main

import (
	"strings"
	"testing"
	"time"

	"drydock/internal/trustbrief"
)

func writeTestBrief(t *testing.T, dir, id string) trustbrief.Brief {
	t.Helper()
	b := trustbrief.Brief{
		SchemaVersion: 1, TaskID: id, GeneratedAt: time.Now().UTC(),
		Task: trustbrief.TaskFacts{
			InstructionSHA256: trustbrief.HashInstruction("x"),
			RepoRef:           "https://github.com/o/r.git",
			BaseCommit:        "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			Sensitive:         true,
		},
		Runtime: trustbrief.RuntimeFacts{ImageRef: "sandbox:test", Agent: "claude", Vendor: "anthropic"},
		Policy: trustbrief.PolicyFacts{
			BudgetUSD: 2, TimeoutSeconds: 1800,
			EgressDefault: []string{"api.anthropic.com:443"},
		},
		Spend: trustbrief.SpendFacts{USDBrokerMetered: 0.1234, DurationMs: 65000},
		Diff: trustbrief.Analyze("diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n" +
			"--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n@@ -1 +1 @@\n-a\n+b\n"),
		Verification:    trustbrief.Verification{Status: trustbrief.VerificationNotConfigured},
		MissingEvidence: []string{"verification not configured"},
	}
	b.Policy.SnapshotSHA256 = b.Policy.Fingerprint()
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunInspect_RendersBrokerObservedFacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)

	out := captureStdout(t, func() { runInspect([]string{id}) })
	for _, want := range []string{
		id,
		"github.com/o/r",
		"deadbeefdead", // truncated base commit prefix
		"claude",
		"$2.00 (soft)",
		"ci-workflow",
		".github/workflows/ci.yml",
		"$0.1234",
		"sensitive",
		"verification not configured",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "AGENT") {
		t.Errorf("v1 brief has no agent claims; output should not invent an agent section:\n%s", out)
	}
}

func TestRunInspect_JSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)
	out := captureStdout(t, func() { runInspect([]string{"--json", id}) })
	if !strings.Contains(out, `"schema_version": 1`) || !strings.Contains(out, `"snapshot_sha256"`) {
		t.Errorf("--json output not raw brief:\n%s", out)
	}
}

// M1: --json must work regardless of position relative to the task id. A
// stdlib flag.FlagSet stops parsing at the first positional arg, so
// `inspect <id> --json` used to silently ignore --json (fs.NArg() == 2,
// "usage" error). Guard the fix by pinning the flag-after-id ordering; the
// flag-before-id ordering is already covered by TestRunInspect_JSON.
func TestRunInspect_JSON_FlagAfterID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)
	out := captureStdout(t, func() { runInspect([]string{id, "--json"}) })
	if !strings.Contains(out, `"schema_version": 1`) || !strings.Contains(out, `"snapshot_sha256"`) {
		t.Errorf("--json (after id) output not raw brief:\n%s", out)
	}
}

// F1: an unmetered lane's brief must render the honest "uncapped" line, not
// a dollar figure/soft-or-hard label that was never actually enforced.
func TestRunInspect_RendersBudgetUnbounded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	b := writeTestBrief(t, dir, id)
	b.Policy.BudgetUnbounded = true
	b.Policy.BudgetUSD = 0
	b.Policy.BudgetHard = false
	b.Policy.MaxRequests = 1000
	b.Policy.SnapshotSHA256 = b.Policy.Fingerprint()
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "budget uncapped (no USD metering on this lane)") {
		t.Errorf("inspect output missing the uncapped-budget line:\n%s", out)
	}
	if strings.Contains(out, "(soft)") || strings.Contains(out, "(hard)") {
		t.Errorf("inspect output should not label an unbounded budget soft/hard:\n%s", out)
	}
}

func TestSafeCell_StripsControlAndCaps(t *testing.T) {
	in := "evil\x1b[31mred\x1b[0m\npath" + strings.Repeat("A", 500)
	got := safeCell(in)
	if strings.ContainsAny(got, "\x1b\n\r") {
		t.Errorf("control chars survived: %q", got)
	}
	if len(got) > 203 { // 200 runes + ellipsis
		t.Errorf("len = %d, want capped near 200", len(got))
	}
}

// C1 controls (e.g. CSI, U+009B) and Unicode formatting characters (e.g.
// U+202E RIGHT-TO-LEFT OVERRIDE) must be stripped: a hostile filename can
// use either to spoof its displayed extension in the evidence line a
// reviewer reads. Ordinary non-ASCII letters must survive unharmed.
func TestSafeCell_StripsControlAndCaps_C1AndBidi(t *testing.T) {
	in := "evil‮fdp.exe"
	got := safeCell(in)
	if strings.ContainsRune(got, '') {
		t.Errorf("C1 control (U+009B) survived: %q", got)
	}
	if strings.ContainsRune(got, '‮') {
		t.Errorf("bidi override (U+202E) survived: %q", got)
	}
	for _, r := range []rune{'é', '©'} {
		if got := safeCell(string(r)); !strings.ContainsRune(got, r) {
			t.Errorf("safeCell(%q) = %q, want the legitimate non-ASCII rune preserved", string(r), got)
		}
	}
}

func TestBriefFlagKinds_ForPendingColumn(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)
	if got := briefFlagKinds(dir, id); got != "ci-workflow" {
		t.Errorf("briefFlagKinds = %q, want ci-workflow", got)
	}
	if got := briefFlagKinds(dir, "ffffffffffffffffffffffffffffffff"); got != "" {
		t.Errorf("briefFlagKinds for missing brief = %q, want empty", got)
	}
}
