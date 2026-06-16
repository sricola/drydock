// Package remote opens a merge/pull request for a pushed branch against the
// hosting platform (GitHub, GitLab, or "push-only" for generic git URLs).
// Each adapter is a thin shell over the vendor CLI (gh, glab); the binary
// must already be authenticated on the host. The push itself happens in
// stage.Push — adapters only run after that succeeds.
package remote

import (
	"fmt"
	"os/exec"
	"strings"
)

// Adapter opens a PR/MR for a freshly pushed branch. workDir is the staged
// work tree the vendor CLI runs in; env carries the GIT_DIR /
// GIT_WORK_TREE / hook-neutralization needed to keep the operation on the
// host-only git dir even if the work tree contains a planted .git.
type Adapter interface {
	Name() string
	OpenRequest(workDir, branch string, env []string) error
}

// AdapterFor selects an adapter. Explicit `platform` wins; otherwise we
// fall back to host-name inference. Self-hosted GitLab callers MUST set
// platform="gitlab" since the hostname won't say so.
func AdapterFor(repoRef, platform string) Adapter {
	switch strings.ToLower(platform) {
	case "github":
		return GitHubAdapter{}
	case "gitlab":
		return GitLabAdapter{}
	case "none", "push-only":
		return PushOnlyAdapter{}
	case "":
		// fall through to autodetect
	default:
		// Unknown explicit platform: be loud, fall through to autodetect.
		fmt.Fprintf(stderr, "remote: unknown platform %q; falling back to autodetect\n", platform)
	}
	switch {
	case strings.Contains(repoRef, "github.com"):
		return GitHubAdapter{}
	case strings.Contains(repoRef, "gitlab.com"):
		return GitLabAdapter{}
	default:
		// Self-hosted git, no PR/MR vendor — push happens, nothing else.
		return PushOnlyAdapter{}
	}
}

// stderr is indirected so tests can swap it.
var stderr = (interface {
	Write(p []byte) (int, error)
})(nilWriter{})

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

// runCLI is the shared shell-out shape. Adapters only differ by argv.
func runCLI(workDir string, env []string, args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = workDir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", args[0], err, out)
	}
	return nil
}
