# Git Push-Credential Preflight Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fail push-credential problems at submit time (before any VM/spend) instead of at end-of-task push time, and make every host-side git operation non-interactive.

**Architecture:** Three independent pieces: (1) a fail-fast env helper applied to all host-side git invocations (stage `runGit`, `gitDiffCapped`, `PushEnv`); (2) a `Stage.PushPreflight(branch)` dry-run push, wired into the broker's `HandleTask` right after stage prepare via an optional-capability type assertion, failing the task with a `classifyPushError`-classified reason; (3) a heuristic `drydock doctor` credentials step.

**Tech Stack:** Go stdlib only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-07-27-push-preflight-design.md` (read it first).

## Global Constraints

- Branch: `push-preflight` (exists, contains the spec).
- No em dashes anywhere (code comments, docs, commit messages); use commas/colons/parens.
- Commit messages end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- `GIT_TERMINAL_PROMPT=0` and `GCM_INTERACTIVE=never` are set unconditionally on every host-side git path; `GIT_SSH_COMMAND=ssh -oBatchMode=yes` is set ONLY when the operator has not set `GIT_SSH_COMMAND` themselves (never clobber a custom transport).
- The probe has no config opt-out, runs once (no retry), uses the same refspec the real push would (`HEAD:refs/heads/agent/<task-id>`), and must run BEFORE `WriteTaskFiles`, credential mint, and the audit-log open.
- Existing broker test fakes must keep compiling untouched (optional-capability assertion, the `BaseCommit` idiom).
- gofmt on every touched file; `go vet ./...` clean.

---

### Task 1: Fail-fast git environment helper in the stage

**Files:**
- Modify: `internal/stage/stage.go` (`runGit` ~line 51, `gitDiffCapped` ~line 175, `PushEnv` ~line 306)
- Test: `internal/stage/gitenv_test.go` (create)

**Interfaces:**
- Consumes: nothing new.
- Produces: `func gitHardenedEnv(base []string) []string` (package-private) appending the three vars per the Global Constraints rule; `runGit` and `gitDiffCapped` set `cmd.Env = gitHardenedEnv(os.Environ())`; `PushEnv` appends the same vars to its curated slice via `gitHardenedEnv(env)`.

- [ ] **Step 1: Write the failing test**

Create `internal/stage/gitenv_test.go`:

```go
package stage

import (
	"strings"
	"testing"
)

func has(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func hasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

func TestGitHardenedEnv_AlwaysDisablesPrompts(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "") // ensure unset semantics via os.LookupEnv below
	// t.Setenv sets it to empty string, which still counts as "set" for
	// os.LookupEnv; unset it explicitly for the not-set case.
	t.Setenv("GIT_SSH_COMMAND", "")
	env := gitHardenedEnv([]string{"PATH=/usr/bin"})
	if !has(env, "GIT_TERMINAL_PROMPT=0") {
		t.Error("GIT_TERMINAL_PROMPT=0 missing")
	}
	if !has(env, "GCM_INTERACTIVE=never") {
		t.Error("GCM_INTERACTIVE=never missing")
	}
	if !has(env, "PATH=/usr/bin") {
		t.Error("base env not preserved")
	}
}

func TestGitHardenedEnv_RespectsOperatorSSHCommand(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /custom/key")
	env := gitHardenedEnv(nil)
	if has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Error("operator GIT_SSH_COMMAND clobbered by BatchMode default")
	}
}

func TestGitHardenedEnv_BatchModeWhenUnset(t *testing.T) {
	// Simulate truly-unset: gitHardenedEnv keys the decision on
	// os.LookupEnv, so clear it for this test.
	t.Setenv("GIT_SSH_COMMAND", "x")
	// no direct way to unset via t.Setenv; use the helper's documented
	// treatment: an empty value counts as unset.
	t.Setenv("GIT_SSH_COMMAND", "")
	env := gitHardenedEnv(nil)
	if !has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Error("BatchMode default missing when operator has no GIT_SSH_COMMAND")
	}
}

