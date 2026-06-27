# GitHub PR robustness + richer PR — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A task whose branch pushes but whose PR can't open reports `pushed` (with a clear "open it manually" note), never `push failed`; PRs carry an agent-written title/body and support `--draft`; and the `gh`/`glab`/`tea` dependency surfaces early via `submit` and `doctor`.

**Architecture:** Decouple the two operations the broker conflates today (git push vs. PR-open) so a saved branch is never reported as a failure. `stage` becomes pure git/fs (no PR knowledge); the broker drives the `remote` adapter and treats PR-open as best-effort.

**Tech Stack:** Go 1.26.4, standard library. Shells out to `gh`/`glab`/`tea` (already the model). macOS/Linux.

## Global Constraints

- **Standard library only**, no new deps. Go 1.26.4. Ends green on `go build ./... && go vet ./... && go test ./...`, gofmt clean.
- **A saved branch is never a failure.** Only `git push` failure is fatal; every PR-open failure (CLI missing, not authed, wrong flag, PR exists, network) degrades to `outcome:"pushed", pr_opened:false, pr_error:<reason>` + a manual-PR hint.
- **Host-topology caveat:** the submit-side preflight is a same-host heuristic, **skipped when `BROKER_ADDR` is set** (brokerd may be on another host). The broker-side graceful degrade is the topology-independent net.
- **Argv-safe PR content:** title ≤ 72 chars (`…` when clipped); body capped at ~4 KB (append `\n\n[truncated — full instruction in the drydock task audit]` when clipped). Full instruction already lives in `<audit>/<id>.jsonl`.
- **Curated env preserved:** the adapter still runs with `stage`'s curated env (`GIT_DIR`, hook-neutralization) — never the work-tree `.git`.

---

### Task 1: Core graceful degrade (push ≠ PR-fail)

Decouple `stage` from PR-opening; the broker drives the adapter best-effort and emits `pr_opened`/`pr_error`. **No change to the `remote.Adapter` interface** — uses the existing `OpenRequest(workDir, branch, env)`.

**Files:**
- Modify: `internal/stage/stage.go` (`Push` → git-only; add `PushEnv()`; drop the `requestOpener` interface)
- Modify: `internal/stage/stage_test.go` (the two `s.Push(opener, …)` calls at lines ~168, 221)
- Modify: `internal/broker/broker.go` (`Task.Draft` NOT yet; `taskStage` seam; `realStage`; `newAdapter` seam; push section ~599-610)
- Modify: `internal/broker/handle_task_test.go` (`fakeStage`; add `fakeAdapter`; `testBroker`; the `pushAdapter` assertion)
- Modify: `cmd/drydock/submit_render.go` (the `pushed` case ~96-99)
- Test: the above test files

**Interfaces:**
- Produces: `stage.Push(branch, message string) error`; `stage.PushEnv() []string`; broker `newAdapter func(repoRef, platform string) remote.Adapter` seam; the `pushed` result event gains `pr_opened bool` and (on failure) `pr_error string` + `pr_hint string`.

- [ ] **Step 1: Split `stage.Push` and add `PushEnv` (replace lines 92-160 region)**

In `internal/stage/stage.go`, delete the `requestOpener` interface (lines ~92-100) and replace the `Push` method with:

```go
// Push commits the staged changes onto a new branch and pushes it via the
// host-only git dir. Opening a PR/MR is a separate, best-effort step the broker
// drives (stage no longer knows about PR adapters). Called only after the
// approval gate.
func (s *Stage) Push(branch, message string) error {
	if _, err := s.git("checkout", "-b", branch); err != nil {
		return err
	}
	if err := s.stageAll(); err != nil {
		return err
	}
	if _, err := s.git("commit", "-m", message); err != nil {
		return err
	}
	if _, err := s.git("push", "origin", branch); err != nil {
		return err
	}
	return nil
}

// PushEnv is the curated host env a PR/MR adapter must run with: the allowlisted
// vars plus GIT_DIR and hook-neutralization that keep any vendor CLI on the
// host-only git dir even if the work tree contains a planted .git. The broker
// passes this to remote.Adapter.OpenRequest after Push succeeds.
func (s *Stage) PushEnv() []string {
	env := curatedEnv()
	env = append(env,
		"GIT_DIR="+s.gitDir,
		"GIT_WORK_TREE="+s.WorkDir,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false",
	)
	return env
}
```

