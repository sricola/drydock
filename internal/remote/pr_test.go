package remote

import (
	"reflect"
	"strings"
	"testing"
)

// TestParsePRURL pins the parse of `gh pr create`'s stdout. The failure modes
// matter more than the happy path twice over: this runs after a push has
// already landed (so anything unparseable must degrade to "no identity", never
// to an error), AND the value it produces is persisted as a durable coordinate
// that becomes a `gh --repo` argv on a timer.
func TestParsePRURL(t *testing.T) {
	const ref = "https://github.com/o/r.git"
	cases := []struct {
		name    string
		out     string
		repoRef string
		want    PullRequest
		ok      bool
	}{
		{
			name: "plain URL as gh prints it",
			out:  "https://github.com/o/r/pull/42\n",
			want: PullRequest{Number: 42, Owner: "o", Repo: "r", URL: "https://github.com/o/r/pull/42"},
			ok:   true,
		},
		{
			name: "extra chatter lines before the URL",
			out: "Creating pull request for agent/abc into main in o/r\n" +
				"Warning: 3 uncommitted changes\n" +
				"https://github.com/o/r/pull/7\n",
			want: PullRequest{Number: 7, Owner: "o", Repo: "r", URL: "https://github.com/o/r/pull/7"},
			ok:   true,
		},
		{
			name: "trailing slash tolerated",
			out:  "https://github.com/o/r/pull/9/\n",
			want: PullRequest{Number: 9, Owner: "o", Repo: "r", URL: "https://github.com/o/r/pull/9"},
			ok:   true,
		},
		{
			name:    "enterprise host accepted when the TASK's ref names it",
			out:     "https://github.mycorp.com/team/svc/pull/1234\n",
			repoRef: "git@github.mycorp.com:team/svc.git",
			want:    PullRequest{Number: 1234, Owner: "team", Repo: "svc", URL: "https://github.mycorp.com/team/svc/pull/1234"},
			ok:      true,
		},
		{
			name:    "host is matched case-insensitively",
			out:     "https://GitHub.com/o/r/pull/5\n",
			repoRef: "https://github.com/o/r",
			want:    PullRequest{Number: 5, Owner: "o", Repo: "r", URL: "https://github.com/o/r/pull/5"},
			ok:      true,
		},
		{
			name:    "scp-style task ref still supplies the host",
			out:     "https://github.com/o/r/pull/11\n",
			repoRef: "git@github.com:o/r.git",
			want:    PullRequest{Number: 11, Owner: "o", Repo: "r", URL: "https://github.com/o/r/pull/11"},
			ok:      true,
		},
		{
			name:    "a clone credential in the TASK ref does not block the match",
			out:     "https://github.com/o/r/pull/12\n",
			repoRef: "https://user:tok@github.com/o/r.git",
			want:    PullRequest{Number: 12, Owner: "o", Repo: "r", URL: "https://github.com/o/r/pull/12"},
			ok:      true,
		},

		// --- the fork case: the PR lives in a DIFFERENT repo on the SAME host ---
		{
			name:    "fork: PR repo differs from the task repo on the same host",
			out:     "https://github.com/upstream/proj/pull/8\n",
			repoRef: "https://github.com/myfork/proj.git",
			want:    PullRequest{Number: 8, Owner: "upstream", Repo: "proj", URL: "https://github.com/upstream/proj/pull/8"},
			ok:      true,
		},

		// --- hostile hosts ---
		{name: "different host entirely", out: "https://evil.com/o/r/pull/1\n", ok: false},
		{name: "suffix-confusion host", out: "https://github.com.attacker.net/o/r/pull/1\n", ok: false},
		{name: "prefix-confusion host", out: "https://notgithub.com/o/r/pull/1\n", ok: false},
		{name: "subdomain of the real host", out: "https://gist.github.com/o/r/pull/1\n", ok: false},
		{name: "userinfo smuggling the real host", out: "https://github.com@evil.com/o/r/pull/1\n", ok: false},
		{name: "userinfo with a token", out: "https://user:tok@github.com/o/r/pull/1\n", ok: false},
		{name: "explicit port the task ref does not have", out: "https://github.com:8443/o/r/pull/1\n", ok: false},
		{name: "explicit default port is still not the ref's host", out: "https://github.com:443/o/r/pull/1\n", ok: false},

		// --- hostile paths ---
		{name: "parent-dir segment as owner", out: "https://github.com/../r/pull/1\n", ok: false},
		{name: "parent-dir segment as repo", out: "https://github.com/o/../pull/1\n", ok: false},
		{name: "encoded parent-dir segment", out: "https://github.com/o/%2e%2e/pull/1\n", ok: false},
		{name: "interior dot-dot in the repo", out: "https://github.com/o/a..b/pull/1\n", ok: false},
		{name: "extra path segments (no longer accepted)", out: "https://github.com/o/sub/r/pull/5\n", ok: false},
		{name: "trailing segment after the number", out: "https://github.com/o/r/pull/5/files\n", ok: false},
		{name: "dash-leading owner (flag confusion)", out: "https://github.com/-o/r/pull/1\n", ok: false},
		{name: "query string", out: "https://github.com/o/r/pull/5?x=1\n", ok: false},
		{name: "fragment", out: "https://github.com/o/r/pull/5#c1\n", ok: false},

		// --- ambiguity: exactly one match, or nothing ---
		{name: "two different PR URLs -> NO identity", out: "https://github.com/o/r/pull/1\nhttps://github.com/o/r/pull/2\n", ok: false},
		{name: "the same PR URL twice -> NO identity", out: "https://github.com/o/r/pull/2 https://github.com/o/r/pull/2\n", ok: false},
		{
			name: "a hostile URL alongside the real one -> NO identity",
			out:  "https://github.com/o/r/pull/1\nhttps://github.com/attacker/repo/pull/9\n",
			ok:   false,
		},
		{
			name: "a hostile URL on ANOTHER host does not make it ambiguous",
			out:  "https://evil.com/o/r/pull/9\nhttps://github.com/o/r/pull/1\n",
			want: PullRequest{Number: 1, Owner: "o", Repo: "r", URL: "https://github.com/o/r/pull/1"},
			ok:   true,
		},

		// --- the original shape checks ---
		{name: "empty output", out: "", ok: false},
		{name: "whitespace only", out: "\n \t\n", ok: false},
		{name: "not a URL", out: "gh: something went sideways\n", ok: false},
		{name: "issue URL is not a PR URL", out: "https://github.com/o/r/issues/42\n", ok: false},
		{name: "no number", out: "https://github.com/o/r/pull/\n", ok: false},
		{name: "non-numeric number", out: "https://github.com/o/r/pull/abc\n", ok: false},
		{name: "zero number", out: "https://github.com/o/r/pull/0\n", ok: false},
		{name: "leading zero rejected", out: "https://github.com/o/r/pull/007\n", ok: false},
		{name: "negative number", out: "https://github.com/o/r/pull/-5\n", ok: false},
		{name: "no host segment", out: "https:///pull/5\n", ok: false},
		{name: "bare host, no repo path", out: "https://github.com/pull/5\n", ok: false},
		{name: "wrong scheme", out: "ftp://github.com/o/r/pull/5\n", ok: false},
		{name: "overflowing number", out: "https://github.com/o/r/pull/99999999999999999999999\n", ok: false},

		// --- a missing/unusable task ref can never yield an identity ---
		{name: "empty repoRef", out: "https://github.com/o/r/pull/5\n", repoRef: " ", ok: false},
		{name: "hostless repoRef", out: "https://github.com/o/r/pull/5\n", repoRef: "garbage", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := tc.repoRef
			if rr == "" {
				rr = ref
			}
			got, ok := parsePRURL(tc.out, rr)
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePRURL(%q, %q) = %+v,%v; want %+v,%v", tc.out, rr, got, ok, tc.want, tc.ok)
			}
			if !ok && got != (PullRequest{}) {
				t.Errorf("a rejected parse returned a non-zero identity: %+v", got)
			}
		})
	}
}

