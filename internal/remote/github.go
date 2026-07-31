package remote

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"drydock/internal/repokey"
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
	out, truncated, err := runCLIOutputIn(r.WorkDir, r.Env, prCaptureMaxBytes, "gh", prCreateArgs(r)...)
	if err != nil {
		return PullRequest{}, err
	}
	if truncated {
		// gh printed more than any real pr-create prints. The PR may well
		// exist, but this output is not evidence of its identity — and a
		// truncated token could parse into a DIFFERENT, valid-looking PR
		// number. Missing evidence, not an error.
		return PullRequest{}, nil
	}
	pr, _ := parsePRURL(string(out), r.RepoRef)
	return pr, nil
}

// parsePRURL extracts the PR identity from `gh pr create`'s stdout, pinned to
// the host of the task's own repo ref.
//
// This value is not cosmetic: it is persisted as a durable coordinate in the
// task's CI marker and becomes a `gh --repo <owner>/<repo>` argv on a timer, so
// it is validated to exactly the standard ParseIssueURL holds the same class of
// value to — and then some, because unlike an issue URL this one arrives on a
// subprocess's stdout rather than from the operator:
//
//   - HOST-PINNED to the task's repo ref. An enterprise host is accepted only
//     when the task's own ref names that same host. Note the pin is on the HOST
//     ALONE, deliberately: `gh pr create` from a fork opens the PR against the
//     UPSTREAM, so the PR's owner/repo legitimately differ from the task's.
//   - NO USERINFO. user:tok@host is rejected outright rather than stripped:
//     a canonical gh URL never carries it, so its presence means this is not
//     the line we think it is.
//   - EXACTLY {owner}/{repo}/pull/{number} — four path segments, no more (so
//     no traversal, no extra segments), each of owner/repo run through
//     validateOwnerRepo (charset, alphanumeric first/last, no interior "..").
//     u.Path is the DECODED path, so %2e%2e and friends are checked as the
//     characters they actually denote.
//   - EXACTLY ONE MATCH IN THE WHOLE OUTPUT. gh is chatty (it may print
//     "Creating pull request for ... into main", a default-branch warning, or
//     an upgrade notice), so every whitespace-separated token is scanned — but
//     if two tokens parse as PR URLs, which one is THIS task's PR is
//     unknowable, and "last match wins" would hand the choice to whoever can
//     get a second URL onto that stream. Zero matches and two-or-more matches
//     are the same answer: no identity. This is the same posture as the rest
//     of this file — absence of evidence is never success.
//   - The returned URL is REBUILT from the validated pieces, so what gets
//     persisted cannot carry a query, fragment, credential, or odd encoding
//     that survived the checks above.
//
// ok=false (with a zero PullRequest) for empty, ambiguous, or unrecognizable
// output, and for an empty/unparseable repoRef. That is not an error
// condition: see OpenRequestResult.
func parsePRURL(out, repoRef string) (PullRequest, bool) {
	wantHost := hostFromRepoRef(repoRef)
	if wantHost == "" {
		return PullRequest{}, false
	}
	var found PullRequest
	matches := 0
	for _, tok := range strings.Fields(out) {
		pr, ok := parseOnePRURL(tok, wantHost)
		if !ok {
			continue
		}
		matches++
		if matches > 1 {
			// Ambiguous output: refuse to pick.
			return PullRequest{}, false
		}
		found = pr
	}
	if matches != 1 {
		return PullRequest{}, false
	}
	return found, true
}

// hostFromRepoRef returns the lowercased host[:port] of a task repo ref, across
// every spelling repokey.Normalize canonicalizes (https, ssh://, scp-style
// git@host:owner/repo, with or without .git, with or without an embedded clone
// credential — userinfo lives outside the host and is dropped here). "" when
// the ref names no host.
func hostFromRepoRef(ref string) string {
	n := repokey.Normalize(ref)
	if i := strings.IndexByte(n, '/'); i >= 0 {
		n = n[:i]
	}
	return strings.ToLower(strings.TrimSpace(n))
}

// parseOnePRURL parses a single token as a pull-request URL on wantHost. See
// parsePRURL for the rules; every one of them is enforced here.
func parseOnePRURL(tok, wantHost string) (PullRequest, bool) {
	tok = strings.TrimSuffix(tok, "/")
	u, err := url.Parse(tok)
	if err != nil {
		return PullRequest{}, false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return PullRequest{}, false
	}
	// A canonical PR URL has none of these. Their presence means the token is
	// something else wearing a PR URL's shape.
	if u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return PullRequest{}, false
	}
	if strings.ToLower(u.Host) != wantHost {
		return PullRequest{}, false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) != 4 || segs[2] != "pull" {
		return PullRequest{}, false
	}
	owner, repo := segs[0], segs[1]
	if !validateOwnerRepo(owner) || !validateOwnerRepo(repo) {
		return PullRequest{}, false
	}
	// A plain base-10 positive integer only, exactly as ParseIssueURL: Atoi
	// would also accept "+42"/"-5", and a leading zero is not a shape gh emits.
	numStr := segs[3]
	if numStr == "" || numStr[0] < '1' || numStr[0] > '9' {
		return PullRequest{}, false
	}
	n, aerr := strconv.Atoi(numStr)
	if aerr != nil || n <= 0 {
		return PullRequest{}, false
	}
	return PullRequest{
		Number: n,
		Owner:  owner,
		Repo:   repo,
		URL:    u.Scheme + "://" + wantHost + "/" + owner + "/" + repo + "/pull/" + numStr,
	}, true
}