- [ ] **Step 2: Fix `stage_test.go` Push callers**

The two calls `s.Push(opener, "agent/...", "...")` (lines ~168, 221) drop the `opener` arg → `s.Push("agent/abc123", "agent: add feature")` and `s.Push("agent/zzz", "agent: try")`. Delete the now-unused fake `opener` value those tests construct (if a test only existed to assert the adapter was called, repoint it: the adapter call now lives in the broker — keep only the git-push assertions here). Run `go test ./internal/stage/` and fix any remaining references to `requestOpener`.

- [ ] **Step 3: Run stage tests (RED→GREEN for the split)**

Run: `go test ./internal/stage/`
Expected: PASS after Step 2. (If a test asserted PR-open behavior, it should now assert only push; that behavior moved to the broker.)

- [ ] **Step 4: Update the broker `taskStage` seam + `realStage`**

In `internal/broker/broker.go`, change the `taskStage` interface (lines ~120-124) and `realStage` (lines ~131-132):

```go
type taskStage interface {
	WorkDir() string
	WriteTaskFiles(prompt, allowlist string) error
	CaptureDiff() (string, error)
	Push(branch, msg string) error
	PushEnv() []string
	Cleanup() error
}
```

```go
func (r realStage) Push(branch, msg string) error { return r.s.Push(branch, msg) }
func (r realStage) PushEnv() []string             { return r.s.PushEnv() }
```

- [ ] **Step 5: Add the `newAdapter` test seam to the Broker struct**

In the `Broker` struct's test-seam block (near `prepareStage`/`runContainer`, ~line 92-95), add:

```go
	// newAdapter selects the remote PR/MR adapter. nil in production ->
	// remote.AdapterFor. White-box tests inject a fake to drive the
	// best-effort PR-open path without shelling out to gh/glab/tea.
	newAdapter func(repoRef, platform string) remote.Adapter
```

- [ ] **Step 6: Rewrite the broker push section (graceful degrade)**

Replace the push block in `HandleTask` (lines ~599-610) with:

```go
	b.setStage(taskID, StagePushing)
	branch := "agent/" + taskID
	adapterFor := b.newAdapter
	if adapterFor == nil {
		adapterFor = remote.AdapterFor
	}
	adapter := adapterFor(t.RepoRef, t.Platform)
	sw.emit(map[string]any{"event": "stage", "stage": "pushing", "task_id": taskID, "branch": branch})
	if err := st.Push(branch, "agent: "+firstLine(t.Instruction)); err != nil {
		sw.emit(errorEvent(taskID, "push failed: "+safeErr(err), "check the remote and push credentials"))
		return
	}
	// Branch is saved. Opening the PR/MR is best-effort — never downgrade a
	// successful push to a failure.
	prErr := adapter.OpenRequest(st.WorkDir(), branch, st.PushEnv())
	ev := map[string]any{"event": "result", "outcome": "pushed",
		"task_id": taskID, "branch": branch, "platform": adapter.Name(),
		"pr_opened": prErr == nil,
		"files": files, "insertions": insertions, "deletions": deletions,
		"duration_ms": time.Since(taskStart).Milliseconds(), "cost_usd": auditCost(auditPath)}
	if prErr != nil {
		ev["pr_error"] = safeErr(prErr)
		ev["pr_hint"] = "branch '" + branch + "' was pushed; open a PR manually (" + adapter.Name() + ")"
	}
	sw.emit(ev)
}
```

