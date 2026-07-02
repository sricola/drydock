# Repo Cleanup Sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute the full cleanup backlog from the 2026-07-01 five-lens repo analysis: dead-code removal, small behavioral-bug fixes, one real egress security gap, docs/release reconsolidation (tag v0.4.0), duplication collapse, and structural refactors.

**Architecture:** Five sequential PRs, each branched off updated `main`, CI-green, squash-merged before the next starts. Tasks within a PR are ordered; later PRs assume earlier ones merged. The companion analysis document `scratchpad/drydock-analysis-2026-07-01.md` carries the exhaustive per-finding `file:line` list — this plan references it by finding ID (P0.1, P2.3, etc.) rather than re-transcribing every location.

**Tech Stack:** Go 1.26 (stdlib `flag`, `net/http`, `crypto/subtle`, `os/exec`), squid userspace proxy, vanilla JS/CSS SPA, GitHub Actions (test.yml, release.yml).

## Global Constraints

- **Behavior-preserving unless a task says otherwise.** Every task keeps `go build ./...` and `go test ./...` green. Run both before every commit.
- **No new dependencies.** Module stays at two deps (goldmark, yaml.v3).
- **Keep the threat-model invariants intact** (A1–A7): real key never in VM, deny-by-default egress, host-only `.git`, `.task/` excluded from diff, push gate, widening gate, no cross-task state. Security tasks tighten these; none may loosen them.
- **Preserve the fail-closed posture** everywhere: on error, deny/abort rather than proceed.
- **TDD for behavioral changes.** Write/extend a failing test first where a task changes observable behavior; pure deletions and comment fixes verify via the existing suite.
- **Match existing style.** `errors.New` for verb-less messages, `slog` for daemon logs, rune-safe truncation, one-rule-per-line CSS.
- **Conventional-commit messages**, one commit per task step-group, ending with the repo's Co-Authored-By/Claude-Session trailers.
- **Deliberate patterns to leave alone** (verified in analysis, do NOT "clean up"): CLI↔broker struct mirroring `taskRequest`/`taskState` (documented hygiene — only the intra-package `domain`/`reqDomain` duplicate in PR4 is in scope); `printPretty` legacy compat path in submit.go; `brokerdLock` write-only var; serial test suite (no `t.Parallel`); stage host-only-git and curated-env invariants; broker's injected seams.

---

## PR 1 — Quick wins (dead code + small behavioral fixes + opencode surfacing)

Branch: `cleanup/quick-wins`. All tasks here are S-effort, high-confidence.

### Task 1: Delete confirmed dead code

**Files:**
- Modify: `internal/creds/creds.go` (remove `StaticProvider` + `staticGrant`, keep `Provider`/`Grant` interfaces)
- Delete: `internal/creds/creds_test.go` (exists only to test the removed types — confirm it tests nothing else first; if it has other tests, keep those)
- Modify: `internal/netfw/netfw.go` (remove `GatewayIP`, drop now-unused `net` import, fix package doc line that references it)
- Modify: `internal/netfw/netfw_test.go` (remove `TestGatewayIP`)
- Modify: `internal/provider/provider.go` (remove `Labels()`)
- Modify: `internal/provider/provider_test.go` (remove the `Labels` test)
- Modify: `internal/config/config.go` (remove `Config.String()`, ~417-427)
- Modify: `cmd/drydock/logs.go` (remove the `stdout()`/`stderr()` no-op indirection ~45-48 and inline `os.Stdout`/`os.Stderr` at call sites in logs.go/kill.go; remove the dead `errors.Is(err, io.EOF)` check ~33 and the now-unused `errors` import if unused)
- Modify: `cmd/drydock/kill.go` (update call sites if they used `stdout()`/`stderr()`)
- Modify: `cmd/drydock/start.go` (remove the pointless IIFE ~41: `cfg, _ = config.Load("")` directly)
- Modify: `internal/webui/assets/app.js` (remove dead `b.dataset.orig = label` write ~364)
- Modify: `internal/webui/assets/style.css` (remove dead `.pill` selector ~28)

**Interfaces:**
- Produces: `creds.Provider`, `creds.Grant` interfaces remain; nothing else consumes the deleted symbols (deadcode-confirmed).

