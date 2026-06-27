package remote

import "fmt"

// GitLabAdapter opens a merge request via `glab mr create --fill`.
// Requires `glab` to be installed and authenticated on the host (the
// GitLab CLI handles auth via `glab auth login`). Works against
// gitlab.com and self-hosted GitLab instances.
type GitLabAdapter struct{}

func (GitLabAdapter) Name() string { return "gitlab" }

func (GitLabAdapter) Available() error {
	if _, err := lookPath("glab"); err != nil {
		return fmt.Errorf("glab not found on PATH")
	}
	if err := probeCLI("glab", "auth", "status"); err != nil {
		return fmt.Errorf("glab not authenticated (run: glab auth login)")
	}
	return nil
}

func (GitLabAdapter) OpenRequest(r Request) error {
	args := []string{"glab", "mr", "create", "--source-branch", r.Branch, "--yes"}
	if r.Title != "" {
		args = append(args, "--title", r.Title, "--description", r.Body)
	} else {
		args = append(args, "--fill")
	}
	if r.Draft {
		args = append(args, "--draft")
	}
	return runCLI(r.WorkDir, r.Env, args...)
}
