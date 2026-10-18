# Testing, Coverage and Specification Validation (2026-10-13)

Branch `chore/qa-validation-sprint`, worktree `../drydock-qa-sprint`, baseline
`4c9cef4` (v0.8.2). This is the record of a validation initiative: map the
documented behaviour to tests, find where the tests would not notice a wrong
implementation, close those gaps, fix the defects that surfaced, and say
plainly what is still unproven. Nothing here adds a feature. Every number
below was measured in this sprint; where something could not run, it says so.

Companion: `docs/engineering-hygiene-sprint.md` (the previous pass, 2026-10-10
to 10-12) carries the architecture summary and the open debt list (D2, D3)
that this document does not repeat.

## 1. How the work was done

1. **Baseline.** `go test -race -count=1 -covermode=atomic -coverprofile ./...`
   on the clean worktree: every package green in 1m31s, 1,411 `Test*`
   functions, 77.1% total statement coverage (`go tool cover -func`), 76.8% by
   statement weight (`tools/covercheck`, the number CI now gates on).
2. **Discovery.** Three read-only agents produced the raw material: a
   96-requirement list with sources and implementation pointers (section 3),
   a security-claims table for THREAT_MODEL.md and SECURITY.md (folded into
   sections 3 and 4), and a test-infrastructure assessment (section 5).
3. **Mutation testing.** `gremlins` v0.5.0 (installed into the scratch
   directory, no repository dependency added) was run on the ten packages
   whose tests are fast enough to make it practical. Each surviving mutant was
   read and classified; the meaningful ones became tests (section 6).
4. **Verification of the claimed contradictions.** Each one the agents raised
   was reproduced by hand before anything was changed (section 4).
5. **Fixes under TDD.** Every production change in this sprint has a test that
   was watched to fail first, or a controlled mutation that was watched to
   make the new test fail. Those runs are cited inline.
6. **Independent adversarial review** on a scratch copy of the tree
   (`../drydock-review`), with the reviewer told to break the fixes and the
   tests. Its findings and their resolution are in section 9.
7. **Final validation** (section 10).

## 2. Scope boundaries and limitations

- The VM-backed claims (A1, A2, A7, A8, V1) cannot run on GitHub-hosted CI.
  They were run locally on this Mac with Apple `container` 1.5.0 at the end of
  the sprint (`make redteam-vm`, section 10). The launchd round trip and the
  live Gemini/Codex integration tests need credentials or launchd and were
  not run.
- Mutation testing was not run on `internal/broker` (its suite takes 45s per
  run; a mutant set of several hundred would take most of a day). Its
  effectiveness is argued from the targeted mutation checks and the adversarial
  review instead.
- Coverage targets from the brief (90% critical logic, 85% branch) are not
  met everywhere and were not chased with low-value tests. Go's tooling
  reports statement coverage only; branch coverage is approximated by the
  mutation scores in section 6.

## 3. Requirements traceability matrix

Sources: `README.md`, `CONTRIBUTING.md`, `site/docs/*.md`, `THREAT_MODEL.md`,
`SECURITY.md`, `CHANGELOG.md` (v0.8.0 to v0.8.2), `config/*.yaml`. Status
vocabulary: **VERIFIED** (a test exercises the behaviour and ran green in this
sprint), **PARTIALLY VERIFIED** (covered at a unit or config-string level but
not at the enforcement point, or only part of the statement), **NOT VERIFIED**
(no test), **CONTRADICTORY** (code and documentation disagreed; the
resolution is named), **BLOCKED** (needs an environment CI does not have; the
local run is named). Tests added in this sprint are marked **(new)**.

### Configuration (CFG)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| CFG-1 | `drydock init` creates `~/.drydock` 0700, seeds config and egress, never overwrites | configuration.md:3-12 | `cmd/drydock/init.go` ensureUserConfig | `TestEnsureUserConfig_SeedsOnceAndPreservesEdits` (new) | VERIFIED |
| CFG-2 | env > file > default | configuration.md:11,724 | `config.go` applyEnvOverrides | `TestEnvOverridesWinOverFile`, `TestEnvOverrides_AllOperatorKnobs` | VERIFIED |
| CFG-3 | Default values table | configuration.md:22-43 | `config.go` Defaults | `TestDefaults_MatchV01EnvFallbacks`, `TestSeedTemplate_MatchesOnDiskTemplate` | VERIFIED |
| CFG-4 | `task_timeout`, `approval_timeout` have no env override | configuration.md:37 | applyEnvOverrides | `TestExplain_CoversEveryEnvOverride` | VERIFIED |
| CFG-5 | Validation of the core knobs, exact bounds | config.yaml comments | `config.go` validate | `TestValidate_Rejects`, `TestValidate_ExactBoundaries` (new) | VERIFIED |
| CFG-6 | `global_max_tasks` below `max_concurrent_tasks` rejected, env included | configuration.md:35 | validate | `TestGlobalCeiling_CrossFieldAppliesToEnv` | VERIFIED |
| CFG-7 | `ci.max_attempts` with `approval_timeout: 0` rejected | configuration.md:487 | validate | `TestCI_ValidateRejects` | VERIFIED |
| CFG-8 | CI poll, attempts and watch-timeout bounds | configuration.md:412-422 | validate, ciDispatchFloor | `TestCI_ValidateRejects/Accepts`, `TestCIDispatchFloor_Boundaries` (new) | VERIFIED |
| CFG-9 | `DRYDOCK_CI_WATCH` enables only on exactly `1` | configuration.md:419 | config.go:652 | `TestCI_WatchEnvOnlyExactlyOneEnables` | VERIFIED |
| CFG-10 | `queue.max_denials` 0..10, default 2 | configuration.md:599 | validate | `TestQueueMaxDenials_*` | VERIFIED |
| CFG-11 | `diff_policy` glob validation | configuration.md:304 | validate | `TestDiffPolicy_Rejects` | VERIFIED |
| CFG-12 | canonical `verify.repos` / `profiles.repos` keys | configuration.md:316 | validate | `TestVerifyConfig_Rejects`, `TestProfiles_Rejects` | VERIFIED |
| CFG-13 | `openai_compat.base_url` https, http only for localhost | configuration.md:641 | validate | `TestValidate_OpenAICompat` | VERIFIED |
| CFG-14 | unknown YAML fields and trailing documents rejected | strict config | `config.go` Load | `TestLoad_RejectsUnknownField`, `TestLoad_RejectsTrailingYAMLDocument` | VERIFIED |
| CFG-15 | `policy explain` source attribution | configuration.md:706 | `explain.go` | `TestExplain_SourceAttribution` | VERIFIED |
| CFG-16 | in sync / DIVERGENT / UNVERIFIED and `--json` shape | configuration.md:732 | `cmd/drydock/policy.go`, HandlePolicy | `TestPolicyExplain_*` | VERIFIED |
| CFG-17 | `api-keys.env` 0600, partial read never rewritten | configuration.md:16 | `apikeys.go` | `TestWriteAPIKey_UpsertsPreservesOtherAnd0600`, `TestLoadAPIKeys_ScannerError` | VERIFIED |
| CFG-18 | env `0` applies where 0 is legal, is ignored where it is not | configuration.md env column | applyEnvOverrides | `TestEnvOverride_ZeroSemantics*` (new) | VERIFIED |

