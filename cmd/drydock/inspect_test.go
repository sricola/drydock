package main

import (
	"os"
	"path/filepath"
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
		Setup:           trustbrief.SetupEvidence{Status: trustbrief.SetupNotConfigured},
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

// The verification block renders every broker-observed fact a reviewer
// triages: overall status, the VM's capability posture (network denied, no
// credentials), the sealed tree hash, per-command argv → exit (duration)
// lines, and the display-only log path.
func TestRunInspect_RendersVerification(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	b := writeTestBrief(t, dir, id)
	b.Verification = trustbrief.Verification{
		Status:      trustbrief.VerificationFailed,
		Network:     "denied",
		Credentials: "none",
		TreeSHA:     "aabbccddeeff00112233445566778899aabbccdd",
		LogSHA256:   "0000000000000000000000000000000000000000000000000000000000000000",
		Commands: []trustbrief.VerifyCommand{
			{Argv: []string{"go", "build", "./..."}, Status: trustbrief.VerifyCmdPassed, DurationMs: 3100},
			{Argv: []string{"go", "test", "./..."}, Status: trustbrief.VerifyCmdFailed, ExitCode: 2, DurationMs: 42000},
			{Argv: []string{"go", "vet", "./..."}, Status: trustbrief.VerifyCmdSkipped},
		},
	}
	b.MissingEvidence = nil
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { runInspect([]string{id}) })
	for _, want := range []string{
		"verify   failed",
		"network denied",
		"no credentials",
		"tree aabbccddeeff", // truncated tree hash prefix
		"go build ./... → exit 0 (3.1s)",
		"go test ./... → exit 2 (42s)",
		"go vet ./... → skipped",
		id + ".verify.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

// A timed-out or errored command has no meaningful exit code: render its
// status word, never a fabricated "exit 0".
func TestRunInspect_RendersVerificationTimedOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	b := writeTestBrief(t, dir, id)
	b.Verification = trustbrief.Verification{
		Status: trustbrief.VerificationInconclusive, Network: "denied", Credentials: "none",
		TreeSHA: "aabbccddeeff00112233445566778899aabbccdd",
		Commands: []trustbrief.VerifyCommand{
			{Argv: []string{"go", "test", "./..."}, Status: trustbrief.VerifyCmdTimedOut, DurationMs: 600000},
		},
	}
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "go test ./... → timed_out (10m00s)") {
		t.Errorf("inspect output missing the timed_out command line:\n%s", out)
	}
	if strings.Contains(out, "exit 0") {
		t.Errorf("a timed-out command must not render a fabricated exit code:\n%s", out)
	}
}

// A not_configured brief must render exactly as it does today: the plain
// status plus the gap line — no capability posture, no log-path hint, no
// command lines (there were none).
func TestRunInspect_NotConfiguredRendersNoVerificationSection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id) // Verification: not_configured

	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "verify   not_configured") {
		t.Errorf("inspect output missing the plain not_configured status:\n%s", out)
	}
	if !strings.Contains(out, "verification not configured") {
		t.Errorf("inspect output missing the gap line:\n%s", out)
	}
	for _, forbid := range []string{"network denied", "no credentials", ".verify.log", "→ exit"} {
		if strings.Contains(out, forbid) {
			t.Errorf("not_configured brief rendered a verification section (%q):\n%s", forbid, out)
		}
	}
}

// The setup block renders every broker-observed fact from the setting_up
// stage with the same grammar as verification: overall status + the setup
// VMs' egress posture, per-command argv → exit (duration) / skipped lines,
// and the display-only .setup.log path. It renders BEFORE the verification
// block, because setup runs first.
func TestRunInspect_RendersSetup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	b := writeTestBrief(t, dir, id)
	b.Setup = trustbrief.SetupEvidence{
		Status:  trustbrief.SetupFailed,
		Network: "egress-allowlisted",
		Commands: []trustbrief.SetupCommand{
			{Argv: []string{"npm", "ci"}, Status: trustbrief.VerifyCmdPassed, DurationMs: 9100},
			{Argv: []string{"node", "--version"}, Status: trustbrief.VerifyCmdFailed, ExitCode: 1, DurationMs: 300},
			{Argv: []string{"curl", "-fsS", "localhost:3000"}, Status: trustbrief.VerifyCmdSkipped},
		},
	}
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { runInspect([]string{id}) })
	for _, want := range []string{
		"setup    failed",
		"network egress-allowlisted",
		"npm ci → exit 0 (9.1s)",
		"node --version → exit 1 (300ms)",
		"curl -fsS localhost:3000 → skipped",
		id + ".setup.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
	su, ve := strings.Index(out, "setup    "), strings.Index(out, "verify   ")
	if su < 0 || ve < 0 || su > ve {
		t.Errorf("setup section must render before verification (setup@%d verify@%d):\n%s", su, ve, out)
	}
}