(`remote.Adapter.OpenRequest` still has its current signature `OpenRequest(workDir, branch string, env []string) error` — unchanged in this task.)

- [ ] **Step 7: Migrate the broker test fakes**

In `internal/broker/handle_task_test.go`:
- `fakeStage`: drop the `pushAdapter` field; change `Push` to `func (f *fakeStage) Push(branch, msg string) error { if f.pushErr != nil { return f.pushErr }; f.pushed = true; f.pushBranch = branch; return nil }`; add `func (f *fakeStage) PushEnv() []string { return []string{"GIT_DIR=/fake"} }`.
- Add a fake adapter:

```go
type fakeAdapter struct {
	name    string
	openErr error
	opened  bool
}

func (a *fakeAdapter) Name() string { return a.name }
func (a *fakeAdapter) OpenRequest(workDir, branch string, env []string) error {
	a.opened = true
	return a.openErr
}
```

- In `testBroker` (the shared helper, ~line 76), set a default adapter seam so no real CLI runs:

```go
		newAdapter: func(repoRef, platform string) remote.Adapter {
			return &fakeAdapter{name: remote.AdapterFor(repoRef, platform).Name()}
		},
```

  (`remote.AdapterFor(...).Name()` only picks the name; it runs no CLI.)
- The assertion in `TestHandleTask_ClaudeAutoApprove_Pushes` (line ~195) changes from `st.pushAdapter != "github"` to reading the terminal event's platform: `term["platform"] != "github"` (it already decodes `term`). Keep `st.pushed` and `st.pushBranch` checks.

- [ ] **Step 8: Add the graceful-degrade test (the core guarantee)**

Add to `internal/broker/handle_task_test.go` a test that injects a failing adapter and asserts the run still reports `pushed`:

```go
// A PR-open failure must NOT downgrade a successful push to a failure: the
// branch is saved, so the result is "pushed" with pr_opened=false.
func TestHandleTask_PROpenFailure_StillPushed(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, claudeRunWritesResult) // existing run helper
	b.newAdapter = func(repoRef, platform string) remote.Adapter {
		return &fakeAdapter{name: "github", openErr: errors.New("gh: not authenticated")}
	}
	term := postTaskAutoApprove(t, b) // existing helper that POSTs an auto-approve task and returns the terminal event
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed (a saved branch must never report failure)", term["outcome"])
	}
	if term["pr_opened"] != false {
		t.Errorf("pr_opened = %v, want false", term["pr_opened"])
	}
	if term["pr_error"] == nil {
		t.Error("pr_error should carry the adapter failure reason")
	}
	if !st.pushed {
		t.Error("the branch must still have been pushed")
	}
}
```

Adapt the helper names (`claudeRunWritesResult`, `postTaskAutoApprove`) to the actual helpers in the file — mirror `TestHandleTask_ClaudeAutoApprove_Pushes`’s setup exactly, changing only the `newAdapter` injection and the assertions. If no reusable POST helper exists, inline the same `httptest` POST that `TestHandleTask_ClaudeAutoApprove_Pushes` uses.

- [ ] **Step 9: Render the new state in `submit`**

In `cmd/drydock/submit_render.go`, replace the `case "pushed":` block (~96-99) with:

```go
	case "pushed":
		stat := fmt.Sprintf("%d files +%d/-%d", num(ev["files"]), num(ev["insertions"]), num(ev["deletions"]))
		if ev["pr_opened"] == false {
			r.persist(fmt.Sprintf("✓ pushed %s (%s) — PR not opened: %s · open it manually · %s · %s%s",
				str(ev["branch"]), str(ev["platform"]), str(ev["pr_error"]), stat, durStr(ev["duration_ms"]), costStr(ev["cost_usd"])))
		} else {
			r.persist(fmt.Sprintf("✓ pushed %s (%s) · %s · %s%s",
				str(ev["branch"]), str(ev["platform"]), stat, durStr(ev["duration_ms"]), costStr(ev["cost_usd"])))
		}
```

