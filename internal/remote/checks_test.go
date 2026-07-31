package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// These tests pin the host-side CI *conclusion* read (D3). The single rule
// every case below exists to protect: ABSENCE OF EVIDENCE MUST NEVER READ AS
// PASSING. An error, a truncated read, an empty PR, a skipped-only run, and an
// unrecognized bucket must all resolve to something that is not RollupPassed.

// ghScript is a scripted stand-in for the capped gh runner. It dispatches on
// the subcommand so one script can answer both `pr checks` and the
// `pr view --json statusCheckRollup` disambiguation probe.
type ghScript struct {
	checksOut   string
	checksTrunc bool
	checksErr   error
	viewOut     string
	viewTrunc   bool
	viewErr     error

	calls [][]string // full argv of each invocation, argv[0] == name
	envs  [][]string
}

func (g *ghScript) install(t *testing.T) {
	t.Helper()
	orig := runCLIOutputCapped
	t.Cleanup(func() { runCLIOutputCapped = orig })
	runCLIOutputCapped = func(env []string, limit int, name string, args ...string) ([]byte, bool, error) {
		g.calls = append(g.calls, append([]string{name}, args...))
		g.envs = append(g.envs, env)
		if len(args) >= 2 && args[1] == "view" {
			return []byte(g.viewOut), g.viewTrunc, g.viewErr
		}
		return []byte(g.checksOut), g.checksTrunc, g.checksErr
	}
}

// ---- rollup ----

// TestRollupFor is the heart of the contract. Note in particular:
//   - no checks is NOT passing (a PR with no CI configured has produced no
//     evidence that anything works);
//   - a terminal failure DOMINATES still-pending checks, mirroring the
//     verifier's "any failed -> failed" precedence (verify.go);
//   - a run in which every check was skipped produced no pass evidence either.
func TestRollupFor(t *testing.T) {
	mk := func(states ...CheckState) CheckSummary {
		var s CheckSummary
		for _, st := range states {
			s.count(st)
		}
		s.Rollup = rollupFor(s)
		return s
	}
	cases := []struct {
		name   string
		states []CheckState
		want   CheckRollup
	}{
		{"no checks at all is NOT passed", nil, RollupNoChecks},
		{"every check passed", []CheckState{CheckPassed, CheckPassed}, RollupPassed},
		{"one failure among passes", []CheckState{CheckPassed, CheckFailed, CheckPassed}, RollupFailed},
		{"pending with one failure -> failed (a terminal failure is conclusive)",
			[]CheckState{CheckPending, CheckFailed, CheckPassed}, RollupFailed},
		{"pending with only passes so far", []CheckState{CheckPassed, CheckPending}, RollupPending},
		{"all pending", []CheckState{CheckPending, CheckPending}, RollupPending},
		{"cancelled is terminal and not successful", []CheckState{CheckPassed, CheckCancelled}, RollupFailed},
		{"everything skipped is no evidence, not a pass", []CheckState{CheckSkipped, CheckSkipped}, RollupNoChecks},
		{"a pass plus skips is a pass", []CheckState{CheckPassed, CheckSkipped}, RollupPassed},
		{"an unmodeled bucket is not evidence of passing", []CheckState{CheckPassed, CheckUnknown}, RollupPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mk(tc.states...)
			if got.Rollup != tc.want {
				t.Fatalf("rollup = %q, want %q (counts %+v)", got.Rollup, tc.want, got)
			}
			if tc.want != RollupPassed && got.Rollup == RollupPassed {
				t.Fatalf("absence of evidence read as passing")
			}
		})
	}
}