### Submission and CLI (SUB)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| SUB-1 | `repo_ref` must be https/git@/ssh | submitting-tasks.md:760 | HandleTask, `gitURLRef` | `TestHandleTask_400OnLocalPathRepoRef`, `TestGitURLRef` | VERIFIED |
| SUB-2 | task body capped at 64 KiB | submitting-tasks.md:403 | `MaxTaskBodyBytes` | `TestHandleQueueAdd_CapsBody` | VERIFIED |
| SUB-3 | invalid `--egress-extra` is 400 before any gate | egress.md:51 | HandleTask, `egress.ValidateDomains` | `TestValidateDomains_*`, `TestGlobalCap_EgressValidationStillPrecedesTheCeiling` | VERIFIED |
| SUB-4 | push preflight before the VM boots | submitting-tasks.md:424 | `stage.PushPreflight` | `TestHandleTask_PushPreflightAuthFailure_FailsBeforeAnyWork` | VERIFIED |
| SUB-5 | `--plan` ends `planned`, never verifies or pushes | submitting-tasks.md:57 | broker.go:1238 | `TestPlanMode_TerminatesPlannedNeverPushes` | VERIFIED |
| SUB-6 | `--issue` host-side fetch, curated env, size cap | submitting-tasks.md:44 | `cmd/drydock/submit.go` | `TestResolveIssue_*`, `TestIssueInstruction_TruncatesHugeBodyWithMarker` | VERIFIED |
| SUB-7 | platform adapter selection and **hostname** autodetect | submitting-tasks.md:758-771 | `remote.AdapterFor` | `TestAdapterFor`, `TestAdapterFor_AutodetectUsesTheHostOnly` (new) | CONTRADICTORY (fixed: code, section 4 D-6) |
| SUB-8 | push ok but PR open fails still reports pushed | submitting-tasks.md:487 | push path | `TestHandleTask_PROpenFailure_StillPushed` | VERIFIED |
| SUB-9 | push retry and fresh-branch recovery | submitting-tasks.md:606 | `push.go`, `pushfail.go` | `TestPushWithRecovery_*`, `TestClassifyPushError` | VERIFIED |
| SUB-10 | outcome vocabulary | submitting-tasks.md:625,712 | broker.go outcomes, `audit.OutcomeKeyWithMetrics` | `TestWriteStats_*InFixedOrder` | CONTRADICTORY (fixed: docs, `planned` was missing) |
| SUB-11 | `drydock stats` flags and offline operation | submitting-tasks.md:669 | `stats.go` | `TestWriteStats_*` | VERIFIED |
| SUB-12 | `prune` dry run by default, never prunes live markers | troubleshooting.md:55 | `prune.go` | `TestSelectForPrune`, `TestTaskIDFromFile_LiveStateMarkersAreNeverPruned` | VERIFIED |
| SUB-13 | `doctor --repo` checks | troubleshooting.md:8 | `repodoctor.go` | `TestDiagnoseRepo_*` | VERIFIED |
| SUB-14 | exit codes 2 (usage) and 1 (runtime) | release-qa.md:29 | `main.go` | `TestCLIExitContracts` | VERIFIED |
| SUB-15 | `queue add` shares the submit flag surface | submitting-tasks.md:66 | `queue.go` | `TestSharedBuilder_SubmitAndQueueAddIdentical` | VERIFIED |
| SUB-16 | chain fields zeroed from request bodies | configuration.md:519 | HandleQueueAdd, HandleTask | `TestHandleQueueAdd_ZeroesTheCarriedRootInstruction` | VERIFIED |
| SUB-17 | request bodies are exactly one JSON object | implied by SUB-16, A5-ui | `internal/strictjson` | `TestHandleQueueAdd_RejectsTrailingBytes…`, `TestHandleTask_RejectsTrailingBytes…` (new) | VERIFIED (new behaviour, section 4 D-2) |

### Queue and concurrency (QUEUE)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| QUEUE-1 | 503 when slots are full | configuration.md:39 | acquireSlot | `TestHandleTask_503WhenSaturated`, `TestAcquireSlot_RaceClean` (invariants added) | VERIFIED |
| QUEUE-2 | one shared pool, never over-committed | submitting-tasks.md:101 | dispatchPass | `TestQueue_DrainsRespectingConcurrency` | VERIFIED |
| QUEUE-3 | forward-only state machine; cancelled from every non-terminal | submitting-tasks.md:80-127 | `validTransition` | `TestQueueStateMachineIsForwardOnly`, `TestQueueStateCanTransitionTo` | CONTRADICTORY (fixed: docs, two `queued -> dead_letter` writers and missing edges) |
| QUEUE-4 | queued task shows `verifying` | CHANGELOG v0.8.2 | runQueued | `TestQueuedTask_EntersVerifyingState` | VERIFIED |
| QUEUE-5 | spend-capped human items park, CI retries drop | submitting-tasks.md:103 | takeDispatchable | `TestQueue_ParksOnAggregateExceeded`, `TestCIRetry_AggregateCapRefusesRatherThanParks` | VERIFIED |
| QUEUE-6 | boot: queued re-dispatched, in-flight dead-lettered, gate resumed | submitting-tasks.md:113 | ResumeQueue | `TestResumeQueue_*` | VERIFIED |
| QUEUE-7 | `queue list` columns, sanitized REASON | submitting-tasks.md:75 | `cmd/drydock/queue.go` | `TestQueueList_*` | VERIFIED |
| QUEUE-8 | `queue cancel` in every state, 404 unknown | submitting-tasks.md:76,215 | cancelQueued, cancelAwaitingCI | `TestHandleQueueCancel_*` | VERIFIED |
| QUEUE-9 | rejection loop 409 at enqueue | configuration.md:581 | rejectionGuard | `TestRejectionLoop_ThirdEnqueueRefusedBeforeSpend` | VERIFIED |
| QUEUE-10 | identical denied diff auto-denied | configuration.md:591 | `rejections.go` | `TestRejectionLoop_RepeatDiffNeverReposesGate` | VERIFIED |
| QUEUE-11 | only human deny counts, only human approve resets | configuration.md:606 | `rejections.go` | `TestRejectionLoop_OnlyHumanDiffDenialsCount` | VERIFIED |
| QUEUE-12 | degraded ledger: 503, park, healthz | configuration.md:626 | admin.go | `TestRejectionLoop_DegradedLedgerRefusesAndParks` | VERIFIED |
| QUEUE-13 | `max_denials: 0` disables guards, still records | configuration.md:604 | `rejections.go` | `TestRejectionLoop_OffAtZeroStillRecords` | VERIFIED |
| QUEUE-14 | `stage_ms.queued` present only for queued tasks | submitting-tasks.md:420 | `metrics.go` | `TestQueue_MetricsRowRecordsQueuedMs` | VERIFIED |

### Approval gate, setup and verify (GATE)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| GATE-1 | no push before approve; deny keeps the diff | submitting-tasks.md:441 | gatePushMarked | `TestGatePush_BlocksUntilApprove`, `TestRedteam_A5_GateBlocksUnapprovedPush` | VERIFIED |
| GATE-2 | 404 not pending, 409 double signal, 204 success | code only | admin.go signal | `TestGatePush_UnknownIDReturns404`, `TestHandleApprove_AlreadySignaledIs409` | VERIFIED (undocumented) |
| GATE-3 | `approval_timeout` auto-deny | configuration.md:38 | awaitGate | `TestGatePush_AutoDeniesOnApprovalTimeout` | VERIFIED |
| GATE-4 | diff policy caps and blocked paths, even with auto-approve | configuration.md:281 | checkDiffCaps | `TestDiffCaps_*` | VERIFIED |
| GATE-5 | second-look acks, 422 | configuration.md:290 | requiredAcks, signal | `TestSecondLook_*`, `TestDoSignalApprove422PrintsMissingAndHint` | VERIFIED |
| GATE-6 | `review` prompts per category | submitting-tasks.md:472 | promptAcks | `TestPromptAcks` | VERIFIED |
| GATE-7 | required verify blocks on anything but passed | submitting-tasks.md:583 | runVerify | `TestRunVerify_RequiredBlocksOn*` | VERIFIED |
| GATE-8 | verified tree must equal pushed tree | submitting-tasks.md:565, THREAT_MODEL A5/A9 | verifiedTreeGuard, **ExportStaged** | `TestFinishPush_TreeMismatchFailsClosed`, `TestExportStaged_IgnoresInTreeExportAttributes` (new), `TestVerifyExportComplete_RefusesAMissingPath` (new) | CONTRADICTORY (fixed: code, section 4 D-1) |
| GATE-9 | one VM per verify command, no network, fail fast, timeout inconclusive | submitting-tasks.md:561 | execVerify | `TestRunVerify_FailFastAndAdvisory`, `TestRunVerify_TimeoutIsInconclusiveNeverPassed`, `TestRedteam_V1_*` | VERIFIED (V1 part BLOCKED in CI, run locally) |
| GATE-10 | setup failure before the agent VM, no spend | configuration.md:335 | runSetup | `TestRunSetup_FailClosedBeforeSpend` | VERIFIED |
| GATE-11 | output cannot flip a verdict | configuration.md:351 | execSetup/execVerify | `TestRun*_ForgedPassOutputCannotFlipVerdict` | VERIFIED |
| GATE-12 | `kill` tears down and unblocks gates | submitting-tasks.md:657 | HandleKill | `TestHandleKill_*` | VERIFIED |
| GATE-13 | client disconnect does not cancel | broker.go comment | HandleTask | `TestHandleTask_SurvivesClientDisconnect` | VERIFIED |
| GATE-14 | 32 MiB diff fails closed | security-defaults.md:23 | `MaxDiffBytes` | `TestCaptureDiff_OversizeDiffFailsClosed` | VERIFIED |
| GATE-15 | `task_timeout` terminates without push | security-defaults.md:17 | lifecycle ctx | `TestHandleTask_TimeoutTerminatesAndDoesNotPush` | VERIFIED |
| GATE-16 | macOS notification unless opted out; text escaped | submitting-tasks.md:18 | notifyMac | `TestNotifyMac_HonoursOptOutAndEscapesText` (new) | VERIFIED |
| GATE-17 | host commit never runs a planted hook (A3) | THREAT_MODEL A3 | `stage.git` | `TestHostCommit_IgnoresPlantedHook`, `TestHostCommit_IgnoresHookInHostGitDir` (new; the old test passes with `core.hooksPath` removed, the new one fails) | VERIFIED |

