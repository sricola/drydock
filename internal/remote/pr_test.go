package remote

import (
	"reflect"
	"testing"
)

// TestParsePRURL pins the parse of `gh pr create`'s stdout. The failure modes
// matter more than the happy path: this runs after a push has already landed,
// so anything unparseable must degrade to "no identity", never to an error.
func TestParsePRURL(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want PullRequest
		ok   bool
	}{
		{
			name: "plain URL as gh prints it",
			out:  "https://github.com/o/r/pull/42\n",
			want: PullRequest{Number: 42, URL: "https://github.com/o/r/pull/42"},
			ok:   true,
		},
		{
			name: "extra chatter lines before the URL",
			out: "Creating pull request for agent/abc into main in o/r\n" +
				"Warning: 3 uncommitted changes\n" +
				"https://github.com/o/r/pull/7\n",
			want: PullRequest{Number: 7, URL: "https://github.com/o/r/pull/7"},
			ok:   true,
		},
		{
			name: "trailing slash tolerated",
			out:  "https://github.com/o/r/pull/9/\n",
			want: PullRequest{Number: 9, URL: "https://github.com/o/r/pull/9"},
			ok:   true,
		},
		{
			name: "enterprise host is accepted (gh is a trusted local binary)",
			out:  "https://github.mycorp.com/team/svc/pull/1234\n",
			want: PullRequest{Number: 1234, URL: "https://github.mycorp.com/team/svc/pull/1234"},
			ok:   true,
		},
		{
			name: "nested path segments",
			out:  "https://github.com/o/sub/r/pull/5\n",
			want: PullRequest{Number: 5, URL: "https://github.com/o/sub/r/pull/5"},
			ok:   true,
		},
		{
			name: "last URL wins — gh prints the PR URL last",
			out:  "see also https://github.com/o/r/pull/1\nhttps://github.com/o/r/pull/2\n",
			want: PullRequest{Number: 2, URL: "https://github.com/o/r/pull/2"},
			ok:   true,
		},
		{name: "empty output", out: "", want: PullRequest{}, ok: false},
		{name: "whitespace only", out: "\n \t\n", want: PullRequest{}, ok: false},
		{name: "not a URL", out: "gh: something went sideways\n", want: PullRequest{}, ok: false},
		{name: "issue URL is not a PR URL", out: "https://github.com/o/r/issues/42\n", want: PullRequest{}, ok: false},
		{name: "no number", out: "https://github.com/o/r/pull/\n", want: PullRequest{}, ok: false},
		{name: "non-numeric number", out: "https://github.com/o/r/pull/abc\n", want: PullRequest{}, ok: false},
		{name: "zero number", out: "https://github.com/o/r/pull/0\n", want: PullRequest{}, ok: false},
		{name: "leading zero rejected", out: "https://github.com/o/r/pull/007\n", want: PullRequest{}, ok: false},
		{name: "negative number", out: "https://github.com/o/r/pull/-5\n", want: PullRequest{}, ok: false},
		{name: "no host segment", out: "https:///pull/5\n", want: PullRequest{}, ok: false},
		{name: "bare host, no repo path", out: "https://github.com/pull/5\n", want: PullRequest{}, ok: false},
		{name: "wrong scheme", out: "ftp://github.com/o/r/pull/5\n", want: PullRequest{}, ok: false},
		{name: "overflowing number", out: "https://github.com/o/r/pull/99999999999999999999999\n", want: PullRequest{}, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePRURL(tc.out)
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePRURL(%q) = %+v,%v; want %+v,%v", tc.out, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// GitHubAdapter must satisfy the optional capability, and no other adapter may
// claim it (they cannot report a PR identity, and pretending otherwise would
// make the broker record evidence that does not exist).
func TestPullRequestOpener_OnlyGitHub(t *testing.T) {
	if _, ok := Adapter(GitHubAdapter{}).(PullRequestOpener); !ok {
		t.Error("GitHubAdapter must implement PullRequestOpener")
	}
	for _, a := range []Adapter{GitLabAdapter{}, GiteaAdapter{}, PushOnlyAdapter{}} {
		if _, ok := a.(PullRequestOpener); ok {
			t.Errorf("%s must NOT implement PullRequestOpener", a.Name())
		}
	}
}

// OpenRequestResult must build byte-identical argv to OpenRequest: the two are
// alternatives, and a drift between them would mean the capability path opens
// a different PR than the plain path.
func TestOpenRequestResult_ArgvMatchesOpenRequest(t *testing.T) {
	origRun, origCapture := runCLI, runCLIOutputIn
	t.Cleanup(func() { runCLI, runCLIOutputIn = origRun, origCapture })

	var plainArgs, captureArgs []string
	var plainDir, captureDir string
	var plainEnv, captureEnv []string
	runCLI = func(workDir string, env []string, name string, args ...string) error {
		plainDir, plainEnv, plainArgs = workDir, env, append([]string{name}, args...)
		return nil
	}
	runCLIOutputIn = func(workDir string, env []string, name string, args ...string) ([]byte, error) {
		captureDir, captureEnv, captureArgs = workDir, env, append([]string{name}, args...)
		return []byte("https://github.com/o/r/pull/3\n"), nil
	}

	for _, req := range []Request{
		{WorkDir: "/work", Branch: "agent/abc123", Env: []string{"GIT_DIR=/host/git"}, Title: "T", Body: "B"},
		{WorkDir: "/work", Branch: "agent/abc123", Env: []string{"GIT_DIR=/host/git"}},
		{WorkDir: "/work", Branch: "agent/abc123", Env: []string{"GIT_DIR=/host/git"}, Title: "T", Body: "B", Draft: true},
	} {
		if err := (GitHubAdapter{}).OpenRequest(req); err != nil {
			t.Fatalf("OpenRequest: %v", err)
		}
		if _, err := (GitHubAdapter{}).OpenRequestResult(req); err != nil {
			t.Fatalf("OpenRequestResult: %v", err)
		}
		if !reflect.DeepEqual(plainArgs, captureArgs) {
			t.Errorf("argv drift: OpenRequest %q vs OpenRequestResult %q", plainArgs, captureArgs)
		}
		// The staged work tree MUST reach the capturing helper — gh pr create
		// is scoped by cwd, unlike gh issue view's --repo.
		if captureDir != plainDir || captureDir != "/work" {
			t.Errorf("workDir = %q, want %q", captureDir, plainDir)
		}
		if !reflect.DeepEqual(captureEnv, plainEnv) {
			t.Errorf("env = %q, want %q", captureEnv, plainEnv)
		}
		// argv[0] is the compile-time literal "gh" (CodeQL contract).
		if captureArgs[0] != "gh" {
			t.Errorf("argv[0] = %q, want the literal \"gh\"", captureArgs[0])
		}
	}
}

func TestOpenRequestResult_ParsesIdentity(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	runCLIOutputIn = func(string, []string, string, ...string) ([]byte, error) {
		return []byte("Creating pull request for agent/x into main in o/r\nhttps://github.com/o/r/pull/42\n"), nil
	}
	pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pr.Number != 42 || pr.URL != "https://github.com/o/r/pull/42" {
		t.Errorf("pr = %+v, want 42 / https://github.com/o/r/pull/42", pr)
	}
}

// Unparseable stdout from a SUCCESSFUL gh run is missing evidence, not an
// error: the push already landed, so this must never manufacture a failure.
func TestOpenRequestResult_UnparseableOutputIsNotAnError(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	for _, out := range []string{"", "\n", "https://github.com/o/r/issues/9\n", "who knows what this is"} {
		runCLIOutputIn = func(string, []string, string, ...string) ([]byte, error) {
			return []byte(out), nil
		}
		pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x"})
		if err != nil {
			t.Errorf("output %q: err = %v, want nil (a successful open must not error)", out, err)
		}
		if pr != (PullRequest{}) {
			t.Errorf("output %q: pr = %+v, want the zero value", out, pr)
		}
	}
}

// A real gh failure still surfaces — that is the same signal OpenRequest gives
// and the broker reports it as pr_opened=false.
func TestOpenRequestResult_PropagatesCLIError(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	runCLIOutputIn = func(string, []string, string, ...string) ([]byte, error) {
		return nil, errSentinel
	}
	pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x"})
	if err != errSentinel {
		t.Errorf("err = %v, want the sentinel propagated", err)
	}
	if pr != (PullRequest{}) {
		t.Errorf("pr = %+v, want the zero value on failure", pr)
	}
}
