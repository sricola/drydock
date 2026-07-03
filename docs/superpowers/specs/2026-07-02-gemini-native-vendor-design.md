# Native Gemini Vendor (ROADMAP 3B, phase 2) — Design

**Status:** design approved 2026-07-02. Gated on the phase-1 spike, which returned GREEN (`docs/superpowers/specs/2026-07-02-gemini-spike-findings.md`).

## Goal

Add a native `gemini → google` vendor so drydock brokers Google's Gemini API directly (Google auth header, native token metering, the official Gemini CLI) — the ROADMAP's proof that a new native provider is essentially one `provider.Registry` row plus its vendor plumbing. Gemini already works via the openai-compat lane; this is the first-class native path.

## Decisions (brainstorming + staff)

- **API-key auth only.** No Google OAuth / Code Assist. `GEMINI_API_KEY` is brokered: the VM gets a per-task bearer, the gateway swaps in the real key. Opt-in — the backend is built only when `GEMINI_API_KEY` is present.
- **Native metering.** A Gemini `usageMetadata` parser + a per-model price table so `task_budget_usd` works natively.
- **Default model `gemini-2.5-pro`** (coding-capable flagship; overridable via `--model` / `default_model`). The entrypoint always passes an explicit `-m` — the spike found the auto-model router's classifier calls otherwise blow the task timeout.
- **Flat per-model pricing** (the ≤200k-context tier). The >200k tier is out of scope: the budget is a safety cap, not billing. Documented as approximate.
- **Pin `@google/gemini-cli@0.49.0`** (the spike-validated version).
- **Disable all CLI phone-home** (telemetry, usage stats, update checks) in the written settings, to keep the sandboxed CLI within deny-by-default egress.

## Architecture

### 1. Gateway admission from `x-goog-api-key` (the one load-bearing change)

`internal/gateway/gateway.go` `ServeHTTP` currently extracts the per-task bearer only from `Authorization: Bearer <tok>`. The Gemini CLI, in API-key mode, sends its key **only** in the `x-goog-api-key` header (spike-confirmed) — so a Gemini request would be rejected 401 before reaching the vendor. Change: extract the token from `Authorization: Bearer` **or**, if absent, `x-goog-api-key`. Everything downstream (constant-time `admit`, lease lookup, vendor selection) is unchanged.

- Safe: claude/codex/opencode send `Authorization: Bearer` and are untouched; this only adds a second source. The token is validated identically.
- Vendor-agnostic: admission still happens before vendor selection (the token names the lease, the lease names the vendor).
- Precise extraction: `Authorization: Bearer <x>` wins if present; else `X-Goog-Api-Key: <x>`; else empty (→ 401 as today).

### 2. `GoogleVendor()` — `internal/gateway/vendor.go`

```
Name:    "google"
BaseURL: "https://generativelanguage.googleapis.com"
Inject:  func(r, realKey) {
    r.Header.Del("Authorization")     // drop the inbound bearer
    r.Header.Del("x-goog-api-key")    // drop the inbound bearer-as-key
    // Defensive: the CLI sends the key in the header, but strip any ?key= too
    // so a per-task bearer never leaks upstream in the query string.
    stripQueryParam(r.URL, "key")
    r.Header.Set("x-goog-api-key", realKey)
}
ParseUsage: parseGoogleUsage
Prices:     GooglePrices()
// no BasePath, no StripFields
```

The VM posts to `{GOOGLE_GEMINI_BASE_URL}/v1beta/models/<model>:generateContent` (or `:streamGenerateContent?alt=sse`); the director rewrites scheme/host to the upstream and forwards the path byte-identical (no `BasePath`), so it lands correctly on `generativelanguage.googleapis.com`.

### 3. `parseGoogleUsage` — `internal/gateway/usage.go`