### Gateway, budgets, metering (GW)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| GW-1 | 401 unknown, expired, mismatched token | troubleshooting.md:29 | admit | `TestGateway_UnknownToken401`, `TestGateway_ExpiredToken401` | VERIFIED |
| GW-2 | 402 at budget | security-defaults.md:9 | admit | `TestGateway_OverBudget402` | VERIFIED |
| GW-3 | reservation bound, exact fit admitted | configuration.md:30 | admit | `TestAdmit_InFlightReservationBounds`, `TestAdmit_ReservationExactFitIsAdmitted` (new) | VERIFIED |
| GW-4 | request cap 429 without Retry-After; 0 means 1000 | configuration.md:31 | admit, effectiveRequestCap | `TestRequestCap_RejectsOverLimit`, `TestServeHTTP_RequestCapExhausted_NoRetryAfter`, `TestEffectiveRequestCap` | VERIFIED |
| GW-5 | in-flight 429 retryable | configuration.md:29 | admit | `TestAdmit_InFlightLimit`, `TestServeHTTP_InFlightLimit` | VERIFIED |
| GW-6 | aggregate cap: 402 at the gateway and at submit; metered spend counts | daemon.md:18 | admit, meter, spendLedger | `TestGateway_AggregateCap`, `TestGateway_MeteredSpendTripsTheAggregateCap` (new) | VERIFIED |
| GW-7 | cache token pricing | CHANGELOG v0.8.2 | `pricing.go` | `TestCost_CacheTokens*` | VERIFIED |
| GW-8 | route allowlist, batches 403, traversal 403 | configuration.md:205 | routeAllowed | `TestGateway_RouteAllowlist`, `TestGateway_MessagesBatchesBlockedLocally`, `TestGateway_EncodedTraversalBlocked` | VERIFIED |
| GW-9 | task token in `X-Goog-Api-Key` never reaches a non-Google upstream | CHANGELOG v0.8.2 | director | `TestGateway_TaskTokenInGoogleHeaderNeverReachesNonGoogleUpstream` | VERIFIED |
| GW-10 | openai_compat unknown model meters $0 (documented gap) | configuration.md:656 | pricing | `TestCost_FallsBackToDefault` | VERIFIED |
| GW-11 | global ceiling 402 naming the limb | configuration.md:45 | admitGlobalStart | `TestGlobalCap_HandleTaskRefusesWith402` | VERIFIED |
| GW-12 | ceiling fails closed per limb | configuration.md:114 | globalcap, globalledger | `TestGlobalCap_FailsClosedPerLimb` | VERIFIED |
| GW-13 | no overshoot under concurrency | security-defaults.md:15 | admitGlobalStartDetail | `TestGlobalCap_ConcurrentAdmissionsCannotOvershoot` | VERIFIED |
| GW-14 | ceiling never kills; parks humans, drops retries | configuration.md:94 | takeDispatchable | `TestGlobalCap_NeverKillsARunningTask`, `TestGlobalCap_DispatcherParksHumanWorkAndDropsRetries` | VERIFIED |
| GW-15 | ledger survives restart in both window modes | configuration.md:247 | globalledger | `TestGlobalCeiling_RefusalSurvivesARestartInBothWindowModes` | VERIFIED |
| GW-16 | only broker-metered spend counts | configuration.md:221 | globalrecord, globalreconcile | `TestGlobalRecord_USDIsTheLeaseNotTheAudit`, `TestReconcile_NeverSourcesATrustedValueFromTraceContent` | VERIFIED |
| GW-17 | `/admin/ceiling`, stats headroom | configuration.md:137 | globalheadroom | `TestRenderCeiling_*` | VERIFIED |
| GW-18 | gate shows broker-metered figure, unknown when resumed | web-ui.md:45 | gate spend | `TestGateSpend_*` | VERIFIED |
| GW-19 | Gemini batch actions refused locally | configuration.md:205 (now explicit) | `Route.Actions` | `TestRouteAllowed_GoogleActionsAreExplicit`, `TestGateway_GoogleBatchGenerateBlockedLocally` (new) | VERIFIED (new behaviour, section 4 D-3) |
| GW-20 | usage parsing is chunking-independent; buffers capped exactly | gateway.go comments | usageReader | `TestUsageReader_KeptLinesAreChunkingInvariant`, `TestUsageReader_LineCapIsExact` (new) | VERIFIED |

### Egress (EGR)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| EGR-1 | CONNECT only to 443 | egress.md:77 | squid.go | `TestCompileSquidConf_DefaultDenyShape` (config string) | PARTIALLY VERIFIED |
| EGR-2 | private/loopback/link-local/CGNAT/this-host denied before the allowlist | egress.md:105 | `to_local` ACL | `TestCompileSquidConf_AuthAndInclude` (field-level, now includes `0.0.0.0/8`); `TestSquidProxyAuth_Live` case 6 (loopback, live squid, CI) | PARTIALLY VERIFIED (only loopback is proven live; see section 11) |
| EGR-3 | model-API hosts never in the squid allowlist | egress.md:11 | netfw.go | `TestCompileSquidAllowlist_ExcludesModelAPI` | VERIFIED |
| EGR-4 | CONNECT to an unlisted host is 403 | egress.md:17 | squid | `TestSquidProxyAuth_Live` (CI), `TestRedteam_A2_*` (VM) | VERIFIED |
| EGR-5 | widening gate, per-task auth, `<id>.widen.json` 0600 | egress.md:58 | gateEgressWiden, controller | `TestGateEgressWiden_*`, `TestGateEgressWiden_MarkerWriteNeverFollowsASymlink` (new) | VERIFIED |
| EGR-6 | auto-approve cannot bypass widening; retry re-poses | THREAT_MODEL A6 | broker | `TestRedteam_A6_*` | VERIFIED |
| EGR-7 | egress.yaml fails closed; `EGRESS_CONFIG` | configuration.md:701 | `egress.Load` | `TestLoad_FailClosed`, `TestFindEgressConfig_EnvVarTakesPrecedence` | VERIFIED |
| EGR-8 | setup VM: proxy only; verify VM: no network | configuration.md:339 | setup.go, verify.go | `TestRunSetup_NoGrantEnvInSetupVM`, `TestRedteam_V1_*` | VERIFIED (V1 BLOCKED in CI) |
| EGR-9 | host and port validation bounds | egress.go | `ValidateHost`, `ValidatePorts` | `TestValidateHost_LengthBoundary`, `TestValidatePorts_RangeBoundary` (new) | VERIFIED |

