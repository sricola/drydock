package remote

// GiteaAdapter opens a PR via `tea pr create` against any Gitea or
// Forgejo instance. `tea` (https://gitea.com/gitea/tea) is the official
// Gitea CLI; auth via `tea login add`. Works against gitea.com and any
// self-hosted Gitea/Forgejo (caller passes "platform": "gitea" since the
// hostname isn't fingerprintable).
type GiteaAdapter struct{}

func (GiteaAdapter) Name() string { return "gitea" }

func (GiteaAdapter) OpenRequest(workDir, branch string, env []string) error {
	// `--head` selects the source branch. tea uses repo defaults for
	// title/description from the commit; no `--fill` flag exists.
	return runCLI(workDir, env, "tea", "pr", "create", "--head", branch)
}