Handles non-streaming JSON and SSE (`text/event-stream`), keep-last for streams (mirrors `parseOpenAIUsage`). Reads:
- `usageMetadata.promptTokenCount` → input tokens
- `usageMetadata.candidatesTokenCount` (+ any `thoughtsTokenCount`, if present, folded into output) → output tokens
- `modelVersion` → model (fallback: the request's model if the response omits it)

The existing `usageMarker = "usage"` substring matches `usageMetadata`, so the streaming tee already retains the usage-bearing SSE lines — no change to `usageReader`.

### 4. `GooglePrices()` — `internal/gateway/pricing.go`

A `map[string]Price` keyed by the `modelVersion` strings Gemini returns (`gemini-2.5-pro`, `gemini-2.5-flash`, `gemini-2.5-flash-lite`), input/output USD per 1M tokens (≤200k tier), plus a `"default"` fallback row so an unlisted model still meters (non-zero) rather than running uncapped. Exact values filled from Google's published pricing at implementation time and marked approximate in a comment.

### 5. The registry row — `internal/provider/provider.go`

```
{
  Agent: "gemini", Vendor: "google", Label: "Gemini (Google)",
  APIKeyEnv: "GEMINI_API_KEY", AuthCmd: "",       // api-key only; no auth subcommand
  BaseURLEnv: "GOOGLE_GEMINI_BASE_URL", TokenEnv: "GEMINI_API_KEY",
  APIVendor: gateway.GoogleVendor,
  NoOperatorDefault: true,  // operator default_model is claude/codex-oriented;
                            // must not leak into Gemini (same reason as opencode).
  // OAuthBackend/OAuthFile/LoadOAuthSnap nil; ConfigBuilt false; NeedsModel false
  // (the entrypoint supplies the gemini-2.5-pro default when DRYDOCK_MODEL is empty).
}
```

Model resolution consequence: a per-task `--model gemini-2.5-flash` flows through
`taskModelFor` as normal; with no task model, `effectiveDefaultModel("google")`
returns `""` (because `NoOperatorDefault`), so `DRYDOCK_MODEL` is empty and the
entrypoint's `gemini-2.5-pro` default applies — the operator's claude/codex
`default_model` never reaches the Gemini CLI.

What the row auto-wires (verified against current code — this is the "one row" payoff):
- **Agent validation / CLI** — `provider.Agents()` includes `gemini`; `submit --agent gemini`, the wizard menu, and top-level help pick it up.
- **Backend build** — `buildBackends` hits `switch cfg.AuthMode("google")`; `AuthMode` returns `""` for google (no `google_auth` field exists), so it falls to the `default: // api_key` branch and appends `{GoogleVendor(), StaticKey(GEMINI_API_KEY)}` **only if the key resolves**. No subscription path is even representable — so no config-validation change is needed.
- **VM env injection** — brokerd builds a `gateway.Provider` from the row's `BaseURLEnv`/`TokenEnv`; `grant.EnvVars()` yields `GOOGLE_GEMINI_BASE_URL=<gateway>` and `GEMINI_API_KEY=<bearer>` into the VM.
- **Squid exclusion** — `provider.GatewayHosts()` derives the gateway-fronted host set from `APIVendor().BaseURL`, so `generativelanguage.googleapis.com` is automatically excluded from the squid allowlist (the VM reaches it via the gateway, not squid).
- **api-keys.env** — `knownAPIKeys` is registry-derived, so `GEMINI_API_KEY` is a recognized host-side key automatically.

Manual additions beyond the row: the gateway vendor/parser/prices (§2–4), the gateway admission change (§1), the image install + entrypoint (§6), the doctor line (§7), and tests (§8).

### 6. Image + entrypoint — `image/`

- `image/Dockerfile`: add `ARG GEMINI_CLI_VERSION=0.49.0` and `RUN npm install -g @google/gemini-cli@${GEMINI_CLI_VERSION} && gemini --version`; `COPY write-gemini-config.sh` + chmod.
- `image/write-gemini-config.sh`: writes `$GEMINI_DIR/settings.json` with `{"security":{"auth":{"selectedType":"gemini-api-key"}}, <telemetry/usage-stats/update-check all disabled>}`. Exact settings keys confirmed against the pinned CLI at implementation time (the findings doc names the mandatory `selectedType`; the phone-home-disable keys are the build task's to confirm).
- `image/entrypoint.sh`: new `gemini)` case, modeled on `opencode)`:
  - `: "${GOOGLE_GEMINI_BASE_URL:?}"`; `MODEL="${DRYDOCK_MODEL:-gemini-2.5-pro}"`.
  - `export XDG_CONFIG_HOME`/`GEMINI_DIR` under `/home/agent` (never `/work`, so config never pollutes the diff); write settings via the script; `chown` to agent.
  - `export GEMINI_CLI_TRUST_WORKSPACE=true`.
  - `exec gosu agent env HOME=/home/agent GEMINI_DIR=... GOOGLE_GEMINI_BASE_URL=... GEMINI_API_KEY=... gemini -p "$PROMPT" -m "$MODEL" --skip-trust` (plain-text output — the spike-validated path; `-o stream-json` deferred until validated in-sandbox).

The key (`GEMINI_API_KEY`) and base URL arrive as VM env via the grant (§5); the CLI sends the key in `x-goog-api-key` to the gateway, which admits it (§1) and swaps in the real key (§2).

### 7. Doctor — `cmd/drydock/doctor.go`

Add a `gemini present` check mirroring the `codex present` block: `container run … gemini --version` against the sandbox image, with a "predates native Gemini; run `drydock init` to rebuild" hint on absence. (Doctor's per-CLI presence checks are hardcoded, not registry-driven; a targeted line is the in-pattern change — a registry-driven refactor of doctor is explicitly out of scope.)

## Data flow (native gemini task)

1. `drydock submit --agent gemini` → broker validates agent via the registry, mints a gateway lease for vendor `google`, injects `GOOGLE_GEMINI_BASE_URL=<gw>` + `GEMINI_API_KEY=<bearer>` into the VM env, sets `DRYDOCK_AGENT=gemini`, `DRYDOCK_MODEL` (if set).
2. entrypoint `gemini)` writes settings, runs the CLI headless with the bearer.
3. CLI → `{gw}/v1beta/models/<model>:streamGenerateContent`, `x-goog-api-key: <bearer>`.
4. Gateway `admit`s the bearer (read from `x-goog-api-key`), `Inject` swaps in the real `GEMINI_API_KEY`, director forwards to `generativelanguage.googleapis.com`.
5. `meter` tees the SSE, `parseGoogleUsage` reads `usageMetadata`, cost added to the lease vs `task_budget_usd`.
6. Diff captured, human gate, push — unchanged.

## The residual risk & its mitigation (egress)

The spike ran the CLI on the **host**. In the sandbox, egress is deny-by-default: the VM can reach only the gateway (`:8088`) and squid (`:3128`). If the pinned CLI contacts any other host (telemetry, auth discovery, update check) even with phone-home disabled, the sandboxed run fails. Mitigation: disable every phone-home in settings, and make the **final build task a full end-to-end sandboxed run** (macOS/Apple-container) that proves no extra egress is needed. If one host proves unavoidable, resolve by the narrowest means — the disabling flag if it exists, else an explicit, documented squid allowlist entry for that exact host. This is the single most likely place for iteration and is only fully verifiable on macOS.

## Test plan

**CI (unit, no VM, no spend):**
- `internal/gateway`: `GoogleVendor.Inject` sets `x-goog-api-key=realKey` and removes the inbound bearer + any `key=` query; `ServeHTTP` admits a token presented in `x-goog-api-key` (and still admits `Authorization: Bearer`); a wired `ServeHTTP` test through `GoogleVendor` against a fake upstream asserts the real key is injected and the bearer is not forwarded (A1 at the unit layer).
- `parseGoogleUsage`: non-streaming + SSE, `usageMetadata` → (in,out), `modelVersion` → model, keep-last on streams.
- `GooglePrices()`/`cost()`: a known usage meters to the expected USD.
- `internal/provider`: the `gemini`/`google` row is present and complete; `GatewayHosts()` now includes `generativelanguage.googleapis.com`; `knownAPIKeys` includes `GEMINI_API_KEY`.
- `cmd/brokerd`: `buildBackends` adds the google backend when `GEMINI_API_KEY` is set and omits it when absent.

**macOS-gated (integration/red-team, `make test-integration`, not CI — matches the existing A1/A2 VM tests):**
- A1: a real gemini task's VM env carries the bearer, never the real `GEMINI_API_KEY`; the gateway forwards the real key upstream.
- A2: the gemini sandbox enforces deny-by-default egress (the CLI reaches only the gateway + squid).
- End-to-end: a real gemini task runs through the gateway to completion (this is the egress-risk verification).

## Scope boundaries (YAGNI)

Out: Google OAuth / Code Assist; the >200k-context pricing tier; config-declared providers (ROADMAP 3D); JSON/stream-json output UX; a registry-driven refactor of doctor's CLI checks.

## Done

Native Gemini runs end-to-end through the gateway, added via a single registry row (plus the shared vendor plumbing), with CI-green gateway/usage/pricing/registry unit tests and a macOS-gated A1/A2 + end-to-end integration test — the same bar and CI reality under which claude, codex, and opencode shipped. Docs (`models.md`, `authentication.md`, `configuration.md`) and the CHANGELOG note the native lane.

## File map

- Modify: `internal/gateway/gateway.go` (admission), `vendor.go` (GoogleVendor + `stripQueryParam` helper), `usage.go` (parseGoogleUsage), `pricing.go` (GooglePrices)
- Modify: `internal/provider/provider.go` (the row)
- Modify: `image/Dockerfile`, `image/entrypoint.sh`; Create: `image/write-gemini-config.sh`
- Modify: `cmd/drydock/doctor.go` (gemini presence)
- Tests: `internal/gateway/*_test.go`, `internal/provider/provider_test.go`, `cmd/brokerd/backends_test.go`, and a macOS-gated `tests/integration/` gemini case
- Docs: `site/docs/models.md`, `authentication.md`, `configuration.md`, `CHANGELOG.md`