### Credentials (AUTH)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| AUTH-1 | real key never in the container env | README:26 | buildTaskEnv | `TestHandleTask_RealKeyNeverEntersContainerEnv`, `TestRedteam_A1_*` | VERIFIED (VM part BLOCKED in CI; the VM tests check `grant.EnvVars()`, not `buildTaskEnv`, see section 11) |
| AUTH-2 | `auth claude/codex` write 0600 files; opencode excluded | authentication.md:57 | gwcreds | `TestFileCredStore_RoundTripAndPerms`, `TestAuthAgents_ExcludesOpencode` | VERIFIED |
| AUTH-3 | Keychain re-import before own refresh; never writes the Keychain | authentication.md:71 | `oauth.go` | `TestOAuthCred_Reimport*` | VERIFIED |
| AUTH-4 | refresh keeps the disk refresh token when the endpoint omits it | CHANGELOG v0.8.2 | recoverFromDisk | `TestOAuthCred_RecoverFromDisk_KeepsDiskRefreshWhenEndpointOmitsIt` | VERIFIED |
| AUTH-5 | unrefreshable subscription token is 502 | troubleshooting.md:30 | gateway | `TestOAuthCred_RefreshFailure` | VERIFIED |
| AUTH-6 | env wins over `api-keys.env` | configuration.md:14 | resolveAPIKey | `TestResolveAPIKey_Precedence` | VERIFIED |
| AUTH-7 | `default_model` only for claude and codex | configuration.md:27 | taskModelFor | `TestTaskModelFor` | VERIFIED |

### Daemon lifecycle (DAEMON)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| DAEMON-1 | `daemon install` plist shape and credential check | daemon.md:47 | daemon.go | `TestRenderPlist_Golden`, `TestLaunchdCredentialAvailable` | PARTIALLY VERIFIED (launchctl round trip is tagged, never run in CI) |
| DAEMON-2 | single instance via flock | daemon.md:120 | lock.go | `TestAcquireLock_SecondAcquireIsContention` | VERIFIED |
| DAEMON-3 | `--version`, `--help`, unknown arg exit 2 | daemon.md:105 | argvAction | `TestArgvAction_*` | VERIFIED |
| DAEMON-4 | non-loopback `broker.addr` fatal; TCP Host/Origin check | config.yaml:155 | listen, brokerAuthed | `TestLoopbackHostPort`, `TestBrokerHandler_TCPModeRejectsRebinding` | CONTRADICTORY (fixed: docs said "no auth" and omitted loopback-only) |
| DAEMON-5 | socket 0600 in a 0700 per-uid dir; symlinked dir refused | configuration.md:697 | sockpath | `TestListen_UnixSocketPermissions`, `TestEnsureParent_*` | VERIFIED |
| DAEMON-6 | container system start; version drift policy | daemon.md:64 | runner, checkContainerVersion | `TestEnsureContainerSystem_*`, `TestCheckContainerVersion_*` | VERIFIED |
| DAEMON-7 | gate marker survives restart; approve pushes without re-run | daemon.md:71 | gatemarker, ResumeAwaiting | `TestResumeAwaiting_StageSurvives_ApprovePushes` | VERIFIED |
| DAEMON-8 | stage gone at resume writes `interrupted` | daemon.md:84 | ResumeAwaiting | `TestResumeAwaiting_StageGone_WritesInterrupted` | VERIFIED |
| DAEMON-9 | never resume mid-verify | daemon.md:91 | ResumeQueue, TerminateStuckAudits | `TestResumeQueue_PreparingAndVerifyingDeadLetterUniformly` | PARTIALLY VERIFIED (the graceful-stop half has no direct test) |
| DAEMON-10 | SIGTERM drain order; cleanup once | CHANGELOG v0.8.2 | main.go | `TestBeginQueueDrain_*`, `TestWaitQueueDrain_CoversResumedGate`; `TestBrokerd_ShutdownReapsSquid` (tagged) | PARTIALLY VERIFIED |
| DAEMON-11 | boot reaps orphans, keeps gated stages | daemon.md:60 | pruneOrphanTasks | `TestPruneOrphanTasks_*` | VERIFIED |
| DAEMON-12 | boot warnings (max_attempts without cap; USD-only ceiling) | configuration.md:477,202 | main.go:492, globalconfig | `TestApplyGlobalCeiling_AUSDOnlyInstall…`; max_attempts warning has none | PARTIALLY VERIFIED |
| DAEMON-13 | resumed gate opens the audit trail without following a symlink | A3/audit hygiene | reconcile.go:139 | `TestResumeAwaiting_RefusesASymlinkedAuditTrail` (new) | VERIFIED |

### Audit and state files (AUDIT)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| AUDIT-1 | metrics row shape, last row wins | submitting-tasks.md:700 | metrics.go | `metrics_test.go` | VERIFIED |
| AUDIT-2 | markers atomic, 0600, O_NOFOLLOW, id validated | CHANGELOG v0.8.2 | atomicfile, markerstore | `TestMarkerStore_*`, atomicfile tests | VERIFIED |
| AUDIT-3 | CI observation row never a result row | submitting-tasks.md:200 | audit.CIObservation | `TestCIObservationRow_*` | VERIFIED |
| AUDIT-4 | rejection ledger location and contents | configuration.md:576 | rejections.go | `TestRejectionLedger_*` | VERIFIED |
| AUDIT-5 | setup and verify logs display-only | configuration.md:355 | setup.go, verify.go | `TestRunVerify_AllPass_RecordedInBrief` | VERIFIED |
| AUDIT-6 | audit root is 0700 | THREAT_MODEL N6 | MkdirAll 0700 at creation | none; an existing wider dir is not tightened | NOT VERIFIED (accepted: an operator-chosen `AUDIT_ROOT` must not be chmodded, same reasoning as `sockpath.EnsureParent` in the previous sprint) |

### CI watch and retry (CI)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| CI-1 | watch off: immediate completion | submitting-tasks.md:221 | ciqueue | `TestQueue_WatchOffStillCompletesImmediately` | VERIFIED |
| CI-2 | `awaiting_ci` holds no slot | submitting-tasks.md:131 | ArmCIWatch | `TestCIWatch_HoldsNoConcurrencySlot` | VERIFIED |
| CI-3 | dispatch floor | submitting-tasks.md:175 | ciDispatchFloor | `TestCIWatch_*BelowTheFloor*`, `TestCIDispatchFloor_Boundaries` (new) | VERIFIED |
| CI-4 | terminal mapping; never completed without evidence | submitting-tasks.md:155 | concludeCIWatch | `TestApplyCIObservation_UnobservedNeverReadsAsSuccess` | VERIFIED |
| CI-5 | deadline anchored at push, kept across restart | configuration.md:421 | ciDeadline | `TestCIWatch_ResumeKeepsTheOriginalAbsoluteDeadline` | VERIFIED |
| CI-6 | Enterprise hosts complete unwatched | submitting-tasks.md:188 | ciRefHostWatchable | `TestQueue_EnterpriseHostRefCompletesUnwatched` | VERIFIED |
| CI-7 | only `ci_failed` retries; chain stops at `max_attempts` | submitting-tasks.md:298 | maybeEnqueueCIRetry | `TestCIRetry_ChainTerminatesAtExactlyMaxAttempts` | VERIFIED |
| CI-8 | retry child: auto-approve off, re-poses the gate, own PR | submitting-tasks.md:270 | BuildRetryTask | `TestBuildRetryTask_AutoApproveNeverSurvives`, `TestCIRetry_ChildReposesTheHumanGate` | VERIFIED |
| CI-9 | instruction size bounded; fence holds | submitting-tasks.md:389 | assembleRetryInstruction | `TestBuildRetryTask_InstructionSizeDoesNotGrowWithChainDepth`, `TestBuildRetryTask_HostileCheckNameCannotBreakTheFence` | VERIFIED |
| CI-10 | one child per parent across crash and replay | configuration.md:514 | markCIRetryEnqueued | `TestCIRetry_ReplayedObservationDoesNotDoubleEnqueue` | VERIFIED |
| CI-11 | `gh` argv pinned, github.com only, no logs | configuration.md:558 | checks.go | `TestChecks_Argv`, `TestCIRetry_LogTextCannotInfluenceTheDecision` | VERIFIED |

