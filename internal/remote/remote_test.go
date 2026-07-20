package remote

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAdapterFor(t *testing.T) {
	cases := []struct {
		repoRef  string
		platform string
		want     string
	}{
		// Explicit platform wins over URL hostname.
		{"git@gitlab.mycorp.com:o/r", "gitlab", "gitlab"},
		{"git@github.com:o/r", "gitlab", "gitlab"},
		{"https://github.com/o/r", "none", "push-only"},
		{"https://github.com/o/r", "push-only", "push-only"},
		{"", "github", "github"},
		{"git@gitea.example.com:o/r", "gitea", "gitea"},
		{"git@gitea.example.com:o/r", "forgejo", "gitea"}, // alias

		// Autodetect by hostname when platform is empty.
		{"https://github.com/sricola/drydock", "", "github"},
		{"git@github.com:sricola/drydock.git", "", "github"},
		{"ssh://git@github.com/sricola/drydock", "", "github"},
		{"https://gitlab.com/group/project", "", "gitlab"},
		{"git@gitlab.com:group/project.git", "", "gitlab"},
		{"https://gitea.com/some/project", "", "gitea"},
		{"https://codeberg.org/forgejo/forgejo", "", "gitea"},

		// Self-hosted, unknown vendor → push-only when no platform.
		// (Self-hosted GitLab/Gitea callers must set platform explicitly.)
		{"git@gitlab.mycorp.com:group/project", "", "push-only"},
		{"https://gitea.mycorp.com/group/project", "", "push-only"},
		{"https://git.kernel.org/torvalds/linux", "", "push-only"},
		// Bitbucket: no CLI to wrap, falls back to push-only.
		{"git@bitbucket.org:o/r", "", "push-only"},
		{"https://bitbucket.org/o/r", "", "push-only"},

		// Unknown platform string falls through to autodetect.
		{"https://github.com/o/r", "huggingface", "github"},
	}
	for _, tc := range cases {
		got := AdapterFor(tc.repoRef, tc.platform).Name()
		if got != tc.want {
			t.Errorf("AdapterFor(%q, %q) = %q, want %q", tc.repoRef, tc.platform, got, tc.want)
		}
	}
}

func TestPushOnly_NeverErrors(t *testing.T) {
	if err := (PushOnlyAdapter{}).OpenRequest(Request{WorkDir: "/tmp", Branch: "feature/x"}); err != nil {
		t.Errorf("PushOnly must be a no-op success: %v", err)
	}
}

// TestAdapterArgv pins the exact vendor-CLI invocation each adapter builds.
// These argv strings are the contract with gh/glab/tea; a stray edit (dropped
// --yes, renamed flag, wrong branch position) silently breaks PR/MR creation
// in production where the CLI actually runs. We swap runCLI to capture argv
// instead of shelling out.
func TestAdapterArgv(t *testing.T) {
	orig := runCLI
	t.Cleanup(func() { runCLI = orig })

	var gotWorkDir string
	var gotEnv []string
	var gotArgs []string
	runCLI = func(workDir string, env []string, args ...string) error {
		gotWorkDir, gotEnv, gotArgs = workDir, env, args
		return nil
	}

	cases := []struct {
		name    string
		adapter Adapter
		title   string
		body    string
		draft   bool
		want    []string
	}{
		{
			name: "github/title-set-no-draft", adapter: GitHubAdapter{}, title: "T", body: "B",
			want: []string{"gh", "pr", "create", "--head", "agent/abc123", "--title", "T", "--body", "B"},
		},
		{
			name: "github/no-title-fill", adapter: GitHubAdapter{},
			want: []string{"gh", "pr", "create", "--head", "agent/abc123", "--fill"},
		},
		{
			name: "github/draft", adapter: GitHubAdapter{}, title: "T", body: "B", draft: true,
			want: []string{"gh", "pr", "create", "--head", "agent/abc123", "--title", "T", "--body", "B", "--draft"},
		},
		{
			name: "gitlab/title-set", adapter: GitLabAdapter{}, title: "T", body: "B",
			want: []string{"glab", "mr", "create", "--source-branch", "agent/abc123", "--yes", "--title", "T", "--description", "B"},
		},
		{
			name: "gitlab/draft", adapter: GitLabAdapter{}, title: "T", body: "B", draft: true,
			want: []string{"glab", "mr", "create", "--source-branch", "agent/abc123", "--yes", "--title", "T", "--description", "B", "--draft"},
		},
		{
			name: "gitea/title-set", adapter: GiteaAdapter{}, title: "T", body: "B",
			want: []string{"tea", "pr", "create", "--head", "agent/abc123", "--title", "T", "--description", "B"},
		},
		{
			name: "gitea/draft", adapter: GiteaAdapter{}, title: "T", body: "B", draft: true,
			want: []string{"tea", "pr", "create", "--head", "agent/abc123", "--title", "WIP: T", "--description", "B"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotWorkDir, gotEnv, gotArgs = "", nil, nil
			env := []string{"GIT_DIR=/host/git"}
			req := Request{WorkDir: "/work", Branch: "agent/abc123", Env: env, Title: tc.title, Body: tc.body, Draft: tc.draft}
			if err := tc.adapter.OpenRequest(req); err != nil {
				t.Fatalf("OpenRequest returned error: %v", err)
			}
			if !reflect.DeepEqual(gotArgs, tc.want) {
				t.Errorf("argv = %q, want %q", gotArgs, tc.want)
			}
			if gotWorkDir != "/work" {
				t.Errorf("workDir = %q, want %q", gotWorkDir, "/work")
			}
			if !reflect.DeepEqual(gotEnv, env) {
				t.Errorf("env = %q, want %q (the host git-dir env must reach the CLI)", gotEnv, env)
			}
		})
	}
}