- [ ] **Step 1: Confirm zero non-test callers** for each symbol: `grep -rn 'StaticProvider\|staticGrant\|GatewayIP\|\.Labels()\|Config.*String()\|dataset.orig\|\.pill' --include=*.go --include=*.js --include=*.css .` Verify each hit is a definition, its own test, or (for `.pill`) absent from JS/HTML.
- [ ] **Step 2: Delete the symbols and their tests** as listed. For `creds_test.go`, if it contains only `StaticProvider` tests, delete the file; otherwise remove just those tests.
- [ ] **Step 3: Fix imports and doc comments** — drop `net` from netfw.go, `errors` from logs.go if now unused; update netfw.go package doc that mentions the gateway IP derivation.
- [ ] **Step 4: Verify** `go build ./... && go vet ./... && go test ./...` all green; `gofmt -l .` clean.
- [ ] **Step 5: Commit** `chore: remove confirmed dead code (StaticProvider, GatewayIP, Labels, Config.String, logs indirection)`

### Task 2: Small behavioral fixes

Each item is an observable-behavior fix; add or extend a test where the behavior is testable. Locations are exact in the analysis (P2.1–P2.10).

**Files:**
- Modify: `cmd/drydock/wizard.go` (~263-266): make the config-write path surface failure — check the `os.WriteFile` error, and only print "wrote %s" on success; on error, return/report it so `drydock start` doesn't boot stale.
- Modify: `internal/remote/remote.go` (~66-90): default the `stderr` sink to `os.Stderr` (keep the injectable var for tests) so the "be loud" unknown-platform warning is actually emitted.
- Modify: `cmd/drydock/client.go` (~46-51): make `socketPath` honor `broker.socket`/`broker.addr` from config.yaml (load config, fall back to env then default), matching brokerd (`cmd/brokerd/main.go:357-370`) — mirror the `auditDir()` precedent in `cmd/drydock/util.go`.
- Modify: `internal/broker/broker.go` (~429-484): add `slog.Warn(..., "err", err)` on the clone / `WriteTaskFiles` / `Mint` / audit-setup failure paths (keep the scrubbed client event via `safeErr`).
- Modify: `cmd/brokerd/main.go` (~224-230): call `cleanup()` before `die` on the `buildBackends` failure path, matching the adjacent `gateway.New` path.
- Modify: `internal/broker/broker.go` (~717-766): fix the misleading gate log string at ~762 — since taskCtx is rooted at `context.Background()`, `ctx.Done()` means kill/shutdown, not client disconnect; reword to "task killed or broker shutting down before approval; aborting".
- Modify: `internal/broker/broker.go` (~291-293): make the instruction-snippet truncation rune-safe (mirror `firstLine`/`prContent` rune truncation in the same file) so `/admin/tasks` JSON never contains split runes.
- Modify: `internal/webui/assets/style.css` (~61) or `internal/webui/assets/app.js` (~540): reconcile the `.submit-form .lab` selector with `label()` — give the span the `lab` class (JS side) so the intended muted caption style applies.
- Modify: `internal/gateway/gateway.go` (~165-169): in `stripRequestFields`, do not leave `r.Body` closed on the early-return path — only close/replace after deciding to rewrite, or restore a readable body on early return.
- Modify: `internal/config/apikeys.go` (~46-62): check `sc.Err()` after the scan in `LoadAPIKeys`; on scanner error return it rather than a silently-truncated key set.

**Interfaces:**
- Produces: no signature changes except `socketPath` may gain a config read (internal); `remote` keeps its injectable `stderr` var.

- [ ] **Step 1: Write/extend failing tests** where testable: wizard write-failure surfaces error (temp dir made read-only); `remote` unknown-platform warning writes to the injected sink; `client.socketPath` honors a config `broker.socket`; rune-safe snippet truncation on a multi-byte instruction; `LoadAPIKeys` propagates a scanner error. For log-string and slog-only changes, no test (verify by reading).
- [ ] **Step 2: Run new tests, confirm they fail** for the right reason.
- [ ] **Step 3: Implement each fix** as specified above.
- [ ] **Step 4: Verify** new tests pass; `go build ./... && go test ./... && go vet ./...` green; `gofmt -l .` clean.
- [ ] **Step 5: Commit** `fix: surface swallowed errors and correct misleading gate log (wizard write, remote warning, socket config, task snippet)`

### Task 3: Surface `opencode` consistently in CLI strings and seeded config

