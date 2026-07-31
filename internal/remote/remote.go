// Package remote opens a merge/pull request for a pushed branch against the
// hosting platform (GitHub, GitLab, or "push-only" for generic git URLs).
// Each adapter is a thin shell over the vendor CLI (gh, glab); the binary
// must already be authenticated on the host. The push itself happens in
// stage.Push — adapters only run after that succeeds.
package remote

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// remoteCLITimeout bounds a vendor-CLI shell-out. The PR/MR open is best-effort
// and runs after the push already landed, but a hung gh/glab (blackholed API,
// stuck auth prompt) would otherwise pin the task goroutine and its concurrency
// slot indefinitely on a long-running broker. Generous: a real pr-create is
// seconds. WaitDelay force-closes the pipes shortly after the timeout fires.
const remoteCLITimeout = 60 * time.Second

// lookPath / probeCLI are package vars so tests can simulate a missing or
// unauthenticated CLI without the real binaries.
var lookPath = exec.LookPath

// probeCLI runs `args` and returns its error (used for `gh auth status` etc.).
var probeCLI = func(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// Request describes a PR/MR to open for a freshly pushed branch. Title/Body
// empty -> the adapter falls back to the vendor CLI's commit-message --fill.
type Request struct {
	WorkDir string
	Branch  string
	Env     []string
	Title   string
	Body    string
	Draft   bool
	// RepoRef is the task's repository reference, in any spelling
	// repokey.Normalize canonicalizes. It NEVER reaches an argv — `gh pr
	// create` is scoped by WorkDir, not by a --repo flag. Its single job is to
	// supply the HOST that the PR-identity capture pins against: a URL printed
	// by the vendor CLI is accepted as this task's PR only if its host equals
	// this ref's host (see parsePRURL). Empty => no identity can be captured,
	// which is the fail-closed direction.
	RepoRef string
}

// Adapter opens a PR/MR for a freshly pushed branch. workDir is the staged
// work tree the vendor CLI runs in; env carries the GIT_DIR /
// GIT_WORK_TREE / hook-neutralization needed to keep the operation on the
// host-only git dir even if the work tree contains a planted .git.
type Adapter interface {
	Name() string
	OpenRequest(r Request) error
	// Available returns nil if the vendor CLI is installed and authenticated.
	// It is advisory — callers use it for early-warning, not as a gate.
	Available() error
}

// PullRequest is the identity of a PR/MR an adapter just opened: the platform's
// number, the repository the PR itself lives in, and its canonical URL. The
// zero value means "no PR identity was observed" — it is missing evidence,
// never a claim that no PR exists and never a claim that one does. Callers must
// treat the zero value as "cannot watch this PR", not as a failure of the push
// that preceded it.
//
// Owner/Repo are the AUTHORITATIVE coordinate for anything that reads the PR
// (checks, comments, merges): `gh pr create` run from a fork opens the PR
// against the UPSTREAM, so they legitimately differ from the task's own repo
// ref. They are validated with validateOwnerRepo at capture time — the same
// gate ParseIssueURL applies — so a caller may build a `gh --repo` argv from
// them directly and must never re-derive them by re-parsing URL.
//
// A non-zero PullRequest always carries all three: Number > 0 and a validated
// Owner/Repo. There is no "number but no repo" state.
type PullRequest struct {
	Number int
	Owner  string
	Repo   string
	URL    string
}

// PullRequestOpener is the OPTIONAL capability an adapter may implement to
// report the identity of the PR it just opened. It is deliberately NOT part of
// Adapter: adding a method there would break every adapter (and every test
// fake) for a capability only the GitHub adapter can honor today. Callers
// type-assert for it, exactly as the broker does for BaseCommit /
// PushPreflight / WithContext / stagedExporter.
//
// Contract for implementations:
//   - OpenRequestResult opens the request EXACTLY as OpenRequest does (same
//     argv), so a caller picks one or the other — never both, or two PRs open.
//   - A non-nil error means the open itself failed, and means the same thing
//     OpenRequest's error means.
//   - A nil error with a zero PullRequest means the open succeeded but the
//     identity could not be determined (unexpected CLI output). That is a
//     clean "no identity" result, never an error: the push already landed.
type PullRequestOpener interface {
	OpenRequestResult(r Request) (PullRequest, error)
}

// AdapterFor selects an adapter. Explicit `platform` wins; otherwise we
// fall back to host-name inference. Self-hosted GitLab/Gitea callers MUST
// set platform explicitly since the hostname won't say so.
//
// Bitbucket has no widely-used CLI that opens PRs from the shell, so
// drydock falls back to PushOnly there. A future contribution could ship
// a small REST client; the slot is open.
func AdapterFor(repoRef, platform string) Adapter {
	switch strings.ToLower(platform) {
	case "github":
		return GitHubAdapter{}
	case "gitlab":
		return GitLabAdapter{}
	case "gitea", "forgejo":
		return GiteaAdapter{}
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
	case strings.Contains(repoRef, "gitea.com"), strings.Contains(repoRef, "codeberg.org"):
		return GiteaAdapter{}
	default:
		// Self-hosted git, no PR/MR vendor — push happens, nothing else.
		// Bitbucket lands here today.
		return PushOnlyAdapter{}
	}
}

// stderr is the sink for operator-facing warnings (unknown platform, etc.).
// Defaults to os.Stderr so warnings actually reach the user; tests can swap it
// to capture output without polluting test output.
var stderr io.Writer = os.Stderr

// runCLI is the shared shell-out shape. Adapters only differ by argv. It is a
// package var (not a plain func) so tests can swap it to capture the argv each
// adapter builds without invoking the real vendor CLI.
//
// name is split out of the variadic args (rather than args[0]) so CodeQL's
// go/command-injection can prove argv[0] is never tainted: every call site
// passes a compile-time string literal ("gh"/"glab"/"tea"), never a value
// derived from the Request (title/body/branch). The two invariants that make
// this safe: (1) name is always a literal at the call site, and (2) every
// user-influenced value (branch, title, body) in args only ever appears as
// the VALUE following a flag (e.g. "--head", branch), never as a bare
// positional: pflag/urfave-style CLIs consume flag values verbatim even
// when they start with a dash, so a title like "--evil" can't be
// reinterpreted as a flag of the vendor CLI.
//
// ONE CARVE-OUT, and it is exhaustive: a VALIDATED POSITIVE INTEGER, rendered
// with strconv.Itoa, may appear as a bare positional (checks.go passes a PR
// number that way, as `gh pr checks <n>` requires). It is safe for the reason
// the flag-value rule is safe — the dash is the whole risk, and an int the
// caller has already refused unless it is > 0 cannot render with a leading
// dash. Nothing else may be a bare positional; a string never qualifies.
var runCLI = func(workDir string, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workDir
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", name, err, out)
	}
	return nil
}

