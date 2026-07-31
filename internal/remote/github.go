package remote

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

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

// prCreateArgs builds the `gh pr create` argv. Shared by OpenRequest and
// OpenRequestResult so the two can never drift into opening different PRs.
//
// Every user-influenced value (branch, title, body) appears ONLY as the value
// following a flag — never as a bare positional — per the CodeQL contract
// documented on runCLI in remote.go.
func prCreateArgs(r Request) []string {
	args := []string{"pr", "create", "--head", r.Branch}
	if r.Title != "" {
		args = append(args, "--title", r.Title, "--body", r.Body)
	} else {
		args = append(args, "--fill")
	}
	if r.Draft {
		args = append(args, "--draft")
	}
	return args
}

func (GitHubAdapter) OpenRequest(r Request) error {
	return runCLI(r.WorkDir, r.Env, "gh", prCreateArgs(r)...)
}

// OpenRequestResult implements the optional PullRequestOpener capability: it
// opens the PR with exactly the argv OpenRequest uses and additionally reports
// the PR's identity, parsed from the URL `gh pr create` prints on stdout.
//
// Failure modes are deliberately asymmetric, because this runs AFTER a
// successful push:
//   - gh itself failed  -> error (identical meaning to OpenRequest's error).
//   - gh succeeded but printed nothing parseable -> zero PullRequest, nil
//     error. Missing evidence, not a failure. The caller records no PR
//     identity and proceeds exactly as it would have without this capability.
func (GitHubAdapter) OpenRequestResult(r Request) (PullRequest, error) {
	out, err := runCLIOutputIn(r.WorkDir, r.Env, "gh", prCreateArgs(r)...)
	if err != nil {
		return PullRequest{}, err
	}
	pr, _ := parsePRURL(string(out))
	return pr, nil
}

// prURLRE matches one GitHub pull-request URL: scheme, host (github.com or an
// enterprise host — gh is a trusted local binary, so the host is not pinned
// here), one or more path segments, then /pull/<number>. The number must have
// no leading zeros and at least one digit; strconv.Atoi in parsePRURL rejects
// anything that would overflow an int.
var prURLRE = regexp.MustCompile(`^https?://[^/\s]+(?:/[^/\s]+)+/pull/([1-9][0-9]*)$`)

// parsePRURL extracts the PR identity from `gh pr create`'s stdout. gh is
// chatty — it may print "Creating pull request for ... into main", warnings
// about the default branch, or an upgrade notice — so this scans every
// whitespace-separated token rather than assuming a one-line output, and takes
// the LAST match, which is the URL gh emits as its final line.
//
// ok=false (with a zero PullRequest) for empty or unrecognizable output. That
// is not an error condition: see OpenRequestResult.
func parsePRURL(out string) (PullRequest, bool) {
	var found PullRequest
	var ok bool
	for _, tok := range strings.Fields(out) {
		m := prURLRE.FindStringSubmatch(strings.TrimSuffix(tok, "/"))
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		found, ok = PullRequest{Number: n, URL: strings.TrimSuffix(tok, "/")}, true
	}
	return found, ok
}