- [ ] **Step 10: Build, vet, fmt, test**

Run: `gofmt -w internal/stage/stage.go internal/broker/broker.go && go build ./... && go vet ./... && go test ./internal/stage/ ./internal/broker/ ./cmd/drydock/`
Expected: build/vet clean; all three packages PASS (including the new `TestHandleTask_PROpenFailure_StillPushed`).

- [ ] **Step 11: Commit**

```bash
gofmt -w internal/ cmd/
git add internal/stage/ internal/broker/ cmd/drydock/submit_render.go
git commit -m "broker: a pushed branch whose PR fails to open reports 'pushed', not 'push failed'"
```

---

### Task 2: Richer PR — title/body/draft

Change the `remote.Adapter.OpenRequest` signature to a `Request` struct carrying title/body/draft, and feed it agent-written content. This atomically touches `remote` + the broker call site (stage is already decoupled).

**Files:**
- Modify: `internal/remote/remote.go` (`Request` struct; `Adapter.OpenRequest(Request)`)
- Modify: `internal/remote/{github,gitlab,gitea,pushonly}.go`
- Modify: `internal/remote/remote_test.go` (`TestAdapterArgv`, `TestPushOnly_NeverErrors`)
- Modify: `internal/broker/broker.go` (`Task.Draft`; `prContent` helper; build `remote.Request` in the push section)
- Modify: `internal/broker/handle_task_test.go` (`fakeAdapter.OpenRequest` signature)
- Modify: `cmd/drydock/submit.go` (`--draft` flag; `taskRequest.Draft`)
- Test: the above

**Interfaces:**
- Consumes: Task 1's broker push section and `fakeAdapter`.
- Produces: `remote.Request{WorkDir, Branch, Env, Title, Body string, Draft bool}`; `remote.Adapter.OpenRequest(Request) error`; broker `prContent(instruction, taskID) (title, body string)`; `Task.Draft`; `drydock submit --draft`.

- [ ] **Step 1: Add `Request` and change the interface (`remote.go`)**

In `internal/remote/remote.go`, add the struct and change the interface:

```go
// Request describes a PR/MR to open for a freshly pushed branch. Title/Body
// empty -> the adapter falls back to the vendor CLI's commit-message --fill.
type Request struct {
	WorkDir string
	Branch  string
	Env     []string
	Title   string
	Body    string
	Draft   bool
}

type Adapter interface {
	Name() string
	OpenRequest(r Request) error
}
```

- [ ] **Step 2: Update the four adapters**

`github.go`:

```go
func (GitHubAdapter) OpenRequest(r Request) error {
	args := []string{"gh", "pr", "create", "--head", r.Branch}
	if r.Title != "" {
		args = append(args, "--title", r.Title, "--body", r.Body)
	} else {
		args = append(args, "--fill")
	}
	if r.Draft {
		args = append(args, "--draft")
	}
	return runCLI(r.WorkDir, r.Env, args...)
}
```

`gitlab.go`:

```go
func (GitLabAdapter) OpenRequest(r Request) error {
	args := []string{"glab", "mr", "create", "--source-branch", r.Branch, "--yes"}
	if r.Title != "" {
		args = append(args, "--title", r.Title, "--description", r.Body)
	} else {
		args = append(args, "--fill")
	}
	if r.Draft {
		args = append(args, "--draft")
	}
	return runCLI(r.WorkDir, r.Env, args...)
}
```

`gitea.go` (no draft flag — Gitea uses a `WIP:` title prefix):

```go
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
```

`pushonly.go`:

```go
func (PushOnlyAdapter) OpenRequest(r Request) error { return nil }
```

- [ ] **Step 3: Update `remote_test.go`**

`TestPushOnly_NeverErrors`: `(PushOnlyAdapter{}).OpenRequest(Request{WorkDir: "/tmp", Branch: "feature/x"})`.

