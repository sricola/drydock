# Execution Profiles (Host-Config Setup Phase) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Before the agent runs, execute host-configured **setup** commands (`npm install`, `go mod download`, a build) and optional **readiness** checks inside the task VM, so the workspace is ready when the agent starts. Fail-closed (setup non-zero exit → task terminates `setup_failed` **before any API spend**), bounded (timeout + output cap), same containment as the agent run. Host-config only. Recorded as broker-observed setup evidence in the trust brief.

**Architecture:** The setup phase is the verifier stage reflected pre-agent: a `setup-<id>` VM per command, sequential, fail-fast, run through the `b.runAgent` seam so the exit code is broker-observed (never parsed from VM output). It differs from the verifier in exactly two ways — (1) it uses the **agent's egress posture** (gateway + squid allowed, so deps can be fetched) instead of the verifier's deny-all pin, and (2) it mounts the **live stage `/work`** (not a sealed export) so setup writes (`node_modules/`, `vendor/`, build artifacts) persist into the agent run. It is injected **no grant bearer**, so it cannot spend API budget. Placed after the audit log opens and before `runSandbox`, so a setup failure terminates before the agent VM ever boots.

**Tech Stack:** Go stdlib, the Apple `container` CLI, the existing sandbox image.

## Decision record (locked)

- **Host-config only** (`profiles.repos[key]`, keyed by `repokey.Normalize`). Repos cannot propose their own setup — no repo→policy channel. Empty/absent = today's behavior (no setup phase).
- **NO persistent dependency cache in this slice.** Setup runs fresh each task inside the disposable VM; deps are wiped with the stage at task end (A7 fully preserved — no state persists *between* tasks). The content-addressed cache is a deliberately separate future slice (it is where the cache-poisoning / A7-tension analysis lives).
- Setup VM gets the **agent egress posture** (same `--network`, gateway+squid proxy env, `init-firewall.sh` gateway pin, `drop-agent` privilege drop) but **no grant/bearer** — it can fetch deps, it cannot call the model or spend.
- **Fail-closed before spend:** setup runs after the audit log opens but *before* the agent VM boots; a non-zero setup/readiness exit writes a broker-authored `setup_failed` result and terminates. The agent grant is minted (free) but its bearer is never injected into the setup VM and the agent VM never runs, so spend is $0.
- **Per-command VMs sharing `/work`:** each setup command runs in its own `setup-<id>` VM (sequential, fail-fast) over the same live `/work` bind mount, so filesystem state (installed deps) carries across commands. Shell env (PATH tweaks, venv activation) does NOT carry between commands — host-config commands must be self-contained (documented). Readiness commands run after setup commands, same mechanism.
- Verdict = the container process exit code (F-07); VM stdout can never flip it. The `.setup.log` is display-only with a host-computed digest.

## Global Constraints

- The setup VM injects **no grant bearer** (assert in a test that no `tok_`/`AUTH_TOKEN`/`API_KEY` reaches `BuildSetupArgs`). It DOES get the proxy env (`HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`/`DRYDOCK_GW_IP`) so egress works.
- Every setup command runs under a bounded `context.WithTimeout(tr.ctx, timeout)` with the output wrapped in the existing `outputCap`; a wedged setup VM is force-deleted (`runner.SetupContainerName(id)` = `setup-<id>`, distinct from `task-`/`verify-`; the brokerd orphan reaper must match `setup-<32hex>` too).
- `setup_failed` is a broker-authored `src:"broker"` result subtype (mirror `verify_failed`), flowing to metrics/stats/tasks. Add to the `taskRun.outcome` vocabulary and `audit.Metrics.Outcome` doc; add `StageMs.Setup` (omitempty, additive).
- No Brief/metrics field derived from VM output; the `.setup.log` is the only VM-derived artifact (display-only, host-digested).
- Setup writes to the same `/work` bounded by the existing stage-size guard and quota — a runaway `npm install` hits the same wall as the agent.
- Go gate before each commit: `go vet ./...`, `go test -race ./...` (scope per task), `gofmt -l internal/ cmd/` silent, `staticcheck ./...` clean. VM-backed behavior is tested via the `b.runAgent` seam (no real container), like the verifier.
- Commit `type(scope): summary`; trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; no PR footer.

