package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// This file is the host-side read of a pull request's CI *conclusions* (D3).
//
// The one rule everything below is shaped by: CI STATUS drives control flow,
// CI LOG TEXT NEVER DOES. Nothing here fetches, parses, or returns a single
// byte of a check's log output — the only facts produced are the per-check
// conclusion buckets gh already computes and a rollup over them. The B2 retry
// decision reads that rollup and nothing else.
//
// The second rule, inherited from the verifier (broker/verify.go): ABSENCE OF
// EVIDENCE MUST NEVER READ AS PASSING. Every failure mode in this file — an
// error, an unauthenticated gh, an undecodable payload, a read that hit the
// byte cap, a PR with no checks at all, a run where every check was skipped —
// resolves to something that is NOT RollupPassed.

const (
	// checksJSONMaxBytes bounds the check-list read AT FETCH TIME. A repo can
	// name its jobs anything and run any number of them, so the payload is
	// attacker-influenced in both size and content. Exceeding the cap is a hard
	// error, never a rollup over whatever prefix happened to fit.
	checksJSONMaxBytes = 1 << 20 // 1 MiB
	// cliStderrMaxBytes bounds the stderr tail folded into an error string.
	// That tail is for the operator's log only; nothing parses it.
	cliStderrMaxBytes = 4 << 10
	// checksMaxRetained bounds the per-check slice kept in a summary. Counts
	// are computed over EVERY check; only the retained detail list is capped,
	// so a thousand-leg matrix cannot balloon an in-memory observation.
	checksMaxRetained = 200
	// checkNameMaxRunes bounds one sanitized check name.
	checkNameMaxRunes = 120
	// cliErrorMaxRunes bounds a sanitized stderr tail in an error string.
	cliErrorMaxRunes = 400
)

// CheckState is one check's conclusion bucket as OBSERVED BY THE HOST. It
// mirrors the `bucket` field gh computes (documented in `gh pr checks --help`)
// rather than the raw platform state, so a new platform state value degrades to
// a known bucket instead of silently becoming "not a failure".
type CheckState string

const (
	// CheckPassed: terminal and successful.
	CheckPassed CheckState = "pass"
	// CheckFailed: terminal and unsuccessful.
	CheckFailed CheckState = "fail"
	// CheckPending: queued, waiting, or still running — no conclusion yet.
	CheckPending CheckState = "pending"
	// CheckSkipped: terminal, did not run. Not a failure, and NOT evidence
	// that anything passed.
	CheckSkipped CheckState = "skipping"
	// CheckCancelled: terminal, aborted. Not successful.
	CheckCancelled CheckState = "cancel"
	// CheckUnknown: gh reported a bucket/state this build does not model. It
	// is explicitly NOT treated as a pass.
	CheckUnknown CheckState = "unknown"
)

// Check is one check's identity and conclusion. Name is sanitized at ingestion
// (see safeCheckName) because it is repo-controlled text that reaches operator
// terminals. There is deliberately no log, URL, or output field: B1 reads
// conclusions only.
type Check struct {
	Name  string     `json:"name"`
	State CheckState `json:"state"`
}

// CheckRollup is the broker's single-word verdict over a PR's whole check set.
type CheckRollup string

const (
	// RollupPending: no terminal verdict yet; keep watching.
	RollupPending CheckRollup = "pending"
	// RollupPassed: at least one check actually passed and none failed,
	// was cancelled, is pending, or is unrecognized.
	RollupPassed CheckRollup = "passed"
	// RollupFailed: at least one check reached a terminal non-success.
	RollupFailed CheckRollup = "failed"
	// RollupNoChecks: the PR produced no pass evidence at all — either it has
	// no checks configured, or every check was skipped. DISTINCT from
	// RollupPassed on purpose: "nothing ran" is not "everything worked".
	RollupNoChecks CheckRollup = "no_checks"
)

// CheckSummary is one host-side observation of a PR's CI. The counts cover
// every check gh reported; Checks holds up to checksMaxRetained of them for
// operator display. The zero value is NOT a verdict — Rollup is empty, which
// no consumer may read as passing; Checks returns the zero value with a
// non-nil error on every failure path for exactly that reason.
type CheckSummary struct {
	Rollup CheckRollup `json:"rollup"`
	Checks []Check     `json:"checks,omitempty"`

	Total     int `json:"total"`
	Passed    int `json:"passed"`
	Failed    int `json:"failed"`
	Pending   int `json:"pending"`
	Skipped   int `json:"skipped"`
	Cancelled int `json:"cancelled"`
	Unknown   int `json:"unknown"`
}

