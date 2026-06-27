package remote

import "fmt"

// GiteaAdapter opens a PR via `tea pr create` against any Gitea or
// Forgejo instance. `tea` (https://gitea.com/gitea/tea) is the official
// Gitea CLI; auth via `tea login add`. Works against gitea.com and any
// self-hosted Gitea/Forgejo (caller passes "platform": "gitea" since the
// hostname isn't fingerprintable).
type GiteaAdapter struct{}

func (GiteaAdapter) Name() string { return "gitea" }

func (GiteaAdapter) Available() error {
	if _, err := lookPath("tea"); err != nil {
		return fmt.Errorf("tea not found on PATH")
	}
	if err := probeCLI("tea", "login", "list"); err != nil {
		return fmt.Errorf("tea not authenticated (run: tea login add)")
	}
	return nil
}

func (GiteaAdapter) OpenRequest(r Request) error {
	title := r.Title
	if r.Draft {
		title = "WIP: " + title // Gitea's draft convention; empty title -> "WIP: "
	}
	args := []string{"tea", "pr", "create", "--head", r.Branch}
	if title != "" {
		args = append(args, "--title", title)
	}
	if r.Body != "" {
		args = append(args, "--description", r.Body)
	}
	return runCLI(r.WorkDir, r.Env, args...)
}
