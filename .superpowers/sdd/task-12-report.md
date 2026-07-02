# Task 12 Report: Split broker.go, Decompose HandleTask, resolveAgent → error

## File layout after split (line counts)

| File | Lines | Responsibility |
|------|-------|----------------|
| `broker.go` | 530 | Task/Broker/taskStage/seams, HandleTask + helpers, resolveAgent |
| `taskstate.go` | 190 | TaskStage/TaskState, slot management, register/stage/egress/cancel |
| `gates.go` | 151 | awaitGate, gateEgressWiden, summariseExtras, gatePush, notifyMac |
| `admin.go` | 117 | HandleApprove/Deny/Pending/Tasks/Health/Kill, signal, writeJSON |
| `text.go` | 95 | firstLine, prContent, safeErr, safeStr, errorEvent |
| `model.go` | 36 | taskModelFor, effectiveDefaultModel, modelEnv |
| `build_task_env_test.go` | 88 | Direct unit tests for buildTaskEnv |
| `resolve_agent_test.go` | 74 | Updated for new error-returning signature |

Previous `broker.go` was 1043 lines; now split into 6 files totalling ~1119 lines of code (added comments + white space in new files, plus the helpers).

## HandleTask helpers extracted

### `runEgressGate(ctx, taskID, extras, sw) bool`
Encapsulates the `awaiting_egress` gate: checks `RequiresApproval`, emits stage event, calls `gateEgressWiden`, clears `EgressExtra`. Returns `true` to continue, `false` to abort (terminal event already emitted). Lives in `broker.go`.

### `buildTaskEnv(grantEnv, proxyAuth, gatewayIP, proxyPort, agentName, taskModel, openAICompatModel, operatorDefaultModel, taskVendor) []string`
Pure function (no Broker receiver). Assembles the complete container env slice: grant vars, proxy vars, NO_PROXY, DRYDOCK_GW_IP, DRYDOCK_MODEL (via modelEnv/taskModelFor/effectiveDefaultModel), DRYDOCK_AGENT. Lives in `broker.go`.

### `runSandbox(ctx, taskID, agentName, taskModel, args, logf, auditPath, grant, sw) (time.Time, bool)`
Runs the agent container (with timeout), emits the `running` stage event, handles force-delete on failure, writes synthetic result lines to the audit log for error and non-claude success cases. Returns `(taskStart, true)` on success; `(taskStart, false)` if a terminal event was emitted and the caller should return. Lives in `broker.go`.

### `pushAndOpenPR(ctx, taskID, diff, instruction, autoApprove, draft, sw, st, repoRef, platform, taskStart, auditPath)`
Handles the diff-gate (`gatePush`), branch push (`st.Push`), and PR creation (`adapter.OpenRequest`). Always emits a terminal event (result/denied/cancelled or error on push failure). HandleTask has no code after calling this — it is the last step. Lives in `broker.go`.

## resolveAgent signature change

**Old:** `(name string, prov creds.Provider, status int, msg string)` — caller checked `status != 0`.

**New:** `(name string, prov creds.Provider, err error)` — caller checks `err != nil`.

Error messages are identical to the original strings:
- Unknown agent: `"unknown agent: %s (want %s)"` (same text, now `fmt.Errorf`)
- Missing provider: `"agent unavailable — no API key configured for %s"` (same)

Caller in HandleTask:
```go
// old
agentName, prov, status, msg := b.resolveAgent(t.Agent)
if status != 0 { sw.emit(errorEvent(taskID, msg, "")); return }

// new
agentName, prov, err := b.resolveAgent(t.Agent)
if err != nil { sw.emit(errorEvent(taskID, err.Error(), "")); return }
```

`resolve_agent_test.go` updated: `wantStatus int` → `wantErr bool`; `msg` checks now use `err.Error()`. All same scenarios covered.

## buildTaskEnv unit test

`build_task_env_test.go` (88 lines, 3 subtests):

1. **TestBuildTaskEnv_ContainsExpectedVars** — real key absent, bearer present, HTTPS_PROXY/HTTP_PROXY/NO_PROXY/DRYDOCK_GW_IP correct, operator default model forwarded for anthropic lane, DRYDOCK_AGENT set.
2. **TestBuildTaskEnv_ProxyAuthIncluded** — per-task `user:secret@` credential spliced into proxy URLs.
3. **TestBuildTaskEnv_OpenAICompatModelNotLeaked** — operator default absent for `openai-compat` vendor; `openai_compat.model` forwarded instead.

The A1 real-key-absence assertion from `TestHandleTask_RealKeyNeverEntersContainerEnv` (Task 6) still passes via the end-to-end path (unchanged). The new test pins the same invariant at the pure-function level.

## What was left inline (and why)

HandleTask still contains:
- JSON decode/validate, URL validate, slot acquire, register/defers (~30 lines) — these are linear pre-flight checks with HTTP error returns; extracting them would add indirection with no benefit.
- audit dir setup / log open (~20 lines) — uses local `logf` variable consumed by runSandbox; extracting would require returning the file handle.
- drydock_meta write (~5 lines) — one-liner band between audit setup and env assembly.
- diff capture + no_diff early return (~12 lines) — between runSandbox and pushAndOpenPR; too small to extract cleanly.

All extracted helpers are named and testable; HandleTask is now ~100 lines vs 290.

## Test results

```
go test -race ./... -count=1
ok  drydock/internal/broker  1.335s  (all subtests pass)
ok  all other packages pass
```

`go build ./...` and `go vet ./...` both clean. `gofmt -l ./internal/broker/` produces no output.