func TestPushEnv_CarriesHardenedGitEnv(t *testing.T) {
	s := &Stage{WorkDir: t.TempDir(), gitDir: t.TempDir()}
	env := s.PushEnv()
	if !has(env, "GIT_TERMINAL_PROMPT=0") || !has(env, "GCM_INTERACTIVE=never") {
		t.Errorf("PushEnv missing hardened git env: %v", env)
	}
	if !hasKey(env, "GIT_DIR") {
		t.Errorf("PushEnv lost its existing GIT_DIR: %v", env)
	}
}
```

Semantics decision encoded above (make the helper match): an empty-string `GIT_SSH_COMMAND` counts as unset (empty means "no custom transport"); document that in the helper comment.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/stage/ -run 'GitHardenedEnv|PushEnv_Carries' -v`
Expected: compile FAIL (`gitHardenedEnv` undefined).

- [ ] **Step 3: Implement**

In `internal/stage/stage.go`:

```go
// gitHardenedEnv returns base plus the vars that make every host-side git
// invocation non-interactive: a missing credential fails in milliseconds
// with a classifiable error instead of prompting on stdin (foreground
// start) or wedging under launchd (no TTY). The SSH BatchMode default is
// applied only when the operator has no GIT_SSH_COMMAND of their own
// (empty counts as unset); a custom transport is never clobbered.
func gitHardenedEnv(base []string) []string {
	env := append(append([]string{}, base...),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	)
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	}
	return env
}
```