// count folds one check state into the summary's counters.
func (s *CheckSummary) count(st CheckState) {
	s.Total++
	switch st {
	case CheckPassed:
		s.Passed++
	case CheckFailed:
		s.Failed++
	case CheckPending:
		s.Pending++
	case CheckSkipped:
		s.Skipped++
	case CheckCancelled:
		s.Cancelled++
	default:
		s.Unknown++
	}
}

// rollupFor derives the verdict from the counters ALONE. Precedence mirrors the
// verifier's overall-status rule (broker/verify.go): a terminal failure
// dominates everything, anything non-terminal or unrecognized blocks a pass,
// and a pass requires positive evidence.
//
//  1. Nothing reported          -> no_checks   (not passed)
//  2. Any fail or cancel        -> failed      (a terminal failure is
//     conclusive; the checks still running cannot un-fail it, and waiting for
//     them only delays an honest verdict)
//  3. Any pending or unknown    -> pending     (no verdict yet)
//  4. No check actually passed  -> no_checks   (an all-skipped run produced no
//     pass evidence; calling it "passed" would be the exact
//     absence-of-evidence bug this rule exists to prevent)
//  5. otherwise                 -> passed
func rollupFor(s CheckSummary) CheckRollup {
	switch {
	case s.Total == 0:
		return RollupNoChecks
	case s.Failed > 0 || s.Cancelled > 0:
		return RollupFailed
	case s.Pending > 0 || s.Unknown > 0:
		return RollupPending
	case s.Passed == 0:
		return RollupNoChecks
	default:
		return RollupPassed
	}
}

// checkStateFor maps gh's output to a CheckState. `bucket` is gh's own
// categorization and is preferred; `state` is the raw platform value and is the
// fallback for a gh build (or a --required run) that omits the bucket. An
// unrecognized value maps to CheckUnknown, which rollupFor refuses to read as
// passing — new platform states fail toward "keep watching", never toward "ok".
func checkStateFor(bucket, state string) CheckState {
	switch strings.ToLower(strings.TrimSpace(bucket)) {
	case "pass":
		return CheckPassed
	case "fail":
		return CheckFailed
	case "pending":
		return CheckPending
	case "skipping":
		return CheckSkipped
	case "cancel":
		return CheckCancelled
	}
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return CheckPassed
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return CheckFailed
	case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "EXPECTED":
		return CheckPending
	case "SKIPPED", "NEUTRAL":
		return CheckSkipped
	case "CANCELLED", "CANCELED", "STALE":
		return CheckCancelled
	}
	return CheckUnknown
}

// safeCheckName sanitizes a check name for operator display. A check name comes
// from the repository's own workflow file — untrusted, repo-controlled text
// that lands in `drydock queue list`, the audit, and the web UI. Same rule as
// cmd/drydock's safeCell: strip C0 controls and DEL (the classic terminal
// escape vector), C1 controls (CSI at U+009B is just as live over a UTF-8
// terminal), and Unicode format characters (bidi overrides let a job name
// visually spoof itself in the line a reviewer reads). Ordinary non-ASCII
// letters are untouched. Truncation carries an explicit marker.
func safeCheckName(s string) string { return sanitize(s, checkNameMaxRunes) }

// safeCLIError sanitizes a vendor-CLI stderr tail before it is folded into an
// error string that reaches an operator log. Same stripping as safeCheckName
// with a longer budget, because this is a diagnostic and not a column. It is
// PROSE ONLY: no control decision anywhere reads it (D3).
func safeCLIError(s string) string { return sanitize(s, cliErrorMaxRunes) }

func sanitize(s string, maxRunes int) string {
	var sb strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if r >= 0x80 && r <= 0x9f {
			continue
		}
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		sb.WriteRune(r)
		n++
		if n >= maxRunes {
			sb.WriteString("…")
			break
		}
	}
	return strings.TrimSpace(sb.String())
}

