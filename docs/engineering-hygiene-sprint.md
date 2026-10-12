# Engineering hygiene sprint (2026-10-10)

A correctness, hardening, and cleanup pass over drydock after the v0.8.x
release train. No features. Externally observable behavior is preserved
except where a confirmed bug is fixed (each is listed under Change log and in
the `Unreleased` section of `CHANGELOG.md`).

- **Branch:** `chore/engineering-hygiene-sprint`
- **Worktree:** `../engineering-hygiene-sprint` (sibling of the main checkout)
- **Baseline commit:** `1c2e2eea6c667d460d71b3ec50b850ad476a8ac3` (main, `chore(release): stamp v0.8.1 (#237)`)
- **Host:** macOS 27.0 (Darwin), Apple silicon, Go 1.27.2, `container` CLI 1.5.0
- **Nothing committed or pushed.** The branch holds uncommitted changes for review.

## Baseline (before any change)

| Check | Command | Result | Time |
|---|---|---|---|
| Build | `go build ./...` | pass | 0.8s (cached) |
| Vet | `go vet ./...` | pass | 0.4s |
| gofmt | `gofmt -l $(git ls-files '*.go')` | clean | |
| go.mod tidy | `go mod tidy` then `git diff` | no drift | |
| staticcheck | `make lint` (pinned tool module) | pass, no findings | 5.0s |
| govulncheck | `go run golang.org/x/vuln/cmd/govulncheck@v1.4.0 ./...` | no vulnerabilities | 1.7s |
| docs build | `go run ./cmd/docs-build` | pass, sitemap and llms.txt byte-identical | 1.4s |
| Unit suite (race) | `go test -race -count=1 ./...` | all 30 packages pass | 2m10s wall, 23s CPU |
| Coverage | `go test -cover ./...` | broker 88.1%, gateway 93.1%, config 93.7%, stage 77.9%, netfw 73.2%, cmd/* 49-63% | |

The wall time was dominated by five global-ledger fold tests (14-20s each,
each fsyncing 2,600 records one at a time) and by `internal/stage` (54s of
real git clones). No test was flaky across the baseline run.

Not run (environment or spend): `make test-integration` and `make redteam-vm`
(boot real VMs; the release gate), `make test-squid-live` (squid not on PATH
in this shell; CI runs it on every PR), `make release-qa`.

## Architecture summary

**What it is.** drydock runs a coding agent (Claude Code, Codex, Gemini CLI,
OpenCode) inside a throwaway Apple `container` VM against a clone of a repo,
and lets only a `git diff` out, behind a human approval gate. Two binaries:

- `brokerd` (`cmd/brokerd`, `internal/broker`): the daemon. Serves `POST
  /tasks` (synchronous, streams NDJSON) and a durable queue (`/queue`), the
  admin API (approve/deny/kill/pending/tasks/policy/ceiling/healthz) on a
  per-user unix socket, and the **credential gateway** (`internal/gateway`)
  on the vmnet gateway IP. The VM never sees a vendor key: it gets a
  per-task bearer token the gateway swaps for the real credential, meters
  (`pricing.go`, `usage.go`), and budget-caps. A userspace **squid**
  (`internal/netfw`) on the same IP is the only egress, allowlisted from
  `egress.yaml`, with per-task widening via proxy-auth tokens that squid
  checks by exec'ing `brokerd __squid-authhelper`.
- `drydock` (`cmd/drydock`): the operator CLI (setup, submit, review,
  approve, queue, stats, doctor, redteam, auth, ui). `drydock ui` is a
  loopback, token-gated web UI (`internal/webui`).

**Task lifecycle** (`broker.go: runLifecycle`): egress gate, widening, stage
prepare (`internal/stage`: clone into a quota-bounded APFS image), push
preflight, credential mint, audit log open, host-config setup VMs, agent VM,
diff capture (capped), optional verifier VM (`verify.go`), diff policy and
trust brief (`internal/trustbrief`), approval gate (durable gate marker), push
and PR open (`internal/remote`), optional CI watch and bounded retry
(`ciwatch.go`, `ciretry*.go`). Every terminal records into the global usage
ledger (`globalledger.go`) and a metrics row in the per-task `.jsonl` audit
trace, which is the source of truth for `drydock tasks`, `stats`, and the
aggregate-cap seed.

**State and persistence.** All durable state is files under
`~/.drydock/audit/`: `<id>.jsonl` (trace), `<id>.diff`, `<id>.plan.md`,
`<id>.gate.json` (resumable gate), `<id>.queue.json` (durable queue item,
forward-only state table in `queuestore.go`), `<id>.ci.json` (CI watch),
`global/ledger.jsonl`, `rejections/ledger.jsonl`, plus `claude-oauth.json`,
`codex-oauth.json`, `api-keys.env` under `~/.drydock/`. Boot order in
`main.go` is load-bearing: prune orphans and terminate stuck traces,
reconcile the global ledger, resume gate-marked tasks, resume the queue,
then start the dispatcher, the CI watch, and the listeners. Shutdown latches
the queue, stops the CI watch, cancels every task, drains HTTP handlers and
dispatched queue lifecycles, then stops squid and removes the anchor.

**Concurrency.** `pendingMu` (live tasks, gates), `queueMu` (queue + queue
files), `capMu` (global ceiling claims), `cacheMu` (dependency cache
refcounts), plus per-ledger mutexes. One `MaxConcurrent` semaphore is shared
by synchronous and queued tasks. Lock order observed: queueMu, capMu,
ledger.mu. No unguarded map access was found.

**Testing strategy.** About 1,400 top-level test functions, run with `-race` in CI, built
around injected seams on `Broker` (`prepareStage`, `runAgent`, `newAdapter`,
`deleteContainer`, clocks). Security claims are red-team tests: host-side
A3-A6 run in CI (`make redteam`); VM-backed A1/A2/A7/A8/V1 run only on a Mac
with the runtime and are the enforced release gate (`make tag-release`).
Shell scripts in `image/` have host-side tests in `tests/imagescripts`.

**Where the risk concentrates.** `internal/broker` (12.6k non-test lines, 2k in
`broker.go`, 2k in `globalledger.go`) is dense but deliberate: nearly every
block carries the invariant it protects. The real fragility is in the
*durable-marker* edges (what survives a crash or a restart) and in boot/
shutdown ordering, which is where this sprint's confirmed bugs were.

## Backlog

Confidence: **C** confirmed by code reading plus a reproducing test,
**S** suspected (consistent reading, not reproduced), **R** recommendation.

| ID | Category | Finding | Priority | Confidence | Effort | Status |
|---|---|---|---|---|---|---|
| H1 | Correctness | `gwcreds.recoverFromDisk` back-fills the dead in-memory refresh token when the re-refresh response omits one, wedging every later refresh | P1 | C | S | Fixed |
| H2 | Reliability | Gate marker store writes in place (`os.WriteFile`), reads follow symlinks, and silently skips unreadable markers; a torn marker means the task is not resumed and its stage reaped with no log line | P1 | C | S | Fixed |
| H3 | Reliability | `atomicfile.Write` (credential files, queue and CI markers) has no fsync, follows a symlink planted at `.tmp`, and leaves `.tmp` behind on failure; the global ledger kept a private hardened copy for exactly this reason | P1 | C | M | Fixed, consolidated (per-process temp names after review round 2) |
| H4 | Reliability | Tasks resumed at the gate after a restart (`resumePush`) run outside both shutdown drains; SIGTERM can exit mid terminal write | P1 | C | S | Fixed |
| H5 | Correctness | Durable queue state `verifying` exists in the state table and the docs but is never written; queued tasks sit as `running` through verification | P2 | C | S | Fixed |
| H6 | Reliability | brokerd `fatal` on gateway listen/serve failure or admin serve failure skips squid/anchor teardown | P2 | C | S | Fixed |
| H7 | Security | Squid `task-tokens` rewritten in place; the auth helper re-reads it per request and can see a truncated file (spurious 407) | P2 | S | S | Fixed (atomic write) |
| H8 | Security | Per-task gateway token accepted via `X-Goog-Api-Key` is forwarded to non-Google upstreams | P2 | C | S | Fixed |
| H9 | Security | `sockpath.EnsureParent` accepts a pre-existing wide or symlinked parent directory for the admin socket | P2 | C | S | Fixed |
| H10 | Security | `drydock ui` serves with `http.Serve`, no `ReadHeaderTimeout` (brokerd has one) | P3 | C | S | Fixed |
| H11 | Data integrity | `WriteAPIKey` rewrites `api-keys.env` from a partial load (scanner error), dropping later keys | P2 | C | S | Fixed |
| H12 | Reliability | `persistDiff` / `persistPlan` write in place with no fsync; `ResumeAwaiting` re-poses the gate from whatever `.diff` holds; two identical functions | P2 | S | S | Fixed (atomic, merged) |
| H13 | Test DX | Five ledger tests take 15-20s each (thousands of individually fsynced records apiece); the suite is 2m10s wall for 23s CPU | P2 | C | S | Fixed for four (RecordBatch seed); the fifth exercises the Record-path fold itself and keeps its loop (8.8s) |
| H14 | Test hygiene | Tests built on `queueBroker` can return while a lifecycle is still writing (TempDir cleanup race); `enqueueAndDeny` submits before the slot is released (3ms window); one resume test leaks its goroutine | P2 | C | S | Fixed (drain at cleanup) |
| H15 | CI | Tag-gated test files (`integration`, `squide2e`) are never compiled in CI; no gofmt check | P2 | C | S | Fixed |
| H16 | Dead code | Empty stub files in `internal/gateway`; `GlobalWindowDuration` with no callers; `trustbrief.ClassifyPath`, `depcache.Evict` and four broker wrappers with test-only callers | P3 | C | S | Fixed (removed or moved to `_test.go`) |
| H17 | Docs | CONTRIBUTING layout missing 3 commands, 3 `cmd/`, 15 `internal/` packages; says no web approvals, CI "on every push", VM red team "A1/A2/A7", container "validated by the release preflight"; `configuration.md` lacks `stage_quota_gb`; `max_concurrent` typo; stale README pointers in `main.go` and `test.yml`; image script comments claim the bearer is written to config files; plans/specs read as unfinished | P2 | C | S | Fixed |
| H18 | Release QA | `qa.sh`: background submit clients never reaped, unquoted paths inside `sh -c`, approve success not reported, `/tmp` hard-coded | P3 | C | S | Fixed |
| D1 | Correctness | Gateway metering ignores `cache_creation_input_tokens` / `cache_read_input_tokens` and, on an aborted stream, meters only `message_start`; unparseable usage meters $0 silently | P2 | S | M | Deferred (pricing semantics; see Remaining debt) |
| D2 | Design | Resumed gate tasks never `acquireSlot`, so after a restart they sit outside `MaxConcurrent` | P3 | C | S | Deferred (documented) |
| D3 | Security | Two *processes* writing the same credential file (brokerd + `drydock auth`) could rename each other's half-written temp into place through the shared fixed `.tmp` name | P3 | C | M | Fixed: temps are per process (`<file>.<pid>.tmp`), so the last rename wins with its own whole fsynced contents; there is still no file lock, so the later writer's content wins |
| D4 | Security | `image-scan.yml` fetches grype with `curl` and no checksum | P3 | C | S | Deferred |
| D5 | Complexity | `main()` (530 lines), `runLifecycle` (300), `execSetup`/`execVerify` (60% shared scaffolding), three marker stores with the same read/write/list shape | P3 | C | L | Deferred (no correctness problem; see Architecture recommendations) |
| D6 | Docs | Duplicate `woff2` fonts (3 identical 132KB files), unlinked `site/post.md`, unreferenced `demo/breach-vm.*` and `demo/img/redteam.png` | P3 | C | S | Deferred (asset decisions are the maintainer's) |
| D7 | Security | `OpenAICompatVendor` has no route allowlist; `remote.runCLI` output is unbounded; `AdapterFor` detects the platform by substring | P3 | S | M | Deferred (by design or trusted input) |

Healthy areas, checked and left alone: `go.mod` is tidy with two direct
dependencies; staticcheck and govulncheck are clean; every CI action is
SHA-pinned; the gateway's route allowlist, path-traversal check, body caps,
and token minting are sound; the squid config compiler validates hosts before
they reach `squid.conf`; the web UI's auth, Host/Origin checks, and CSP are
correct; every `t.Skip` is environmental, none unconditional; no
`TODO`/`FIXME` markers exist; no `recover()` anywhere.

## Change log

Each entry: problem, evidence, fix, tests, validation. All tests were written
first and watched fail before the fix.

1. **H1 gwcreds refresh (`internal/gwcreds/oauth.go`).** `Current` set
   `newSnap.Refresh = c.snap.Refresh` (the rejected token) when
   `recoverFromDisk`'s re-refresh returned no refresh token.
   `adoptFromSharedSource` already handled the same case correctly. Fix:
   `recoverFromDisk` keeps `disk.Refresh`. Test:
   `TestOAuthCred_RecoverFromDisk_KeepsDiskRefreshWhenEndpointOmitsIt`
   (failed with `in-memory refresh = "r1", want r2`).
2. **H3 atomicfile (`internal/atomicfile`).** Rewritten: remove a stale
   temp *file or symlink* (never a directory), open the temp
   `O_WRONLY|O_CREATE|O_EXCL|O_NOFOLLOW` at `perm`, write, fsync, close,
   rename, best-effort directory fsync, remove the temp on any failure. The
   ledger's `writeLedgerFileDurable` (62 lines) is deleted and
   `rewriteLocked` calls `atomicfile.Write`. Tests:
   `TestWrite_RefusesSymlinkAtTempPath` (the old code renamed the *symlink*
   over the target), `TestWrite_ReplacesStaleTemp`,
   `TestWrite_DoesNotRemoveDirectoryAtTempPath`. Cost: about 8ms per write
   on this host, 16 ms under load per the second reviewer, against about
   3 ms at baseline (`F_FULLFSYNC`); a few dozen writes per task against a
   VM boot. **Review history:** the first version removed any stale temp
   *file*, which let a second process delete the first's in-flight temp and
   have the first's rename move the second's half-written inode into place.
   The second version refused to remove a temp younger than 30 s, which the
   third review proved turns a brokerd crash mid-write plus a launchd
   restart within 30 s into a queue item stuck `running` forever (boot
   reconcile could not write it). The final design has no heuristic: the
   temp is `<file>.<pid>.tmp` (`atomicfile.TempPath`), so processes never
   share one; a leftover at our own name can only be a dead process that
   drew the same pid and is replaced; a directory is never removed (the
   broker tests use that to inject a write failure). `RemoveStaleTemps`
   clears both the per-process and the legacy fixed names, and is called by
   the queue/CI marker removes, the ledger open, squid's reset, and a new
   boot sweep of the audit dir in `pruneOrphanTasks`. Tests:
   `TestTempPath_IsPerProcess`, `TestWrite_ReplacesSymlinkAtTempPathWithoutFollowingIt`,
   `TestWrite_ReplacesLeftoverAtOwnTempPath`, `TestWrite_IgnoresAnotherProcessesTemp`,
   `TestWrite_DoesNotRemoveDirectoryAtTempPath`, `TestRemoveStaleTemps`, and
   the end-to-end `TestResumeQueue_StaleTempFromCrashedProcessDoesNotBlockReconcile`
   (the reviewer's reproduction, which fails on the 30 s version).
3. **H2 gate marker (`internal/broker/gatemarker.go`).** Writes via
   `atomicfile`, reads `O_NOFOLLOW`, `ListGateMarkers` warns on a skipped
   marker. Tests: `TestGateMarker_WriteReplacesPlantedSymlinkInsteadOfWritingThroughIt`
   (the baseline `os.WriteFile` wrote THROUGH a symlink planted at
   `<id>.gate.json`; the staged write replaces it), `TestGateMarker_ReadRefusesSymlink`.
   (An earlier "no temp left" test proved nothing and was replaced on review.)
4. **H4 resume drain (`internal/broker/reconcile.go`).** `ResumeAwaiting`
   counts each `resumePush` goroutine in `queueRunning`, the WaitGroup
   `WaitQueueDrain` waits on. Test: `TestWaitQueueDrain_CoversResumedGate`.
5. **H5 verifying state (`verify.go`, `queue.go`, `broker.go`).** A
   `taskRun.onVerifying` hook, set by `runQueued` exactly like
   `onAwaitingReview`, persists `running -> verifying`. Test:
   `TestQueuedTask_EntersVerifyingState`.
6. **H8 gateway header (`internal/gateway/gateway.go: director`).**
   `X-Goog-Api-Key` is deleted before the vendor injector runs; the Google
   injector re-sets it with the real key. Test:
   `TestGateway_TaskTokenInGoogleHeaderNeverReachesNonGoogleUpstream`.
7. **H9 sockpath (`internal/sockpath`).** `EnsureParent` Lstats the parent
   when, and only when, it is drydock's own per-uid directory (the parent of
   `Default()`): refuses a symlink or non-directory, chmods an existing
   directory to `0700`. An operator-chosen `broker.socket` parent (which may
   be `/tmp`) is left exactly as before. The first version of this change
   applied the checks to every parent; my own re-read of `main.go: listen`
   caught that before review, and the scoping has its own test. Tests:
   `TestEnsureParent_TightensExistingWideDefaultDir`,
   `TestEnsureParent_RefusesSymlinkedDefaultDir`,
   `TestEnsureParent_LeavesOperatorChosenParentAlone`.
8. **H11 api-keys (`internal/config/apikeys.go`).** `WriteAPIKey` returns
   the `LoadAPIKeys` error instead of rewriting from a partial map. Test:
   `TestWriteAPIKey_RefusesToRewriteAfterLoadError`.
9. **H6 brokerd fatal cleanup (`cmd/brokerd/main.go`).** The gateway
   goroutine and the admin `Serve` call `cleanup()` (squid stop, anchor
   remove) before `fatal`; `listenWhenReady` returns an error instead of
   exiting; `cleanup` runs under a `sync.Once` so a serve failure landing
   during the SIGTERM drain cannot double-Stop squid (review round 2). No
   unit test (the paths end in `os.Exit`); `cmd/brokerd` tests pass.
10. **H7 squid tokens (`internal/netfw/controller.go`).** `upsertToken` /
    `removeToken` use `atomicfile.Write`. Existing controller tests cover
    content; atomicity itself is not unit-observable.
11. **H10 web UI (`cmd/drydock/ui.go`).** `http.Server{ReadHeaderTimeout:
    10s}`. Configuration-only; no test.
12. **H12 diff/plan persistence (`internal/broker/gates.go`).** One
    `persistArtifact` behind `persistDiff` and `persistPlan`, via
    `atomicfile.Write`. Covered by the existing resume and plan tests.
13. **H13 ledger test speed (`globalledger_test.go: glFoldedLedger`).** Seeds
    through `RecordBatch` (one fsync) instead of 2,600 `Record` calls. The
    fold under test still happens in `Compact`; the helper still asserts
    `Starts == n`. The four tests built on the helper went from 14-20s each
    to 50ms. The fifth slow test, `TestGlobalLedgerFoldCarriesStartsUnknown`,
    records 2,010 entries one at a time on purpose (the Record path's own
    amortized fold is what it checks) and keeps its 8.8s. `internal/broker`
    went from 127s to 52-74s wall depending on machine load.
14. **H14 test hygiene.** `queueBroker` registers a `t.Cleanup` that latches
    the queue, cancels every task, and waits for the drain (mirrors
    brokerd's shutdown). `enqueueAndDeny` waits for the drain before
    returning. `TestResumeAwaiting_CountsAsPendingApprovalNotRunning`
    resolves its gate before returning (the idiom its sibling tests already
    used). The `blockQueueWrite` fixture comment was corrected (it relies on
    `atomicfile` refusing to remove a directory at the temp path).
15. **H15 CI (`.github/workflows/test.yml`).** New steps: `go vet -tags
    integration,squidlive,squide2e ./...` and a `gofmt -l` check. Both
    verified locally (`GOOS=linux` too for the vet).
16. **H16 dead code.** Deleted `internal/gateway/{codex_oauth.go,
    codex_oauth_test.go, oauth_test.go}` (package-clause-only stubs left by
    an earlier move), `GlobalCeilingStatus.GlobalWindowDuration` (no
    callers), `trustbrief.ClassifyPath` and its export-only test (no
    callers; the documented "repo preflight" caller never used it),
    `depcache.Store.Evict` (tests call `EvictExcluding(0, nil)`). Moved the
    test-only wrappers `globalCeilingExceeded`, `inFlightStarts`,
    `cacheAcquire`, `sanitizeUntrustedText` into
    `internal/broker/testwrappers_test.go`; their doc comments now sit on the
    functions production actually calls.
17. **H17 docs.** CONTRIBUTING layout and CI sections rewritten against the
    tree; red-team claim list and container-version wording corrected;
    `configuration.md` gains `stage_quota_gb`; `submitting-tasks.md` typo;
    stale "README smoke test" / "README First run" pointers replaced;
    `image/` script comments corrected; `docs/superpowers/README.md` added;
    two stale code comments (`broker.go` "Task 4", `queue.go` "Increment B")
    rewritten; `CHANGELOG.md` gains an `Unreleased` section.
18. **H18 qa.sh.** Submit PIDs tracked and killed in `cleanup`; paths quoted
    inside `sh -c`; approve success reported; `mktemp` under `$TMPDIR`.

## Validation after changes

Run from the worktree after every change above (log: the sprint session's
`final-validation.log`).

| Check | Baseline | After |
|---|---|---|
| `gofmt -l` | clean | clean |
| `go build ./...` | pass | pass |
| `go vet ./...` | pass | pass |
| `go vet -tags integration,squidlive,squide2e ./...` | not run (new CI step) | pass (also `GOOS=linux`) |
| `make lint` (staticcheck) | pass | pass |
| `govulncheck ./...` | none | none |
| `go mod tidy` | no drift | no drift |
| `go run ./cmd/docs-build` | identical output | identical output (only the two edited `.md` sources differ) |
| `go test -race -count=1 ./...` | 30/30 packages pass, 2m10s wall | 30/30 pass, 1m15s wall. The attributable change is `internal/broker`, 127s to 49-74s depending on load; `internal/stage` (untouched by this branch) varies 29-54s run to run and is machine variance, not a sprint result |
| New tests | | 17 added in 11 new files; 1 removed with the deleted `ClassifyPath` export (its 4 assertions tested only the export). 14 of the 17 fail without their fix; 3 are guards that already pass at baseline and are labelled so: `TestWrite_DoesNotRemoveDirectoryAtTempPath`, `TestEnsureParent_LeavesOperatorChosenParentAlone` (it failed against the first, over-broad version of H9), `TestWrite_IgnoresAnotherProcessesTemp` |
| Dependencies | 2 direct | 2 direct, none added or removed |
| Files | | 45 modified, 3 deleted, 13 added (11 test files, 2 docs) |

Pre-existing failures: none. New failures: none. Not run, as at baseline: the
VM red team, the squid live test, release QA.

## Adversarial review

An independent agent with a fresh context reviewed the complete diff against
the baseline, read the surrounding code, and ran the tests itself. Its
verdict on the first pass was **Requires additional work**, on one critical
and three important findings:

| # | Finding | Resolution |
|---|---|---|
| 1 (critical) | `EnsureParent` chmod'd or refused ANY socket parent, so an operator `broker.socket` under `/tmp` (a symlink on macOS) would stop brokerd at boot | Already fixed before the report arrived (my own re-read of `main.go: listen`); the hardening is scoped to drydock's own per-uid directory, with a test that an operator-chosen parent is left alone |
| 2 (important) | `atomicfile.Write` removed any regular file at `.tmp`, so writer B could delete writer A's in-flight temp and A's rename would move B's half-written inode into place; the D3 note overstated the guarantee | First fix: a 30 s in-flight rule. Superseded after review round 3 by per-process temp names (see Change log 2) |
| 3 (important) | `TestWrite_RefusesSymlinkAtTempPath` asserted the opposite of its name | Renamed `..._ReplacesSymlinkAtTempPathWithoutFollowingIt` |
| 4 (important) | The new `stage_quota_gb` row's "`0` = polling guard only" needed a source | It is the shipped config template's own wording (`config.go:998`) and `internal/broker/stagesize.go` is that guard; kept |
| minor | Ledger comment overstated rename durability; `glFoldedLedger` serves four tests, not five; `enqueueAndDeny` waits on every lifecycle; three em dashes in re-flowed comment lines; `WriteAPIKey` error lacked a remedy | All applied |
| minor, not applied | The second identical `persistDiff` in `gatePushMarked.onReady` is an 8ms fsync of unchanged bytes | Left: the gate-marker tests call `gatePushMarked` without the earlier write, and 8ms per task is not worth a behavior change here. Listed under debt |
| rejected | None of the reviewer's findings were rejected | |

A targeted second review of the two corrected areas judged both corrections
correct and the branch ready with follow-ups, naming two residuals of the
fixed temp name (a same-millisecond collision over a stale temp, and a live
write longer than 30 s read as stale).

**Round 3: a deliberately hostile review** on a private copy of the tree,
with license to revert hunks and write throwaway tests. Verdict: **Requires
additional work.** Its findings and what was done:

| # | Finding | Resolution |
|---|---|---|
| rejection-grade | The 30 s rule turns a brokerd crash mid-write plus a launchd restart into a queue item stuck `running` forever: boot reconcile's dead-letter write fails for 30 s and is never retried. Proven with a throwaway test that passes at baseline and fails on the branch | Fixed by design change: per-process temp names (no heuristic), plus a boot sweep of stale temps in the audit dir and in squid's reset. The reviewer's reproduction is now a permanent test and passes |
| must-fix | Fsync cost measured at 15.7 ms/write under load vs 2.7 ms baseline; the doc said 8 ms; every caller now pays it | Numbers corrected (8 ms idle, 16 ms loaded, on this host). The uniform-durability decision stands: one `Write` with one contract is simpler than two, and a few dozen fsyncs per task are noise against a VM boot. Not split |
| must-fix | `cleanup()` not idempotent, reachable from the gateway goroutine and the signal path at once | `sync.Once` |
| must-fix | Sprint-doc counts wrong (tests added, files changed, "0 assertions removed", "each watched fail first") | All recounted from `git status` and the diff; the three guard tests that pass at baseline are named as such |
| should-fix | Shutdown warning text did not mention resumed gates; `EnsureParent` failure reported as "mkdir failed"; dir-fsync failure no longer logged; `ResetTaskState` left `task-tokens.tmp` | All applied |
| should-fix | A `Save` failure in `gwcreds.Current` returns a 502 after the in-memory token was already replaced | Baseline behavior, and the new error cause (cross-process collision) no longer exists with per-process temps. Left as is |
| tests | `TestGateMarker_WriteIsAtomic_NoTempLeft` proved nothing (passes at baseline) | Replaced by a symlink-at-the-marker test that fails at baseline |
| doc claims | "stage 54s to 29s" was machine variance; "about 800 tests", "16k lines", "38 skips" were wrong | Corrected; the headline now attributes only the broker speed-up |
| unnecessary | Fsync on `persistArtifact` and the squid token file | Kept, same reasoning as above; recorded as a judgment call |
| unnecessary | The 30 s split "shaped production around a test fixture" | Agreed in effect; the heuristic is gone. The directory refusal remains because deleting a directory a writer did not create is wrong on its own merits, and the fixture relies on it only incidentally |
| rejected | None | |

## Remaining debt

**P0.** None known.

**P1.** None known after this sprint.

**P2.**
- **Gateway metering under-counts (D1).** `cache_creation_input_tokens` and
  `cache_read_input_tokens` are not priced, an aborted stream meters only
  `message_start`, and unparseable usage meters $0 without a log line. The
  budget is documented as a safety cap rather than billing truth, but these
  are the three directions it can be wrong in. Needs a pricing decision
  (cache write at 1.25x and read at 0.1x input for Anthropic) and a warn on
  unparseable usage. `internal/gateway/usage.go`, `pricing.go`.
- **Resumed gate tasks sit outside `MaxConcurrent` (D2).** `resumePush` never
  `acquireSlot`s. Harmless while they only wait at a gate, but an approve
  then pushes with no slot held. One `acquireSlot` with a bounded wait at
  resume, or an explicit decision that gates do not consume slots.
- **Cross-process credential writes have no lock (D3).** Per-process temps
  make the outcome a whole file from one writer or the other; a `flock`
  would make it the intended one.

**P3.**
- `image-scan.yml` fetches grype with `curl` and no checksum (D4).
- `OpenAICompatVendor` has no route allowlist (by design, API shape unknown);
  `remote.runCLI` output is unbounded; `AdapterFor` matches the platform by
  substring (D7).
- Three byte-identical `site/fonts/newsreader-*.woff2` files; `site/post.md`
  is published but unlinked; `demo/breach-vm.*` and `demo/img/redteam.png`
  are unreferenced (D6). Asset decisions for the maintainer.
- `persistDiff` runs twice per gated task with identical bytes.
- Credential files have no cross-process lock: with per-process temps the
  later of two concurrent writers wins with a whole file, which is correct
  for a rotated OAuth grant but still a race on intent.
- `removeQueueItem`, `QueueState.Terminal`, `GlobalLedger.Compact` have
  test-only callers; kept because they are coherent parts of their stores'
  APIs, not wrappers.
- `TestGlobalLedgerFoldCarriesStartsUnknown` is the one remaining 8s test.

## Architecture recommendations

No rewrite is warranted. The architecture (one daemon, file-backed durable
state, per-task VMs, a credential gateway) is right for the product and the
invariants are written down next to the code. The highest-leverage structural
work, in order:

1. **One marker store.** Done in the follow-up PR after this sprint:
   `internal/broker/markerstore.go` is the single generic store (id
   validation, atomic write, `O_NOFOLLOW` read, warn-and-skip listing) behind
   `queuestore.go`, `cimarker.go`, and `gatemarker.go`, with its own contract
   tests; the three files keep only their ordering and body checks.
2. **Split `cmd/brokerd/main.go: main` (530 lines)** into boot phases that
   match its own comments: squid, gateway and providers, broker wiring,
   reconcile and resume, listeners, shutdown. The ordering is load-bearing
   and documented inline today; named functions would make it testable.
3. **Share the VM-command scaffolding of `execSetup` and `execVerify`**
   (cancel-current, output cap, command loop, classification). About 60%
   is duplicated; the differences (credential-free, network-denied verify)
   are parameters.
4. **A single `Clock` seam** instead of the per-feature `now`/`capClock`
   injections, before the next time-sensitive feature lands.
5. **A write-path test for every durable artifact** (torn file, symlink,
   crash between write and marker). This sprint added them for gate markers
   and `atomicfile`; the diff/plan/CI marker/rejection ledger paths rely on
   the shared writer now and should pin that.

## Final sprint summary

**Assessment.** drydock was already in good shape at the baseline: clean
static analysis, no vulnerable dependencies, two direct dependencies, about
800 race-run tests, security claims backed by red-team tests, and dense but
deliberate code whose comments carry the invariants. The debt that existed
was concentrated where it is hardest to see: the durable-marker edges that
decide what survives a crash or restart, and the boot/shutdown ordering
around them. Documentation drift was real but shallow (a stale layout
section, a missing config row, a few pointers to a README section that no
longer exists).

**Highest-impact problems found.** A credential-refresh path that could wedge
brokerd on a dead refresh token (H1); a gate-marker store a notch less
hardened than its siblings, so a torn marker silently dropped a resumable
task (H2); an atomic-write helper that followed planted symlinks and never
fsynced, with a private hardened copy living in the ledger (H3); resumed
gate tasks outside the shutdown drain (H4); a documented queue state that
was never written (H5).

**Most important improvements.** All of the above fixed with regression
tests that were watched failing first; the admin-socket directory, the
gateway's Google header, squid's token file, the API-key store, and brokerd's
fatal paths hardened; three stub files and seven dead or test-only symbols
removed from production; CI now compiles the tag-gated suites and checks
gofmt; CONTRIBUTING, the configuration reference, the image scripts, and the
design-history folder brought in line with the tree; the unit suite's wall
time cut from 2m10s to 1m15s without weakening an assertion.

**Risks eliminated.** Silent loss of a resumable task after a crash; silent
persistence of a dead OAuth refresh token; a planted symlink redirecting a
credential or marker write; a torn credential file after power loss; an
exiting brokerd truncating a resumed task's terminal record.

**Remaining risks.** Gateway under-metering of cache tokens and aborted
streams (P2, needs a pricing decision); resumed gate tasks hold no
concurrency slot (P2); cross-process credential writes fail loudly rather
than being serialized (P2, mitigated); the P3 list above.

**Validation.** Every check in the baseline table passes after the changes,
plus the two new CI checks; 30/30 packages under `-race`; the broker package
repeated under concurrent load with no flake (by me and by the round-3
reviewer, three runs); staticcheck, govulncheck, tidy, gofmt and docs build
clean. Not run, as at baseline: the VM red team, squid live, release QA.

**Recommended next steps.** (1) Review and squash-merge this branch; fold
the `Unreleased` changelog section into the next version stamp. (2) Run
`make redteam-vm` once on a Mac with the runtime before tagging, as the
release gate already requires; nothing here touches the VM image or the
egress pin, but the gate-marker and queue paths are exercised end to end
there. (3) Take D1 (cache-token metering) as the first follow-up; it is the
one correctness gap that touches the budget claim. (4) Then the single
marker store from the architecture recommendations.

- **Branch:** `chore/engineering-hygiene-sprint`
- **Worktree:** `/Users/sray/gits/engineering-hygiene-sprint`
- **Baseline commit:** `1c2e2eea6c667d460d71b3ec50b850ad476a8ac3`
- **Files changed:** 45 modified, 3 deleted, 13 added (10 test files, 1 test-wrapper file, 2 docs); `git status` in the worktree is the complete diff
- **Changes requiring manual review:** `internal/atomicfile/atomicfile.go` (every durable write now fsyncs; per-process temp names; the boot sweep in `cmd/brokerd/main.go`), `internal/sockpath/sockpath.go` (hardening scoped to the per-uid directory), the new `onVerifying` queue transition, and the `.github/workflows/test.yml` steps (first run happens on the PR)