Wire it:
- `runGit` (~line 52): after `cmd.Dir = dir` add `cmd.Env = gitHardenedEnv(os.Environ())`.
- `gitDiffCapped` (~line 182): after `cmd.Dir = s.WorkDir` add `cmd.Env = gitHardenedEnv(os.Environ())`.
- `PushEnv` (~line 306): change the return to `return gitHardenedEnv(env)` (after the existing appends).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/stage/` (the package suite includes real-git tests; they must stay green with the new env)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stage/stage.go internal/stage/gitenv_test.go
git commit -m "fix(stage): host-side git never prompts (terminal-prompt off, GCM never, ssh BatchMode default)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: `Stage.PushPreflight` dry-run probe

**Files:**
- Modify: `internal/stage/stage.go` (new method near `PushBranch` ~line 297)
- Test: `internal/stage/preflight_test.go` (create)

**Interfaces:**
- Consumes: the existing `s.git(...)` wrapper (hardened, cancellable, hooks-neutralized).
- Produces: `func (s *Stage) PushPreflight(branch string) error` running `git push --dry-run origin HEAD:refs/heads/<branch>`; the error carries git's combined output (runGit already wraps it) so `classifyPushError` can classify it. Task 3 discovers it via `interface{ PushPreflight(string) error }`.

- [ ] **Step 1: Write the failing test**

Create `internal/stage/preflight_test.go`. Find the existing local-remote fixture pattern first (`grep -n "git init\|bare\|Prepare(" internal/stage/*_test.go`) and reuse its helpers for creating a source repo; the shape is:

```go
package stage

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// bareRemoteAndStage clones a one-commit local repo into a Stage whose
// origin is a local bare repo, so dry-run pushes need no network.
// REUSE the existing test helpers for repo creation if the package has
// them (grep first); otherwise this minimal builder stands alone.
func bareRemoteAndStage(t *testing.T) *Stage {
	t.Helper()
	src := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(src, "init", "-q", "-b", "main")
	run(src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "origin.git")
	run(src, "clone", "-q", "--bare", ".", bare)

	st, err := Prepare(context.Background(), t.TempDir(), bare)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = st.Cleanup() })
	return st
}

func TestPushPreflight_SucceedsAgainstWritableRemote(t *testing.T) {
	st := bareRemoteAndStage(t)
	if err := st.PushPreflight("agent/test123"); err != nil {
		t.Fatalf("PushPreflight: %v", err)
	}
	// A dry-run must not create the ref on the remote.
	cmd := exec.Command("git", "--git-dir", st.gitDir, "ls-remote", "origin", "refs/heads/agent/test123")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("dry-run created the remote ref:\n%s", out)
	}
}

func TestPushPreflight_FailsAgainstMissingRemote(t *testing.T) {
	st := bareRemoteAndStage(t)
	// Point origin at a path that does not exist.
	if _, err := st.git("remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git")); err != nil {
		t.Fatal(err)
	}
	if err := st.PushPreflight("agent/test123"); err == nil {
		t.Fatal("PushPreflight succeeded against a nonexistent remote")
	}
}
```

(If `st.gitDir` is unexported but the test is in-package, direct field access is fine; it is.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/stage/ -run PushPreflight -v`
Expected: compile FAIL (`PushPreflight` undefined).

- [ ] **Step 3: Implement**

In `internal/stage/stage.go`, near `PushBranch`:

```go
// PushPreflight proves write auth against the actual remote before any VM
// work: a dry-run push authenticates and computes ref updates without
// sending objects or moving refs. Uses the same refspec the real push
// will, through the hardened non-interactive git wrapper, so a missing
// credential fails here in milliseconds instead of after the agent ran.
func (s *Stage) PushPreflight(branch string) error {
	_, err := s.git("push", "--dry-run", "origin", "HEAD:refs/heads/"+branch)
	return err
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/stage/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stage/stage.go internal/stage/preflight_test.go
git commit -m "feat(stage): PushPreflight dry-run write-auth probe

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Broker wires the probe into HandleTask

**Files:**
- Modify: `internal/broker/broker.go` (in `HandleTask`, immediately after `tr.st = st` ~line 459, before `st.WriteTaskFiles`)
- Test: `internal/broker/preflight_test.go` (create)

**Interfaces:**
- Consumes: `interface{ PushPreflight(string) error }` from Task 2 (type assertion on `tr.st`); `classifyPushError(errText string) pushReason` (pushfail.go); `errorEvent(taskID, reason, hint string)` (text.go); `safeErr`, `firstLine` (text.go).
- Produces: tasks failing at accept with a terminal error event `reason: "push preflight failed (<class>): <first line>"`; hint present for the auth class.

- [ ] **Step 1: Write the failing test**

Create `internal/broker/preflight_test.go` (same package; reuses `fakeStage`, `testBroker`, `submit` from handle_task_test.go):

```go
package broker

import (
	"errors"
	"strings"
	"testing"
)

// preflightStage wraps fakeStage with the optional PushPreflight capability.
type preflightStage struct {
	fakeStage
	preflightErr error
	gotBranch    string
}

func (p *preflightStage) PushPreflight(branch string) error {
	p.gotBranch = branch
	return p.preflightErr
}

func TestHandleTask_PushPreflightAuthFailure_FailsBeforeAnyWork(t *testing.T) {
	st := &preflightStage{
		fakeStage:    fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"},
		preflightErr: errors.New("git push --dry-run: fatal: Authentication failed for 'https://github.com/o/r.git'"),
	}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)

	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error event", term)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "push preflight failed (auth)") {
		t.Errorf("reason=%q, want push preflight failed (auth)", reason)
	}
	if hint, _ := term["hint"].(string); hint == "" {
		t.Error("auth-class preflight failure carries no hint")
	}
	id, _ := events[0]["task_id"].(string)
	if st.gotBranch != "agent/"+id {
		t.Errorf("probe branch=%q, want agent/%s", st.gotBranch, id)
	}
	// Fails BEFORE any task work: no prompt written, nothing pushed.
	if st.gotPrompt != "" {
		t.Errorf("WriteTaskFiles ran despite failed preflight (prompt=%q)", st.gotPrompt)
	}
	if st.pushed.Load() {
		t.Error("push happened despite failed preflight")
	}
}

func TestHandleTask_PushPreflightPasses_TaskRuns(t *testing.T) {
	st := &preflightStage{fakeStage: fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("terminal=%v, want pushed (probe passed)", term)
	}
}
```

(The no-capability path, a plain `fakeStage`, is exercised by every existing HandleTask test staying green.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/broker/ -run PushPreflight -v`
Expected: the auth test FAILS (task currently proceeds; terminal is result/pushed, not error).

- [ ] **Step 3: Implement**

In `HandleTask`, immediately after `tr.st = st` and before the `st.WriteTaskFiles` call:

```go
	// Write-auth preflight (see the 2026-07-27 push-preflight spec): prove
	// push credentials against the actual remote NOW, before task files,
	// credential mint, or any VM spend. Fail-closed with the same
	// classification the real push uses. Optional capability so test fakes
	// without it are unaffected (the BaseCommit idiom).
	if pf, ok := tr.st.(interface{ PushPreflight(string) error }); ok {
		if err := pf.PushPreflight("agent/" + taskID); err != nil {
			class := classifyPushError(err.Error())
			slog.Warn("task push preflight failed", "task_id", taskID, "class", string(class), "err", err)
			hint := ""
			if class == reasonAuth {
				hint = "no working push credential for this remote; run `gh auth setup-git` (https) or check your SSH key/agent"
			}
			sw.emit(errorEvent(taskID,
				"push preflight failed ("+string(class)+"): "+firstLine(safeErr(err)), hint))
			return
		}
	}
```

Check `firstLine`'s exact signature in text.go before use (it exists; adapt if it takes different args).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/broker/ -race`
Expected: PASS including all existing HandleTask tests (plain fakeStage has no PushPreflight, so nothing changes for them).

- [ ] **Step 5: Commit**

```bash
git add internal/broker/broker.go internal/broker/preflight_test.go
git commit -m "feat(broker): fail tasks at accept when the push preflight finds no write auth

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: `drydock doctor` credentials step

**Files:**
- Modify: `cmd/drydock/doctor.go`
- Test: `cmd/drydock/doctor_test.go` (extend)

**Interfaces:**
- Consumes: the `step(label string, ok bool, detail string)` helper (init.go:187); doctor's `failed` flag convention.
- Produces: `func pushCredsAvailable(credHelper, sshAuthSock string, sshKeys []string) (bool, string)` (pure, unit-tested) and a doctor step calling it with live inputs.

- [ ] **Step 1: Write the failing test**

Append to `cmd/drydock/doctor_test.go` (match its existing style):

```go
func TestPushCredsAvailable(t *testing.T) {
	cases := []struct {
		name       string
		credHelper string
		sshSock    string
		sshKeys    []string
		wantOK     bool
	}{
		{"https helper", "osxkeychain", "", nil, true},
		{"ssh agent", "", "/tmp/agent.sock", nil, true},
		{"ssh key on disk", "", "", []string{"/Users/x/.ssh/id_ed25519"}, true},
		{"nothing", "", "", nil, false},
	}
	for _, c := range cases {
		ok, detail := pushCredsAvailable(c.credHelper, c.sshSock, c.sshKeys)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v want %v (%s)", c.name, ok, c.wantOK, detail)
		}
		if detail == "" {
			t.Errorf("%s: empty detail", c.name)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/drydock/ -run TestPushCredsAvailable -v`
Expected: compile FAIL.

- [ ] **Step 3: Implement**

In `cmd/drydock/doctor.go`:

```go
// pushCredsAvailable is the pure heart of doctor's git-credential check: a
// heuristic (doctor knows no target repo; the per-repo preflight at submit
// is the real gate). Passes when an HTTPS credential helper is configured
// or SSH looks usable (agent socket or a private key on disk).
func pushCredsAvailable(credHelper, sshAuthSock string, sshKeys []string) (bool, string) {
	switch {
	case credHelper != "":
		return true, "https credential helper: " + credHelper
	case sshAuthSock != "":
		return true, "ssh agent socket present"
	case len(sshKeys) > 0:
		return true, "ssh key on disk: " + filepath.Base(sshKeys[0])
	default:
		return false, "no https credential helper and no ssh key/agent; pushes will fail at the submit preflight. Fix: `gh auth setup-git` (https) or create/load an ssh key"
	}
}
```

In `runDoctor`, add a step after the existing checks (before the summary), gathering live inputs:

```go
	// N. Git push credentials (heuristic; the per-repo submit preflight is
	// the enforced gate). Non-interactive: prompts are disabled everywhere.
	helperOut, _ := runCmd("git", "config", "--get", "credential.helper")
	keys, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".ssh", "id_*"))
	// Public keys don't count; keep only files without .pub.
	priv := keys[:0]
	for _, k := range keys {
		if !strings.HasSuffix(k, ".pub") {
			priv = append(priv, k)
		}
	}
	ok, detail := pushCredsAvailable(strings.TrimSpace(string(helperOut)), os.Getenv("SSH_AUTH_SOCK"), priv)
	step("git push credentials", ok, detail)
	if !ok {
		failed = true
	}
```

Check `runCmd`'s signature in doctor.go first and match it; place the step to read naturally in doctor's output order (after auth/key checks). Adapt the numbered comment to doctor's actual comment style.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/drydock/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/drydock/doctor.go cmd/drydock/doctor_test.go
git commit -m "feat(doctor): heuristic git push-credential check with actionable hints

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Docs, changelog, full verification

**Files:**
- Modify: `site/docs/submitting-tasks.md` (probe behavior near where push/gate flow is documented)
- Modify: `site/docs/troubleshooting.md` (auth failure entry)
- Modify: `CHANGELOG.md` (new Unreleased section at the top, above `## v0.6.5`)
- Test: full suite

- [ ] **Step 1: site docs**

`submitting-tasks.md`: where the task lifecycle / push behavior is documented (grep `push` in the file and pick the natural spot), add a short subsection: at submit, before the sandbox boots, drydock runs `git push --dry-run` against the repo with the exact branch it would later push; a failure ends the task immediately with a classified reason and nothing spent. State the consequence plainly: a task against a repo you cannot push to fails at submit; there is no opt-out. Note git never prompts: HTTPS needs a credential helper (`gh auth setup-git`), SSH runs in BatchMode unless `GIT_SSH_COMMAND` is set.

`troubleshooting.md`: add an entry keyed on the error text `push preflight failed (auth)`: what it means, the two fixes (credential helper via `gh auth setup-git`, or SSH key/agent), and that `drydock doctor` now surfaces this generically.

- [ ] **Step 2: CHANGELOG**

Add at the top (above `## v0.6.5`):

```markdown
## Unreleased

### Added

- **Push-credential preflight at submit.** Before the sandbox boots,
  drydock proves write auth against the task's repo with a
  `git push --dry-run` on the exact branch it would later push; a
  failure ends the task at accept time with a classified reason
  (`push preflight failed (auth): ...`) and an actionable hint, so a
  missing credential can no longer cost a full agent run. There is no
  opt-out: a task against a repo you cannot push to now fails at
  submit. `drydock doctor` gains a generic push-credential check.

### Changed

- **Host-side git is now strictly non-interactive.** Every git
  invocation (clone, diff, push, and the env handed to gh/glab) runs
  with terminal prompts disabled and, unless `GIT_SSH_COMMAND` is set
  by the operator, SSH in BatchMode; a credential gap fails in
  milliseconds instead of prompting on stdin or wedging under launchd.
```

- [ ] **Step 3: Full verification**

Run, expecting every one to pass:

```bash
go test -race -count=1 ./...
go vet ./...
make lint
make redteam
go test ./cmd/docs-build/
```

- [ ] **Step 4: Commit**

```bash
git add site/docs/ CHANGELOG.md
git commit -m "docs: push preflight, non-interactive git, and the doctor credentials check

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: PR

- [ ] **Step 1: Push and open the PR** (no banner in the body): summarize the three pieces, the stated no-opt-out consequence, and the verification results.
- [ ] **Step 2: Request review** via the requesting-code-review flow; review agents must be read-only or worktree-isolated.

---

## Self-Review Notes

- Spec coverage: piece 1 (Task 1), piece 2 probe (Tasks 2-3), piece 3 doctor (Task 4), docs/changelog (Task 5). The spec's "before WriteTaskFiles, mint, and audit open" ordering is asserted by Task 3's test (gotPrompt empty).
- The one deliberate judgment encoded: empty-string `GIT_SSH_COMMAND` counts as unset (Task 1 comment + test).
- Type consistency: `PushPreflight(string) error` matches between Task 2's method, Task 3's assertion, and the fake; `classifyPushError(string) pushReason` and `reasonAuth` exist in pushfail.go as quoted.
- Deferred to implementer verification (with grep instructions in place): `runCmd`/`firstLine` exact signatures, doctor step placement, existing stage test fixture helpers.