// cappedWriter is a bounded capture sink: it accepts every byte the child
// writes (so the child is never EPIPE'd into a misleading failure) but never
// buffers more than limit of them. This is a CAP AT FETCH TIME, not a
// read-then-truncate: the oversize bytes are dropped as they arrive and the
// process's peak memory is bounded by limit regardless of how much gh prints.
type cappedWriter struct {
	buf   []byte
	limit int
	over  bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		if len(p) <= room {
			w.buf = append(w.buf, p...)
		} else {
			w.buf = append(w.buf, p[:room]...)
			w.over = true
		}
	} else if len(p) > 0 {
		w.over = true
	}
	return len(p), nil
}

// runCLIOutputCapped is the capped capturing shell-out. It is the third sibling
// of runCLI / runCLIOutput, and the only one whose stdout buffer is bounded: it
// exists because a CI-facing gh read can return an unbounded payload (D3's
// "hard byte cap at fetch time"). truncated reports that the cap was hit, which
// every caller must treat as a hard error rather than as a partial result.
//
// Dir is deliberately empty: like `gh issue view`, `gh pr checks --repo ...` is
// scoped by the flag and not by a working directory.
//
// CodeQL-safety (identical contract to runCLI / runCLIOutput, carve-out and
// all): name is split out of the variadic args and is a compile-time string
// literal at every call site, and every user-influenced value appears only as
// the VALUE following a flag or as a validated stringified positive integer
// (the PR number, which `gh pr checks <n>` requires as a positional and which
// Checks refuses unless it is > 0) — never as an unvalidated bare positional
// and never as argv[0].
//
// A package var so tests can script gh without shelling out.
var runCLIOutputCapped = func(env []string, limit int, name string, args ...string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "" // repo comes from --repo, not the working directory
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	out := &cappedWriter{limit: limit}
	errOut := &cappedWriter{limit: cliStderrMaxBytes}
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		// The stderr tail is sanitized before it can reach an operator log.
		// It is diagnostic prose ONLY: no control decision anywhere reads it.
		return out.buf, out.over, fmt.Errorf("%s: %w\n%s", name, err, safeCLIError(string(errOut.buf)))
	}
	return out.buf, out.over, nil
}

// ghCheck is the decoded shape of one `gh pr checks --json bucket,name,state`
// element. Only these three fields are requested: no link, no description, no
// annotation, nothing that could carry CI log text into a control decision.
type ghCheck struct {
	Bucket string `json:"bucket"`
	Name   string `json:"name"`
	State  string `json:"state"`
}

// checksJSONFields is the exact --json field list. Keep it minimal by policy,
// not by accident: every field added here becomes a new untrusted input.
const checksJSONFields = "bucket,name,state"