// An identity is all-or-nothing: a non-zero PullRequest always carries a
// validated owner/repo, so no caller can end up with a number and no repo.
func TestParsePRURL_IdentityIsAllOrNothing(t *testing.T) {
	pr, ok := parsePRURL("https://github.com/o/r/pull/3\n", "https://github.com/o/r")
	if !ok {
		t.Fatal("expected a parse")
	}
	if pr.Number <= 0 || pr.Owner == "" || pr.Repo == "" || pr.URL == "" {
		t.Fatalf("incomplete identity: %+v", pr)
	}
	if !ValidOwnerRepo(pr.Owner) || !ValidOwnerRepo(pr.Repo) {
		t.Fatalf("owner/repo did not survive the shared validator: %+v", pr)
	}
}

func TestHostFromRepoRef(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r.git":          "github.com",
		"https://user:tok@github.com/o/r.git": "github.com",
		"git@github.com:o/r.git":              "github.com",
		"ssh://git@github.com/o/r.git":        "github.com",
		"https://GitHub.com/O/R":              "github.com",
		"https://github.mycorp.com:8443/o/r":  "github.mycorp.com:8443",
		"github.com/o/r":                      "github.com",
		"":                                    "",
	}
	for ref, want := range cases {
		if got := hostFromRepoRef(ref); got != want {
			t.Errorf("hostFromRepoRef(%q) = %q, want %q", ref, got, want)
		}
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
// a different PR than the plain path. RepoRef must NOT appear in the argv — it
// is host-matching material, not a flag.
func TestOpenRequestResult_ArgvMatchesOpenRequest(t *testing.T) {
	origRun, origCapture := runCLI, runCLIOutputIn
	t.Cleanup(func() { runCLI, runCLIOutputIn = origRun, origCapture })

	var plainArgs, captureArgs []string
	var plainDir, captureDir string
	var plainEnv, captureEnv []string
	var gotLimit int
	runCLI = func(workDir string, env []string, name string, args ...string) error {
		plainDir, plainEnv, plainArgs = workDir, env, append([]string{name}, args...)
		return nil
	}
	runCLIOutputIn = func(workDir string, env []string, limit int, name string, args ...string) ([]byte, bool, error) {
		captureDir, captureEnv, captureArgs, gotLimit = workDir, env, append([]string{name}, args...), limit
		return []byte("https://github.com/o/r/pull/3\n"), false, nil
	}

	base := Request{WorkDir: "/work", Branch: "agent/abc123", Env: []string{"GIT_DIR=/host/git"},
		RepoRef: "https://github.com/o/r.git"}
	for _, req := range []Request{
		func() Request { r := base; r.Title, r.Body = "T", "B"; return r }(),
		base,
		func() Request { r := base; r.Title, r.Body, r.Draft = "T", "B", true; return r }(),
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
		// The capture is bounded at fetch time.
		if gotLimit != prCaptureMaxBytes {
			t.Errorf("capture limit = %d, want the %d-byte cap", gotLimit, prCaptureMaxBytes)
		}
		for _, a := range captureArgs {
			if strings.Contains(a, "github.com") {
				t.Errorf("the repo ref leaked into argv: %q", captureArgs)
			}
		}
	}
}

func TestOpenRequestResult_ParsesIdentity(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	runCLIOutputIn = func(string, []string, int, string, ...string) ([]byte, bool, error) {
		return []byte("Creating pull request for agent/x into main in o/r\nhttps://github.com/o/r/pull/42\n"), false, nil
	}
	pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x",
		RepoRef: "https://github.com/o/r.git"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pr.Number != 42 || pr.URL != "https://github.com/o/r/pull/42" || pr.Owner != "o" || pr.Repo != "r" {
		t.Errorf("pr = %+v, want 42 o/r / https://github.com/o/r/pull/42", pr)
	}
}

// The fork path end to end: the task cloned myfork/proj, gh opened the PR on
// upstream/proj, and the captured coordinate is the PR's own — that is where
// the checks live.
func TestOpenRequestResult_ForkPRTargetsTheUpstreamRepo(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	runCLIOutputIn = func(string, []string, int, string, ...string) ([]byte, bool, error) {
		return []byte("https://github.com/upstream/proj/pull/8\n"), false, nil
	}
	pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x",
		RepoRef: "https://github.com/myfork/proj.git"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if pr.Owner != "upstream" || pr.Repo != "proj" || pr.Number != 8 {
		t.Errorf("pr = %+v, want the PR's OWN upstream/proj#8", pr)
	}
}

// Unparseable, hostile, or ambiguous stdout from a SUCCESSFUL gh run is missing
// evidence, not an error: the push already landed, so this must never
// manufacture a failure.
func TestOpenRequestResult_UnusableOutputIsNotAnError(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	for _, out := range []string{
		"",
		"\n",
		"https://github.com/o/r/issues/9\n",
		"who knows what this is",
		"https://evil.com/o/r/pull/9\n",
		"https://user:tok@github.com/o/r/pull/9\n",
		"https://github.com/o/r/pull/1\nhttps://github.com/o/r/pull/2\n",
	} {
		runCLIOutputIn = func(string, []string, int, string, ...string) ([]byte, bool, error) {
			return []byte(out), false, nil
		}
		pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x",
			RepoRef: "https://github.com/o/r.git"})
		if err != nil {
			t.Errorf("output %q: err = %v, want nil (a successful open must not error)", out, err)
		}
		if pr != (PullRequest{}) {
			t.Errorf("output %q: pr = %+v, want the zero value", out, pr)
		}
	}
}

