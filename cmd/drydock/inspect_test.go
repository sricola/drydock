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
