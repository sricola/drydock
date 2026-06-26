# GitHub PR robustness + richer PR — design

**Roadmap item:** new-user integration #1 (the PR output path — load-bearing on every task).

## Problem

drydock already opens a PR/MR after a push (`internal/remote`: `gh pr create
--fill`, `glab mr create`, `tea pr create`). Three new-user failures remain:

1. **A successful push that fails to open a PR reports "push failed."**
   `stage.Push` (`internal/stage/stage.go`) runs `git push` and then
   `opener.OpenRequest(...)` and returns the adapter's error directly. So when
   the branch pushes fine but `gh pr create` fails — `gh` not installed, not
   authenticated, or a PR already exists — the broker emits `push failed`
   (`broker.go:603-604`). The user's code is safely on the remote, but the tool
   says it failed: a terrible experience at the moment of payoff.

2. **The `gh`/`glab`/`tea` dependency is discovered only after a task runs.**
   The platform CLI must be installed and authenticated on the host. A new user
   has no early signal; they find out after waiting for a full agent run.

3. **PRs are bare.** `--fill` derives title/body from the commit message
   (`agent: <first line of instruction>`), so the PR has no real description,
   and there is no draft-PR option.

## Scope

In scope: graceful degrade (push success ≠ PR-open failure); a preflight that
warns early (`submit`) and reports in `doctor`; richer PR content (agent-written
title/body from the task instruction; `--draft`). Out of scope: deriving
per-platform "compare" URLs for the fallback hint (a generic actionable hint is
enough); Bitbucket (stays push-only); changes to the gateway/agent layers.

## Architecture

All changes live in `internal/remote`, `internal/stage`, `internal/broker`, and
the `submit`/`doctor` CLI. The single-host model holds: `drydock submit` and
`brokerd` run on the same machine, so a `submit`-side CLI preflight is
representative of where `gh` will actually run.

## Components

### 1. Adapter interface: `Request` struct + `Available()`

In `internal/remote/remote.go`, replace the positional `OpenRequest(workDir,
branch string, env []string)` with a struct argument and add a preflight:

```go
type Request struct {
	WorkDir string
	Branch  string
	Env     []string
	Title   string // PR title; empty → adapter falls back to --fill
	Body    string // PR description; empty → adapter falls back to --fill
	Draft   bool
}

type Adapter interface {
	Name() string
	// Available reports whether this adapter can open a request now: the
	// vendor CLI is on PATH and authenticated. PushOnly always returns nil.
	Available() error
	OpenRequest(r Request) error
}
```

Per-adapter behavior (each still a thin shell over the vendor CLI via the
swappable `runCLI`; add a swappable `lookPath`/probe for `Available`):

- **GitHub** — `Available`: `exec.LookPath("gh")` then `gh auth status`.
  `OpenRequest`: `gh pr create --head <branch>`; if `Title`/`Body` set,
  `--title <t> --body <b>` (else `--fill`); `--draft` when `Draft`.
- **GitLab** — `Available`: `glab` on PATH then `glab auth status`.
  `OpenRequest`: `glab mr create --source-branch <branch> --yes`; `--title
  <t> --description <b>` when set (else `--fill`); `--draft` when `Draft`.
