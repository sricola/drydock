# cmd-coverage report

Branch: test/cmd-coverage  
Date: 2026-07-04

## Coverage before → after

| Package           | Before | After |
|-------------------|--------|-------|
| cmd/brokerd       | 33.7%  | 42.2% |
| cmd/docs-build    | 34.2%  | 39.5% |
| cmd/drydock       | 34.6%  | 35.2% |

## Functions tested

### cmd/brokerd — `main_test.go` additions

**`hardenedServer`** (`TestHardenedServer_Timeouts`)  
Asserts ReadHeaderTimeout=10s, IdleTimeout=60s, and that ReadTimeout/WriteTimeout remain unset (preserving the property that POST /tasks can block indefinitely and the gateway can stream responses). Catches any accidental body-timeout addition.

**`findEgressConfig`** (`TestFindEgressConfig_EnvVarTakesPrecedence`, `_UserFileFound`, `_NoneFound`)  
- Precedence test: EGRESS_CONFIG env var wins even when HOME is set to a clean tmpdir.  
- User-file test: ~/.drydock/egress.yaml (under a tmp HOME) is found and returned.  
- Not-found test: with EGRESS_CONFIG="", clean HOME, clean HOMEBREW_PREFIX, and CWD changed to an empty tmpdir, findEgressConfig returns an error whose message contains "tried" (so operators can diagnose).  
Security-relevant: wrong precedence would silently load a seed template instead of the operator's edited config.

**`checkContainerVersion`** (`TestCheckContainerVersion_ValidVersion_LogsInfo`, `_MajorMismatch_NonStrict_Warns`, `_UnparseableOutput_NonStrict_Warns`)  
Injects fake `runCmd` returning controlled version strings. Uses a captured slog handler to assert log output:  
- Valid version (1.2.3) → slog.Info with "container CLI" and "1.2.3".  
- Major mismatch (2.0.0), non-strict → slog.Warn containing "not in tested range".  
- Unparseable output, non-strict → slog.Warn containing "could not parse".  
The strict=true + mismatch/error branches call fatal() → os.Exit; those are skipped (see Skipped section).

### cmd/docs-build — `main_test.go` (new file)

**`rankSlug`** (`TestRankSlug_KnownSlugsReturnIndex`, `_UnknownSlugRanksLast`, `_FirstBeforeLast`)  
- Every slug in `order[]` returns its exact table index; consecutive entries are strictly ordered.  
- An unknown slug returns len(order)+1, which is > every known slug's rank.  
- First entry ranks strictly before last entry.  
These tests would fail if a slug were inserted at the wrong position, if the unknown-fallback changed, or if the order slice were accidentally truncated.

### cmd/drydock — `doctor_test.go` addition

**`lastLine`** (`TestLastLine`)  
Six cases: single line, trailing newline, multi-line, trailing-newline multi-line, whitespace trimming, and the realistic container-run preamble case (`[6/6] Starting container [0s]\n0.49.0`). Verifies the helper extracts the last non-empty, trimmed line — the decision used to show a clean version in `drydock doctor` output.

### cmd/drydock — `client_test.go` additions + seam in `client.go`

**`printClientErr`** (`TestPrintClientErr_BrokerDown_PrintsFriendlyHint`, `_OtherError_PrintsRawError`)  
- Down test: BROKER_SOCKET points to a missing file, BROKER_ADDR="", HOME isolated → brokerdDown returns true → output contains brokerDownHint. Catches regressions where the friendly hint is omitted.  
- Other-error test: BROKER_ADDR set (TCP mode) → brokerdDown returns false → output contains raw error text, NOT the hint. Catches the opposite regression (hint printed when it shouldn't be).

**Seam added:** `var errOut io.Writer = os.Stderr` in `cmd/drydock/client.go`. `printClientErr` now writes to `errOut` instead of `os.Stderr` directly. Production initialisation is unchanged (`os.Stderr`); tests swap it to a `*bytes.Buffer` and restore it in `t.Cleanup`. This is the only production-code change; it does not alter runtime behaviour.

## Skipped as untestable glue

- **`checkContainerVersion` strict=true branches** — call `fatal()` → `os.Exit(1)`. Testing would require a subprocess harness (exec.Command the test binary); skipped as disproportionate cost for the incremental coverage.
- **`checkPlatform`** — uses `runtime.GOOS/GOARCH` (not injectable without a seam) and calls `os.Exit`. The non-exit path (darwin/arm64 with a valid sw_vers) runs correctly on CI. The fail paths call os.Exit; subprocess tests would be needed.
- **`checkBinary`** — calls `exec.LookPath` + `os.Exit`. No runCmd seam; would need `var lookPath` seam. Skipped: the assertable behaviour is LookPath returning a path or not, which is OS-dependent and trivially thin logic.
- **`checkSquid`** — wraps `netfw.FindSquid()` + `os.Exit`. Same category as `checkBinary`.
- **`ensureContainerSystem`**, **`ensureNetwork`**, **`ensureImage`** — all use `exec.Command` directly (not the `runCmd` seam) and call `os.Exit`. Would require their own seam variables. Skipped: these are setup-phase glue with no in-process branching logic worth asserting beyond the shell calls themselves.
- **`nudgeStaleSandboxImage`** — loads config and delegates to `sandboxImageNudge`, which is already fully tested in `init_test.go`. The remaining glue (error return on bad config) is a one-liner with no assertable decision of its own.