// TestAdapter_PropagatesError confirms a CLI failure surfaces to the caller
// (the broker reports PR-open failures rather than silently swallowing them).
func TestAdapter_PropagatesError(t *testing.T) {
	orig := runCLI
	t.Cleanup(func() { runCLI = orig })
	runCLI = func(string, []string, ...string) error { return errSentinel }

	if err := (GitHubAdapter{}).OpenRequest(Request{WorkDir: "/work", Branch: "agent/x"}); err != errSentinel {
		t.Errorf("OpenRequest err = %v, want sentinel propagated", err)
	}
}

var errSentinel = &cliError{"boom"}

type cliError struct{ s string }

func (e *cliError) Error() string { return e.s }

func TestAvailable(t *testing.T) {
	origLook, origProbe := lookPath, probeCLI
	t.Cleanup(func() { lookPath, probeCLI = origLook, origProbe })

	cases := []struct {
		name      string
		adapter   Adapter
		binary    string
		probeArgs []string
	}{
		{name: "github", adapter: GitHubAdapter{}, binary: "gh", probeArgs: []string{"auth", "status"}},
		{name: "gitlab", adapter: GitLabAdapter{}, binary: "glab", probeArgs: []string{"auth", "status"}},
		{name: "gitea", adapter: GiteaAdapter{}, binary: "tea", probeArgs: []string{"login", "list"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Missing CLI must fail before attempting an auth probe.
			probeCalled := false
			lookPath = func(got string) (string, error) {
				if got != tc.binary {
					t.Errorf("lookPath(%q), want %q", got, tc.binary)
				}
				return "", errors.New("not found")
			}
			probeCLI = func(string, ...string) error { probeCalled = true; return nil }
			if err := tc.adapter.Available(); err == nil {
				t.Fatalf("missing %s must be unavailable", tc.binary)
			}
			if probeCalled {
				t.Error("auth probe ran even though the CLI was missing")
			}

			// An installed but unauthenticated CLI must also be unavailable,
			// and each adapter must use its vendor-specific auth probe.
			lookPath = func(string) (string, error) { return "/usr/bin/" + tc.binary, nil }
			probeCLI = func(gotBinary string, gotArgs ...string) error {
				if gotBinary != tc.binary || !reflect.DeepEqual(gotArgs, tc.probeArgs) {
					t.Errorf("probeCLI(%q, %q), want (%q, %q)", gotBinary, gotArgs, tc.binary, tc.probeArgs)
				}
				return errors.New("exit 1")
			}
			if err := tc.adapter.Available(); err == nil {
				t.Fatalf("unauthenticated %s must be unavailable", tc.binary)
			}

			probeCLI = func(string, ...string) error { return nil }
			if err := tc.adapter.Available(); err != nil {
				t.Errorf("authenticated %s must be available: %v", tc.binary, err)
			}
		})
	}
	// PushOnly is always available.
	if err := (PushOnlyAdapter{}).Available(); err != nil {
		t.Errorf("push-only must always be available: %v", err)
	}
}

// TestAdapterFor_UnknownPlatformWarnsToSink verifies that an unrecognized
// platform string causes a warning to be written to the injectable stderr sink.
func TestAdapterFor_UnknownPlatformWarnsToSink(t *testing.T) {
	var buf bytes.Buffer
	orig := stderr
	stderr = &buf
	t.Cleanup(func() { stderr = orig })

	_ = AdapterFor("https://github.com/o/r", "huggingface")
	if !strings.Contains(buf.String(), "huggingface") {
		t.Errorf("expected unknown-platform warning in injected sink; got: %q", buf.String())
	}
}