`TestAdapterArgv`: drive each adapter with a `Request`. Replace the cases/loop body so each adapter is called as `tc.adapter.OpenRequest(Request{WorkDir: "/work", Branch: "agent/abc123", Env: env, Title: tc.title, Body: tc.body, Draft: tc.draft})`. Cover at least:
- github, Title set, no draft → `["gh","pr","create","--head","agent/abc123","--title","T","--body","B"]`
- github, Title empty → contains `--fill`, not `--title`
- github, Draft → contains `--draft`
- gitlab, Title set → `--title`/`--description` present, `--source-branch`/`--yes` present
- gitlab, Draft → contains `--draft`
- gitea, Title set → `--title T --description B`
- gitea, Draft → the title arg starts with `WIP: `

Keep the `gotWorkDir`/`gotEnv` assertions (the host git-dir env must still reach the CLI).

- [ ] **Step 4: Run remote tests**

Run: `go test ./internal/remote/`
Expected: PASS.

- [ ] **Step 5: Broker — `Task.Draft`, `prContent`, build `Request`**

In `internal/broker/broker.go`, add to the `Task` struct (after `Agent`):

```go
	// Draft opens the PR/MR as a draft (gh/glab --draft; Gitea via a WIP:
	// title prefix). Default false.
	Draft bool `json:"draft"`
```

Add a helper (near `firstLine`, ~line 867):

```go
// prContent derives a PR title and body from the task instruction. Title is the
// first line, clipped to 72 chars (PR titles must stay short). Body is the
// instruction plus a drydock provenance footer, capped at ~4 KB so it never
// blows argv limits (the full instruction is preserved in the task audit). An
// empty instruction yields ("",""), so adapters fall back to the CLI's --fill.
func prContent(instruction, taskID string) (title, body string) {
	if strings.TrimSpace(instruction) == "" {
		return "", ""
	}
	title = firstLine(instruction)
	if len(title) > 72 {
		title = title[:71] + "…"
	}
	const bodyCap = 4096
	body = instruction
	if len(body) > bodyCap {
		body = body[:bodyCap] + "\n\n[truncated — full instruction in the drydock task audit]"
	}
	body += "\n\n---\nGenerated by drydock (task " + taskID + ")."
	return title, body
}
```

In the push section (from Task 1), change the `OpenRequest` call to build a `Request`:

```go
	title, body := prContent(t.Instruction, taskID)
	prErr := adapter.OpenRequest(remote.Request{
		WorkDir: st.WorkDir(), Branch: branch, Env: st.PushEnv(),
		Title: title, Body: body, Draft: t.Draft,
	})
```

(`strings` is already imported in broker.go.)

- [ ] **Step 6: Update the broker `fakeAdapter`**

In `handle_task_test.go`, change `fakeAdapter.OpenRequest` to the new signature and record the request:

```go
func (a *fakeAdapter) OpenRequest(r remote.Request) error {
	a.opened = true
	a.gotReq = r
	return a.openErr
}
```

Add `gotReq remote.Request` to the `fakeAdapter` struct.

- [ ] **Step 7: `drydock submit --draft`**

In `cmd/drydock/submit.go`: add `Draft bool json:"draft,omitempty"` to `taskRequest`; add the flag `draft = fs.Bool("draft", false, "open the PR/MR as a draft")`; set `Draft: *draft` in the request struct literal (alongside `AutoApprove`, `Platform`, …).

- [ ] **Step 8: Add a `prContent` test**

In `internal/broker` (e.g. a new `reconcile`-style test or append to an existing `_test.go` in package `broker`):

