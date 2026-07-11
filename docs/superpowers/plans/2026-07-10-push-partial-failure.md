# Push Partial-Failure Contract + Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the approved-diff push report one clean terminal outcome (`pushed` | `push_failed`) and recover automatically from transient failures and branch-name collisions.

**Architecture:** Split `Stage.Commit`/`PushBranch` (refspec push lets recovery reuse the one commit), classify git-push failures with a pure function, wrap the push in a `ctx`-cancellable config-bounded recovery loop, and surface a failure in the audit via a synthetic terminal result line so `drydock tasks` stops showing a misleading "ok".

**Tech Stack:** Go, standard library only. Packages `internal/stage`, `internal/broker`, `internal/audit`, `internal/config`, `cmd/brokerd`.

## Global Constraints

- Go standard library only; no new dependencies.
- TDD: test-first. Run tests with `go test -race -count=1 ./...`.
- No em dashes (—) in any code, comments, docs, or commit messages; use commas, colons, or parentheses. Keep en dashes only in numeric ranges.
- `gofmt` and `staticcheck` clean (CI gates on both).
- Backward compatible: config defaults enable recovery (3 retries / 1s backoff / 2 fresh-branch tries); each field set to `0` disables that path.
- A single-ref `git push` is atomic: `push_failed` means nothing landed on the remote; the diff is always preserved in the audit.

---

## File Structure

- `internal/stage/stage.go` (modify): add `Commit`, `PushBranch`; `Push` becomes a wrapper.
- `internal/stage/stage_test.go` (modify): Commit/PushBranch/refspec tests.
- `internal/broker/pushfail.go` (new): `pushReason` + `classifyPushError`. One responsibility: map git stderr to a reason.
- `internal/broker/pushfail_test.go` (new): classifier table tests.
- `internal/broker/push.go` (new): `pushStage` interface, `pushRetry` config, `pushWithRecovery`, `sleepCtx`. One responsibility: the bounded recovery loop.
- `internal/broker/push_test.go` (new): orchestrator tests against a fake stage.
- `internal/broker/broker.go` (modify): `taskStage` gains `Commit`/`PushBranch`; `realStage` forwards; `Broker` gains push-retry fields; `pushAndOpenPR` uses `pushWithRecovery` and emits `push_failed` + the synthetic audit line.
- `internal/broker/handle_task_test.go` (modify): `fakeStage` gains `Commit`/`PushBranch`.
- `internal/audit/audit.go` (modify): `Outcome` gains a `push_failed` case.
- `internal/audit/audit_test.go` (modify): its test.
- `internal/config/config.go` + `config/config.yaml` (modify): three fields + defaults + validation + env + SeedTemplate mirror.
- `internal/config/config_test.go` (modify): validation test.
- `cmd/brokerd/main.go` (modify): wire the three config values onto the Broker.
- `site/docs/configuration.md`, `site/docs/submitting-tasks.md` (or the push-contract doc), `CHANGELOG.md`, `docs/ROADMAP.md` (modify): docs.

---

### Task 1: Stage `Commit` / `PushBranch` split

**Files:**
- Modify: `internal/stage/stage.go`
- Test: `internal/stage/stage_test.go`

**Interfaces:**
- Produces: `(*Stage).Commit(branch, message string) error`; `(*Stage).PushBranch(localBranch, remoteBranch string) error`. `Push(branch, message string) error` stays, now `Commit` then `PushBranch(branch, branch)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/stage/stage_test.go`:

```go
func TestPushBranch_PushesToDifferentRemoteName(t *testing.T) {
	origin, s := setupPushable(t) // builds a bare origin + a Stage with a change

	if err := s.Commit("agent/x", "agent: change"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Push the committed local branch to a DIFFERENTLY named remote branch.
	if err := s.PushBranch("agent/x", "agent/x-2"); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	out, err := exec.Command("git", "--git-dir="+origin, "branch", "--list", "agent/x-2").Output()
	if err != nil {
		t.Fatalf("list remote branches: %v", err)
	}
	if !strings.Contains(string(out), "agent/x-2") {
		t.Errorf("remote missing agent/x-2 branch; got %q", out)
	}
}
```

