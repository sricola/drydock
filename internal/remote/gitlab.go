package remote

// GitLabAdapter opens a merge request via `glab mr create --fill`.
// Requires `glab` to be installed and authenticated on the host (the
// GitLab CLI handles auth via `glab auth login`). Works against
// gitlab.com and self-hosted GitLab instances.
type GitLabAdapter struct{}

func (GitLabAdapter) Name() string { return "gitlab" }

func (GitLabAdapter) OpenRequest(workDir, branch string, env []string) error {
	// `--fill` populates title and description from the commit message;
	// `--yes` skips the interactive confirm prompt so the broker can run
	// non-interactively. `--source-branch` is explicit because the staged
	// clone's HEAD is whatever the agent left it at.
	return runCLI(workDir, env, "glab", "mr", "create",
		"--source-branch", branch,
		"--fill",
		"--yes",
	)
}
