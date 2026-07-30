package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Issue is a GitHub issue fetched on the host via `gh issue view`.
type Issue struct {
	Owner  string
	Repo   string
	Number int
	Title  string
	Body   string
	Labels []string
}

// ownerRepoRE constrains a GitHub owner or repo path segment: only the safe
// charset [A-Za-z0-9._-], and the first and last character must be
// alphanumeric. That shape alone rejects spaces, slashes, shell
// metacharacters, and every all-punctuation or dash-/dot-edged value
// (".", "..", "-", "--evil-flag", ".git", "foo."). Interior ".." runs
// still match the regex, so validateOwnerRepo layers that check on top.
var ownerRepoRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// validateOwnerRepo reports whether seg is acceptable as a GitHub owner or
// repo path segment. The rule (slightly more permissive than GitHub's own,
// strictly safe for argv construction): charset [A-Za-z0-9._-], must start
// and end with an alphanumeric, and must not contain consecutive dots.
// Rejects ".", "..", "-", "--evil-flag", ".git", "foo.", "foo..bar";
// accepts "my-org", "my.repo", "repo_1", "a".
func validateOwnerRepo(seg string) bool {
	return ownerRepoRE.MatchString(seg) && !strings.Contains(seg, "..")
}

// ParseIssueURL strictly parses a GitHub issue URL of the form
// https://github.com/{owner}/{repo}/issues/{number} (the scheme-less
// github.com/o/r/issues/n shorthand is also accepted). A trailing slash and a
// #fragment are tolerated. owner/repo must satisfy validateOwnerRepo
// (charset [A-Za-z0-9._-], alphanumeric first/last character, no "..") and
// number must be a plain positive integer — these are the invariants
// FetchIssue relies on to build a safe gh argv. Everything else (GitLab/Gitea
// issue URLs, PR URLs, bare repo URLs, junk) is an error.
func ParseIssueURL(raw string) (owner, repo string, number int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0, fmt.Errorf("empty issue URL")
	}
	// Accept the scheme-less shorthand by normalizing to https.
	if strings.HasPrefix(raw, "github.com/") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, fmt.Errorf("not a valid issue URL: %q", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", 0, fmt.Errorf("not a valid issue URL: %q (expected https://github.com/{owner}/{repo}/issues/{number})", raw)
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return "", "", 0, fmt.Errorf("only GitHub issues are supported in this version (got host %q)", u.Hostname())
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) == 4 && segs[2] == "pull" {
		return "", "", 0, fmt.Errorf("%q is a pull request URL, not an issue URL", raw)
	}
	if len(segs) != 4 || segs[2] != "issues" {
		return "", "", 0, fmt.Errorf("not a GitHub issue URL: %q (expected https://github.com/{owner}/{repo}/issues/{number})", raw)
	}
	owner, repo = segs[0], segs[1]
	if !validateOwnerRepo(owner) || !validateOwnerRepo(repo) {
		return "", "", 0, fmt.Errorf("invalid owner/repo in issue URL: %q", raw)
	}
	// A plain base-10 positive integer only: Atoi would also accept "+42" and
	// "-5"; the leading-digit check pins the first byte so nothing dash- or
	// sign-prefixed gets through.
	numStr := segs[3]
	if numStr == "" || numStr[0] < '1' || numStr[0] > '9' {
		return "", "", 0, fmt.Errorf("invalid issue number %q in %q", numStr, raw)
	}
	number, aerr := strconv.Atoi(numStr)
	if aerr != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("invalid issue number %q in %q", numStr, raw)
	}
	return owner, repo, number, nil
}

// runCLIOutput is runCLI's capturing sibling: same timeout/WaitDelay, but it
// returns stdout so callers can parse JSON. It uses Output() (not
// CombinedOutput) so stderr chatter never pollutes the JSON; on failure the
// captured stderr is folded into the error instead. Dir is deliberately empty:
// `gh issue view --repo ...` is scoped by the flag, not the cwd. It is a
// package var so tests can swap it to capture argv without shelling out.
//
// CodeQL-safety (mirrors runCLI): name is always a compile-time literal
// ("gh") at every call site — never derived from user input — and every
// user-influenced value appears only as the VALUE following a flag or as a
// validated stringified integer, never as attacker-controlled bare text, so
// go/command-injection can prove argv[0] is untainted and no value can be
// reinterpreted as a flag.
var runCLIOutput = func(env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "" // repo comes from --repo, not the working directory
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s: %w\n%s", name, err, exitErr.Stderr)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// FetchIssue fetches title/body/labels for owner/repo#number via
// `gh issue view`. owner/repo/number are expected to come from ParseIssueURL;
// the invariants are re-checked here anyway since FetchIssue is exported.
// gh must be installed and authenticated on the host (gh auth login).
func FetchIssue(env []string, owner, repo string, number int) (Issue, error) {
	if !validateOwnerRepo(owner) || !validateOwnerRepo(repo) {
		return Issue{}, fmt.Errorf("invalid owner/repo %q/%q: must match %s with no consecutive dots", owner, repo, ownerRepoRE)
	}
	if number <= 0 {
		return Issue{}, fmt.Errorf("invalid issue number %d: must be positive", number)
	}
	// number is the validated positive int stringified (a positive int can
	// never render with a leading dash); owner/repo are charset-validated and
	// appear only as the value of --repo.
	out, err := runCLIOutput(env, "gh", "issue", "view", strconv.Itoa(number),
		"--repo", owner+"/"+repo, "--json", "title,body,labels")
	if err != nil {
		return Issue{}, fmt.Errorf("fetching issue %s/%s#%d failed (gh must be installed and authenticated; run: gh auth login): %w",
			owner, repo, number, err)
	}
	var payload struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return Issue{}, fmt.Errorf("parsing gh issue view output for %s/%s#%d: %w", owner, repo, number, err)
	}
	issue := Issue{Owner: owner, Repo: repo, Number: number, Title: payload.Title, Body: payload.Body}
	for _, l := range payload.Labels {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue, nil
}