---

### Task 1: config `profiles.repos` block

**Files:** Modify `internal/config/config.go` (struct, validation, seed), `internal/config/explain.go` (provenance), `config/config.yaml`; tests in `config_test.go` + `explain_test.go`.

**Interfaces:**
- `type SetupProfile struct { Setup [][]string \`yaml:"setup"\`; Readiness [][]string \`yaml:"readiness"\`; Timeout time.Duration \`yaml:"timeout"\` }`
- `Config.Profiles struct { Repos map[string]SetupProfile \`yaml:"repos"\` } \`yaml:"profiles"\`` (after the `Verify` block). Zero = disabled.
- Validation (mirror the `Verify.Repos` loop at config.go:508-523): canonical key (`repokey.Normalize(key)==key`, else config error); `Setup` must have ≥1 command (a profile with no setup commands is pointless — reject); every command argv non-empty with non-empty argv[0] (both Setup and Readiness); `Timeout >= 0`.
- Explain: register `Profiles` as a yaml-only Field (compact renderer: "N setup / M readiness cmds across K repos" or "disabled"; differs = any repo configured). In `PolicyComparisonFields` (it shapes what runs pre-agent).

- [ ] **Step 1: Failing tests** — mirror the `TestVerifyConfig_*` / diff_policy config tests: `TestProfiles_LoadsAndValidates` (a profile with setup `[["npm","ci"]]` + readiness `[["curl","-fsS","localhost:3000"]]` + timeout 10m loads), `TestProfiles_Rejects` (non-canonical key; empty setup; empty argv; negative timeout — each → error mentioning "profiles"), and an `explain_test` assertion the block is a yaml-only Field. Read the actual `Verify.Repos` validation + `renderVerifyRepos`/explain entry to mirror exactly.
- [ ] **Step 2:** fail. **Step 3:** implement, incl. the commented `profiles:` seed block in BOTH `SeedTemplate` and `config/config.yaml` (documenting host-config-only, fail-closed, no-cache, self-contained-commands). Import `drydock/internal/repokey`.
- [ ] **Step 4:** `go test -race ./internal/config/ ./cmd/docs-build/`. **Step 5:** commit `feat(config): host-side profiles.repos setup/readiness block`.

---

### Task 2: `runner.BuildSetupArgs`

**Files:** Modify `internal/runner/runner.go` + `runner_test.go`.

**Interfaces:**
- `type SetupSpec struct { TaskID, Network, ImageRef, StageDir string; Env []string; Argv []string; MemoryGB, CPUs int }` — `Env` carries the proxy/gateway env (NOT a grant bearer); `StageDir` is the live `stage.WorkDir()`; `Argv` is one setup/readiness command.
- `runner.SetupContainerName(taskID string) string` = `"setup-" + taskID`.
- `runner.BuildSetupArgs(s SetupSpec) []string` — container name `setup-<id>`; `--cap-add CAP_NET_ADMIN` (root installs the egress pin, then drops); `--network s.Network`; the `s.Env` proxy vars; mounts `s.StageDir` at `/work` (live, read-write); `--entrypoint /bin/sh`; runs `setupScript` with the command as positional args.
- `setupScript` (a Go const, mirror `verifyScript` but AGENT egress not deny-all): as root, `init-firewall.sh "$DRYDOCK_GW_IP" 8088 3128` (the SAME gateway+squid pin the agent uses — reuse the image's script), `export HOME=/home/agent`, `cd /work`, `exec /usr/local/bin/drop-agent "$@"`. (Do NOT install the verifier's deny-all pin — setup needs egress.)