// TestCheckStateFor pins the gh bucket/state mapping, including the fallback
// used when a gh build omits `bucket`.
func TestCheckStateFor(t *testing.T) {
	cases := []struct {
		bucket, state string
		want          CheckState
	}{
		{"pass", "SUCCESS", CheckPassed},
		{"fail", "FAILURE", CheckFailed},
		{"pending", "IN_PROGRESS", CheckPending},
		{"skipping", "SKIPPED", CheckSkipped},
		{"cancel", "CANCELLED", CheckCancelled},
		{"", "SUCCESS", CheckPassed},
		{"", "FAILURE", CheckFailed},
		{"", "TIMED_OUT", CheckFailed},
		{"", "ACTION_REQUIRED", CheckFailed},
		{"", "QUEUED", CheckPending},
		{"", "WAITING", CheckPending},
		{"", "NEUTRAL", CheckSkipped},
		{"", "STALE", CheckCancelled},
		{"", "", CheckUnknown},
		{"martian", "MARTIAN", CheckUnknown},
	}
	for _, tc := range cases {
		if got := checkStateFor(tc.bucket, tc.state); got != tc.want {
			t.Errorf("checkStateFor(%q,%q) = %q, want %q", tc.bucket, tc.state, got, tc.want)
		}
	}
}

// ---- argv / host pinning ----

// TestChecks_Argv pins the exact command. The host is hard-pinned INSIDE the
// --repo value for the same reason FetchIssue does it: the curated env
// forwards GH_HOST, and without the pin an exported GH_HOST=github.mycorp.com
// would aim the host's gh credential at an attacker-nominated host while the
// marker still says github.com.
func TestChecks_Argv(t *testing.T) {
	g := &ghScript{checksOut: `[{"bucket":"pass","name":"build","state":"SUCCESS"}]`}
	g.install(t)
	env := []string{"GH_HOST=github.evil.test", "PATH=/usr/bin"}

	if _, err := Checks(env, "my-org", "my.repo", 42); err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if len(g.calls) != 1 {
		t.Fatalf("gh invocations = %d, want 1", len(g.calls))
	}
	got := g.calls[0]
	want := []string{"gh", "pr", "checks", "42",
		"--repo", "github.com/my-org/my.repo",
		"--json", "bucket,name,state"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	if got[0] != "gh" {
		t.Errorf("argv[0] = %q, want the compile-time literal \"gh\"", got[0])
	}
	if !reflect.DeepEqual(g.envs[0], env) {
		t.Errorf("env = %q, want the caller's curated env verbatim", g.envs[0])
	}
	// D3: only conclusions. No log/annotation field may be requested.
	fields := want[len(want)-1]
	for _, banned := range []string{"link", "log", "description", "output", "annotation"} {
		if strings.Contains(fields, banned) {
			t.Errorf("--json fields %q requests %q; B1 must read conclusions only", fields, banned)
		}
	}
}

// TestChecks_RejectsBadOwnerRepo: validation happens BEFORE any argv exists.
func TestChecks_RejectsBadOwnerRepo(t *testing.T) {
	g := &ghScript{checksOut: `[]`}
	g.install(t)
	bad := [][2]string{
		{"--evil-flag", "r"}, {"o", "--evil-flag"}, {"..", "r"}, {"o", ".."},
		{"", "r"}, {"o", ""}, {"o/../x", "r"}, {"o", "a..b"}, {"o", "r r"},
	}
	for _, b := range bad {
		if _, err := Checks(nil, b[0], b[1], 1); err == nil {
			t.Errorf("Checks(%q,%q) accepted an invalid owner/repo", b[0], b[1])
		}
	}
	if _, err := Checks(nil, "o", "r", 0); err == nil {
		t.Error("Checks accepted PR number 0")
	}
	if _, err := Checks(nil, "o", "r", -3); err == nil {
		t.Error("Checks accepted a negative PR number")
	}
	if len(g.calls) != 0 {
		t.Fatalf("gh was invoked %d times for rejected input; want 0", len(g.calls))
	}
}

// ---- decode-first, never exit-code-first ----

// TestChecks_NonZeroExitWithValidJSONStillDecodes: gh exits 8 when checks are
// pending and 1 when they fail — in BOTH cases it prints a valid JSON list.
// The conclusion must come from that payload, never from the exit status.
func TestChecks_NonZeroExitWithValidJSONStillDecodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want CheckRollup
	}{
		{"pending (gh exit 8)", `[{"bucket":"pending","name":"build","state":"IN_PROGRESS"}]`, RollupPending},
		{"failed (gh exit 1)", `[{"bucket":"fail","name":"build","state":"FAILURE"}]`, RollupFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &ghScript{checksOut: tc.out, checksErr: errors.New("gh: exit status 8")}
			g.install(t)
			sum, err := Checks(nil, "o", "r", 7)
			if err != nil {
				t.Fatalf("Checks: %v", err)
			}
			if sum.Rollup != tc.want {
				t.Fatalf("rollup = %q, want %q", sum.Rollup, tc.want)
			}
			if len(g.calls) != 1 {
				t.Errorf("gh invocations = %d; the probe must not run when the list decoded", len(g.calls))
			}
		})
	}
}

