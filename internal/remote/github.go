package remote

// GitHubAdapter opens a PR via `gh pr create --head <branch> --fill`. Requires `gh` to be
// installed and authenticated on the host.
type GitHubAdapter struct{}

func (GitHubAdapter) Name() string { return "github" }

func (GitHubAdapter) OpenRequest(r Request) error {
	args := []string{"gh", "pr", "create", "--head", r.Branch}
	if r.Title != "" {
		args = append(args, "--title", r.Title, "--body", r.Body)
	} else {
		args = append(args, "--fill")
	}
	if r.Draft {
		args = append(args, "--draft")
	}
	return runCLI(r.WorkDir, r.Env, args...)
}