- [ ] **Step 1: Failing test** — `TestBuildSetupArgs_ShapeAndContainment`: asserts `--name setup-<id>`, `--mount type=bind,source=<StageDir>,target=/work`, the command as positional args after `sh`, `init-firewall.sh` (gateway pin, NOT `policy drop`/deny-all), `/usr/local/bin/drop-agent`, and CRUCIALLY that no arg contains `tok_`/`AUTH_TOKEN`/`API_KEY`/a bearer (setup can't spend). Also assert the proxy env (`HTTPS_PROXY`) IS present (egress works). Read `BuildVerifyArgs`/`verifyScript` first and mirror structure; confirm the image installs `init-firewall.sh` and `drop-agent` at the paths used.
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test -race ./internal/runner/`. **Step 5:** commit `feat(runner): setup VM argv — agent egress, live /work, no grant`.

---

### Task 3: trustbrief `SetupEvidence` block

**Files:** Modify `internal/trustbrief/brief.go` + `brief_test.go`.

**Interfaces (mirror `Verification`/`VerifyCommand`):**
- `const SetupNotConfigured = "not_configured"`, `SetupPassed = "passed"`, `SetupFailed = "failed"`, `SetupInconclusive = "inconclusive"`; per-command reuse the `VerifyCmd*` consts or add `SetupCmd*` (prefer reuse if identical; else parallel consts).
- `type SetupCommand struct { Argv []string; Status string; ExitCode int; DurationMs int64 }` (json argv/status/exit_code/duration_ms).
- `type SetupEvidence struct { Status string; Network string; LogSHA256 string; Commands []SetupCommand }` (json status; network,omitempty; log_sha256,omitempty; commands,omitempty). Network records the posture ("egress-allowlisted" — setup DOES have egress, unlike verify's "denied"). All omitempty except Status.
- `Brief.Setup SetupEvidence \`json:"setup"\`` (after `Verification`). A brief with no setup phase marshals `{"status":"not_configured"}` (minimal, like Verification's default). Round-trip test + the secret-field tripwire test must cover it.

- [ ] **Step 1: Failing test** — extend `brief_test.go`: `TestSetup_RoundTripAndOmitEmpty` (populated SetupEvidence round-trips; `not_configured` marshals status-only); the existing `TestBrief_NoSecretShapedFields` must still pass (SetupEvidence has no secret-shaped field). **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test -race ./internal/trustbrief/` + `go test -run=NONE -fuzz=FuzzAnalyze -fuzztime=15s ./internal/trustbrief/` (schema change is inert to Analyze but confirm). **Step 5:** commit `feat(trustbrief): setup evidence block`.

---

### Task 4: broker `runSetup` phase (the core)

**Files:** Create `internal/broker/setup.go` + `setup_test.go`; modify `internal/broker/broker.go` (Broker.Setup field, taskRun.setup/setupDur, HandleTask insertion, writeBrief integration), `internal/broker/taskstate.go` (StageSettingUp), `internal/broker/metrics.go` + `internal/audit/audit.go` (StageMs.Setup + setup_failed doc/outcomeString), `cmd/brokerd/main.go` (wire Broker.Setup + orphan reaper `setup-<32hex>`).

**Interfaces / behavior (mirror `internal/broker/verify.go` closely):**
- `Broker.Setup map[string]SetupProfile` (broker-local `type SetupProfile struct { Setup, Readiness [][]string; Timeout time.Duration }`, mirror `broker.VerifyRepo`). Wired from `cfg.Profiles.Repos` in brokerd.
- `taskRun` gains `setup *trustbrief.SetupEvidence`, `setupDur time.Duration`.
- `const DefaultSetupTimeout = 10 * time.Minute` (exported; a claims-table row citing a test, like `DefaultVerifyTimeout`).
- `(tr *taskRun) runSetup() bool` in `setup.go` — returns false only when it emitted the terminal event (setup failed).
  1. `cfg, ok := b.Setup[repokey.Normalize(tr.repoRef)]`; add the same case-insensitive near-miss `slog.Warn` the verifier has. If `!ok`: `tr.setup = &SetupEvidence{Status: SetupNotConfigured}`; return true.
  2. `b.setStage(tr.id, StageSettingUp)`; emit `{"event":"stage","stage":"setting_up","task_id":id,"commands":len(setup)+len(readiness)}`; record start.
  3. Open `<AuditRoot>/<id>.setup.log` (0600 O_NOFOLLOW, capped via `newOutputCap`).
  4. Build the setup env: the proxy/gateway env WITHOUT the grant bearer (a `buildSetupEnv` helper: the proxy vars + `DRYDOCK_GW_IP` from the existing `buildTaskEnv` inputs, minus `grant.EnvVars()`). Extract/reuse the proxy-building part of `buildTaskEnv` (broker.go ~725) so setup and agent share the proxy config.
  5. For each Setup command then each Readiness command (sequential, fail-fast): child `context.WithTimeout(tr.ctx, cfg.Timeout|DefaultSetupTimeout)`; argv via `runner.BuildSetupArgs`; run via `b.runAgent` (the seam); classify exactly like `execVerify` (exit 0→passed; `*exec.ExitError`→failed+code, fail fast, mark rest skipped; timeout→timed_out + force-delete `setup-<id>`; ctx cancel→cancelled terminal; output-cap→error).
  6. Overall: all passed→SetupPassed; any failed/timed_out/error→SetupFailed (setup is always required-ish: a failed setup means the workspace isn't ready, so ALWAYS fail closed — there's no "advisory setup"). Populate `tr.setup = &SetupEvidence{Status, Network:"egress-allowlisted", LogSHA256:<digest>, Commands:[...]}`; `tr.setupDur`.
  7. If Status != SetupPassed: `tr.outcome="setup_failed"`; write the broker-authored `setup_failed` result row (mirror verify_failed); emit `{"event":"result","outcome":"setup_failed","task_id":id,"reason":<which command failed>,"hint":"drydock inspect <id> — setup failed before the agent ran; no API budget spent"}`; return false.
- **HandleTask insertion:** after the audit log opens + `appendMetrics` defer is registered (broker.go ~611) and BEFORE `runner.BuildRunArgs`/`runSandbox` (~640-651): `if !tr.runSetup() { return }`. (Setup runs with no grant env; the grant minted at ~574 is never injected into the setup VM and the agent VM never boots on failure → $0 spend. Confirm the grant `defer Revoke` still fires.)
- **writeBrief integration:** copy `*tr.setup` into `brief.Setup` (always set after runSetup — not_configured on the no-profile path); add a `MissingEvidence` note when SetupInconclusive; and when SetupNotConfigured don't add noise.
- **Metrics:** `StageMs.Setup = tr.setupDur.Milliseconds()`; `setup_failed` outcome flows via `tr.outcome`. `audit.outcomeString`: `setup_failed`→"setup failed".
- **brokerd:** wire `Setup: setupProfiles(cfg.Profiles.Repos)`; extend the orphan reaper regexp set with `^setup-[0-9a-f]{32}$`.

- [ ] **Step 1: Failing tests** in `setup_test.go` (mirror `verify_test.go`'s seam-split harness — add `isSetupArgs`/route `setup-<id>` argv to a fake). Required:
  - `TestRunSetup_NotConfigured_PassesThrough` — no profile → brief `not_configured`, task proceeds to the agent as today, no `setting_up` stage.
  - `TestRunSetup_AllPass_RecordedInBrief` — fake runAgent returns nil per command → brief `Setup.Status=="passed"`, per-command exit 0, `network=="egress-allowlisted"`, `stage_ms.setup>0`, `.setup.log` 0600 with the noise, LogSHA256 matches.
  - `TestRunSetup_FailClosedBeforeSpend` (THE critical test) — first setup command exits 1 → terminal `outcome=="setup_failed"`, the AGENT run (`task-<id>` argv) **never happened** (assert the seam saw no `task-` run), no grant bearer was ever injected into any setup argv, brief persisted with Setup.Status failed, `.diff` absent (no agent, no diff), no `awaiting_approval`.
  - `TestRunSetup_ForgedPassOutputCannotFlipVerdict` — setup script prints "install OK" but exits non-zero → `setup_failed`.
  - `TestRunSetup_TimeoutForceDeletesVM` — a setup command blocks until ctx done, tiny timeout → `setup_failed`, `setup-<id>` force-deleted (observe via the delete seam).
  - `TestRunSetup_ReadinessFailureBlocks` — setup passes, readiness command exits 1 → `setup_failed`.
  - `TestRunSetup_NoGrantEnvInSetupVM` — assert `BuildSetupArgs`'s env for a configured profile contains the proxy vars but never the grant bearer.
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test -race -count=1 ./internal/broker/ ./internal/audit/ ./cmd/brokerd/` then full `./...`. **Step 5:** commit `feat(broker): host-config setup phase — fail closed before spend, broker-observed`.

---

### Task 5: CLI/UI rendering + docs

**Files:** `cmd/drydock/inspect.go` (render the Setup block, mirror printVerification), `cmd/drydock/status.go`/`stats.go` (setting_up stage count + setup_failed outcome), `internal/webui/assets/app.js` (stage label `setting_up`, and the brief panel's setup block) + `style.css`, `cmd/docs-build/claims.go` (DefaultSetupTimeout row), `site/docs/submitting-tasks.md` + `configuration.md`, `CHANGELOG.md`; tests.

**Behavior:**
- `drydock inspect`: a `setup` section — `not_configured` → single line; else status (passed/failed colored) + per-command `argv → exit N (dur)` / `→ skipped`, + the `.setup.log` path note. Mirror `printVerification` exactly.
- `status.go` healthBody gains `SettingUp int` (json setting_up) + `· N setting up` in the breakdown; `stats.go` fixed list gains `setup_failed`.
- Web UI: `setting_up: "setting up"` in the stage label map + active-stages array; the brief panel renders the setup block above/beside verification (reuse the verification rendering pattern; XSS/CSP-clean).
- Claims: `DefaultSetupTimeout` row citing `TestRunSetup_TimeoutForceDeletesVM`; regenerate `security-defaults.md`.
- Docs: `configuration.md` + `submitting-tasks.md` document `profiles` (host-config-only, fail-closed before spend, no persistent cache yet, self-contained commands, the `setting_up` stage + `setup_failed` outcome). CHANGELOG. Check the docs-drift sentinel.

- [ ] **Step 1: Failing tests** — inspect render test (populated + not_configured), status test (`setting_up` count). **Step 2:** fail. **Step 3:** implement; `node --check app.js`. **Step 4:** FULL gate: `go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/` silent; `staticcheck ./...` clean; `go test ./cmd/docs-build/`; `node --check internal/webui/assets/app.js`. **Step 5:** commit `feat(cli,webui,docs): surface the setup phase`.

---

## Final verification (whole branch)

- Full gate green.
- **Fail-closed-before-spend trace:** a `setup_failed` task never boots the agent VM and never injects a grant bearer into any VM — no API spend. The setup VM has egress (deps) but no bearer.
- Grep gates: no grant/bearer in any `BuildSetupArgs` path; `setup_failed` is a `src:"broker"` result subtype; setup verdict = exit code (never VM stdout); A7 intact (setup writes only to the per-task stage, wiped on Cleanup, no cross-task persistence).
- A VM-backed red-team-style test can be a follow-up (like V1 for the verifier); note it in the PR as deferred to `make redteam-vm` wiring.