// Output that hit the fetch cap is not evidence of anything: a truncated token
// can parse into a different, valid-looking PR number.
func TestOpenRequestResult_TruncatedOutputYieldsNoIdentity(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	runCLIOutputIn = func(string, []string, int, string, ...string) ([]byte, bool, error) {
		return []byte("https://github.com/o/r/pull/12"), true, nil // truncated from .../pull/1234
	}
	pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x",
		RepoRef: "https://github.com/o/r.git"})
	if err != nil {
		t.Errorf("err = %v, want nil (the push landed)", err)
	}
	if pr != (PullRequest{}) {
		t.Errorf("pr = %+v, want the zero value for truncated output", pr)
	}
}

// The cap is applied AT FETCH TIME by a real child process, not by truncating a
// fully-buffered read.
func TestRunCLIOutputIn_CapsAtFetch(t *testing.T) {
	const limit = 1 << 10
	out, truncated, err := runCLIOutputIn(t.TempDir(), nil, limit, "sh", "-c",
		`i=0; while [ $i -lt 2000 ]; do printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; i=$((i+1)); done`)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !truncated {
		t.Error("truncated = false for a ~64 KB payload against a 1 KB cap")
	}
	if len(out) != limit {
		t.Errorf("captured %d bytes, want exactly the %d-byte cap", len(out), limit)
	}
}

// A real gh failure still surfaces — that is the same signal OpenRequest gives
// and the broker reports it as pr_opened=false.
func TestOpenRequestResult_PropagatesCLIError(t *testing.T) {
	orig := runCLIOutputIn
	t.Cleanup(func() { runCLIOutputIn = orig })
	runCLIOutputIn = func(string, []string, int, string, ...string) ([]byte, bool, error) {
		return nil, false, errSentinel
	}
	pr, err := (GitHubAdapter{}).OpenRequestResult(Request{WorkDir: "/work", Branch: "agent/x",
		RepoRef: "https://github.com/o/r.git"})
	if err != errSentinel {
		t.Errorf("err = %v, want the sentinel propagated", err)
	}
	if pr != (PullRequest{}) {
		t.Errorf("pr = %+v, want the zero value on failure", pr)
	}
}