**Files:**
- Modify: `cmd/drydock/main.go` (~20, ~73): top-level `usage()` and `subHelp["auth"]` — derive the agent list from `provider.Agents()` instead of hardcoding `claude|codex`, OR update the literal to include `opencode` where auth is actually supported (see auth filter below).
- Modify: `internal/broker/broker.go` (~1040): the `unknown agent … (want claude|codex)` error — build the want-list from `provider.Agents()`.
- Modify: `internal/config/config.go` (~366) and `config/config.yaml` (~19): `default_agent` comment lists `claude | codex | opencode`.
- Modify: `cmd/drydock/auth.go` (~53-54): filter the `auth` usage string to agents whose provider actually has an auth backend (`OAuthBackend != nil` / has an auth implementation), so `opencode` is not advertised as an `auth` subcommand it can't perform (it hits the switch default and errors).

**Interfaces:**
- Consumes: `provider.Agents()`, `provider.ByAgent`, the provider registry's auth-capability field (inspect `internal/provider/provider.go` for the exact field name — likely `AuthModes` or `OAuthBackend`).
- Produces: no signature changes.

- [ ] **Step 1: Inspect** `internal/provider/provider.go` to find the exact registry field indicating auth capability and the `Agents()`/`ByAgent` signatures.
- [ ] **Step 2: Write failing tests**: a test asserting the broker's unknown-agent error names all registered agents; a test asserting `auth` usage lists only auth-capable agents (excludes `opencode`) while `submit --agent` help still lists all three.
- [ ] **Step 3: Run, confirm fail.**
- [ ] **Step 4: Implement** the registry-derived strings and the auth usage filter; update the two config comments.
- [ ] **Step 5: Verify** tests pass, suite green, gofmt clean.
- [ ] **Step 6: Commit** `fix: surface opencode consistently in CLI help/errors; stop advertising auth opencode`

---

## PR 2 — Security hardening

Branch: `security/egress-and-hardening` off updated main. Enforce egress ports (user-chosen), close the small hardening gaps, add the two high-value tests.

### Task 4: Enforce per-domain egress ports in squid

**Context:** Today squid matches hostnames only; the `ports:` column in `egress.yaml` is unenforced (`http_access allow default_dst` permits any port on an allowed host). Widening fragments (`controller.go:58-62`) share the gap. The compiled `.task/allowlist.txt` (with ports) is read by nothing. Goal: make squid enforce the configured ports for both default and widened domains, so config matches enforcement.

**Files:**
- Modify: `internal/netfw/netfw.go`: change `CompileSquidAllowlist(cfg)` to emit, per non-gateway default domain, a named `dstdomain` ACL + a `port` ACL built from that domain's `Ports`, plus the paired `http_access allow` rules (CONNECT-on-SSL_ports and plain), instead of a bare hostname list. Return the full ACL block as a string included by the conf.
- Modify: `internal/netfw/squid.go` (`CompileSquidConf`): replace the `acl default_dst dstdomain "%s"` + the two `http_access allow default_dst*` lines with an `include` of the generated default-ACL file (written alongside `squid-allow.txt`), so default domains are port-restricted. Keep the CONNECT/SSL_ports deny guard.
- Modify: `internal/netfw/controller.go` (`AddTask`, ~58-62): add a per-task `port` ACL from the widened domains' ports and require it in both the CONNECT and plain `http_access allow` rules for `d_%s`. Since `egress.Domain` carries per-host ports, thread the ports through `AddTask` (extend its signature from `domains []string` to `domains []egress.Domain`, or add a parallel ports param) — check the caller in `internal/broker` (`setupWidening`) and update it.
- Modify: `internal/netfw/squid.go` (`StartSquid`): write the generated default-ACL file into runDir.
- Modify tests: `internal/netfw/*_test.go`, `internal/egress/egress_test.go` as needed.
- Consider: remove `.task/allowlist.txt` generation (`internal/stage/stage.go:73`) or add a comment that it is informational-only. Prefer removing if nothing reads it; verify with grep first.

**Interfaces:**
- Consumes: `egress.Config`, `egress.Domain{Host, Ports}`.
- Produces: `CompileSquidAllowlist(cfg egress.Config) string` now returns squid ACL directives (named `dstdomain`+`port` ACLs and their allow rules), not a plain dstdomain list. `SquidController.AddTask` gains port awareness (exact new signature to be decided by implementer; document it in the report for the broker caller).