// TestChecks_NoChecks_DisambiguatedStructurally: `gh pr checks` exits non-zero
// with EMPTY stdout for BOTH "this PR has no checks" and a genuine failure, so
// the exit code alone cannot tell them apart. The disambiguation is structural
// (`gh pr view --json statusCheckRollup`, which exits 0 with an empty array),
// never a parse of gh's prose.
func TestChecks_NoChecks_DisambiguatedStructurally(t *testing.T) {
	g := &ghScript{
		checksOut: "",
		checksErr: errors.New("gh: exit status 1\nno checks reported on the 'main' branch"),
		viewOut:   `{"statusCheckRollup":[]}`,
	}
	g.install(t)
	sum, err := Checks(nil, "o", "r", 7)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if sum.Rollup != RollupNoChecks {
		t.Fatalf("rollup = %q, want %q", sum.Rollup, RollupNoChecks)
	}
	if sum.Rollup == RollupPassed {
		t.Fatal("no checks configured read as passing")
	}
	if len(g.calls) != 2 || g.calls[1][2] != "view" {
		t.Fatalf("calls = %q; want the pr-checks call then the pr-view probe", g.calls)
	}
	probe := g.calls[1]
	want := []string{"gh", "pr", "view", "7", "--repo", "github.com/o/r", "--json", "statusCheckRollup"}
	if !reflect.DeepEqual(probe, want) {
		t.Fatalf("probe argv = %q\nwant %q", probe, want)
	}
}

// TestChecks_ExplicitEmptyArrayIsNoChecks: a gh build that prints `[]` needs no
// probe — an empty list IS the evidence.
func TestChecks_ExplicitEmptyArrayIsNoChecks(t *testing.T) {
	g := &ghScript{checksOut: "[]\n"}
	g.install(t)
	sum, err := Checks(nil, "o", "r", 7)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if sum.Rollup != RollupNoChecks {
		t.Fatalf("rollup = %q, want %q", sum.Rollup, RollupNoChecks)
	}
	if len(g.calls) != 1 {
		t.Errorf("gh invocations = %d, want 1 (no probe needed)", len(g.calls))
	}
}