Add a `setupPushable(t)` helper if the file lacks one, mirroring the existing `TestPush_CommitsAndPushes` setup (it already builds a bare origin, a working Stage, and writes a change). Reuse that exact setup: extract the shared body of `TestPush_CommitsAndPushes` into `setupPushable(t) (originGitDir string, s *stage.Stage)` and have both tests call it. If extracting is awkward, inline the same origin+stage construction the existing test uses.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/stage/ -run TestPushBranch_PushesToDifferentRemoteName`
Expected: FAIL, `s.Commit undefined` / `s.PushBranch undefined`.

- [ ] **Step 3: Implement**

In `internal/stage/stage.go`, replace the existing `Push`:

```go
// Commit creates the branch and records all agent changes as one commit on the
// host-only git dir. Run once per task.
func (s *Stage) Commit(branch, message string) error {
	if _, err := s.git("checkout", "-b", branch); err != nil {
		return err
	}
	if err := s.stageAll(); err != nil {
		return err
	}
	if _, err := s.git("commit", "-m", message); err != nil {
		return err
	}
	return nil
}

// PushBranch pushes the committed local branch to remoteBranch on origin. The
// explicit refspec lets recovery push the same commit to a fresh remote name
// after a branch-name collision, without re-committing.
func (s *Stage) PushBranch(localBranch, remoteBranch string) error {
	_, err := s.git("push", "origin", localBranch+":"+remoteBranch)
	return err
}