- [ ] **Step 1: Grep** for readers of `.task/allowlist.txt` and of `CompileSquidAllowlist`'s output shape; note the `AddTask` caller in broker.
- [ ] **Step 2: Write failing tests**: (a) generated default ACL block for a domain with `ports:[443]` allows CONNECT:443 and denies plain HTTP on :25; (b) a domain configured with `ports:[443,8080]` allows both and denies :25; (c) a widened task fragment enforces its domain's ports (allow configured port, deny others); (d) golden-string test on the compiled conf/ACL for the default egress.yaml.
- [ ] **Step 3: Run, confirm fail.**
- [ ] **Step 4: Implement** the per-domain ACL generation in `CompileSquidAllowlist`, the conf `include`, and the `AddTask` port threading; update the broker caller.
- [ ] **Step 5: Remove or annotate** `.task/allowlist.txt` per Step 1 findings.
- [ ] **Step 6: Verify** unit suite green; if a `squidlive`/`squide2e` tag exists locally and squid is installed, note whether it was run (it is not in CI). `go build ./... && go test ./...` green.
- [ ] **Step 7: Commit** `feat(egress): enforce per-domain ports in squid for default and widened allowlists`

### Task 5: Close small hardening gaps

**Files:**
- Modify: `internal/webui/server.go` (~113-118): replace the plain `!=` bearer-token comparison with `crypto/subtle.ConstantTimeCompare` (guard the empty-token/no-auth case as today), matching the gateway's deliberate constant-time compare.
- Modify: `internal/audit/audit.go` and/or `internal/webui/audit_routes.go` (~76-78): make `handleHistory`'s file reads use the same `O_NOFOLLOW`-guarded open as `serveAuditFile` — either add `O_NOFOLLOW` variants of `audit.LastResult`/`audit.ReadMeta` that accept a pre-opened `*os.File` from `openAuditFile`, or add an `O_NOFOLLOW` open inside those functions. Keep the 0700 audit-root defense; this is defense-in-depth.
- Modify: `internal/egress/egress.go` (`ValidateHost`, ~40-53): reject IPv4/IPv6 literals — add `if net.ParseIP(host) != nil { return errors.New/fmt.Errorf("egress: IP address literals not allowed …") }` before the regex check. Add `net` import.
- Modify: `internal/netfw/squid.go` (~34): make `access_log` configurable/enabled — write a per-task-run access log into `runDir` (e.g. `access_log <runDir>/access.log squid`) rather than `none`, so proxied traffic is auditable. If a full access log is judged too noisy by default, at minimum add a config toggle; default to logging. Document the choice in the report.

