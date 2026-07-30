package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"drydock/internal/remote"
	"drydock/internal/stage"
)

func TestParseEgressExtras(t *testing.T) {
	cases := []struct {
		in      []string
		want    []domain
		wantErr bool
	}{
		{nil, nil, false},
		{[]string{}, nil, false},
		{
			in: []string{"api.example.com:443"},
			want: []domain{
				{Host: "api.example.com", Ports: []int{443}},
			},
		},
		{
			in: []string{"a.example.com:443,8443", "b.example.com:80"},
			want: []domain{
				{Host: "a.example.com", Ports: []int{443, 8443}},
				{Host: "b.example.com", Ports: []int{80}},
			},
		},
		// Trims whitespace inside the port list.
		{
			in: []string{"a.example.com:443, 8443"},
			want: []domain{
				{Host: "a.example.com", Ports: []int{443, 8443}},
			},
		},

		// Errors
		{in: []string{"no-port"}, wantErr: true},
		{in: []string{":443"}, wantErr: true},
		{in: []string{"host:"}, wantErr: true},
		{in: []string{"host:abc"}, wantErr: true},
		{in: []string{"host:443,xyz"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseEgressExtras(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEgressExtras(%v) want err, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEgressExtras(%v) unexpected err: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseEgressExtras(%v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestRepeatedFlag(t *testing.T) {
	var r repeatedFlag
	if err := r.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("b"); err != nil {
		t.Fatal(err)
	}
	if got := r.String(); got != "a,b" {
		t.Errorf("String = %q", got)
	}
	if len(r) != 2 || r[0] != "a" || r[1] != "b" {
		t.Errorf("slice = %v", r)
	}
}

// Model must round-trip through the request JSON the same way the other
// optional fields do (omitempty), so a Model-less submit doesn't pollute
// audit logs with empty-string fields and an explicit Model lands intact.
func TestTaskRequest_ModelOmitemptyAndRoundtrip(t *testing.T) {
	empty, err := json.Marshal(taskRequest{RepoRef: "git@github.com:o/r", Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), `"model"`) {
		t.Errorf("empty Model should be omitted, got %s", empty)
	}
	set, err := json.Marshal(taskRequest{RepoRef: "git@github.com:o/r", Instruction: "x", Model: "claude-opus-4-8"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(set), `"model":"claude-opus-4-8"`) {
		t.Errorf("Model not emitted: %s", set)
	}
}

// plan_only and issue_url must be omitempty (absent on a plain submit) and
// land intact when set, mirroring the broker.Task contract.
func TestTaskRequest_PlanOnlyIssueURLOmitemptyAndRoundtrip(t *testing.T) {
	empty, err := json.Marshal(taskRequest{RepoRef: "git@github.com:o/r", Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "plan_only") || strings.Contains(string(empty), "issue_url") {
		t.Errorf("unset plan_only/issue_url should be omitted, got %s", empty)
	}
	set, err := json.Marshal(taskRequest{RepoRef: "git@github.com:o/r", Instruction: "x",
		PlanOnly: true, IssueURL: "https://github.com/o/r/issues/7"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(set), `"plan_only":true`) {
		t.Errorf("plan_only not emitted: %s", set)
	}
	if !strings.Contains(string(set), `"issue_url":"https://github.com/o/r/issues/7"`) {
		t.Errorf("issue_url not emitted: %s", set)
	}
}

// issueInstruction renders "# Issue #N: <title>\n\nLabels: a, b\n\n<body>",
// bounded at issueBodyCap with an explicit truncation marker. The issue text
// is untrusted input — this is only about shape and bounds.

func TestIssueInstruction_FormatWithLabels(t *testing.T) {
	iss := remote.Issue{
		Owner: "o", Repo: "r", Number: 42,
		Title:  "flaky test in CI",
		Body:   "It fails on Tuesdays.",
		Labels: []string{"bug", "ci"},
	}
	got := issueInstruction(iss, false)
	want := "# Issue #42: flaky test in CI\n\nLabels: bug, ci\n\nIt fails on Tuesdays."
	if got != want {
		t.Errorf("issueInstruction =\n%q\nwant\n%q", got, want)
	}
}

func TestIssueInstruction_EmptyLabelsOmitsLabelsLine(t *testing.T) {
	iss := remote.Issue{Number: 7, Title: "t", Body: "b"}
	got := issueInstruction(iss, false)
	if strings.Contains(got, "Labels:") {
		t.Errorf("empty labels must omit the Labels line:\n%q", got)
	}
	want := "# Issue #7: t\n\nb"
	if got != want {
		t.Errorf("issueInstruction = %q, want %q", got, want)
	}
}

func TestIssueInstruction_TruncatesHugeBodyWithMarker(t *testing.T) {
	huge := strings.Repeat("x", issueBodyCap+4096)
	iss := remote.Issue{Number: 1, Title: "big", Body: huge}
	got := issueInstruction(iss, false)
	if !strings.Contains(got, "[issue body truncated at 24 KiB]") {
		t.Errorf("truncation marker missing:\n...%q", got[len(got)-120:])
	}
	// Bounded: header + capped body + marker, never the full body.
	if len(got) >= len(huge) {
		t.Errorf("instruction not bounded: len=%d, body len=%d", len(got), len(huge))
	}
	if !strings.Contains(got, strings.Repeat("x", 1024)) {
		t.Errorf("truncated body should keep its prefix")
	}
}

func TestIssueInstruction_SmallBodyNotMarkedTruncated(t *testing.T) {
	iss := remote.Issue{Number: 2, Title: "small", Body: "tiny body"}
	if got := issueInstruction(iss, false); strings.Contains(got, "truncated") {
		t.Errorf("small body must not carry a truncation marker:\n%q", got)
	}
}

func TestIssueInstruction_PlanAppendsPlanDirective(t *testing.T) {
	iss := remote.Issue{Number: 3, Title: "t", Body: "b"}
	plain := issueInstruction(iss, false)
	planned := issueInstruction(iss, true)
	if planned == plain {
		t.Error("plan=true must alter the instruction with a plan-only directive")
	}
	if !strings.HasPrefix(planned, plain) {
		t.Errorf("plan directive should append, not rewrite:\n%q", planned)
	}
	if !strings.Contains(strings.ToLower(planned), "plan") {
		t.Errorf("plan directive missing:\n%q", planned)
	}
}

// --issue is mutually exclusive with every other instruction source.
func TestIssueFlagConflict(t *testing.T) {
	cases := []struct {
		name       string
		inline     string
		file       string
		positional []string
		wantErr    bool
	}{
		{name: "issue alone", wantErr: false},
		{name: "with --instruction", inline: "do it", wantErr: true},
		{name: "with --instruction dash", inline: "-", wantErr: true},
		{name: "with --instruction-file", file: "task.md", wantErr: true},
		{name: "with positional dash stdin", positional: []string{"-"}, wantErr: true},
	}
	for _, tc := range cases {
		err := issueFlagConflict(tc.inline, tc.file, tc.positional)
		if tc.wantErr && err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}
}

// resolveIssue derives the repo ref from the issue URL when --repo is omitted,
// and keeps an explicit --repo untouched. Fetch is faked — no gh involved.
func TestResolveIssue_DerivesRepoWhenOmitted(t *testing.T) {
	fake := func(env []string, owner, repo string, n int) (remote.Issue, error) {
		if owner != "acme" || repo != "widgets" || n != 12 {
			t.Errorf("fetch called with %s/%s#%d", owner, repo, n)
		}
		return remote.Issue{Owner: owner, Repo: repo, Number: n, Title: "T", Body: "B"}, nil
	}
	instr, repoRef, err := resolveIssue("https://github.com/acme/widgets/issues/12", "", false, fake)
	if err != nil {
		t.Fatalf("resolveIssue: %v", err)
	}
	if repoRef != "https://github.com/acme/widgets" {
		t.Errorf("derived repo = %q, want https://github.com/acme/widgets", repoRef)
	}
	if !strings.Contains(instr, "# Issue #12: T") {
		t.Errorf("instruction not built from the issue:\n%q", instr)
	}
}

// resolveIssue must hand the fetch seam the curated allowlist env, never the
// full os.Environ(). The probe var is planted in the process env before the
// call; if a future refactor switches fetchFn to os.Environ() the probe leaks
// through and this test fails.
func TestResolveIssue_FetchGetsCuratedEnv(t *testing.T) {
	t.Setenv("DRYDOCK_SECRET_PROBE", "x") // in os.Environ(), not in the allowlist

	var gotEnv []string
	fake := func(env []string, owner, repo string, n int) (remote.Issue, error) {
		gotEnv = env
		return remote.Issue{Owner: owner, Repo: repo, Number: n, Title: "T"}, nil
	}
	if _, _, err := resolveIssue("https://github.com/acme/widgets/issues/12", "", false, fake); err != nil {
		t.Fatalf("resolveIssue: %v", err)
	}

	if !reflect.DeepEqual(gotEnv, stage.CuratedEnv()) {
		t.Errorf("fetch env = %q, want stage.CuratedEnv() = %q", gotEnv, stage.CuratedEnv())
	}
	var sawPath bool
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "DRYDOCK_SECRET_PROBE=") {
			t.Errorf("non-allowlisted os.Environ() var leaked into fetch env: %q", kv)
		}
		if strings.HasPrefix(kv, "PATH=") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Error("curated var PATH missing from fetch env")
	}
}

func TestResolveIssue_ExplicitRepoWins(t *testing.T) {
	fake := func(env []string, owner, repo string, n int) (remote.Issue, error) {
		return remote.Issue{Owner: owner, Repo: repo, Number: n, Title: "T"}, nil
	}
	_, repoRef, err := resolveIssue("https://github.com/acme/widgets/issues/12",
		"git@github.com:acme/widgets.git", false, fake)
	if err != nil {
		t.Fatalf("resolveIssue: %v", err)
	}
	if repoRef != "git@github.com:acme/widgets.git" {
		t.Errorf("explicit --repo must win, got %q", repoRef)
	}
}

func TestResolveIssue_BadURLSurfaces(t *testing.T) {
	fake := func(env []string, owner, repo string, n int) (remote.Issue, error) {
		t.Error("fetch must not be called for a bad URL")
		return remote.Issue{}, nil
	}
	if _, _, err := resolveIssue("https://gitlab.com/o/r/-/issues/3", "", false, fake); err == nil {
		t.Error("non-GitHub issue URL must error")
	}
}

func TestResolveIssue_FetchErrorSurfaces(t *testing.T) {
	fake := func(env []string, owner, repo string, n int) (remote.Issue, error) {
		return remote.Issue{}, fmt.Errorf("gh: %w", errors.New("not authenticated"))
	}
	if _, _, err := resolveIssue("https://github.com/o/r/issues/1", "", false, fake); err == nil {
		t.Error("fetch failure must surface")
	}
}

// Truncation must cut on a rune boundary: a multibyte character split at
// issueBodyCap would surface as U+FFFD in the rendered prompt (and the audit
// trail). The whole truncated instruction must stay valid UTF-8.
func TestIssueInstruction_TruncatesOnRuneBoundary(t *testing.T) {
	// é is 2 bytes (0xC3 0xA9); an odd byte prefix guarantees the cap lands
	// mid-rune somewhere in a run of them.
	body := "x" + strings.Repeat("é", issueBodyCap)
	iss := remote.Issue{Number: 3, Title: "multibyte", Body: body}
	got := issueInstruction(iss, false)
	if !utf8.ValidString(got) {
		t.Error("truncated instruction is not valid UTF-8 (a rune was split at the cap)")
	}
	if !strings.Contains(got, issueTruncationMarker) {
		t.Error("truncation marker missing on a capped multibyte body")
	}
}

// The truncation marker must state the real cap: derived from issueBodyCap,
// not a hardcoded literal that could drift when the cap changes.
func TestIssueTruncationMarker_DerivedFromCap(t *testing.T) {
	want := fmt.Sprintf("truncated at %d KiB", issueBodyCap/1024)
	if !strings.Contains(issueTruncationMarker, want) {
		t.Errorf("marker %q does not state the %d KiB cap", issueTruncationMarker, issueBodyCap/1024)
	}
}
