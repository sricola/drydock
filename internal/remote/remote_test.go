package remote

import (
	"reflect"
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
	if err := (PushOnlyAdapter{}).OpenRequest("/tmp", "feature/x", nil); err != nil {
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
		want    []string
	}{
		{"github", GitHubAdapter{}, []string{"gh", "pr", "create", "--head", "agent/abc123", "--fill"}},
		{"gitlab", GitLabAdapter{}, []string{"glab", "mr", "create", "--source-branch", "agent/abc123", "--fill", "--yes"}},
		{"gitea", GiteaAdapter{}, []string{"tea", "pr", "create", "--head", "agent/abc123"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotWorkDir, gotEnv, gotArgs = "", nil, nil
			env := []string{"GIT_DIR=/host/git"}
			if err := tc.adapter.OpenRequest("/work", "agent/abc123", env); err != nil {
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

	if err := (GitHubAdapter{}).OpenRequest("/work", "agent/x", nil); err != errSentinel {
		t.Errorf("OpenRequest err = %v, want sentinel propagated", err)
	}
}

var errSentinel = &cliError{"boom"}

type cliError struct{ s string }

func (e *cliError) Error() string { return e.s }