// prCaptureMaxBytes bounds the stdout captured from `gh pr create`. A real
// pr-create prints a line or two; anything approaching this cap is not output
// this code should be parsing at all. The bound is applied AT FETCH TIME by
// cappedWriter (D3: "cap the read, don't read-then-truncate"), so a vendor CLI
// that decides to stream megabytes cannot balloon the broker's memory.
const prCaptureMaxBytes = 64 << 10

// runCLIOutputIn is the capturing, work-tree-aware sibling of runCLI: same
// timeout, same WaitDelay, same working directory, but it returns the command's
// STDOUT so a caller can parse what the vendor CLI printed (runCLI's
// CombinedOutput discards it into an error string, and mixing stderr chatter
// into the parse would be unparseable anyway). On failure the captured,
// sanitized stderr tail is folded into the error instead.
//
// It is distinct from issue.go's runCLIOutput, which hard-pins Dir to "" —
// `gh issue view --repo ...` is scoped by a flag, whereas `gh pr create` is
// scoped by the work tree it runs in and MUST keep Request.WorkDir. The two
// are not interchangeable; conflating them would run pr-create against the
// broker's own cwd.
//
// It is CAPPED, like checks.go's runCLIOutputCapped and for the same reason:
// stdout is buffered through a cappedWriter that accepts every byte the child
// writes (so the child is never EPIPE'd into a misleading failure) but retains
// at most limit of them. truncated reports that the cap was hit; a caller must
// treat that as "no usable output", never as a partial result to parse.
//
// CodeQL-safety (identical contract to runCLI, see its comment for the full
// rule including the validated-positive-integer carve-out): name is split out
// of the variadic args and is a compile-time string literal at every call site,
// and every user-influenced value appears only as the VALUE following a flag —
// never as an unvalidated bare positional and never as argv[0].
var runCLIOutputIn = func(workDir string, env []string, limit int, name string, args ...string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workDir
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	out := &cappedWriter{limit: limit}
	errOut := &cappedWriter{limit: cliStderrMaxBytes}
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		// The stderr tail is sanitized before it can reach an operator log; it
		// is diagnostic prose only and nothing parses it.
		return out.buf, out.over, fmt.Errorf("%s: %w\n%s", name, err, safeCLIError(string(errOut.buf)))
	}
	return out.buf, out.over, nil
}