```go
func TestPRContent(t *testing.T) {
	title, body := prContent("Add a retry to the uploader\nmore detail here", "abc")
	if title != "Add a retry to the uploader" {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(body, "more detail here") || !strings.Contains(body, "task abc") {
		t.Errorf("body missing instruction or provenance: %q", body)
	}
	if tl, _ := prContent("", "abc"); tl != "" {
		t.Errorf("empty instruction must yield empty title, got %q", tl)
	}
	long := strings.Repeat("x", 100)
	if tl, _ := prContent(long, "abc"); len([]rune(tl)) > 72 {
		t.Errorf("title not clipped: %d runes", len([]rune(tl)))
	}
	if _, b := prContent(strings.Repeat("y", 5000), "abc"); !strings.Contains(b, "truncated") {
		t.Error("oversized body must be truncated")
	}
}
```

- [ ] **Step 9: Build, vet, fmt, test**

Run: `gofmt -w internal/remote/ internal/broker/broker.go cmd/drydock/submit.go && go build ./... && go vet ./... && go test ./internal/remote/ ./internal/broker/ ./cmd/drydock/`
Expected: all PASS.

- [ ] **Step 10: Commit**

```bash
gofmt -w internal/ cmd/
git add internal/remote/ internal/broker/ cmd/drydock/submit.go
git commit -m "remote: agent-written PR title/body + --draft (Request struct)"
```

---

### Task 3: Preflight — `Available()` at submit + doctor

Add an authed-CLI check and surface it early. Additive method on the interface.

**Files:**
- Modify: `internal/remote/remote.go` (`Available()` on the interface; swappable `lookPath`/probe)
- Modify: `internal/remote/{github,gitlab,gitea,pushonly}.go`
- Modify: `internal/remote/remote_test.go` (new `TestAvailable`)
- Modify: `internal/broker/handle_task_test.go` (`fakeAdapter.Available`)
- Modify: `cmd/drydock/submit.go` (preflight warning, `BROKER_ADDR`-gated)
- Modify: `cmd/drydock/doctor.go` (PR-tooling check)
- Test: the above

**Interfaces:**
- Consumes: Task 2's `remote.Adapter`.
- Produces: `remote.Adapter.Available() error`; a submit-side warning; a doctor check.

- [ ] **Step 1: Add `Available()` + swappable probes (`remote.go`)**

Add to the `Adapter` interface: `Available() error`. Add package vars so tests simulate install/auth state:

```go
// lookPath / probeCLI are package vars so tests can simulate a missing or
// unauthenticated CLI without the real binaries.
var lookPath = exec.LookPath

// probeCLI runs `args` and returns its error (used for `gh auth status` etc.).
var probeCLI = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
```

- [ ] **Step 2: Implement `Available()` per adapter**

`github.go`:

```go
func (GitHubAdapter) Available() error {
	if _, err := lookPath("gh"); err != nil {
		return fmt.Errorf("gh not found on PATH")
	}
	if err := probeCLI("gh", "auth", "status"); err != nil {
		return fmt.Errorf("gh not authenticated (run: gh auth login)")
	}
	return nil
}
```

`gitlab.go` (same shape, `glab` / `glab auth status` / "glab auth login"); `gitea.go` (`tea` / `tea login list` / "tea login add"). `pushonly.go`:

```go
func (PushOnlyAdapter) Available() error { return nil }
```

Add `"fmt"` imports where missing.

- [ ] **Step 3: `TestAvailable` (`remote_test.go`)**

```go
func TestAvailable(t *testing.T) {
	origLook, origProbe := lookPath, probeCLI
	t.Cleanup(func() { lookPath, probeCLI = origLook, origProbe })

	// CLI missing.
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if err := (GitHubAdapter{}).Available(); err == nil {
		t.Error("missing gh must be unavailable")
	}
	// Installed but not authed.
	lookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	probeCLI = func(string, ...string) error { return errors.New("exit 1") }
	if err := (GitHubAdapter{}).Available(); err == nil {
		t.Error("unauthenticated gh must be unavailable")
	}
	// Installed + authed.
	probeCLI = func(string, ...string) error { return nil }
	if err := (GitHubAdapter{}).Available(); err != nil {
		t.Errorf("authed gh must be available: %v", err)
	}
	// PushOnly is always available.
	if err := (PushOnlyAdapter{}).Available(); err != nil {
		t.Errorf("push-only must always be available: %v", err)
	}
}
```