// The dependency-cache line renders inside the setup block when the brief
// carries cache evidence: status plus the 12-hex key prefix for an active
// cache. Broker-observed facts only — the key prefix comes from the brief,
// never from anything the agent wrote.
func TestRunInspect_RendersSetupCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	b := writeTestBrief(t, dir, id)
	b.Setup = trustbrief.SetupEvidence{
		Status:  trustbrief.SetupPassed,
		Network: "egress-allowlisted",
		Cache:   &trustbrief.SetupCache{Status: trustbrief.CacheHit, Key: "4aa1a73e6b02", Hit: true},
		Commands: []trustbrief.SetupCommand{
			{Argv: []string{"npm", "ci"}, Status: trustbrief.VerifyCmdPassed, DurationMs: 1200},
		},
	}
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "cache    hit · key 4aa1a73e6b02") {
		t.Errorf("inspect output missing the cache evidence line:\n%s", out)
	}
	su, ca := strings.Index(out, "setup    "), strings.Index(out, "cache    ")
	if su < 0 || ca < 0 || ca < su {
		t.Errorf("cache line must render inside the setup block (setup@%d cache@%d):\n%s", su, ca, out)
	}
}

// A cache disabled for cause (no lockfile) renders its status alone — there
// is no key to show, and fabricating one would misstate the evidence. A
// brief with no cache evidence at all renders no cache line.
func TestRunInspect_RendersSetupCacheDisabledAndAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	b := writeTestBrief(t, dir, id)
	b.Setup = trustbrief.SetupEvidence{
		Status:  trustbrief.SetupPassed,
		Network: "egress-allowlisted",
		Cache:   &trustbrief.SetupCache{Status: trustbrief.CacheDisabledNoLockfile},
		Commands: []trustbrief.SetupCommand{
			{Argv: []string{"npm", "ci"}, Status: trustbrief.VerifyCmdPassed, DurationMs: 1200},
		},
	}
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "cache    disabled: no lockfile") {
		t.Errorf("inspect output missing the disabled-cache line:\n%s", out)
	}
	if strings.Contains(out, "· key") {
		t.Errorf("a disabled cache must not render a key:\n%s", out)
	}

	// No cache evidence at all (nil pointer): no cache line.
	b.Setup.Cache = nil
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { runInspect([]string{id}) })
	if strings.Contains(out, "cache    ") {
		t.Errorf("a brief without cache evidence rendered a cache line:\n%s", out)
	}
}

// A timed-out or errored setup command has no meaningful exit code: render
// its status word, never a fabricated "exit 0" (same rule as verification).
func TestRunInspect_RendersSetupTimedOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	b := writeTestBrief(t, dir, id)
	b.Setup = trustbrief.SetupEvidence{
		Status: trustbrief.SetupFailed, Network: "egress-allowlisted",
		Commands: []trustbrief.SetupCommand{
			{Argv: []string{"npm", "ci"}, Status: trustbrief.VerifyCmdTimedOut, DurationMs: 600000},
		},
	}
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "npm ci → timed_out (10m00s)") {
		t.Errorf("inspect output missing the timed_out setup line:\n%s", out)
	}
	if strings.Contains(out, "exit 0") {
		t.Errorf("a timed-out setup command must not render a fabricated exit code:\n%s", out)
	}
}