// Push commits then pushes to the same-named remote branch. Kept for callers
// that do not need recovery.
func (s *Stage) Push(branch, message string) error {
	if err := s.Commit(branch, message); err != nil {
		return err
	}
	return s.PushBranch(branch, branch)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/stage/`
Expected: PASS (including the pre-existing `TestPush_CommitsAndPushes`, which now exercises the wrapper).

- [ ] **Step 5: Commit**

```bash
git add internal/stage/stage.go internal/stage/stage_test.go
git commit -m "refactor(stage): split Commit/PushBranch for push recovery"
```

---

### Task 2: `classifyPushError`

**Files:**
- Create: `internal/broker/pushfail.go`
- Test: `internal/broker/pushfail_test.go`

**Interfaces:**
- Produces: `type pushReason string` with consts `reasonNonFastForward`, `reasonTransient`, `reasonAuth`, `reasonProtected`, `reasonUnknown` (their string values are `"non_fast_forward"`, `"transient"`, `"auth"`, `"protected"`, `"unknown"`); `classifyPushError(errText string) pushReason`.

- [ ] **Step 1: Write the failing test**

Create `internal/broker/pushfail_test.go`:

```go
package broker

import "testing"

func TestClassifyPushError(t *testing.T) {
	cases := []struct {
		name string
		text string
		want pushReason
	}{
		{"non-ff rejected", "! [rejected]        agent/x -> agent/x (non-fast-forward)\nUpdates were rejected", reasonNonFastForward},
		{"fetch first", "error: failed to push some refs\nhint: Updates were rejected because the remote contains work; fetch first", reasonNonFastForward},
		{"dns", "fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com", reasonTransient},
		{"timeout", "fatal: unable to access '...': Failed to connect to github.com port 443: Connection timed out", reasonTransient},
		{"rpc", "error: RPC failed; curl 92 HTTP/2 stream 0 was not closed cleanly\nfatal: the remote end hung up unexpectedly", reasonTransient},
		{"auth failed", "remote: Support for password authentication was removed.\nfatal: Authentication failed for 'https://github.com/o/r/'", reasonAuth},
		{"permission", "ERROR: Permission to o/r.git denied to user.\nfatal: Could not read from remote repository.", reasonAuth},
		{"protected", "remote: error: GH006: Protected branch update failed for refs/heads/main.", reasonProtected},
		{"pre-receive", "remote: error: pre-receive hook declined", reasonProtected},
		{"unknown", "fatal: something nobody has ever seen before", reasonUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPushError(c.text); got != c.want {
				t.Errorf("classifyPushError = %q, want %q", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestClassifyPushError`
Expected: FAIL, `undefined: classifyPushError`.

- [ ] **Step 3: Implement**

Create `internal/broker/pushfail.go`:

```go
package broker

import "strings"

// pushReason is the classified cause of a git-push failure. Its string value is
// what the audit and the stream event report.
type pushReason string

const (
	reasonNonFastForward pushReason = "non_fast_forward"
	reasonTransient      pushReason = "transient"
	reasonAuth           pushReason = "auth"
	reasonProtected      pushReason = "protected"
	reasonUnknown        pushReason = "unknown"
)

// classifyPushError maps git's combined stderr (carried in the push error) to a
// reason. Order matters: protected and auth are checked before the generic
// transient/non-ff matchers so a specific server rejection is not misread. An
// unrecognized failure is reasonUnknown, which the recovery loop treats as
// terminal: never retry a failure we do not understand.
func classifyPushError(errText string) pushReason {
	s := strings.ToLower(errText)
	switch {
	case contains(s, "gh006", "protected branch", "pre-receive hook declined"):
		return reasonProtected
	case contains(s, "authentication failed", "could not read username",
		"permission to", "permission denied", "access denied", "403",
		"invalid username or password"):
		return reasonAuth
	case contains(s, "non-fast-forward", "! [rejected]", "fetch first",
		"updates were rejected"):
		return reasonNonFastForward
	case contains(s, "could not resolve host", "connection timed out",
		"connection reset", "could not read from remote", "rpc failed",
		"early eof", "timed out", "failed to connect", "503", "502", "500",
		"tls", "the remote end hung up"):
		return reasonTransient
	default:
		return reasonUnknown
	}
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/broker/ -run TestClassifyPushError`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/pushfail.go internal/broker/pushfail_test.go
git commit -m "feat(broker): classify git-push failures"
```

---

### Task 3: `pushWithRecovery` orchestrator

**Files:**
- Create: `internal/broker/push.go`
- Test: `internal/broker/push_test.go`

**Interfaces:**
- Consumes: `classifyPushError`, `pushReason` from Task 2.
- Produces: `type pushStage interface { Commit(branch, message string) error; PushBranch(localBranch, remoteBranch string) error }`; `type pushRetry struct { MaxRetries int; Backoff time.Duration; FreshBranchTries int }`; `pushWithRecovery(ctx context.Context, st pushStage, taskID, message string, cfg pushRetry) (branch string, attempts int, reason pushReason, err error)` (nil err = success, `branch` = remote branch that landed).

- [ ] **Step 1: Write the failing test**

Create `internal/broker/push_test.go`:

```go
package broker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// scriptStage returns a queued error per PushBranch call; "" means success.
type scriptStage struct {
	commitErr error
	pushErrs  []error // consumed per PushBranch call
	branches  []string
	commits   int
}

func (s *scriptStage) Commit(branch, message string) error { s.commits++; return s.commitErr }
func (s *scriptStage) PushBranch(local, remote string) error {
	s.branches = append(s.branches, remote)
	if len(s.branches)-1 < len(s.pushErrs) {
		return s.pushErrs[len(s.branches)-1]
	}
	return nil
}

var errTransient = errors.New("fatal: Could not resolve host: github.com")
var errNonFF = errors.New("! [rejected] (non-fast-forward)")
var errAuth = errors.New("fatal: Authentication failed")

func cfg0() pushRetry { return pushRetry{MaxRetries: 3, Backoff: 0, FreshBranchTries: 2} }

func TestPushWithRecovery_TransientThenSuccess(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errTransient, errTransient, nil}}
	branch, attempts, _, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempts != 3 || branch != "agent/abc" {
		t.Errorf("attempts=%d branch=%q, want 3 / agent/abc", attempts, branch)
	}
}

func TestPushWithRecovery_FreshBranchOnCollision(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errNonFF, nil}} // base rejected, -2 ok
	branch, attempts, _, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if branch != "agent/abc-2" || attempts != 2 {
		t.Errorf("branch=%q attempts=%d, want agent/abc-2 / 2", branch, attempts)
	}
}

func TestPushWithRecovery_AuthStopsImmediately(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errAuth, nil}} // would succeed on retry, but auth is terminal
	_, attempts, reason, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err == nil {
		t.Fatal("want error for auth failure")
	}
	if reason != reasonAuth || attempts != 1 {
		t.Errorf("reason=%q attempts=%d, want auth / 1", reason, attempts)
	}
}

func TestPushWithRecovery_TransientExhausted(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errTransient, errTransient, errTransient, errTransient}}
	_, attempts, reason, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err == nil || reason != reasonTransient {
		t.Fatalf("want transient failure, got reason=%q err=%v", reason, err)
	}
	if attempts != 4 { // 1 initial + 3 retries
		t.Errorf("attempts=%d, want 4", attempts)
	}
}

func TestPushWithRecovery_CommitFailureIsTerminal(t *testing.T) {
	st := &scriptStage{commitErr: errors.New("nothing to commit")}
	_, attempts, _, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err == nil || attempts != 0 || len(st.branches) != 0 {
		t.Errorf("commit failure should be terminal with no push attempt; attempts=%d branches=%v err=%v", attempts, st.branches, err)
	}
}

func TestPushWithRecovery_CtxCancelDuringBackoff(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errTransient, nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled up front: the backoff after the first failure returns immediately
	_, _, _, err := pushWithRecovery(ctx, st, "abc", "m", pushRetry{MaxRetries: 3, Backoff: time.Hour, FreshBranchTries: 2})
	if err == nil {
		t.Fatal("want error when ctx is cancelled during backoff")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestPushWithRecovery`
Expected: FAIL, `undefined: pushWithRecovery`.

- [ ] **Step 3: Implement**

Create `internal/broker/push.go`:

```go
package broker

import (
	"context"
	"fmt"
	"time"
)

// pushStage is the subset of the stage used by push recovery.
type pushStage interface {
	Commit(branch, message string) error
	PushBranch(localBranch, remoteBranch string) error
}

// pushRetry bounds recovery. A zero field disables that path.
type pushRetry struct {
	MaxRetries       int           // transient-failure retries
	Backoff          time.Duration // base for exponential backoff (backoff << n)
	FreshBranchTries int           // alternate remote branch names on collision
}

// pushWithRecovery commits once, then pushes agent/<taskID> with bounded
// recovery: transient failures retry the same remote with exponential backoff;
// a branch-name collision retries to agent/<taskID>-2, -3, ...; auth, protected,
// and unknown failures stop immediately. It returns the remote branch that
// landed (nil err) or the classified reason and last error on failure. attempts
// counts PushBranch calls made.
func pushWithRecovery(ctx context.Context, st pushStage, taskID, message string, cfg pushRetry) (string, int, pushReason, error) {
	base := "agent/" + taskID
	if err := st.Commit(base, message); err != nil {
		return "", 0, reasonUnknown, err
	}
	remote := base
	attempts, transientTry, freshTry := 0, 0, 0
	for {
		attempts++
		err := st.PushBranch(base, remote)
		if err == nil {
			return remote, attempts, "", nil
		}
		reason := classifyPushError(err.Error())
		switch reason {
		case reasonTransient:
			if transientTry >= cfg.MaxRetries {
				return "", attempts, reason, err
			}
			if !sleepCtx(ctx, cfg.Backoff<<transientTry) {
				return "", attempts, reason, ctx.Err()
			}
			transientTry++
		case reasonNonFastForward:
			if freshTry >= cfg.FreshBranchTries {
				return "", attempts, reason, err
			}
			freshTry++
			remote = fmt.Sprintf("%s-%d", base, freshTry+1) // -2, -3, ...
		default: // reasonAuth, reasonProtected, reasonUnknown
			return "", attempts, reason, err
		}
	}
}

// sleepCtx waits d, or returns false promptly if ctx is cancelled first. A
// non-positive d returns true immediately (used by tests with Backoff 0).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/broker/ -run 'TestPushWithRecovery|TestClassifyPushError'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/push.go internal/broker/push_test.go
git commit -m "feat(broker): bounded push recovery loop"
```

---

### Task 4: Config fields

**Files:**
- Modify: `internal/config/config.go`
- Modify: `config/config.yaml`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.PushMaxRetries int` (yaml `push_max_retries`, default `3`), `Config.PushRetryBackoff time.Duration` (yaml `push_retry_backoff`, default `1s`), `Config.PushFreshBranchTries int` (yaml `push_fresh_branch_tries`, default `2`). Env `DRYDOCK_PUSH_MAX_RETRIES`, `DRYDOCK_PUSH_RETRY_BACKOFF`, `DRYDOCK_PUSH_FRESH_BRANCH_TRIES`.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestPushRetryDefaultsAndValidation(t *testing.T) {
	d := Defaults()
	if d.PushMaxRetries != 3 || d.PushRetryBackoff != time.Second || d.PushFreshBranchTries != 2 {
		t.Errorf("push defaults = %d/%v/%d, want 3/1s/2", d.PushMaxRetries, d.PushRetryBackoff, d.PushFreshBranchTries)
	}
	for _, y := range []string{
		"network: x\ngateway_ip: 1.2.3.4\npush_max_retries: -1\n",
		"network: x\ngateway_ip: 1.2.3.4\npush_retry_backoff: -5s\n",
		"network: x\ngateway_ip: 1.2.3.4\npush_fresh_branch_tries: -2\n",
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(path, []byte(y), 0o644)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "push_") {
			t.Errorf("yaml=%q want push_ rejection, got %v", y, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestPushRetryDefaultsAndValidation`
Expected: FAIL, `d.PushMaxRetries undefined`.

- [ ] **Step 3a: Struct fields**

In `config.go`, after the `AggregateWindow` field:

```go
	// PushMaxRetries retries a transient (network) push failure this many times
	// with exponential backoff. 0 disables transient retry.
	PushMaxRetries int `yaml:"push_max_retries"`
	// PushRetryBackoff is the base delay for push retry backoff (base << n).
	PushRetryBackoff time.Duration `yaml:"push_retry_backoff"`
	// PushFreshBranchTries retries a branch-name collision against this many
	// alternate remote branch names (agent/<id>-2, -3, ...). 0 disables it.
	PushFreshBranchTries int `yaml:"push_fresh_branch_tries"`
```

- [ ] **Step 3b: Defaults**

In `Defaults()`, after the `AggregateWindow` default:

```go
		PushMaxRetries:       3,
		PushRetryBackoff:     time.Second,
		PushFreshBranchTries: 2,
```

- [ ] **Step 3c: Validation**

In `validate()`, after the `aggregate_window` check:

```go
	if c.PushMaxRetries < 0 {
		return fmt.Errorf("config: push_max_retries must be >= 0, got %d", c.PushMaxRetries)
	}
	if c.PushRetryBackoff < 0 {
		return fmt.Errorf("config: push_retry_backoff must be >= 0, got %v", c.PushRetryBackoff)
	}
	if c.PushFreshBranchTries < 0 {
		return fmt.Errorf("config: push_fresh_branch_tries must be >= 0, got %d", c.PushFreshBranchTries)
	}
```

- [ ] **Step 3d: Env overrides**

In the env-override block, after the `DRYDOCK_AGGREGATE_WINDOW` override:

```go
	if v := os.Getenv("DRYDOCK_PUSH_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.PushMaxRetries = n
		}
	}
	if v := os.Getenv("DRYDOCK_PUSH_RETRY_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.PushRetryBackoff = d
		}
	}
	if v := os.Getenv("DRYDOCK_PUSH_FRESH_BRANCH_TRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.PushFreshBranchTries = n
		}
	}
```

- [ ] **Step 3e: SeedTemplate + config.yaml mirror**

In `SeedTemplate` (config.go), after the `aggregate_window` line, add:

```
push_max_retries:       3              # retry a transient (network) push failure N times with backoff; 0 = no retry
push_retry_backoff:     1s             # base delay for push retry backoff (doubles each retry)
push_fresh_branch_tries: 2             # on a branch-name collision, try agent/<id>-2, -3, ...; 0 = no fresh-branch retry
```

Make the identical addition at the same spot in `config/config.yaml`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/`
Expected: PASS (including `TestSeedTemplate_MatchesOnDiskTemplate`).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go config/config.yaml internal/config/config_test.go
git commit -m "feat(config): push_max_retries + push_retry_backoff + push_fresh_branch_tries"
```

---

### Task 5: Audit `push_failed` rendering

**Files:**
- Modify: `internal/audit/audit.go`
- Test: `internal/audit/audit_test.go`

**Interfaces:**
- Produces: `audit.Outcome` renders a result whose `Subtype == "push_failed"` as `"push failed"`.

- [ ] **Step 1: Write the failing test**

Append to `internal/audit/audit_test.go`:

```go
func TestOutcome_PushFailed(t *testing.T) {
	r := Result{Type: "result", Subtype: "push_failed", IsError: false, NumTurns: 0}
	if got := Outcome(r, true, Meta{}); got != "push failed" {
		t.Errorf("Outcome(push_failed) = %q, want \"push failed\"", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/audit/ -run TestOutcome_PushFailed`
Expected: FAIL, got `"push_failed"` (the default case) not `"push failed"`.

- [ ] **Step 3: Implement**

In `internal/audit/audit.go`, in `Outcome`, add a case BEFORE the `case r.IsError:` line:

```go
	case r.Subtype == "push_failed":
		s = "push failed"
```

(The switch is `switch { case r.Subtype == "interrupted": ...; case r.IsError: ...; case r.Subtype == "success": ...; default: ... }`. Insert the new case immediately after the `interrupted` case so it is reached regardless of `is_error`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/audit/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/audit/audit.go internal/audit/audit_test.go
git commit -m "feat(audit): render push_failed outcome"
```

---

### Task 6: Broker wiring (recovery + terminal outcome)

**Files:**
- Modify: `internal/broker/broker.go`
- Modify: `internal/broker/handle_task_test.go`
- Modify: `cmd/brokerd/main.go`
- Test: `internal/broker/handle_task_test.go` (or the file that drives `pushAndOpenPR`)

**Interfaces:**
- Consumes: `pushWithRecovery`, `pushRetry`, `pushReason` (Task 3); `Stage.Commit`/`PushBranch` (Task 1); `audit.TotalCost` (existing); the config fields (Task 4).
- Produces: `taskStage` gains `Commit(branch, message string) error` and `PushBranch(localBranch, remoteBranch string) error`; `Broker` gains `PushMaxRetries int`, `PushRetryBackoff time.Duration`, `PushFreshBranchTries int`.

- [ ] **Step 1: Write the failing test**

Add to the broker test file that exercises `pushAndOpenPR` (follow the file's existing setup for building a `taskRun` with a `fakeStage` and capturing streamed events; mirror the nearest existing push/gate test). The test drives a task through the push with a `fakeStage` whose `PushBranch` always fails transiently, and asserts a `push_failed` terminal outcome plus the synthetic audit line:

```go
func TestPushAndOpenPR_PushFailedTerminalOutcome(t *testing.T) {
	// fakeStage.pushErr is a transient error; with retries exhausted the push
	// fails and pushAndOpenPR must emit a push_failed result (not a bare error)
	// and append a synthetic push_failed line to the audit log.
	fs := &fakeStage{pushErr: errors.New("fatal: Could not resolve host")}
	events, auditBuf := runPushAndOpenPR(t, fs, pushRetry{MaxRetries: 1, Backoff: 0, FreshBranchTries: 0})

	last := events[len(events)-1]
	if last["event"] != "result" || last["outcome"] != "push_failed" {
		t.Fatalf("last event = %+v, want result/push_failed", last)
	}
	if last["reason"] != "transient" {
		t.Errorf("reason = %v, want transient", last["reason"])
	}
	if !strings.Contains(auditBuf.String(), `"subtype":"push_failed"`) {
		t.Errorf("audit log missing synthetic push_failed result line:\n%s", auditBuf.String())
	}
}
```

`runPushAndOpenPR(t, fs, cfg)` is a small helper you add that builds a `taskRun` (with `tr.st = fs`, an in-memory `tr.logf` buffer, a capturing `tr.sw`, `tr.ctx = context.Background()`, a valid `tr.id`/`tr.instruction`, and `tr.b` a `*Broker` carrying the push-retry fields), auto-approves so the gate passes, calls `tr.pushAndOpenPR("some diff")`, and returns the captured stream events plus the audit buffer. Reuse whatever event-capture and taskRun-construction helpers the existing broker tests already use (e.g. the stream fake and `newAdapter` stub that other `pushAndOpenPR`/`gatePush` tests rely on). If the existing tests construct these inline, mirror that construction.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestPushAndOpenPR_PushFailedTerminalOutcome`
Expected: FAIL. Before the implementation it will fail to compile (`fakeStage` has no `Commit`/`PushBranch`, `Broker` has no push fields) or, once those exist, assert-fail because the current code emits a bare `error` event and writes no audit line.

- [ ] **Step 3a: Extend the `taskStage` interface + `realStage`**

In `broker.go`, add to the `taskStage` interface (keep `Push` for compatibility):

```go
	Commit(branch, message string) error
	PushBranch(localBranch, remoteBranch string) error
```

Add the `realStage` forwarders:

```go
func (r realStage) Commit(branch, msg string) error { return r.s.Commit(branch, msg) }
func (r realStage) PushBranch(local, remote string) error { return r.s.PushBranch(local, remote) }
```

- [ ] **Step 3b: Add push-retry fields to `Broker`**

In the `Broker` struct (near the other tunables like `TaskBudget`):

```go
	PushMaxRetries       int
	PushRetryBackoff     time.Duration
	PushFreshBranchTries int
```

- [ ] **Step 3c: Extend `fakeStage`**

In `internal/broker/handle_task_test.go`, add to `fakeStage`:

```go
func (f *fakeStage) Commit(branch, msg string) error {
	if f.pushErr != nil {
		return nil // let PushBranch surface the failure; Commit succeeds
	}
	return nil
}
func (f *fakeStage) PushBranch(local, remote string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = true
	f.pushBranch = remote
	return nil
}
```

(The existing `fakeStage.Push` stays. `pushErr`, `pushed`, `pushBranch` fields already exist.)

- [ ] **Step 3d: Rewrite the push block in `pushAndOpenPR`**

Replace the block from `b.setStage(tr.id, StagePushing)` through the `if err := tr.st.Push(...)` failure `return` with:

```go
	b.setStage(tr.id, StagePushing)
	base := "agent/" + tr.id
	adapterFor := b.newAdapter
	if adapterFor == nil {
		adapterFor = remote.AdapterFor
	}
	adapter := adapterFor(tr.repoRef, tr.platform)
	tr.sw.emit(map[string]any{"event": "stage", "stage": "pushing", "task_id": tr.id, "branch": base})

	branch, attempts, reason, err := pushWithRecovery(tr.ctx, tr.st, tr.id,
		"agent: "+firstLine(tr.instruction),
		pushRetry{MaxRetries: b.PushMaxRetries, Backoff: b.PushRetryBackoff, FreshBranchTries: b.PushFreshBranchTries})
	if err != nil {
		// Nothing landed on the remote (single-ref push is atomic). Record a
		// terminal push_failed result in the audit (carrying the metered cost so
		// cost + the aggregate-cap seed stay correct) and stream the reason.
		cost := audit.TotalCost(tr.auditPath)
		fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"push_failed","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), cost)
		tr.sw.emit(map[string]any{"event": "result", "outcome": "push_failed",
			"task_id": tr.id, "reason": string(reason), "push_attempts": attempts,
			"branch": base, "error": safeErr(err),
			"files": files, "insertions": insertions, "deletions": deletions,
			"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
			"hint": "nothing was pushed to the remote; the diff is preserved; retry with `drydock retry " + tr.id + "`"})
		return
	}
```

Then, in the success path that follows, use `branch` (the actual remote branch, which may be `agent/<id>-2`) wherever the old code used `branch`, and add `push_attempts` to the `pushed` event:

```go
	title, body := prContent(tr.instruction, tr.id)
	prErr := adapter.OpenRequest(remote.Request{
		WorkDir: tr.st.WorkDir(), Branch: branch, Env: tr.st.PushEnv(),
		Title: title, Body: body, Draft: tr.draft,
	})
	ev := map[string]any{"event": "result", "outcome": "pushed",
		"task_id": tr.id, "branch": branch, "platform": adapter.Name(),
		"pr_opened": prErr == nil, "push_attempts": attempts,
		"files": files, "insertions": insertions, "deletions": deletions,
		"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": audit.TotalCost(tr.auditPath)}
	if prErr != nil {
		ev["pr_error"] = safeErr(prErr)
		ev["pr_hint"] = "branch '" + branch + "' was pushed; open a PR manually (" + adapter.Name() + ")"
	}
	tr.sw.emit(ev)
```

Update the `pushAndOpenPR` doc comment: it now emits `result/outcome=denied|cancelled|pushed|push_failed` (no bare error event on push failure).

- [ ] **Step 3e: Wire config onto the Broker in brokerd**

In `cmd/brokerd/main.go`, where `b := &broker.Broker{...}` is constructed (around line 379), add the three fields:

```go
		PushMaxRetries:       cfg.PushMaxRetries,
		PushRetryBackoff:     cfg.PushRetryBackoff,
		PushFreshBranchTries: cfg.PushFreshBranchTries,
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/broker/ ./cmd/brokerd/`
Expected: PASS (new test green; existing broker tests still green). Also run `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/broker.go internal/broker/handle_task_test.go cmd/brokerd/main.go
git commit -m "feat(broker): push recovery + terminal push_failed outcome"
```

---

### Task 7: Docs + roadmap

**Files:**
- Modify: `site/docs/configuration.md`
- Modify: `site/docs/submitting-tasks.md`
- Modify: `CHANGELOG.md`
- Modify: `docs/ROADMAP.md`

- [ ] **Step 1: Document the config fields**

In `site/docs/configuration.md`, add three rows (matching the existing table format) for `push_max_retries` (env `DRYDOCK_PUSH_MAX_RETRIES`, default `3`), `push_retry_backoff` (env `DRYDOCK_PUSH_RETRY_BACKOFF`, default `1s`), and `push_fresh_branch_tries` (env `DRYDOCK_PUSH_FRESH_BRANCH_TRIES`, default `2`), each noting `0` disables that path.

- [ ] **Step 2: Document the push contract**

In `site/docs/submitting-tasks.md` (the doc covering task outcomes / the push+approval flow; if the push flow lives in a different doc, edit that one), add a short "Push outcomes" note: a single-ref push is atomic, so `push_failed` means nothing landed on the remote and the diff is preserved; drydock retries transient failures (backoff) and branch-name collisions (fresh branch) within the configured bounds; `push_failed` is retry-safe via `drydock retry <id>`; the failure reason (`transient` / `auth` / `protected` / `non_fast_forward` / `unknown`) is in the stream event and the row shows "push failed". No em dashes.

- [ ] **Step 3: CHANGELOG**

Add to the `## Unreleased` section (create it above the latest version if absent) an Added entry for push partial-failure recovery: the classified reasons, transient + fresh-branch recovery, the config knobs, and the `push_failed` terminal outcome now shown in `drydock tasks`.

- [ ] **Step 4: Mark ROADMAP 4.2 landed**

In `docs/ROADMAP.md`, change 4.2 from its current backlog phrasing to Landed with a short description of what shipped, and remove it from the ranked backlog (renumber the remaining items). Ensure no dangling reference to 4.2 as pending.

- [ ] **Step 5: Regenerate docs + verify**

Run: `make docs && go test ./... && grep -rn '—' site/docs/configuration.md site/docs/submitting-tasks.md CHANGELOG.md docs/ROADMAP.md`
Expected: `make docs` OK, tests PASS, grep returns nothing (no em dashes).

- [ ] **Step 6: Commit**

```bash
git add site/docs/configuration.md site/docs/submitting-tasks.md CHANGELOG.md docs/ROADMAP.md
git commit -m "docs: push partial-failure recovery (ROADMAP 4.2 landed)"
```

---

## Notes for the implementer

- `runGit` wraps git's combined output into the error (`fmt.Errorf("git %v: %w\n%s", ...)`), so `classifyPushError(err.Error())` sees git's stderr. No signature change to surface stderr is needed.
- The synthetic `push_failed` audit line carries `total_cost_usd` from `audit.TotalCost(tr.auditPath)` so the cost column and the aggregate-cap boot seed (which read the last result line) stay correct. Do not write `is_error:true` (it would render as a bland "error" before the new `push_failed` case).
- `pushWithRecovery` returns the base branch name only via the caller (`base := "agent/"+tr.id`) on failure; on success it returns the actual remote branch (possibly `agent/<id>-2`). Use the returned `branch` for the PR and the `pushed` event.
- Backoff uses `cfg.Backoff << transientTry` (1s, 2s, 4s for tries 0,1,2). Tests pass `Backoff: 0` so `sleepCtx` returns immediately.
- Run `go test -race ./...`, `gofmt -l .`, and `staticcheck ./...` before opening the PR.
