package remote

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseIssueURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		owner   string
		repo    string
		number  int
		wantErr string // substring the error must contain; "" means success
	}{
		{name: "valid-https", raw: "https://github.com/sricola/drydock/issues/42", owner: "sricola", repo: "drydock", number: 42},
		{name: "valid-http", raw: "http://github.com/o/r/issues/7", owner: "o", repo: "r", number: 7},
		{name: "shorthand-no-scheme", raw: "github.com/o/r/issues/7", owner: "o", repo: "r", number: 7},
		{name: "trailing-slash", raw: "https://github.com/o/r/issues/42/", owner: "o", repo: "r", number: 42},
		{name: "fragment", raw: "https://github.com/o/r/issues/42#issuecomment-123", owner: "o", repo: "r", number: 42},
		{name: "dotted-repo", raw: "https://github.com/my-org/my.repo_x/issues/9", owner: "my-org", repo: "my.repo_x", number: 9},
		{name: "hyphen-owner-dot-repo", raw: "https://github.com/my-org/my.repo/issues/3", owner: "my-org", repo: "my.repo", number: 3},
		{name: "single-char-owner-repo", raw: "https://github.com/a/b/issues/1", owner: "a", repo: "b", number: 1},
		{name: "underscore-repo", raw: "https://github.com/o/repo_1/issues/5", owner: "o", repo: "repo_1", number: 5},

		{name: "dot-owner", raw: "https://github.com/./r/issues/1", wantErr: "invalid owner/repo"},
		{name: "dotdot-owner", raw: "https://github.com/../r/issues/1", wantErr: "invalid owner/repo"},
		{name: "dash-owner", raw: "https://github.com/-/r/issues/1", wantErr: "invalid owner/repo"},
		{name: "flag-owner", raw: "https://github.com/--evil-flag/r/issues/1", wantErr: "invalid owner/repo"},
		{name: "dot-repo", raw: "https://github.com/o/./issues/1", wantErr: "invalid owner/repo"},
		{name: "dotdot-repo", raw: "https://github.com/o/../issues/1", wantErr: "invalid owner/repo"},
		{name: "dash-repo", raw: "https://github.com/o/-/issues/1", wantErr: "invalid owner/repo"},
		{name: "flag-repo", raw: "https://github.com/o/--evil-flag/issues/1", wantErr: "invalid owner/repo"},
		{name: "interior-dotdot-repo", raw: "https://github.com/o/foo..bar/issues/1", wantErr: "invalid owner/repo"},
		{name: "leading-dot-repo", raw: "https://github.com/o/.git/issues/1", wantErr: "invalid owner/repo"},
		{name: "trailing-dot-repo", raw: "https://github.com/o/foo./issues/1", wantErr: "invalid owner/repo"},
		{name: "trailing-dash-owner", raw: "https://github.com/foo-/r/issues/1", wantErr: "invalid owner/repo"},

		{name: "gitlab-issue", raw: "https://gitlab.com/o/r/-/issues/42", wantErr: "only GitHub issues"},
		{name: "gitea-issue", raw: "https://gitea.com/o/r/issues/42", wantErr: "only GitHub issues"},
		{name: "codeberg-issue", raw: "https://codeberg.org/o/r/issues/42", wantErr: "only GitHub issues"},
		{name: "pr-url", raw: "https://github.com/o/r/pull/42", wantErr: "pull request"},
		{name: "repo-url", raw: "https://github.com/o/r", wantErr: ""},
		{name: "issues-list-url", raw: "https://github.com/o/r/issues", wantErr: ""},
		{name: "junk", raw: "not a url at all", wantErr: ""},
		{name: "empty", raw: "", wantErr: ""},
		{name: "zero-number", raw: "https://github.com/o/r/issues/0", wantErr: ""},
		{name: "negative-number", raw: "https://github.com/o/r/issues/-5", wantErr: ""},
		{name: "non-numeric-number", raw: "https://github.com/o/r/issues/abc", wantErr: ""},
		{name: "hostile-owner-charset", raw: "https://github.com/o$(rm)/r/issues/1", wantErr: ""},
		{name: "extra-path-segments", raw: "https://github.com/o/r/issues/42/comments", wantErr: ""},
		{name: "scheme-only-junk", raw: "ftp://github.com/o/r/issues/42", wantErr: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, err := ParseIssueURL(tc.raw)
			wantSuccess := tc.owner != ""
			if wantSuccess {
				if err != nil {
					t.Fatalf("ParseIssueURL(%q) unexpected error: %v", tc.raw, err)
				}
				if owner != tc.owner || repo != tc.repo || number != tc.number {
					t.Errorf("ParseIssueURL(%q) = (%q, %q, %d), want (%q, %q, %d)",
						tc.raw, owner, repo, number, tc.owner, tc.repo, tc.number)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseIssueURL(%q) = (%q, %q, %d), want error", tc.raw, owner, repo, number)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseIssueURL(%q) error = %q, want it to mention %q", tc.raw, err, tc.wantErr)
			}
		})
	}
}