### Web UI (UI)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| UI-1 | binds `127.0.0.1:<port>`, prints the token URL | web-ui.md:9 | `uiAddr`, uiURL | `TestUIURL`, `TestUIServer_LoopbackBindAndHeaderTimeout` (new) | VERIFIED |
| UI-2 | 403 on bad token, cross-origin, non-loopback Host; constant-time compare | release-qa.md:43 | server.go authed | `TestAuth`, `TestHostCheck`, `TestNonLoopbackOriginRejected`, `TestConstantTimeTokenCompare` | VERIFIED (asserted on `/api/tasks`; other routes share the wrapper) |
| UI-3 | UI submit refuses `auto_approve` | release-qa.md:45, THREAT_MODEL N6 | submit.go | `TestSubmitRejectsAutoApprove`, `TestSubmit_TrailingBytes` (new) | CONTRADICTORY (fixed: code, section 4 D-2) |
| UI-4 | CSP, nosniff, DENY headers | web-ui.md:92 | server.go | `TestSecurityHeaders` | VERIFIED |
| UI-5 | `--no-token` warning | web-ui.md:98 | ui.go | `TestNoTokenModeSkipsGate` | VERIFIED |
| UI-6 | `ReadHeaderTimeout` 10s, no body timeouts | CHANGELOG v0.8.2 | `newUIServer` | `TestUIServer_LoopbackBindAndHeaderTimeout` (new) | VERIFIED |
| UI-7 | approve acks proxied, 422 passed through, bad ids 400 | submitting-tasks.md:483 | proxy | `TestProxyApprove*`, `TestProxyRejectsBadID` | VERIFIED |
| UI-8 | polling cadence, two-press deny/kill, shortcuts | release-qa.md:64 | app.js | manual checklist only | NOT VERIFIED (browser behaviour; `site/docs/release-qa.md`) |
| UI-9 | outcome glyphs | web-ui.md:34 | app.js:281 | `ci_assets_test.go` (partial) | CONTRADICTORY (fixed: docs; `setup_failed`, `verify_failed`, `policy_blocked`, `planned` render as the neutral glyph) |

### Security defaults and threat model (SEC)

| ID | Requirement | Spec | Implementation | Tests | Status |
|---|---|---|---|---|---|
| SEC-1 | APFS stage quota, polling guard | security-defaults.md:20 | quota_darwin.go, stagesize.go | `TestQuota_HardBoundENOSPC` (darwin only), `TestHandleTask_StageFillTerminatesAndDoesNotPush`, `TestRedteam_A8_*` | VERIFIED locally; the hard bound now runs in CI on the new macOS job |
| SEC-2 | dependency cache LRU, read-only, per repo+lockfile | configuration.md:359 | depcache, cache.go | `TestStore_EvictLRU`, `TestComputeKey_StableAndInjective`, `TestRedteam_A7Cache_*` | VERIFIED |
| SEC-3 | no agent state persists between tasks (A7) | THREAT_MODEL A7 | `run --rm`, stage cleanup | `TestRedteam_A7_NoStatePersistsBetweenTasks` (VM; passes its own `--rm`), `TestBuildRunArgs_Contains` | PARTIALLY VERIFIED |
| SEC-4 | output flood terminates without push | security-defaults.md:18 | outputcap.go | `TestHandleTask_OutputFloodTerminatesAndDoesNotPush` | VERIFIED |
| SEC-5 | A2 nft pin, agent cannot rewrite it | THREAT_MODEL A2 | init-firewall.sh, drop-agent.sh | `TestRedteam_A2_*` | BLOCKED in CI (run locally, section 10); IPv6 fail-closed has no probe |
| SEC-6 | A4 `.task/` excluded from the diff | THREAT_MODEL A4 | stageAll | `TestCaptureDiff_ExcludesTaskDir` | VERIFIED |
| SEC-7 | gateway strips forwarding headers; squid `via off` | gateway.go:82, squid.go | Rewrite, conf | `TestGateway_NoForwardedHeadersReachUpstream`; squid lines string-only | PARTIALLY VERIFIED |
| SEC-8 | request body cap 16 MiB | gateway.go:330 | MaxBytesReader | `TestGateway_RequestBodyCapped` | VERIFIED |
| SEC-9 | "A1 to A8 defended" | SECURITY.md:64 | n/a | n/a | CONTRADICTORY (fixed: docs, A9 exists) |

### Documented gaps (no implementation, by design)

| ID | Statement | Source | Status |
|---|---|---|---|
| GAP-1 | brokerd-side audit retention is not wired up | CONTRIBUTING.md:147 | NOT VERIFIED (documented absence) |
| GAP-2 | `brokerd.log` is not rotated | daemon.md:62 | NOT VERIFIED (documented absence) |
| GAP-3 | no Slack or remote approval adapter | CONTRIBUTING.md:152 | NOT VERIFIED (documented absence) |

Totals: 110 rows. 93 VERIFIED, 9 PARTIALLY VERIFIED, 5 NOT VERIFIED (one
accepted, one browser-only, three documented absences), 9 CONTRADICTORY (all
resolved: 4 by code, 5 by documentation), and 3 rows whose VM half is BLOCKED
in CI but ran locally. The CONTRADICTORY rows are counted once each; three of
them also carry a VERIFIED test after the fix.

## 4. Defect report

Severity follows impact on a documented guarantee. "Reproduced" means the
failing test or command was run before the fix.