**Interfaces:**
- Consumes: `crypto/subtle`, `net`, `syscall.O_NOFOLLOW`.
- Produces: `ValidateHost` now rejects IP literals (update any test/fixture that relied on an IP passing — check `egress.yaml` has none; it doesn't).

- [ ] **Step 1: Write failing tests**: `ValidateHost("10.0.0.1")` and `ValidateHost("::1")` return errors; a webui auth test still passes with a correct token and rejects a wrong one (constant-time path); a `handleHistory` test with a symlinked audit file does not follow it (mirror `TestSymlinkRejected`).
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** the four changes.
- [ ] **Step 4: Verify** tests pass; suite green; gofmt clean.
- [ ] **Step 5: Commit** `harden: constant-time UI token compare, O_NOFOLLOW history reads, reject IP literals, squid access log`

### Task 6: Add the two high-value security tests

**Files:**
- Create/modify: `internal/broker/` test file (e.g. `widening_test.go` or extend `redteam_test.go`): a test that submits a task with `auto_approve: true` AND `egress_extra` and asserts it still stops at the human egress-widening gate (does not auto-widen). This pins `broker.go:401-424`.
- Modify: `internal/broker/handle_task_test.go` (or the fake grant/provider): give the fake provider a sentinel "real key" and assert the env assembled at `broker.go:496-508` and handed to the container never contains it (only the `tok_…` bearer + base-URL vars).

**Interfaces:**
- Consumes: existing broker test seams (`fakeGrant`, `prepareStage`, `runAgent`, `memStore`).
- Produces: no production change — tests only. If a test reveals a real leak/bypass, fix the production code and note it prominently in the report.

- [ ] **Step 1: Write the auto_approve×widening test**; run it. If it fails (i.e. auto_approve bypasses the gate), that is a real vuln — fix `broker.go` and re-run.
- [ ] **Step 2: Write the real-key-leak env assertion**; run it. If the real key appears in the container env, fix and re-run.
- [ ] **Step 3: Verify** both pass and the full `internal/broker` suite is green.
- [ ] **Step 4: Commit** `test(broker): pin auto_approve cannot bypass widening gate; assert real key never enters VM env`

---

## PR 3 — Docs, CHANGELOG, ROADMAP, and v0.4.0 release

Branch: `docs/reconsolidate-and-release` off updated main.

### Task 7: Reconcile docs and cut v0.4.0

**Files:**
- Modify: `CHANGELOG.md`: fill the `Unreleased` section, then promote it to a `## v0.4.0 — 2026-07-02` heading covering the web UI (`drydock ui`), the opencode/openai-compat "bring your own model" lane, and crash-recovery reconciliation, plus this sweep's user-visible fixes (egress port enforcement, opencode surfacing).
- Modify: `docs/ROADMAP.md` (~128-180): mark Phase 3A (provider registry) and 3C (generic OpenAI-compatible agent) as landed; re-scope Phase 3 around the remaining 3B (native Gemini vendor). Keep 4.1 crash-recovery as landed.
- Modify: `README.md` (~106-113): add the Models / "Bring your own model" page to the documentation list.
- Modify: `internal/config/config.go` SeedTemplate (~365-376) and `config/config.yaml` (~14, ~18, ~25-29): add `opencode` to the `default_agent` comment (if not already done in Task 3 — coordinate; this task only touches what Task 3 didn't), reword the claude-specific `default_model` comment to note all agents honor it, add the "ignored in subscription mode" caveat to `task_budget_usd`, and add a commented `prices:` example under the openai_compat block.

**Interfaces:** docs/config only; no code behavior change. (If Task 3 already edited a given comment, do not double-edit — reconcile.)

- [ ] **Step 1: Draft** the CHANGELOG v0.4.0 entry from `git log v0.3.0..HEAD --oneline` grouped by feature; draft the ROADMAP edits and config-comment edits.
- [ ] **Step 2: Apply** all doc/config edits.
- [ ] **Step 3: Verify docs build** — run the docs-build tool (`go run ./cmd/docs-build` or the documented command) and confirm no error and the sidebar order still resolves; `go test ./...` green (config template tests may assert on SeedTemplate — update expected strings if so).
- [ ] **Step 4: Commit** `docs: changelog v0.4.0, mark ROADMAP phase 3 landed, add models doc link, fix config comments`
- [ ] **Step 5 (controller, post-merge only):** after this PR merges to main and CI is green, tag `v0.4.0` and push the tag to trigger `release.yml`. (Not an implementer step — the controller performs this after merge.)

---

## PR 4 — Duplication collapse

Branch: `refactor/dedup` off updated main. Pure refactors; behavior identical, tests stay green (extend where a new shared helper deserves direct coverage).

### Task 8: Extract `awaitGate` from the two gate clones

**Files:**
- Modify: `internal/broker/broker.go` (~650-697 `gatePush`, ~721-766 `gateEgressWiden`): extract the shared timeout-wrap / pending-channel / persist / notify / select into `awaitGate(ctx context.Context, taskID string, persist func()) bool` (or similar), and reimplement both gates on top of it. Preserve the exact log messages (as corrected in Task 2) and the auto-approve short-circuit semantics.

**Interfaces:**
- Produces: `awaitGate` internal helper. Both gates keep their external behavior and signatures.

- [ ] **Step 1: Confirm** the existing gate tests cover approve, deny, and kill/shutdown paths for both gates; if a path is uncovered, add a test first.
- [ ] **Step 2: Extract** `awaitGate`; reimplement both gates.
- [ ] **Step 3: Verify** the full `internal/broker` suite green, no behavior change.
- [ ] **Step 4: Commit** `refactor(broker): extract awaitGate shared by push and widening gates`

### Task 9: Unify audit result-line parsing on `internal/audit`

**Files:**
- Modify: `internal/broker/stream.go` (~85-104 `auditCost`, ~45-72 `reasonFromAudit`) and `internal/broker/reconcile.go` (~66-95 `hasResultLine`): delegate to `internal/audit` (`LastResult`/`Outcome`/`Cost` and friends) instead of re-implementing the JSONL tail-scan. `audit` is a leaf package; import it (no cycle). If `audit` lacks a needed accessor (e.g. a reason/`hasResult` helper), add it to `internal/audit` with a test, then use it.

**Interfaces:**
- Consumes: `internal/audit` public API; extend it if needed.
- Produces: broker no longer defines its own JSONL parsers; the audit package is the single source of truth (matches its own doc comment).

- [ ] **Step 1: Map** each broker parser to an `audit` equivalent; add any missing `audit` accessor (test-first).
- [ ] **Step 2: Replace** the three broker implementations with `audit` calls; delete the now-dead broker helpers.
- [ ] **Step 3: Verify** `internal/broker` and `internal/audit` suites green; behavior identical (cost/reason strings unchanged — the existing change-detector tests guard this).
- [ ] **Step 4: Commit** `refactor(broker): parse audit result lines via internal/audit (single source of truth)`

### Task 10: Shared unix-socket broker client + merge duplicate domain struct

**Files:**
- Create: `internal/brokerclient/` (or a small helper in an existing shared spot) exposing a constructor that builds the `*http.Client` with the `BROKER_ADDR`-vs-unix-socket dial logic and a caller-supplied timeout; hoist one reusable client where a per-request transport is currently built.
- Modify: `cmd/drydock/client.go` (~53-67), `cmd/drydock/submit.go` (~205-220), `internal/webui/server.go` (~54-62, and fold the per-request `http.Transport` in `Handler`/proxy into one client stored on `Server`), `cmd/drydock/ui.go` (~53): use the shared helper.
- Modify: `cmd/drydock/client.go` (~27-30) and `cmd/drydock/submit.go` (~36-39): merge the byte-identical `domain` and `reqDomain` structs into one type in `package main`.

**Interfaces:**
- Produces: a shared socket-client constructor (document its exact signature in the report). The webui `Server` holds one `*http.Client` instead of building a transport per proxied request.

- [ ] **Step 1: Write a test** for the shared client constructor (unix-socket dial vs `BROKER_ADDR` branch selection).
- [ ] **Step 2: Implement** the helper; migrate all four call sites; store one client on webui `Server`.
- [ ] **Step 3: Merge** `domain`/`reqDomain`.
- [ ] **Step 4: Verify** suite green; the webui no longer allocates a transport per poll (spot-check by reading `Handler`).
- [ ] **Step 5: Commit** `refactor: shared unix-socket broker client; dedup domain struct; reuse webui transport`

### Task 11: Registry-drive auth + unify share-dir probing

**Files:**
- Modify: `cmd/drydock/auth.go` (~88-238): collapse `runAuthClaude`/`runAuthCodex` structural twins and `printValidity` twins into registry-driven helpers keyed off `provider.Registry` (as `doctor.go` already does); replace the 6× `"claude-oauth.json"`/`"codex-oauth.json"` magic strings (and the copies in `internal/provider/provider.go:37,51`) with a single source — prefer reading the filename from the provider registry entry.
- Modify: `cmd/drydock/init.go` (~142-160 `findShareFile`, ~469-496 `findImageDir`) and `cmd/brokerd/main.go` (~446-471 `findEgressConfig`): extract one `Locate(rel string)` helper (shared candidate list: binary-relative `share/drydock`, `$HOMEBREW_PREFIX`, repo-relative) and use it in all three.
- Modify: `cmd/drydock/init.go` (~163-180 `defaultEgressYAML`): replace the hand-synced copy of `config/egress.yaml` with `//go:embed` of the real file, removing the "Must stay in sync" tripwire.

**Interfaces:**
- Consumes: `provider.Registry` entries (add an OAuth-filename field if not present, test-first).
- Produces: `Locate(rel string) (string, error)` shared path helper; embedded egress.yaml default.

- [ ] **Step 1: Write tests**: `Locate` finds a file via each candidate strategy; the embedded default egress.yaml parses and equals the committed `config/egress.yaml`; auth resolves the correct oauth filename per agent from the registry.
- [ ] **Step 2: Implement** the registry-driven auth, `Locate`, and the `//go:embed`.
- [ ] **Step 3: Verify** suite green; `drydock auth`/`doctor` still resolve the same paths.
- [ ] **Step 4: Commit** `refactor: registry-drive auth filenames; unify share-dir probing; embed default egress.yaml`

---

## PR 5 — Structural refactors + style polish

Branch: `refactor/structural` off updated main. Mechanical splits and generalizations; behavior preserved.

### Task 12: Split `broker.go` and decompose `HandleTask`

**Files:**
- Create within `internal/broker/`: `taskstate.go` (TaskStage/TaskState, register/setStage/unregister, slot mgmt ~167-347), `gates.go` (the gate primitives + `awaitGate` from Task 8 + `notifyMac`), `admin.go` (`HandleApprove`/`Deny`/`Kill`/`Tasks`/`Health`/`Pending` + `writeJSON` ~768-892), `text.go` (`firstLine`/`prContent`/`safeErr`/`safeStr`/`errorEvent` ~894-989), `model.go` (`taskModelFor`/`effectiveDefaultModel`/`modelEnv` ~991-1047). `broker.go` keeps `Task`/`Broker`/seams/`HandleTask`.
- Modify: `internal/broker/broker.go` `HandleTask` (~355-643): extract `runEgressGate`, `buildTaskEnv` (pure, unit-testable — takes task+grant, returns the env slice), `runSandbox`, `pushAndOpenPR`. Also change `resolveAgent` (~1030-1047) to return `error` instead of the vestigial HTTP status the caller checks `!= 0`.

**Interfaces:**
- Produces: `buildTaskEnv(...)` becomes independently unit-testable (add a direct test, incl. the real-key-absence assertion moved/duplicated from Task 6 if natural). `resolveAgent(...) (…, error)`.

- [ ] **Step 1:** Move code into the new files (pure relocation, no logic change); build green.
- [ ] **Step 2:** Extract the four `HandleTask` helpers + `buildTaskEnv`; change `resolveAgent` to return `error`; update the caller.
- [ ] **Step 3:** Add a direct `buildTaskEnv` unit test (env contents incl. no real key).
- [ ] **Step 4: Verify** `internal/broker` suite green, no behavior change.
- [ ] **Step 5: Commit** `refactor(broker): split broker.go by responsibility; decompose HandleTask; resolveAgent returns error`

### Task 13: Split OAuth credential storage out of `gateway`

**Files:**
- Create: a new package (e.g. `internal/gwcreds` or `internal/oauthstore`) holding `FileCredStore`, `CredSnapshot`, `OAuthCred`/`OAuthCredCodex` and their refresh logic (currently `internal/gateway/oauth.go`, `codex_oauth.go`).
- Modify: `internal/gateway/*` to consume the new package; `cmd/drydock/auth.go` (~84,196) to import the credential-store package directly instead of the proxy package.
- Move the corresponding tests.

**Interfaces:**
- Produces: the new credential-store package's exported API (document exact names in the report). `gateway` keeps only the reverse-proxy/metering responsibility.

- [ ] **Step 1: Move** the OAuth store/refresh types and tests into the new package; fix imports across `gateway`, `cmd/drydock`, `cmd/brokerd`.
- [ ] **Step 2: Verify** the whole suite green; `cmd/drydock/auth.go` no longer imports `internal/gateway` (confirm with grep) unless it still needs the proxy for another reason.
- [ ] **Step 3: Commit** `refactor: extract OAuth credential store into its own package (gateway keeps proxy only)`

### Task 14: Exec seam in cmd + brokerd boundary coverage

**Files:**
- Modify: `cmd/brokerd/main.go`: introduce a package-level `var runCmd = func(name string, args ...string) ([]byte, error)` seam and route `checkContainerVersion`, `pruneOrphanTasks`, `startAnchor` through it; likewise `cmd/drydock` for `runKill`/`runDoctor` exec calls. Let `run*` return errors; confine `os.Exit` to `main()`.
- Add tests: `cmd/brokerd/main_test.go` covering `listen`'s socket-permission logic (0600 socket, 0700 parent, umask) and `pruneOrphanTasks` ordering via the injected `runCmd`; `internal/egress/egress_test.go` covering `Load`'s fail-closed rejection of a malformed/wildcard host; one `internal/gateway` `ServeHTTP` test through `AnthropicOAuthVendor` exercising `stripRequestFields` body rewrite + a real OAuth constructor.

**Interfaces:**
- Produces: `runCmd` seam in `cmd/brokerd` (and `cmd/drydock` where applicable). No external behavior change.

- [ ] **Step 1: Write the failing tests** (listen perms, egress fail-closed, gateway strip-fields via vendor, prune ordering via injected runCmd).
- [ ] **Step 2: Introduce** the `runCmd` seam; make `run*` return errors.
- [ ] **Step 3: Verify** new tests pass, coverage rises on the named boundaries, full suite green with `-race`.
- [ ] **Step 4: Commit** `refactor+test(cmd): exec seam; cover listen socket perms, egress fail-closed, gateway strip-fields`

### Task 15: Small generalizations + style polish

**Files (generalizations):**
- Modify: fold `internal/agent` (17-line `Vendor` shim, `agent.go:9-17`) into `internal/provider` as `provider.VendorForAgent(...)`; update callers; delete the `agent` package.
- Modify: express the `"openai-compat"` special-casing as fields on `provider.Provider` (e.g. `NeedsModel`, `ConfigBuilt`, `NoOperatorDefault`) and branch on those instead of the literal in `internal/broker/broker.go` (~996,1007), `cmd/brokerd/backends.go` (~71), `internal/config/config.go` (~329-341). Leave `entrypoint.sh`'s dispatch as-is (shell).
- Modify: derive `internal/netfw/netfw.go` `gatewayHosts` (~14-17) from the provider registry / vendor BaseURLs instead of a hand-maintained map, so a new gateway-fronted upstream is excluded from the squid allowlist automatically.

**Files (style polish — batch, all mechanical):**
- `errors.New` for verb-less `fmt.Errorf` (~11 sites: `internal/config/config.go:303-331`, `internal/netfw/squid.go:87`, `internal/egress/egress.go:42,58`, `internal/gateway/oauth.go:139`, `cmd/brokerd/backends.go:104`).
- Rename `cmd/brokerd`'s `die` (slog) to `fatal` to avoid the name clash with `cmd/drydock`'s printf `die`; align the ad-hoc `"auth:"`/`"drydock start:"` prefixes.
- `HandleTasks` insertion sort (~801-806) → `slices.SortFunc`.
- Move misplaced helpers to `util.go`: `tty` (init.go:20), `stdinIsTTY` (setup.go:130), `humanBytes` (prune.go:133).
- Rename `gateway.check` (mutates `l.Requests++`) → `admit` (~110-131).
- `internal/config/apikeys.go` `knownAPIKeys` (~11): derive from `provider.Registry` instead of hardcoding.
- `DefaultPath`/`EgressPath` (~134-158): use the `Dir()` pattern like newer siblings instead of re-resolving home.
- `cmd/docs-build/main.go` (~78-85): fix the O(n²) title re-lookup.
- `cmd/drydock/ui.go`: add a comment documenting the deliberate loopback-only posture (`--open` macOS-only, ignores `BROKER_ADDR`) OR wire `xdg-open` + `BROKER_ADDR` if the posture is not intentional — default to documenting.

**Interfaces:**
- Produces: `provider.VendorForAgent`; new `provider.Provider` behavior fields; `provider`-derived gateway-host set. The `agent` package is removed.

- [ ] **Step 1: Write/extend tests**: `provider.VendorForAgent("")` defaults to claude; the openai-compat behavior fields drive the same branches the literal did (add table tests); `gatewayHosts`-equivalent derivation excludes exactly the gateway-fronted vendors.
- [ ] **Step 2: Implement** the three generalizations; update all callers; delete `internal/agent`.
- [ ] **Step 3: Apply** the style-polish batch.
- [ ] **Step 4: Verify** `go build ./... && go vet ./... && go test ./...` green with `-race`; `gofmt -l .` clean; `grep -rn '"openai-compat"'` shows only the registry definition and shell, not scattered branches.
- [ ] **Step 5: Commit** `refactor: fold agent into provider; express provider quirks as fields; derive gateway hosts; style polish`

---

## Execution notes for the controller

- Between PRs: create branch off updated `main`, run tasks, open PR, wait for CI green, squash-merge, delete branch, `git checkout main && git pull`.
- The `access_log` (Task 5) and squid-port (Task 4) changes are not exercised by CI (integration tags are macOS-gated) — rely on the unit/golden tests and read the generated conf carefully in review.
- After PR 3 merges and CI is green: tag and push `v0.4.0`.
- Minor findings not individually tasked (P7 leftovers like `shorten` URL mangling, keyboard-hint string dedup, `LoadAPIKeys` scanner already in Task 2) are swept in Task 15's style batch or noted for the final whole-branch review to triage.

## Self-review notes

- Every P0–P5 finding from the analysis maps to a task: P0.1→T4, P0.2→T6, P0.3→T5, P1→T3+T7, P2→T2, P3→T1, P4→T8-T11, P5→T12-T15, P7→T15. Product-journey observations are intentionally NOT tasked (they're design questions, not cleanups).
- Type/signature consistency: `awaitGate` (T8) is consumed by the split in T12; `AddTask` port-signature change (T4) must be reflected in its broker caller in the same task; `resolveAgent` error-return (T12) updates its sole caller; `provider.VendorForAgent` (T15) replaces `agent.Vendor` at all call sites.
- Ordering dependency: T8 (awaitGate) precedes T12 (which relocates it). T3 and T7 both touch config comments — T7 defers to whatever T3 already changed.