// TestFetchIssue_Argv pins the exact gh invocation FetchIssue builds, mirroring
// TestAdapterArgv's seam-swap: runCLIOutput is replaced to capture argv and
// return canned JSON instead of shelling out. The argv is the contract with gh;
// number must be the stringified validated int and owner/repo must appear only
// as the value of --repo, never interpolated anywhere else.
func TestFetchIssue_Argv(t *testing.T) {
	orig := runCLIOutput
	t.Cleanup(func() { runCLIOutput = orig })

	var gotEnv []string
	var gotArgv []string
	runCLIOutput = func(env []string, name string, args ...string) ([]byte, error) {
		gotEnv = env
		gotArgv = append([]string{name}, args...)
		return []byte(`{"title":"Fix the flux capacitor","body":"It drifts.\n\nSteps to reproduce...","labels":[{"name":"bug"},{"name":"p1"}]}`), nil
	}

	env := []string{"GH_TOKEN=t"}
	got, err := FetchIssue(env, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchIssue returned error: %v", err)
	}

	// The --repo value pins the host to github.com (HOST/OWNER/REPO form) so a
	// forwarded GH_HOST cannot redirect the fetch to another host.
	wantArgv := []string{"gh", "issue", "view", "42", "--repo", "github.com/owner/repo", "--json", "title,body,labels"}
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Errorf("argv = %q, want %q", gotArgv, wantArgv)
	}
	if !reflect.DeepEqual(gotEnv, env) {
		t.Errorf("env = %q, want %q (caller env must reach gh)", gotEnv, env)
	}

	want := Issue{
		Owner:  "owner",
		Repo:   "repo",
		Number: 42,
		Title:  "Fix the flux capacitor",
		Body:   "It drifts.\n\nSteps to reproduce...",
		Labels: []string{"bug", "p1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Issue = %+v, want %+v", got, want)
	}
}

// TestFetchIssue_HostileNumberRejected proves the parse gate: any URL whose
// issue "number" is not a plain positive integer never reaches FetchIssue,
// so the number FetchIssue stringifies can never carry hostile text.
func TestFetchIssue_HostileNumberRejected(t *testing.T) {
	hostile := []string{
		"https://github.com/o/r/issues/42;rm -rf /",
		"https://github.com/o/r/issues/--evil",
		"https://github.com/o/r/issues/1e3",
		"https://github.com/o/r/issues/0x2a",
		"https://github.com/o/r/issues/42abc",
		"https://github.com/o/r/issues/%2e%2e",
	}
	for _, raw := range hostile {
		if _, _, _, err := ParseIssueURL(raw); err == nil {
			t.Errorf("ParseIssueURL(%q) accepted a non-numeric issue number; FetchIssue must never see this", raw)
		}
	}
}

// TestFetchIssue_ErrorMentionsGH confirms a gh failure surfaces with guidance
// that gh must be installed and authenticated.
func TestFetchIssue_ErrorMentionsGH(t *testing.T) {
	orig := runCLIOutput
	t.Cleanup(func() { runCLIOutput = orig })
	runCLIOutput = func([]string, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	}

	_, err := FetchIssue(nil, "o", "r", 1)
	if err == nil {
		t.Fatal("FetchIssue must propagate gh failure")
	}
	if !strings.Contains(err.Error(), "gh") {
		t.Errorf("error %q should name gh (installed + authenticated)", err)
	}
}

// TestFetchIssue_RejectsUnvalidatedInput: FetchIssue is exported, so it
// defensively re-checks the invariants ParseIssueURL establishes.
func TestFetchIssue_RejectsUnvalidatedInput(t *testing.T) {
	orig := runCLIOutput
	t.Cleanup(func() { runCLIOutput = orig })
	called := false
	runCLIOutput = func([]string, string, ...string) ([]byte, error) {
		called = true
		return []byte(`{}`), nil
	}

	cases := []struct {
		name  string
		owner string
		repo  string
		num   int
	}{
		{"bad-owner", "o wner", "r", 1},
		{"bad-repo", "o", "r/../x", 1},
		{"dot-owner", ".", "r", 1},
		{"dotdot-owner", "..", "r", 1},
		{"dash-owner", "-", "r", 1},
		{"flag-owner", "--evil-flag", "r", 1},
		{"interior-dotdot-repo", "o", "foo..bar", 1},
		{"leading-dot-repo", "o", ".git", 1},
		{"trailing-dot-repo", "o", "foo.", 1},
		{"zero-number", "o", "r", 0},
		{"negative-number", "o", "r", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			if _, err := FetchIssue(nil, tc.owner, tc.repo, tc.num); err == nil {
				t.Errorf("FetchIssue(%q, %q, %d) must reject invalid input", tc.owner, tc.repo, tc.num)
			}
			if called {
				t.Errorf("FetchIssue(%q, %q, %d) shelled out despite invalid input", tc.owner, tc.repo, tc.num)
			}
		})
	}
}