// A not_configured setup block is a single status line: no egress posture,
// no command lines, no log-path hint (there is no log). Old briefs written
// before the setup stage existed (empty status) render the same way.
func TestRunInspect_SetupNotConfiguredRendersSingleLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id) // Setup: not_configured

	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "setup    not_configured") {
		t.Errorf("inspect output missing the plain not_configured setup status:\n%s", out)
	}
	for _, forbid := range []string{"egress-allowlisted", ".setup.log", "→ exit"} {
		if strings.Contains(out, forbid) {
			t.Errorf("not_configured brief rendered a setup section (%q):\n%s", forbid, out)
		}
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
	in := "evil\u009b\u202efdp.exe"
	got := safeCell(in)
	if strings.ContainsRune(got, '\u009b') {
		t.Errorf("C1 control (U+009B) survived: %q", got)
	}
	if strings.ContainsRune(got, '\u202e') {
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

// writePlanBrief writes a plan-only brief (PlanOnly + IssueURL set) for id.
func writePlanBrief(t *testing.T, dir, id, issueURL string) {
	t.Helper()
	b := trustbrief.Brief{
		SchemaVersion: 1, TaskID: id, GeneratedAt: time.Now().UTC(),
		Task: trustbrief.TaskFacts{
			InstructionSHA256: trustbrief.HashInstruction("plan it"),
			RepoRef:           "https://github.com/o/r.git",
			PlanOnly:          true,
			IssueURL:          issueURL,
		},
		Runtime:      trustbrief.RuntimeFacts{ImageRef: "sandbox:test", Agent: "claude", Vendor: "anthropic"},
		Policy:       trustbrief.PolicyFacts{BudgetUSD: 2, TimeoutSeconds: 1800},
		Verification: trustbrief.Verification{Status: trustbrief.VerificationNotConfigured},
		Setup:        trustbrief.SetupEvidence{Status: trustbrief.SetupNotConfigured},
	}
	b.Policy.SnapshotSHA256 = b.Policy.Fingerprint()
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
}

// A plan-only brief renders the issue provenance, the planned indicator with
// the artifact path, and the captured plan inline (untrusted agent output:
// control characters stripped per line).
func TestRunInspect_PlanOnlyRendersIssueAndPlanInline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	issueURL := "https://github.com/o/r/issues/42"
	writePlanBrief(t, dir, id, issueURL)
	planText := "## Plan\n1. add the feature\x1b[31m\n2. test it\n"
	if err := os.WriteFile(filepath.Join(dir, id+".plan.md"), []byte(planText), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { runInspect([]string{id}) })
	for _, want := range []string{
		"issue    " + issueURL,
		"planned",
		id + ".plan.md",
		"## Plan",
		"1. add the feature",
		"2. test it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("plan content rendered with a raw escape byte (terminal-injection):\n%q", out)
	}
}

// No plan artifact (the agent never wrote one): the planned indicator still
// renders, pointing at the path, without inventing content.
func TestRunInspect_PlanOnlyWithoutArtifactStillIndicates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writePlanBrief(t, dir, id, "")
	out := captureStdout(t, func() { runInspect([]string{id}) })
	if !strings.Contains(out, "planned") || !strings.Contains(out, id+".plan.md") {
		t.Errorf("plan indicator missing without artifact:\n%s", out)
	}
	if strings.Contains(out, "issue ") {
		t.Errorf("no issue line expected when IssueURL is empty:\n%s", out)
	}
}

// A symlinked plan artifact must be refused (O_NOFOLLOW), not followed — the
// audit dir is host-side but the read path must match every other audit
// artifact's defense posture.
func TestRunInspect_SymlinkedPlanArtifactRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writePlanBrief(t, dir, id, "")
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, id+".plan.md")); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runInspect([]string{id}) })
	if strings.Contains(out, "SECRET-CONTENT") {
		t.Errorf("symlinked plan.md was followed:\n%s", out)
	}
}

// A non-plan brief must not grow issue/plan lines.
func TestRunInspect_NonPlanBriefHasNoPlanLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)
	out := captureStdout(t, func() { runInspect([]string{id}) })
	if strings.Contains(out, "issue ") || strings.Contains(out, "plan ") {
		t.Errorf("non-plan brief rendered issue/plan lines:\n%s", out)
	}
}