// TestChecks_ErrorNeverPasses sweeps every failure shape and asserts each one
// yields an error AND a zero summary — never RollupPassed, never a partial
// summary a caller could mistake for evidence.
func TestChecks_ErrorNeverPasses(t *testing.T) {
	cases := []struct {
		name string
		g    ghScript
	}{
		{"auth failure: empty stdout, probe also fails", ghScript{
			checksErr: errors.New("gh: exit status 4"),
			viewErr:   errors.New("gh: exit status 4"),
		}},
		{"garbage stdout, probe says the PR does have checks", ghScript{
			checksOut: "SEGFAULT\n",
			checksErr: errors.New("gh: exit status 1"),
			viewOut:   `{"statusCheckRollup":[{"name":"build"}]}`,
		}},
		{"garbage stdout, exit 0", ghScript{
			checksOut: "not json at all",
			viewOut:   `{"statusCheckRollup":[{"name":"build"}]}`,
		}},
		{"probe output is itself garbage", ghScript{
			checksErr: errors.New("gh: exit status 1"),
			viewOut:   "<html>proxy error</html>",
		}},
		{"a JSON object where a list was promised", ghScript{
			checksOut: `{"message":"Not Found"}`,
			viewOut:   `{"statusCheckRollup":[{"name":"build"}]}`,
		}},
		{"truncated check list", ghScript{
			checksOut:   `[{"bucket":"pass","na`,
			checksTrunc: true,
		}},
		{"truncated probe output", ghScript{
			checksErr: errors.New("gh: exit status 1"),
			viewOut:   `{"statusCheckRo`,
			viewTrunc: true,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.g
			g.install(t)
			sum, err := Checks(nil, "o", "r", 7)
			if err == nil {
				t.Fatalf("Checks returned no error; got summary %+v", sum)
			}
			if sum.Rollup == RollupPassed {
				t.Fatal("an error rolled up as PASSED")
			}
			if !reflect.DeepEqual(sum, CheckSummary{}) {
				t.Fatalf("summary = %+v, want the zero value on error", sum)
			}
		})
	}
}

// TestChecks_TruncationIsAnErrorNotAGuess: an oversize check list must fail
// loudly rather than roll up whatever prefix happened to fit.
func TestChecks_TruncationIsAnErrorNotAGuess(t *testing.T) {
	// A prefix that is, on its own, a *valid* JSON list of passing checks.
	g := &ghScript{checksOut: `[{"bucket":"pass","name":"build","state":"SUCCESS"}]`, checksTrunc: true}
	g.install(t)
	sum, err := Checks(nil, "o", "r", 7)
	if err == nil {
		t.Fatalf("a truncated read produced a summary: %+v", sum)
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should name the byte cap, got %v", err)
	}
	if len(g.calls) != 1 {
		t.Errorf("gh invocations = %d; a truncated read must not fall through to the probe", len(g.calls))
	}
}

// ---- sanitization ----

// TestChecks_HostileCheckNamesSanitized: a check name is repo-controlled text
// (it comes from the repository's own workflow file) and reaches operator
// displays. ANSI escapes, C1 controls, and bidi overrides are stripped at the
// point of ingestion, and an absurd name is capped.
func TestChecks_HostileCheckNamesSanitized(t *testing.T) {
	// The hostile names, JSON-encoded exactly as a repo's own workflow file
	// could name a job: an ANSI clear-screen + colour + BEL, a C1 CSI plus a
	// NUL, a bidi RIGHT-TO-LEFT OVERRIDE spoofing an extension, and an absurd
	// length.
	names := []string{
		"\x1b[2J\x1b[31mALL CHECKS PASSED\x07",
		"build\u009b[31m\x00",
		"cod\u202efdp.exe",
		strings.Repeat("A", 5000),
	}
	raw := make([]map[string]string, 0, len(names))
	for _, n := range names {
		raw = append(raw, map[string]string{"bucket": "pass", "name": n, "state": "SUCCESS"})
	}
	payload, merr := json.Marshal(raw)
	if merr != nil {
		t.Fatal(merr)
	}
	g := &ghScript{checksOut: string(payload)}
	g.install(t)
	sum, err := Checks(nil, "o", "r", 7)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if len(sum.Checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(sum.Checks))
	}
	for _, c := range sum.Checks {
		for _, r := range c.Name {
			if r < 0x20 || r == 0x7f {
				t.Errorf("check name %q retains a C0/DEL control %U", c.Name, r)
			}
			if r >= 0x80 && r <= 0x9f {
				t.Errorf("check name %q retains a C1 control %U", c.Name, r)
			}
			if r == 0x202e {
				t.Errorf("check name %q retains a bidi override", c.Name)
			}
		}
	}
	if got := sum.Checks[0].Name; got != "[2J[31mALL CHECKS PASSED" {
		t.Errorf("escape-stripped name = %q", got)
	}
	if got := sum.Checks[1].Name; got != "build[31m" {
		t.Errorf("C1/NUL-stripped name = %q", got)
	}
	if got := sum.Checks[2].Name; got != "codfdp.exe" {
		t.Errorf("bidi-stripped name = %q", got)
	}
	if n := len([]rune(sum.Checks[3].Name)); n > checkNameMaxRunes+1 {
		t.Errorf("oversize name kept %d runes, want <= %d", n, checkNameMaxRunes+1)
	}
	if !strings.HasSuffix(sum.Checks[3].Name, "…") {
		t.Errorf("truncated name must carry an explicit marker, got %q", sum.Checks[3].Name)
	}
}

