# Issue Ingestion + Scope/Plan Gate (issue→PR foundation) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Turn a GitHub issue into a scope-gated task. Fetch the issue host-side (`gh`, creds never in the VM), feed it as a bounded instruction, and run in **plan mode** first: the agent produces a reviewable plan and the task terminates `planned` **without ever entering the push gate**. The operator reviews the plan, then runs implement mode → the existing diff-push gate. Two human gates, no orchestration state.

**Scope note:** This is the *foundation* of issue→PR — ingestion + the scope gate. The full orchestration engine (durable task queue, CI-feedback loop, retry budgets, dead-letter, the queued/needs-input/CI-failed/… state machine) is a deliberately separate further arc that builds on this.

**Architecture:** Issue fetch is CLI-side in `submit` (the CLI already holds host creds and calls `remote.Available()` at submit time). `remote.FetchIssue` mirrors the PR-open adapters (host-side `gh issue view --json`, CodeQL-safe argv). `--plan` sets `Task.PlanOnly`; the broker injects `DRYDOCK_MODE=plan` and, after the run, captures the plan from `/work/.task/plan.md` and terminates `planned` — the push path is never reached (the hard no-push guarantee; the prompt preamble is only best-effort steering).

**Tech Stack:** Go stdlib, `gh` CLI (host), the sandbox image.

## Decision record (locked)