// Checks reads the broker-observed CI conclusions for owner/repo#number via
// `gh pr checks`. env must be the curated adapter env (stage.CuratedEnv()),
// never os.Environ().
//
// The host is hard-pinned to github.com INSIDE the --repo value, exactly as
// FetchIssue does and for the same reason: the curated env forwards GH_HOST, so
// without the pin an exported GH_HOST=github.mycorp.com would resolve
// owner/repo against an operator-unintended host while every persisted record
// still says github.com — the host's gh credential aimed somewhere else.
//
// Failure semantics — the whole point of this function:
//
//	success             -> a CheckSummary whose Rollup is a real verdict, nil error
//	no checks on the PR -> CheckSummary{Rollup: RollupNoChecks}, nil error
//	anything else       -> the ZERO CheckSummary and a non-nil error
//
// gh exits non-zero for pending checks (8), for failing checks (1), AND for a
// PR with no checks at all (1) — so the exit code alone cannot classify the
// result. The payload is therefore decoded FIRST and the exit status ignored
// when it decodes. When it does not decode, the "no checks" case is separated
// from a genuine error STRUCTURALLY, with a second gh call whose JSON shape
// answers the question — never by parsing gh's prose.
func Checks(env []string, owner, repo string, number int) (CheckSummary, error) {
	if !validateOwnerRepo(owner) || !validateOwnerRepo(repo) {
		return CheckSummary{}, fmt.Errorf("invalid owner/repo %q/%q: must match %s with no consecutive dots", owner, repo, ownerRepoRE)
	}
	if number <= 0 {
		return CheckSummary{}, fmt.Errorf("invalid pull request number %d: must be positive", number)
	}
	// number is the validated positive int stringified (a positive int can
	// never render with a leading dash); owner/repo are charset-validated and
	// appear only as the value of --repo.
	repoFlag := "github.com/" + owner + "/" + repo
	out, truncated, runErr := runCLIOutputCapped(env, checksJSONMaxBytes, "gh",
		"pr", "checks", strconv.Itoa(number),
		"--repo", repoFlag,
		"--json", checksJSONFields)
	if truncated {
		// A prefix of a check list can be perfectly valid JSON describing only
		// the checks that happened to fit — and the ones that did not fit are
		// exactly the ones that might have failed. Refuse to guess.
		return CheckSummary{}, fmt.Errorf("reading checks for %s#%d: gh output exceeded the %d-byte cap; refusing to derive a conclusion from a partial check list",
			repoFlag, number, checksJSONMaxBytes)
	}

	var raw []ghCheck
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err == nil {
		// The payload decoded: it IS the evidence, whatever gh exited with.
		// An explicit empty list is positive evidence of "no checks".
		return summarize(raw), nil
	}

	// stdout was not a check list. Disambiguate structurally.
	none, probeErr := noChecksProbe(env, repoFlag, number)
	if probeErr == nil && none {
		return CheckSummary{Rollup: RollupNoChecks}, nil
	}
	cause := runErr
	if cause == nil {
		cause = errors.New("gh printed no decodable JSON check list")
	}
	if probeErr == nil {
		// The PR demonstrably HAS checks, but the list did not decode. That is
		// a real failure, and it must not become a verdict.
		return CheckSummary{}, fmt.Errorf("reading checks for %s#%d: the pull request has checks but gh returned no decodable check list: %w",
			repoFlag, number, cause)
	}
	return CheckSummary{}, fmt.Errorf("reading checks for %s#%d failed (gh must be installed and authenticated; run: gh auth login): %w (status probe also failed: %v)",
		repoFlag, number, cause, probeErr)
}

// noChecksProbe answers one question: does this PR have zero checks?
//
// It exists because `gh pr checks` exits 1 with EMPTY stdout both for a PR with
// no checks and for a genuine failure (bad auth, unresolvable repo, network) —
// the two are indistinguishable by exit code, and telling them apart by reading
// gh's English error text would be exactly the prose-parsing D3 forbids.
// `gh pr view --json statusCheckRollup` has no such ambiguity: it exits 0 with
// {"statusCheckRollup":[]} for a PR with no checks, and non-zero for a real
// error. The ANSWER IS THE JSON SHAPE, not any message.
//
// Returns (true, nil) only on a clean, decoded, empty rollup. Any error at all
// is returned as an error, so the caller can never mistake a failed probe for
// "no checks".
func noChecksProbe(env []string, repoFlag string, number int) (bool, error) {
	out, truncated, err := runCLIOutputCapped(env, checksJSONMaxBytes, "gh",
		"pr", "view", strconv.Itoa(number),
		"--repo", repoFlag,
		"--json", "statusCheckRollup")
	if truncated {
		return false, fmt.Errorf("gh pr view output exceeded the %d-byte cap", checksJSONMaxBytes)
	}
	if err != nil {
		return false, err
	}
	var payload struct {
		StatusCheckRollup []json.RawMessage `json:"statusCheckRollup"`
	}
	if derr := json.Unmarshal(bytes.TrimSpace(out), &payload); derr != nil {
		return false, fmt.Errorf("parsing gh pr view statusCheckRollup: %w", derr)
	}
	return len(payload.StatusCheckRollup) == 0, nil
}

// summarize folds gh's check list into a CheckSummary. Counts cover every
// element; the retained detail list is capped and its names sanitized.
func summarize(raw []ghCheck) CheckSummary {
	var s CheckSummary
	for _, c := range raw {
		st := checkStateFor(c.Bucket, c.State)
		s.count(st)
		if len(s.Checks) < checksMaxRetained {
			s.Checks = append(s.Checks, Check{Name: safeCheckName(c.Name), State: st})
		}
	}
	s.Rollup = rollupFor(s)
	return s
}
