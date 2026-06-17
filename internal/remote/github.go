package remote

// GitHubAdapter opens a PR via `gh pr create --head <branch> --fill`. Requires `gh` to be
// installed and authenticated on the host.
type GitHubAdapter struct{}

func (GitHubAdapter) Name() string { return "github" }

func (GitHubAdapter) OpenRequest(workDir, branch string, env []string) error {
	return runCLI(workDir, env, "gh", "pr", "create", "--head", branch, "--fill")
}