// TestChecks_RetainedCheckListIsBounded: counts stay exact over every check,
// but the retained per-check slice is capped so a PR with thousands of matrix
// legs cannot balloon an in-memory observation.
func TestChecks_RetainedCheckListIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	const n = checksMaxRetained + 50
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"bucket":"pass","name":"c%d","state":"SUCCESS"}`, i)
	}
	b.WriteString("]")
	g := &ghScript{checksOut: b.String()}
	g.install(t)
	sum, err := Checks(nil, "o", "r", 7)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if sum.Total != n || sum.Passed != n {
		t.Errorf("counts = total %d passed %d, want %d/%d (counts must cover every check)", sum.Total, sum.Passed, n, n)
	}
	if len(sum.Checks) != checksMaxRetained {
		t.Errorf("retained checks = %d, want %d", len(sum.Checks), checksMaxRetained)
	}
	if sum.Rollup != RollupPassed {
		t.Errorf("rollup = %q, want passed", sum.Rollup)
	}
}

// ---- the capped read itself ----

// TestCappedWriter proves the cap is applied WHILE capturing: the buffer never
// grows past the limit, no matter how much is written.
func TestCappedWriter(t *testing.T) {
	w := &cappedWriter{limit: 10}
	n, err := w.Write([]byte("0123456789ABCDEF"))
	if err != nil || n != 16 {
		t.Fatalf("Write = %d,%v; want the full length consumed and no error (a short write would EPIPE the child)", n, err)
	}
	if string(w.buf) != "0123456789" {
		t.Fatalf("buf = %q, want the first 10 bytes", w.buf)
	}
	if !w.over {
		t.Fatal("over = false after exceeding the limit")
	}
	// Further writes must not grow the buffer.
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("Write after cap: %v", err)
	}
	if len(w.buf) != 10 {
		t.Fatalf("buf grew past the cap to %d bytes", len(w.buf))
	}
}

// TestRunCLIOutputCapped_CapsAtFetch drives the REAL helper against a real
// process that emits far more than the cap, proving the bound is enforced at
// fetch time (never read-then-truncate).
func TestRunCLIOutputCapped_CapsAtFetch(t *testing.T) {
	const limit = 1024
	out, truncated, err := runCLIOutputCapped(nil, limit, "sh", "-c",
		"i=0; while [ $i -lt 5000 ]; do echo AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA; i=$((i+1)); done")
	if err != nil {
		t.Fatalf("runCLIOutputCapped: %v", err)
	}
	if len(out) != limit {
		t.Fatalf("captured %d bytes, want exactly the %d-byte cap", len(out), limit)
	}
	if !truncated {
		t.Fatal("truncated = false for output far larger than the cap")
	}
}

// TestRunCLIOutputCapped_FoldsStderrIntoTheError keeps the operator-facing
// failure informative without ever letting that prose reach a control decision.
func TestRunCLIOutputCapped_FoldsStderrIntoTheError(t *testing.T) {
	out, _, err := runCLIOutputCapped(nil, 1<<16, "sh", "-c", "printf hi; printf 'boom\\n' >&2; exit 3")
	if err == nil {
		t.Fatal("want an error for exit 3")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error omits stderr: %v", err)
	}
	if string(out) != "hi" {
		t.Errorf("stdout = %q, want the captured stdout even on failure", out)
	}
}
