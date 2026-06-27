package remote

import "fmt"

// GitHubAdapter opens a PR via `gh pr create --head <branch> --fill`. Requires `gh` to be
// installed and authenticated on the host.
type GitHubAdapter struct{}

func (GitHubAdapter) Name() string { return "github" }

func (GitHubAdapter) Available() error {
	if _, err := lookPath("gh"); err != nil {
		return fmt.Errorf("gh not found on PATH")
	}
	if err := probeCLI("gh", "auth", "status"); err != nil {
		return fmt.Errorf("gh not authenticated (run: gh auth login)")
	}
	return nil
}

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