- **Scope-gated** (the user's choice): plan first, human approves, then implement. Plan mode is a first-class task mode; implement is a normal task. Two explicit human decision points.
- **Issue text is untrusted** — it becomes the agent's prompt, same threat class as N2 (repo-content prompt injection, already accepted; the human gates are the boundary). Ingestion bounds the size. No new trust boundary; document the new source.
- **Creds stay host-side:** `gh` runs on the host with the curated env allowlist (`GH_TOKEN` etc.); only the fetched issue *text* enters the task. Nothing GitHub-cred-shaped goes into the request JSON or the VM.
- **Plan mode structurally cannot push:** the broker never calls `pushAndOpenPR` when `PlanOnly`. Even if the agent ignores the plan preamble and writes code, the diff is captured for the human but never pushed. Terminal outcome `planned`.
- **No durable queue / no in-run two-phase state machine** in this slice — plan and implement are separate task submits; durability is inherited (the plan is in the fsynced audit + a persisted `<id>.plan.md`).

## Global Constraints

- `gh` argv is CodeQL-safe (mirror `internal/remote/remote.go`'s discipline): binary is a literal `"gh"`; issue number and `owner/repo` appear only as validated flag values, never bare positionals. Issue number must match `^[0-9]+$`; `owner/repo` from a strict GitHub-issue-URL parse.
- Ingested instruction is size-bounded (cap the fetched title+body+labels; truncate with a marker) so a giant issue can't blow the `MaxTaskBodyBytes` submit cap.
- Plan capture (`/work/.task/plan.md`) is read host-side with `O_NOFOLLOW` + a byte cap (display-only text, like the diff); a symlinked `plan.md` is refused. `.task/` is already diff-excluded.
- Plan mode NEVER pushes — assert it with a fail-safe test (PlanOnly + a non-empty diff → outcome `planned`, `pushed==false`, no gate).
- Go gate before each commit: `go vet ./...`, `go test -race ./...` (scope per task), `gofmt -l internal/ cmd/` silent, `staticcheck ./...` clean. Image script changes tested in `tests/imagescripts`.
- Commit `type(scope): summary`; trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; no PR footer.

---

### Task 1: `remote.FetchIssue` + issue-URL parse

**Files:** Modify `internal/remote/remote.go` (add a capturing CLI seam + `FetchIssue` + URL parse) + a new `internal/remote/issue.go` if cleaner; `internal/remote/remote_test.go` / a new `issue_test.go`.

**Interfaces:**
- `type Issue struct { Owner, Repo string; Number int; Title, Body string; Labels []string }`
- `func ParseIssueURL(raw string) (owner, repo string, number int, err error)` — strict parse of `https://github.com/{owner}/{repo}/issues/{n}` (and the `github.com/o/r/issues/n` shorthand). Reject anything else (gitlab/gitea issue URLs → a clear "only GitHub issues supported in this version" error). `owner`/`repo` restricted to a safe charset; `number > 0`.
- `func FetchIssue(env []string, owner, repo string, number int) (Issue, error)` — runs `gh issue view <number> --repo <owner>/<repo> --json title,body,labels` via a new capturing seam `runCLIOutput` (package var, like `runCLI` but returns `([]byte, error)`), parses the JSON, returns the `Issue`. `number` is stringified from the validated int (never raw user text); `--repo <owner>/<repo>` is a flag value.

- [ ] **Step 1: Failing tests** — `issue_test.go`:
  - `TestParseIssueURL` table: valid github URL → owner/repo/number; trailing slash / `#`-fragment tolerated; a gitlab issue URL, a PR URL, a repo URL, junk → error.
  - `TestFetchIssue_Argv` — swap `runCLIOutput` to capture argv + return canned JSON; assert argv is exactly `gh issue view 42 --repo owner/repo --json title,body,labels` (number as a flag/positional value, not interpolated) and the parsed Issue matches the JSON. Mirror `TestAdapterArgv`'s seam-swap.
  - `TestFetchIssue_HostileNumberRejected` — ParseIssueURL rejects a URL whose "number" is non-numeric, so FetchIssue never sees non-numeric input; assert.
- [ ] **Step 2:** fail. **Step 3:** implement. Add `runCLIOutput` package var (`exec.CommandContext` with the same timeout/WaitDelay as `runCLI`, `CombinedOutput` or `Output`). Read `remote.go`'s CodeQL-safety comment and preserve it. **Step 4:** `go test -race ./internal/remote/`. **Step 5:** commit `feat(remote): host-side FetchIssue + GitHub issue-URL parse`.

---

### Task 2: CLI `--issue`/`--plan` ingestion + broker plumbing

**Files:** Modify `cmd/drydock/submit.go` (flags, ingest, derive repo, bounded instruction), `cmd/drydock/main.go` if a `plan` convenience is added; `internal/broker/broker.go` (Task.PlanOnly field, `DRYDOCK_MODE=plan` in buildTaskEnv); tests in `submit_test.go` + `build_task_env_test.go`.

**Interfaces / behavior:**
- `taskRequest` (submit.go) + broker `Task` gain `PlanOnly bool \`json:"plan_only,omitempty"\``.
- `submit` flags: `--issue <url>` and `--plan`. When `--issue` is set: `ParseIssueURL` → if `--repo` unset, derive `owner/repo` → `https://github.com/owner/repo`; `FetchIssue(curatedEnv, ...)`; build the instruction from the issue via a bounded formatter `issueInstruction(iss Issue, plan bool) string` = a header (`# Issue #N: <title>` + labels) + the body, truncated to a cap (e.g. 24 KiB) with a marker. The issue text is untrusted — it's just the instruction. `--issue` is mutually exclusive with `--instruction`/stdin (error if both).
- The curated env for the CLI-side gh: reuse the allowlist (extract/share `stage.adapterAllowedEnv` or a `remote`/CLI helper that builds the curated env from `os.Environ()` — do NOT pass full `os.Environ()` to gh). Confirm the allowlist is reachable from the CLI (it's in `internal/stage`; either export a `stage.CuratedEnv()` or replicate the small allowlist in the CLI with a shared source).
- `--plan` sets `PlanOnly=true`. A convenience `drydock plan --issue <url>` (or `drydock plan <url>`) = `submit --issue <url> --plan` — optional; a `--plan` flag on submit is the minimum.
- Broker `buildTaskEnv`: when the task is plan-only, append `DRYDOCK_MODE=plan` (pure function change; the entrypoint consumes it in Task 4). Thread `PlanOnly` from `Task` through to `buildTaskEnv` (or set it on `taskRun` and read there).

- [ ] **Step 1: Failing tests** — `submit_test.go`: `issueInstruction` bounds + format (title/labels/body, truncation marker on a huge body); `--issue` + `--instruction` → error; ParseIssueURL-derived repo. `build_task_env_test.go`: plan-only task env contains `DRYDOCK_MODE=plan`; non-plan task does not. (Fetch itself is Task 1's seam; here test the instruction-building + flag wiring, injecting a fake `FetchIssue` or the `runCLIOutput` seam.)
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test -race ./cmd/drydock/ ./internal/broker/`. **Step 5:** commit `feat(cli): submit --issue ingestion + --plan plumbing (DRYDOCK_MODE=plan)`.

---

### Task 3: broker plan-mode terminal + plan capture + brief provenance

**Files:** Modify `internal/broker/broker.go` (HandleTask plan branch, plan capture, `planned` outcome), `internal/stage/stage.go` (a `ReadPlan()` helper — O_NOFOLLOW capped read of `.task/plan.md`), `internal/broker/taskstate.go` (if a stage/const needed), `internal/audit/audit.go` (`planned` outcome doc + outcomeString), `internal/trustbrief/brief.go` (`TaskFacts.IssueURL` + maybe a `PlanOnly` flag; schema note); tests.

**Behavior:**
- `(s *Stage) ReadPlan() (string, bool)` — O_NOFOLLOW read of `<WorkDir>/.task/plan.md`, capped (e.g. 256 KiB), returns (text, true) or ("", false) if absent/symlinked. Mirror the prompt-write hardening.
- HandleTask: after `CaptureDiff` (and after the setup/verify phases as today), if `tr.planOnly`:
  - capture the plan: `plan, ok := tr.st.ReadPlan()`; persist `<AuditRoot>/<id>.plan.md` (0600 O_NOFOLLOW) when present.
  - `tr.outcome = "planned"`; write the broker-authored `planned` result row (mirror the `no_diff`/`verify_failed` terminal idiom); emit `{"event":"result","outcome":"planned","task_id":id,"plan_bytes":len(plan),"has_plan":ok,"hint":"drydock inspect <id> — review the plan, then run without --plan to implement"}`; **return — never call pushAndOpenPR** (the hard no-push guarantee). Do this whether or not the diff is empty.
- `writeBrief` is still called for a plan task (so the brief records the run): set a `brief.Task.PlanOnly = true` and `brief.Task.IssueURL` (thread the issue URL from the request → Task → taskRun → brief; add `Task.IssueURL` to the broker `Task` struct + `taskRequest`, `omitempty`). Bump `trustbrief` schema doc if a field is added (fields are omitempty/additive — no migration).
- `audit.outcomeString`: `planned` → "planned".

- [ ] **Step 1: Failing tests** (broker harness):
  - `TestPlanMode_TerminatesPlannedNeverPushes` (THE fail-safe test) — `PlanOnly` task, fakeStage returns a NON-EMPTY diff → terminal outcome `planned`, `fakeStage.pushed==false`, no `awaiting_approval`, no `pushAndOpenPR`. Plan mode can't push even with changes.
  - `TestPlanMode_CapturesPlanArtifact` — fakeStage.ReadPlan returns plan text → `<id>.plan.md` persisted 0600, the result event has `has_plan:true, plan_bytes>0`.
  - `TestPlanMode_NoPlanFileStillTerminatesPlanned` — ReadPlan absent → outcome `planned`, `has_plan:false` (no crash).
  - `TestPlanMode_BriefRecordsIssueAndPlanFlag` — brief.Task.PlanOnly true + IssueURL set.
  - `TestStage_ReadPlan_RefusesSymlink` (stage test) — a symlinked `.task/plan.md` → not read.
- [ ] **Step 2:** fail. **Step 3:** implement (add `ReadPlan` to the `taskStage` interface + `realStage` + `fakeStage`; thread PlanOnly/IssueURL). **Step 4:** `go test -race ./internal/broker/ ./internal/stage/ ./internal/audit/ ./internal/trustbrief/`. **Step 5:** commit `feat(broker): plan-mode scope gate — capture plan, terminate 'planned', never push`.

---

### Task 4: entrypoint plan preamble + surfacing + docs

**Files:** Modify `image/entrypoint.sh` (DRYDOCK_MODE=plan preamble) + `tests/imagescripts` test; `cmd/drydock/inspect.go` (show IssueURL + plan indicator + the plan artifact / a `drydock plan` route), `cmd/drydock/main.go` (optional `plan` subcommand = submit --issue --plan), `cmd/drydock/status.go`/`stats.go` (`planned` outcome), `internal/webui/assets/app.js` (planned outcome label if surfaced); `site/docs/submitting-tasks.md` + a new issue-workflow section; `THREAT_MODEL.md` (issue-text-is-untrusted note); `CHANGELOG.md`; tests.

**Behavior:**
- `entrypoint.sh`: near the top (after reading PROMPT, before the agent `case`), if `DRYDOCK_MODE=plan`: prepend a plan preamble to `PROMPT` — "PLAN MODE: do NOT create, modify, or delete any repository files. Produce a concise implementation plan for the task and write it to /work/.task/plan.md. Make no code changes." Keep it one env-gated block; all agents already take `-p "${PROMPT}"`, so no per-agent flag surgery. (The hard no-push guarantee is broker-side; this only steers the agent.)
- imagescripts test: assert the plan preamble appears in PROMPT when DRYDOCK_MODE=plan and is absent otherwise (mirror how the image scripts are tested — read tests/imagescripts).
- `drydock inspect`: render `issue <url>` and a `plan: planned` indicator when the brief is plan-only; a hint to review `<id>.plan.md` / the audit. Optionally a `drydock plan <issue-url>` convenience command (= submit --issue --plan) in main.go + subHelp + usage.
- `status`/`stats`: `planned` in the outcome rendering (`outcomeString` already; add to the stats fixed list for stable ordering).
- Docs: a "From a GitHub issue" workflow section in `submitting-tasks.md` (fetch host-side, plan mode first, review the plan, run to implement; creds stay host-side; issue text is untrusted). `THREAT_MODEL.md`: a one-line note that issue-sourced instructions are attacker-influenced like repo content (N2) — the human plan+diff gates are the boundary; NOT a new A-code. CHANGELOG. Check the docs-drift sentinel.

- [ ] **Step 1: Failing tests** — imagescripts plan-preamble test; inspect render test (plan-only brief shows issue + planned). **Step 2:** fail. **Step 3:** implement; `node --check app.js` if touched. **Step 4:** FULL gate: `go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/` silent; `staticcheck ./...` clean; `go test ./cmd/docs-build/`. **Step 5:** commit `feat(cli,image,docs): plan-mode preamble + issue workflow surfacing`.

---

## Final verification (whole branch)

- Full gate green.
- **No-push-in-plan-mode trace:** a PlanOnly task never reaches `pushAndOpenPR` regardless of diff content — the terminal is `planned`.
- **Creds host-side:** `gh` runs with the curated env (not full `os.Environ()`); no GitHub token in the request JSON or the VM. The issue *text* is the only thing ingested.
- **CodeQL-safe argv:** issue number/`owner/repo` are validated flag values; a hostile issue URL is rejected by `ParseIssueURL`.
- Plan artifact read is O_NOFOLLOW + capped; symlinked plan.md refused.
- Manual: `drydock submit --issue <a-real-issue-url> --plan` (if creds available) fetches, runs plan mode, terminates `planned`, `<id>.plan.md` captured, nothing pushed.
- PR notes: the full issue→PR orchestration (durable queue, CI feedback, retry budgets, state machine) is the documented next arc; this ships the ingestion + scope-gate foundation.