| ID | Severity | Component | Defect | Root cause | Fix | Regression test |
|---|---|---|---|---|---|---|
| D-1 | **HIGH** | `internal/stage` Prepare, ExportStaged | The verifier VM could be shown different bytes from the ones pushed. First found: a `.gitattributes` line `evil.go export-ignore` hid the file from the export (`export-subst` rewrites one) while `StagedTreeHash` still matched, so a "passed" verdict covered code the verifier never saw (reproduced: four staged files missing). The adversarial review then showed the first fix was incomplete: `text`/`eol`, `ident` and `working-tree-encoding` still rewrote the verifier's bytes, and a `filter=` attribute ran the operator's smudge driver (git-lfs is the common one) on the host against agent-chosen input (reproduced: CRLF, `$Id$` expansion, UTF-16 re-encoding and a driver marker file). `git --attr-source` and `attr.tree` were tried first and do not override an archived tree's own attributes. | `git archive` and `git add` apply the archived tree's own attributes and the operator's filter drivers. | `$GIT_DIR/info/attributes` (host-only, outranks every in-tree file and macro) is written at `Prepare` and again before each export with `* -export-ignore -export-subst -text -ident -filter !working-tree-encoding`; `verifyExportComplete` fails closed if any non-gitlink path is missing or the export's entry count differs from the tree's (case-folding on APFS). | `TestExportStaged_IgnoresInTreeExportAttributes`, `TestExportStaged_IgnoresConvertAttributesAndFilterDrivers`, `TestVerifyExportComplete_RefusesAMissingPath`, `TestVerifyExportComplete_RefusesAnEntryCountMismatch` |
| D-10 | **HIGH** (pre-existing, found by the review) | `internal/stage` CaptureDiff | `git diff --cached` ran without `--no-ext-diff --no-textconv`, so an in-tree `evil.go diff=op` line selected a `[diff "op"] textconv` from the operator's `~/.gitconfig` (plist, pdf, exif, sops and similar drivers are common in dotfiles): the `.diff` the approver reads showed the driver's output while the push shipped the blob, and the driver ran on the host against agent input. Reproduced: the reviewed diff for `evil.go` read `// nothing to see here`; the staged blob was `package evil // rm -rf /`. | Diff drivers are selected by a tree-controlled attribute. | Both flags added to `gitDiffCapped`; the attribute override above also leaves the attribute inert. | `TestCaptureDiff_IgnoresTextconvAndExternalDiff` |
| D-2 | **HIGH** | `internal/webui` submit, `internal/broker` HandleTask/HandleQueueAdd | The UI's `auto_approve` refusal could be skipped: `{"…","auto_approve":true} x` failed the UI's whole-body `json.Unmarshal` (probe skipped), was forwarded verbatim, and brokerd's streaming decoder accepted the object and started an auto-approved task. Reproduced: the broker side returned 200 and enqueued three items for three trailing-byte bodies (`TestHandleQueueAdd_RejectsTrailingBytes…`). Needs the UI token, so the trust boundary is unchanged, but the documented refusal was false. | Parser differential between `json.Unmarshal` and `json.Decoder`. | New `internal/strictjson.Decode` (one value, nothing after it) used by the UI probe and by both brokerd task endpoints; bodies with trailing data are 400. | `TestSubmit_TrailingBytes`, `TestHandleQueueAdd_RejectsTrailingBytesAfterTheJSONObject`, `TestHandleTask_RejectsTrailingBytesAfterTheJSONObject`, `TestDecode` |
| D-3 | MEDIUM | `internal/gateway` Google routes | `POST /v1beta/models/` as a directory prefix forwarded `:batchGenerateContent` (and `:asyncBatchGenerateContent`), batch jobs whose spend the response meter never sees, the hole F-03 closed for Anthropic. Reproduced: `routeAllowed` returned true for seven paths that should be refused. | Prefix match with no action list. | `Route.Actions`: the Google POST route now matches `{model}:{action}` with one path segment for the model and the action in {generateContent, streamGenerateContent, countTokens, embedContent}. | `TestRouteAllowed_GoogleActionsAreExplicit`, `TestGateway_GoogleBatchGenerateBlockedLocally` |
| D-4 | MEDIUM | `internal/netfw` squid ACL | The SSRF `to_local` ACL omitted `0.0.0.0/8`, which reaches host loopback services on macOS and Linux. Reproduced: the field-level ACL test failed. | Range missing from the list. | Added. The live squid test (CI) still proves only the loopback case; see section 11. | `TestCompileSquidConf_AuthAndInclude` (field-level; the previous substring check could not tell `0.0.0.0/8` from `10.0.0.0/8`) |
| D-5 | MEDIUM | `internal/broker` gates.go | `<id>.widen.json` was written with `os.WriteFile`: non-atomic and follows a planted symlink. Reproduced: the test overwrote a victim file through the link. | Not routed through `atomicfile` like the other artifacts. | `atomicfile.Write`. | `TestGateEgressWiden_MarkerWriteNeverFollowsASymlink` |
| D-6 | LOW | `internal/remote` AdapterFor | Autodetect matched `github.com` anywhere in the URL, so `https://evil.example/github.com/o/r` selected the `gh` adapter; the docs say hostname autodetect. Reproduced: four failing cases. | `strings.Contains` on the whole reference. | `repoHost()` parses `scheme://`, and scp-like forms, strips userinfo and port, lower-cases; exact host match. | `TestAdapterFor_AutodetectUsesTheHostOnly` |
| D-7 | LOW | `internal/broker` reconcile.go | The resumed approval gate opened `<id>.jsonl` for append without `O_NOFOLLOW`, unlike every other audit open. Reproduced: a symlinked trail was resumed as pending. | Missing flag. | `syscall.O_NOFOLLOW` added. | `TestResumeAwaiting_RefusesASymlinkedAuditTrail` |
| D-8 | LOW | `cmd/redteam-report` | V1 (verifier VM) tests were listed under their raw name rather than grouped as a claim. | `claimRe` matched `A[0-9]+` only. | `A[0-9]+\|V[0-9]+`. | `TestSummarize_PerClaimVerdicts` (V1 case) |
| D-9 | test infra, HIGH for developers | `internal/broker` tests | Eleven unit tests shelled out to the real Apple `container delete --force` (the `testBroker` fixture had no `deleteContainer` stub; only `newQueueBroker` did). Reproduced with a PATH shim: 11 invocations in `go test ./...`. | Missing stub. | Stub added; `TestMain` now installs a failing `container` shim and fails the package if any test reaches it (watched to fail with the stub removed). | `TestMain` guard |

Documentation defects (no code change): the queue state diagram and the
"exactly one writer" claim for `queued -> dead_letter` (two writers since
v0.8.0); `planned` missing from two outcome lists; `broker.addr` described as
"no auth" without the loopback-only rule; the quickstart's `drydock status`
sample line and its 8-character task ids used as commands (ids are 32 hex, no
prefix matching); SECURITY.md "A1 to A8"; the web UI glyph description. All
corrected in this branch. Two remaining doc inconsistencies were left for the
maintainer because they are product wording choices: `examples/hello-task.md`
says `drydock init` with "3 green checks" where README says `drydock setup`
and `release-qa.md` lists more checks; `models.md:23` likewise.

## 5. Testing infrastructure assessment and changes

Findings (from the read-only assessment, verified by hand where acted on):

- Fakes are injected through function fields on `Broker` (`prepareStage`,
  `runAgent`, `newAdapter`, `deleteContainer`) and a few package vars
  (`runCmd`, `squidReconfigure`, OAuth token URLs, `readBlob`). All swaps are
  restored. There are **no** `t.Parallel()` calls in the repository, so the
  package-var seams are safe today but block future parallelism.
- 44 skip sites: 17 symlink guards (never fire on CI), 8 root guards (would
  fire silently in a root container), hdiutil (darwin only), squid, container
  and launchd (never in CI), and one dead `testing.Short` gate.
- Synchronization: 52 polls at 5s, 9 at 50ms (`broker_test.go`), about 17
  fixed sleeps used as absence checks (40 to 80ms). The sleeps were left as
  they are; converting them needs a tick counter on the dispatcher, which is a
  production change this sprint did not take on.