- **Gitea** — `Available`: `tea` on PATH then `tea login list` (non-empty).
  `OpenRequest`: `tea pr create --head <branch>`; `--title <t> --description
  <b>` when set. `tea` has no draft flag, so `Draft` prefixes the title with
  `WIP: ` (Gitea's draft convention); if `Title` is empty under `Draft`, set
  the title to `WIP:` so the convention still applies.
- **PushOnly** — `Available`: nil. `OpenRequest`: nil (no-op).

`runCLI` and the new probe stay package vars so tests assert the exact argv and
simulate "not installed"/"not authed" without invoking real binaries.

### 2. Graceful degrade in stage + broker

Split the conflated push/PR steps:

- `stage.Push(branch, message string) error` — git checkout/add/commit/**push**
  only. Failure here is fatal (the branch was not saved). Drop the adapter
  argument.
- `stage.OpenRequest(adapter remote.Adapter, branch, title, body string, draft
  bool) error` — builds the curated env + `GIT_DIR`/hook-neutralization
  (today's `Push` tail), assembles a `remote.Request`, and calls
  `adapter.OpenRequest`. Returns the adapter's error.

The broker drives stage through the `taskStage` interface + `realStage` wrapper
(`broker.go:120,131`), which the white-box tests fake. Update that seam to match
the new `Push(branch, msg)` / `OpenRequest(adapter, branch, title, body, draft)`
split, and update the test fakes accordingly.

In `broker.go` (the push section ~599-610):

```go
adapter := remote.AdapterFor(t.RepoRef, t.Platform)
if err := st.Push(branch, "agent: "+firstLine(t.Instruction)); err != nil {
	sw.emit(errorEvent(taskID, "push failed: "+safeErr(err), "check the remote and push credentials"))
	return
}
// Branch is saved. Opening the PR is best-effort — never downgrade a
// successful push to a failure.
title, body := prContent(t.Instruction, taskID)
prErr := st.OpenRequest(adapter, branch, title, body, t.Draft)
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
```

`prContent(instruction, taskID)` (broker helper): `title = firstLine(instruction)`;
`body = instruction + "\n\n---\nGenerated by drydock (task " + taskID + ")."`.
Empty instruction → `("", "")`, so adapters fall back to `--fill`.

### 3. Draft flag plumbing

- `drydock submit --draft` (bool, "open the PR/MR as a draft") → add `Draft bool
  json:"draft,omitempty"` to `taskRequest` (`cmd/drydock/submit.go`).
- Add `Draft bool json:"draft"` to `broker.Task` (`internal/broker/broker.go`),
  consumed as `t.Draft` above.

### 4. Preflight UX

- **`drydock submit`** (client-side, repo-aware): after building the request,
  `adapter := remote.AdapterFor(repo, platform)`; if `err := adapter.Available();
  err != nil`, print to stderr (non-blocking):
  `⚠ <name> CLI unavailable (<reason>): the task will run and push a branch, but
  the PR won't open automatically. Fix it (e.g. 'gh auth login') and open the PR
  manually, or pass --platform none.` Then proceed with the submit.
- **`drydock doctor`** (`cmd/drydock/doctor.go`): a new non-fatal check that, for
  each of gh/glab/tea found on PATH, reports authed/not-authed via the adapter's
  `Available()`; if none is authed, a single advisory line (not a failure —
  doctor is repo-agnostic and push-only is valid).

### 5. `submit` rendering

In `cmd/drydock/submit_render.go`, the `pushed` case reads `pr_opened`:
- `true`  → `✓ pushed <branch> (<platform>) · <stat> · <dur><cost>` (unchanged).
- `false` → `✓ pushed <branch> (<platform>) — PR not opened: <pr_error> · open it manually · <stat> · <dur><cost>`.

The JSON path (`--json`) is unaffected (it prints the raw event, which now
carries `pr_opened`/`pr_error`).

## Error handling

The contract: **a saved branch is never reported as a failure.** Only
`stage.Push` (git push) is fatal; everything after is best-effort and surfaced
as `pr_opened:false` + `pr_error`. `Available()` is advisory only — it never
blocks a submit (push-only is a legitimate outcome). Unknown/empty platform
keeps today's autodetect (`AdapterFor`).

## Testing

`internal/remote` (swap `runCLI` + the probe):
- GitHub/GitLab argv with `Title`/`Body` set → `--title`/`--body`/`--description`
  present, `--fill` absent; with them empty → `--fill` present.
- `--draft` present iff `Request.Draft` for gh/glab; Gitea `Draft` → title
  starts `WIP:`.
- `Available()` → nil when probe says installed+authed; error when LookPath
  fails and when the auth probe exits non-zero. PushOnly.Available → nil.

`internal/broker` (white-box, fake stage):
- A stage whose `OpenRequest` returns an error still yields `outcome:"pushed"`,
  `pr_opened:false`, `pr_error` set — NOT a `push failed`/error event. (Guards
  the central fix.)
- A stage whose `OpenRequest` succeeds yields `pr_opened:true`, no `pr_error`.
- A stage whose `Push` fails yields the `push failed` error event (fatal path
  preserved).
- `prContent`: non-empty instruction → title=first line, body contains the
  instruction + provenance footer; empty instruction → ("","").

`cmd/drydock`:
- `submit_render` renders the `pr_opened:false` event with the "PR not opened"
  text and still shows the branch.
- `--draft` sets `taskRequest.Draft` and round-trips through JSON.

## Done when

A task whose branch pushes but whose PR can't open reports `pushed` (with a
clear "PR not opened, open it manually" note), never `push failed`; `drydock
submit` warns up front when the platform CLI isn't authed; `drydock doctor`
reports PR-tooling status; PRs carry an agent-written title/body from the task
instruction; and `drydock submit --draft` opens a draft PR/MR. All new logic is
unit-tested.