- [ ] **Step 4: `fakeAdapter.Available` (broker test)**

Add to `handle_task_test.go`: `func (a *fakeAdapter) Available() error { return nil }`.

- [ ] **Step 5: Submit-side preflight (`submit.go`)**

After the request is built and before/around the POST, add (only when `BROKER_ADDR` is unset):

```go
	if os.Getenv("BROKER_ADDR") == "" {
		adapter := remote.AdapterFor(*repo, *platform)
		if err := adapter.Available(); err != nil {
			fmt.Fprintf(os.Stderr,
				"⚠ %s CLI unavailable on this host (%v): the task will run and push a branch, but the PR won't open automatically. Fix it (e.g. 'gh auth login') and open the PR manually, or pass --platform none.\n",
				adapter.Name(), err)
		}
	}
```

Add the `drydock/internal/remote` import to `submit.go`. (PushOnly's `Available()` is nil, so `--platform none` never warns.)

- [ ] **Step 6: Doctor check (`doctor.go`)**

Add a non-fatal step that reports PR-tooling status. After the existing checks:

```go
	// PR tooling: report which platform CLI (if any) is authenticated. Not a
	// failure — push-only is a legitimate mode, and doctor is repo-agnostic.
	anyAuthed := false
	for _, a := range []remote.Adapter{remote.GitHubAdapter{}, remote.GitLabAdapter{}, remote.GiteaAdapter{}} {
		if err := a.Available(); err == nil {
			step("PR tooling: "+a.Name(), true, "authenticated")
			anyAuthed = true
		}
	}
	if !anyAuthed {
		fmt.Println("note: no PR CLI (gh/glab/tea) is authenticated — tasks will push a branch but not open a PR until you authenticate one.")
	}
```

Add the `remote` import to `doctor.go`. (Match `step(...)`’s actual signature in the file.)

- [ ] **Step 7: Build, vet, fmt, full test**

Run: `gofmt -w internal/remote/ cmd/drydock/ && go build ./... && go vet ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
gofmt -w internal/ cmd/
git add internal/remote/ internal/broker/handle_task_test.go cmd/drydock/submit.go cmd/drydock/doctor.go
git commit -m "remote: preflight gh/glab/tea auth at submit + doctor"
```

---

## Self-Review

**Spec coverage:**
- Graceful degrade (A): Task 1 (stage split, broker best-effort, `pr_opened`/`pr_error`, render). ✓
- Preflight (B): Task 3 (`Available()`, submit warning `BROKER_ADDR`-gated, doctor). ✓
- Richer PR (C): Task 2 (`Request` title/body/draft, `prContent` with 72-char title + 4 KB body caps, `--draft`). ✓
- Core safety property (every PR-step failure → `pushed`, never `push failed`): Task 1 Step 8 guards it. ✓
- Test matrix (argv per adapter, `Available`, broker graceful-degrade, render, `prContent`): distributed across tasks. ✓

**Placeholder scan:** none — every step has complete code or an exact mechanical instruction against a named file/line. The two helper names in Task 1 Step 8 (`claudeRunWritesResult`, `postTaskAutoApprove`) are explicitly flagged to be matched to the file’s real helpers, mirroring `TestHandleTask_ClaudeAutoApprove_Pushes`.

**Type consistency:** `stage.Push(branch, msg)`/`PushEnv() []string` defined in Task 1 and consumed by `realStage`/`taskStage`/`fakeStage` consistently; `remote.Adapter` evolves additively (Task 1 uses the existing `OpenRequest(workDir,branch,env)`; Task 2 → `OpenRequest(Request)`; Task 3 adds `Available()`), with the `fakeAdapter` updated in lockstep each task so the tree compiles per commit; `remote.Request` fields match between the struct (Task 2 Step 1), the adapters (Step 2), and the broker call (Step 5).