- Isolation defects fixed: the real `container` CLI reached from unit tests
  (D-9); `os.Unsetenv` without restore in `cmd/drydock/start_test.go` (now a
  restoring helper); config tests reading the developer's `DRYDOCK_*` shell
  (now a `TestMain` that clears them); `TestAuditDir_DefaultAndOverride`
  accepting either of two host-dependent answers (now pins `HOME`);
  `TestAcquireSlot_RaceClean` with no assertion (now checks the peak holder
  count and that every slot is released); `TestConfig_OpenAIAuthRejectsGarbage`
  accepting any error (now names the field and value); the integration
  harness's `pkill -f "squid -N -f"` killing the operator's live squid (now
  scoped to the harness's own `/tmp/drydk-*` run dirs).
- Left as found and recorded: git tests inherit `~/.gitconfig`; the
  integration brokerd inherits the real `HOME`; `TestGlobalLedgerScalesToManyEntries`
  always runs because nothing passes `-short`.

## 6. Mutation testing

Tool: gremlins v0.5.0 (`go install` into the scratch directory; nothing added
to `go.mod`). Default operators (arithmetic, conditional boundary and negation,
increment/decrement, invert negatives). Efficacy is killed / (killed + lived).

| Package | Before: killed / lived / efficacy | After: killed / lived / efficacy | What was added |
|---|---|---|---|
| gateway | 115 / 40 / 74.2% | 131 / 26 / 83.4% | aggregate cap fed by metered spend; reservation exact fit; `anthropic-version` default vs preserve; per-field usage parsing; chunking-invariance and exact buffer cap for the usage reader; provider fallbacks; post-revoke zeros; Gemini either-side usage |
| config | 213 / 57 / 78.9% | 233 / 37 / 86.3% | exact validation boundaries; env `0` semantics (two tables); zero price and zero verify timeout; `ciDispatchFloor` boundaries |
| egress | 12 / 3 / 80.0% | 15 / 0 / 100% | host length 253/254; ports 1, 65535, 0, 65536 |
| globmatch | 29 / 6 / 82.9% | 33 / 4 / 89.2% | 30,000-case comparison against a memo-free reference matcher |
| netfw | 33 / 10 / 76.7% | not re-run | survivors are the stale-pid reaper's liveness branches (`processAlive`, `processIsSquid`); a real squid is needed to exercise them, recorded in section 11 |
| audit | 78 / 9 / 89.7% | not re-run | survivors are tail-window and buffer-size arithmetic; equivalent or display-only |
| gwcreds | 29 / 4 / 87.9% | not re-run | refresh-margin boundaries at exactly the margin |
| trustbrief | 40 / 13 / 75.5% | not re-run | header-line and path caps at exact lengths |
| atomicfile | 13 / 5 / 72.2% | not re-run | error-branch logging |
| sockpath | 6 / 2 / 75.0% | not re-run | `user.Current()` failure branch |

Remaining gateway survivors were read one by one: boundary flips on `> 0`
guards whose `== 0` case is behaviourally identical (`aggBudget`,
`MaxRequestCostUSD`, `len(StripFields)`), the release floors (defensive), and
the `status < 400` warning log. None changes an observable decision.

Controlled mutations outside gremlins, each watched to fail its new test:
`core.hooksPath` neutralization removed from `stage.git` (fails
`TestHostCommit_IgnoresHookInHostGitDir`; the pre-existing A3 test still
passes, which is why the new one exists); `deleteContainer` stub removed
(fails the broker package through the `TestMain` guard).

## 7. Test inventory

- **Added:** 37 test functions and one `TestMain` in 15 new files, plus 3
  tests appended to existing files; 0 tests removed, 0 weakened. 1,411 to
  1,449 `Test*` functions.
- **By type:** unit (config, egress, globmatch, gateway parsing and admission,
  strictjson, covercheck), contract (Gemini route grammar, env-override zero
  semantics, UI listener shape, init seeding idempotence), integration-in-
  process (broker HTTP handlers with trailing bodies, resumed gate with a
  symlinked trail, widen marker with a symlink, stage export against a real
  git repository, host git dir hooks), property-based (glob matcher vs
  reference, usage reader chunking invariance), negative (every boundary
  table), security (export attributes, strict JSON, Gemini batch, SSRF ACL
  fields, symlink refusals, hook neutralization, osascript escaping).
- **Modified tests:** `TestAcquireSlot_RaceClean` (invariants),
  `TestCompileSquidConf_AuthAndInclude` (field-level ACL),
  `TestConfig_OpenAIAuthRejectsGarbage`, `TestAuditDir_DefaultAndOverride`,
  `TestSummarize_PerClaimVerdicts` (V1), `start_test.go` (restoring unset),
  `remote_test.go`, `ui_test.go`, `submit_test.go`.
- **Edge cases now pinned:** 253/254-character hosts; ports 0/1/65535/65536;
  `max_concurrent_tasks: 1`, `task_timeout: 999ms`, `approval_timeout` 0/1s/999ms;
  env `0` for eleven knobs; reservation exactly filling the budget; a
  `message_delta` with zero output; usage lines split at every byte; a
  500-byte newline-free line against a 64-byte cap; `{model}:{action}` with an
  empty model, a slashed model, an unknown action, a trailing segment;
  `.gitattributes` macros, nested files and directory-level `export-ignore`;
  trailing `x`, `{}` and `\n[1]` after a JSON body; hooks named
  pre-commit/post-commit/commit-msg in the host git dir.

## 8. CI/CD changes

`.github/workflows/test.yml`:

- `go-test` now writes a coverage profile (`-covermode=atomic`), prints the
  total, and gates on a 70% floor through `tools/covercheck` (a 60-line
  program with its own tests; no third-party action). The floor sits well
  under the 76.8% baseline so it catches a package's tests silently stopping,
  not ordinary movement. `-timeout 15m` is explicit.
- New `go-test-macos` job (`macos-15`, Apple silicon) running the full race
  suite on every push and PR. drydock ships for macOS only; until now the
  darwin-tagged tests (APFS stage quota through hdiutil, launchd rendering,
  per-uid socket dir under `$TMPDIR`) never ran in CI. The repository is
  public, so macOS runners are free.
- New `go-test-nightly-shuffle` job on a daily schedule only:
  `-shuffle=on -count=2`, to surface order dependence and flakes.

Execution-time impact: the Linux job is unchanged apart from coverage
instrumentation (a few seconds); the macOS job runs in parallel and is bounded
at 20 minutes (locally the suite takes about 1m40s on an M-series Mac); the
nightly job costs nothing on PRs.

Remaining CI limitations: the VM-backed red team, the launchd round trip, the
squid end-to-end (`squide2e`) and the credentialed Gemini and Codex
integration tests still run only by hand (`make redteam-vm`,
`make release-preflight`); `release.yml` publishes on the strength of the
local preflight receipt and runs no tests of its own; `govulncheck` has no
schedule of its own (the image scan does).

## 9. Independent adversarial review

A separate agent reviewed the change set on a scratch worktree
(`../drydock-review`, removed afterwards) with the brief: break each fix, show
which production change each new test would miss, revert each fix and confirm
its test fails, probe concurrency with `-race -shuffle=on -count=3`, and judge
the CI changes. Its mutation table confirmed every new test for fixes 2 to 8
fails when its fix is reverted. Findings and resolution:

| Severity | Finding | Resolution |
|---|---|---|
| HIGH | Fix 1 pinned only two attributes; `text`/`eol`, `ident`, `working-tree-encoding` still rewrote the verifier's bytes and `filter=` ran an operator driver on the host (reproduced with CRLF, `$Id$`, UTF-16 and a marker file). | Override widened and moved to `Prepare` (D-1); the reviewer's reproducer ported as `TestExportStaged_IgnoresConvertAttributesAndFilterDrivers`. |
| HIGH | Pre-existing: `CaptureDiff` honours `diff=<driver>` textconv/external diff, rewriting the reviewed diff and running a host tool. | Fixed (D-10); reproducer ported. |
| MEDIUM | `verifyExportComplete` was blind to case-folding collisions on APFS (`Case.go` + `case.go` both Lstat fine, export holds one). | Entry-count check added; `TestVerifyExportComplete_RefusesAnEntryCountMismatch`. |
| MEDIUM | On macOS an agent can force "verification inconclusive" by committing `._x` next to `x` (bsdtar AppleDouble handling); only `required: true` then blocks the push. | Pre-existing; not changed. Recorded in section 11 and the non-required semantics are already documented in submitting-tasks.md. |
| MEDIUM | `TestSubmit_TrailingBytes` passed with the strict decoder's trailing check deleted, because every body also carried `auto_approve`. | A body without `auto_approve` added, asserting 400 and the "single JSON object" text; the mutant now fails it (re-run recorded above). |
| MEDIUM | Release notes overstated fix 1. | CHANGELOG rewritten to the widened fix. |
| LOW | `GET /v1beta/models/x:batchGenerateContent` was still forwarded through the GET directory route. | Directory routes now never match a `:` remainder; two cases added. |
| LOW | `repoHost` sent `www.github.com` and `github.com.` to push-only where the old code picked `gh`. | Both canonicalised; cases added. |
| LOW | `tools/covercheck` comment claimed parity with `go tool cover -func` (whole-profile vs function-only weighting differ by a few tenths). | Comment corrected; the floor is set from covercheck's own number. |
| LOW | `claimRe` grouped any `V4`/`A1` substring (`TestIPV4…`) as a claim. | Anchored to `Redteam_(A|V)<n>_`; negative case added. |
| LOW | Widen marker: a directory planted at the marker path makes the atomic rename fail and the gate proceeds with only a warning. | Accepted and recorded: the marker is a reviewer convenience, the gate itself does not depend on it. |
| LOW | `strictjson` accepts a top-level `null` (both endpoints then 400 on `repo_ref`). | Accepted: the decoder is generic; the handlers validate the object. |
| Info | Fix 2 probed with BOM, NUL, NBSP, key-case and unicode-escape variants, duplicate keys, 64 KiB truncation: UI and brokerd now agree on every body because they run the same function on the same bytes. Fix 3 probed with `%3A`, `//`, `;`, `..`, `/v1/`, uppercase and the control-plane routes; the generated squid conf passes `squid -k parse` on Squid 7.7; fix 5 is TOCTOU-safe; fix 6 held across 25 reference shapes. | No change. |
| Info | `-race -count=3 -shuffle=on` on broker and `-count=2` on the other changed packages: clean. | No change. |

## 10. Final validation

Run on the finished tree (after the review fixes), on this Mac (Apple
silicon, Go 1.27.2, Apple `container` 1.5.0, Squid 7.7, git 2.54).

| Check | Command | Result |
|---|---|---|
| Unit suite, race, coverage | `go test -race -count=1 -covermode=atomic -coverprofile ./...` | every package ok, 58.9s wall; total 77.4% (`go tool cover -func`), 77.1% (`tools/covercheck`, floor 70%) |
| Static analysis | `make lint` (staticcheck, pinned tool module) | clean |
| Tag-gated compile | `go vet -tags integration,squidlive,squide2e ./...` | clean |
| Formatting | `gofmt -l` over tracked and new Go files | clean |
| VM red team | `go test -tags=integration -run TestRedteam_ -v ./tests/...` | 11 of 11 passed, 0 skipped (A1 x4, A2 x2, A7 x3, A8, V1), 29.9s |
| Live squid | `go test -tags squidlive -run TestSquidProxyAuth_Live ./internal/netfw/` | passed with the widened ACL (1.6s) |
| Reviewer's revert table | section 9 | every new test fails with its fix reverted |

Coverage before and after (statement, per `go test -cover`):

| Package | Before | After |
|---|---|---|
| total | 77.1% | 77.4% |
| internal/gateway | 93.2% | 94.6% |
| internal/config | 93.7% | 94.1% |
| internal/stage | 77.9% | 78.0% |
| internal/webui | 84.1% | 84.4% |
| internal/remote | 89.0% | 88.6% (new `canonicalHost`/`repoHost` branches, one untested error path) |
| internal/broker | 88.8% | 88.8% |
| cmd/drydock | 58.5% | 59.3% |
| internal/strictjson (new) | n/a | 100% |

The small movement in line coverage is expected: the sprint's gains are in
what the tests can detect (mutation efficacy, section 6) and in the defects
fixed (section 4), not in lines executed. The untested areas that remain are
the CLI shells that call `os.Exit` or launchctl (`cmd/drydock/daemon.go`,
`init.go` platform checks, `redteam.go`), `cmd/brokerd/main.go` boot wiring,
and the darwin quota attach path, each exercised by the release QA script and
the VM suite rather than by unit tests.

Flakiness: no new test sleeps on the success path; the two symlink-refusal
tests poll with a 5s ceiling. The reviewer's `-race -count=3 -shuffle=on` run
of `internal/broker` and `-count=2` of every other changed package was clean.

Not run: the launchd round trip (`daemon_integration_test.go`), the
credentialed Gemini and Codex integration tests, `squide2e`, and the first
scheduled GitHub run of the new macOS and nightly jobs.

## 12. Final quality assessment

- **Specification conformance.** 110 requirement rows; after this sprint 93
  are VERIFIED by an executed test, 9 PARTIALLY VERIFIED, 5 NOT VERIFIED (one
  accepted, one browser-only, three documented absences), and all 9
  CONTRADICTORY rows are resolved (4 in code, 5 in documentation).
- **Correctness.** Ten defects fixed (D-1 to D-10), each with a regression
  test that was watched to fail first or under a controlled mutation. Two were
  HIGH against the approval and verification guarantees (the verifier and the
  reviewer could both be shown bytes other than the ones pushed); none was
  reachable without first passing the task-submission trust boundary.
- **Reliability.** Suite green under race, shuffle and repetition; unit tests
  can no longer reach the real container runtime; the integration harness no
  longer kills an operator's live squid.
- **Test effectiveness.** Mutation efficacy rose from 74% to 83% (gateway),
  79% to 86% (config), 80% to 100% (egress), 83% to 89% (globmatch); every
  surviving gateway mutant was read and found behaviourally equivalent.
- **Security validation.** The red-team suites (host and VM) pass on the
  changed tree; the new tests close export-attribute, parser-differential,
  batch-route, SSRF-range, symlink and hook-neutralisation gaps. Section 11
  lists what remains unproven.
- **Regression risk.** Behaviour changes an operator could notice: bodies with
  trailing bytes after the JSON object are now 400; Gemini batch actions are
  403; `www.`-less host matching for platform autodetect is exact; LFS
  repositories no longer run `git-lfs clean`/`smudge` on the host during
  staging, so a new large binary committed by an agent is pushed as a plain
  blob rather than an LFS pointer (fail-safe, documented in the changelog).

Outstanding risks: **HIGH** none known. **MEDIUM** items 1 to 3 of section 11
(verifier distinguishability, advisory non-required verification, operator git
config reaching stage operations). **LOW** items 4 to 12.

## 11. WHAT COULD STILL BE BROKEN?

Blind spots that remain after this sprint, with what it would take to close
each. These are statements of what is **not** proven, not suspicions of
defects.

1. **The verifier is distinguishable.** The export now carries the staged
   bytes, but the verifier VM has no network and no credentials, so agent code
   can detect "I am being verified" and behave differently. No export fix
   changes that; only a verifier that is indistinguishable from the agent VM
   would, which is an architecture decision (THREAT_MODEL A9 already frames
   verification as a check, not a proof).
2. **Non-required verification is advisory.** An export failure, a timeout or
   an inconclusive run blocks the push only under `required: true`
   (`verify.go:107`). On macOS an agent can force "inconclusive" by committing
   `._x` beside `x` (bsdtar treats it as an AppleDouble sidecar). Operators who
   rely on verification should set `required: true`; making export failure
   terminal regardless is a product decision left open.
3. **The host git still runs with the operator's full config.**
   `gitHardenedEnv` sets prompt and SSH variables only. This sprint neutralised
   the tree-selectable attributes and diff drivers, but `core.attributesFile`,
   `init.templateDir`, credential helpers and `url.<base>.insteadOf` rewrites
   still apply to the staging clone and the push. Closing it means an explicit
   `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_NOSYSTEM` policy for every stage git call,
   with a decision about which operator settings (SSH command, credential
   helper) must survive. Not done here because it changes how pushes
   authenticate.
4. **SSRF ranges other than loopback are proven at the config level only.**
   The live squid test (CI) shows the deny beats the allowlist for
   `localhost`; `0.0.0.0/8`, RFC 1918, link-local and CGNAT are asserted as
   ACL fields and parsed by Squid 7.7, not exercised with a resolving
   hostname. A live case needs a name that resolves to such an address under
   the conf's pinned resolvers (`1.1.1.1`, `8.8.8.8`); `*.nip.io` would do but
   makes CI depend on a third party.
5. **IPv6 fail-closed in the VM has no probe.** `table inet fw` is read, not
   tested. A VM test issuing an IPv6 connect would close it.
6. **A1 VM tests check `grant.EnvVars()`, not `buildTaskEnv`.** The host-side
   `TestHandleTask_RealKeyNeverEntersContainerEnv` covers the real env builder,
   but the VM-side tests would not notice a key leaking through a new env var
   or mount added to `BuildRunArgs`. Routing the VM tests through
   `buildTaskEnv` and `BuildRunArgs` would make them non-tautological.
7. **The Gemini CLI against the narrowed allowlist.** The four actions match
   `image/write-gemini-config.sh` and the SDK's calls; the pinned 0.63.0 binary
   was not run against the gateway in this sprint. `make test-integration`
   with `GEMINI_API_KEY` set exercises it.
8. **`internal/broker` has no mutation score.** Its suite is 45s per run, so
   gremlins was not run on it. The targeted mutations in section 6 and the
   review's revert table cover the changed lines only. A nightly gremlins job
   on a subset of broker files would be the next step.
9. **Absence assertions still sleep.** About 17 tests assert "nothing
   happened" after a 40 to 80ms sleep. They pass on a slow runner whether or
   not the regression exists. Converting them needs a dispatcher tick counter.
10. **The macOS CI job and the nightly shuffle have not run yet.** The suite
    passes under `-race -shuffle=on -count=3` locally on an M-series Mac and
    the hdiutil quota tests pass here; the first scheduled run is the proof.
11. **`AUDIT_ROOT` permissions are not re-tightened.** An existing audit dir
    wider than 0700 stays wider (accepted, same reasoning as the previous
    sprint's `sockpath.EnsureParent` scoping).
12. **Documented absences remain absent:** audit retention, log rotation,
    remote approval adapters (GAP-1 to GAP-3).
