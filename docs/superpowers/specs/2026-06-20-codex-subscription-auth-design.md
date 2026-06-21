# Codex (ChatGPT subscription) auth — design spec

**Date:** 2026-06-20
**Status:** Approved (brainstorming) — ready for implementation plan
**Parallel of:** `docs/superpowers/specs/2026-06-20-claude-subscription-auth-design.md`
(the shipped Claude Pro/Max subscription OAuth feature, PR #52)

## Goal

Let an operator run drydock **Codex** agent tasks with **no `OPENAI_API_KEY`**,
using their ChatGPT (Plus/Pro/Team) subscription via the credential that
`codex login` already produced — while preserving drydock's core invariant:
**the real upstream credential never enters the sandbox VM.**

## Background

drydock already runs Codex as a sandbox agent when an `OPENAI_API_KEY` is set
on the broker host (`agent: codex` → vendor `openai`; `cmd/brokerd/main.go`
builds `OpenAIVendor()` + `StaticKey`). The credential gateway mints a per-task
`tok_` bearer that the VM sees and swaps the real key in host-side when proxying
to `api.openai.com`.

This feature adds a **subscription** auth mode for the OpenAI/Codex side, exactly
mirroring the just-shipped Claude subscription mode (`anthropic_auth:
subscription`). The Claude OAuth machinery — `OAuthCred`, `CredSnapshot`,
`CredStore`/`FileCredStore`, the request-count cap (`task_max_requests`), the
per-task `drydock_meta` audit label, the `drydock doctor` health step, and the
red-team isolation test — is reused.

## Key difference from the Claude path (why a spike gates this)

The Claude subscription endpoint was the **same host** as the API
(`api.anthropic.com`); only the auth header changed (Bearer + `anthropic-beta`,
strip `context_management`). Codex-via-ChatGPT diverges in **three ways at once**:

1. **Different upstream host/path.** A ChatGPT-authed Codex talks to the Codex
   backend (`https://chatgpt.com/backend-api/codex/...`), not
   `api.openai.com/v1/...`.
2. **Extra header.** It sends a `chatgpt-account-id` header (the account id from
   the login), in addition to `Authorization: Bearer <oauth access token>`.
3. **Possible request/response shape differences.** The ChatGPT backend may not
   accept a byte-identical standard Responses-API request.

So the gateway must **translate** an `api.openai.com`-style request (what the
VM's Codex CLI, running in API-key mode against the gateway, emits) into a
Codex-backend request. Whether that translation is clean enough to do in a proxy
is the **entire go/no-go question** of the validation spike (Task 1).

The exact wire facts below (endpoints, client id, header names, `auth.json`
shape) are the author's current understanding and **MUST be confirmed by the
spike against a real `codex login`** before any downstream task relies on them.
They are starting points, not settled constants.

## Architecture

Host-side only. Inside the VM, Codex CLI always runs in **API-key mode** pointed
at the gateway with a per-task `tok_` bearer — it never knows a subscription is
involved. The subscription-ness lives entirely host-side:

```
VM: codex CLI ──(OPENAI_API_KEY=tok_…, base=gateway)──▶ gateway (host)
                                                          │ resolve OAuthCred.Current()
                                                          │ inject Bearer <oauth> + chatgpt-account-id
                                                          │ rewrite host/path → Codex backend
                                                          ▼
                                              chatgpt.com/backend-api/codex/…
```

Real tokens live only in: host process memory (`OAuthCred.snap`), the `0600`
file (`~/.drydock/codex-oauth.json`), and the transient upstream request. Only
`tok_` ever enters the VM.

### Gateway modeling (Approach A — chosen)

Add one vendor, `OpenAIOAuthVendor(accountID string)`:
- `BaseURL` = Codex backend host (spike-confirmed).
- `Inject(r, secret)` sets `Authorization: Bearer <secret>` and
  `chatgpt-account-id: <accountID>`, removing any inbound `X-Api-Key`.
- Reuse `parseOpenAIUsage` and `OpenAIPrices()`.
- `account_id` is **stable across token refreshes**, so brokerd captures it once
  at backend construction and the `Inject` closure injects it. **No change to the
  `Credential` interface.**

If the spike shows a path remap is required (e.g. `/v1/responses` →
`/backend-api/codex/responses`), add a minimal `BasePath` field to `Vendor`,
joined in the proxy director — not a per-vendor hack. If the spike shows the
ChatGPT backend rejects fields the API accepts (the Codex analogue of
`context_management`), reuse the existing `StripFields` mechanism.

Rejected alternatives: **B** — a separate Codex backend with its own proxy logic
(duplicates metering/cap/budget plumbing); **C** — enriching the `Credential`
interface to return token + headers (speculative generality for one non-rotating
header; churns the interface the Claude path just stabilized).

## Components (files)

All mirror the Claude path unless noted.

- **`internal/gateway/codex_oauth.go`** *(new)* — `refreshOpenAI(refreshToken)
  (CredSnapshot, error)` hitting the OpenAI OAuth token endpoint with the Codex
  client id. Reuse `OAuthCred`, `CredSnapshot`, `FileCredStore` unchanged. The
  `account_id` is carried alongside the snapshot (decision: add an optional
  `account_id` field to `CredSnapshot`, or a small Codex carrier struct — settled
  in the spike based on whether refresh returns/needs it).
- **`internal/gateway/codex_constants.go`** *(new)* — `openaiOAuthClientID`,
  `openaiOAuthTokenURL` (`var` for test overrideability, as with Anthropic), the
  Codex backend base URL/path, and any required static headers. All pinned by the
  spike.
- **`internal/gateway/vendor.go`** — add `OpenAIOAuthVendor(accountID string)`.
  Add `BasePath` to `Vendor` only if the spike requires a path remap.
- **`cmd/brokerd/main.go`** — build the OpenAI backend per `cfg.OpenAIAuth`:
  `subscription` → load `~/.drydock/codex-oauth.json` (die-with-hint on load
  error) + `OpenAIOAuthVendor(accountID)` + `NewOAuthCred` (refresh =
  `refreshOpenAI`) + `Budget = math.MaxFloat64`; `api_key` path **byte-for-byte
  unchanged**. Boot guard treats a Codex subscription as satisfying the OpenAI
  side. Every Provider keeps `MaxRequests = cfg.TaskMaxRequests`.
- **`cmd/drydock/auth.go`** — `drydock auth codex`: read `~/.codex/auth.json`,
  parse `tokens.{access_token,refresh_token,account_id}` + expiry, write
  `~/.drydock/codex-oauth.json` (0600, atomic temp+rename), token-free status,
  `--status`. **Never prints tokens.** Registered in dispatch/subHelp/usage.
- **`internal/config/config.go`** — `OpenAIAuth string` (default `api_key`, yaml
  `openai_auth`, env `DRYDOCK_OPENAI_AUTH`); `validate()` rejects values not in
  {`api_key`, `subscription`}; both in `SeedTemplate` + `config/config.yaml`.
  `task_max_requests` already applies; `task_budget_usd` is a no-op in
  subscription mode (no metered spend), same as Claude.
- **`cmd/drydock/doctor.go`** — a "codex subscription → token valid" step (loads
  cred, `OAuthCred.Current()` validates/refreshes, token never printed), skipped
  in api_key mode.
- **`cmd/drydock/start.go`** — extend `agentCredentialAvailable` so a Codex
  subscription satisfies the OpenAI side (no `OPENAI_API_KEY` required when
  `openai_auth: subscription`).
- **`internal/broker/broker.go`** — the per-task `drydock_meta` line already
  records subscription-ness for the Anthropic vendor; extend the same line to
  cover the OpenAI/codex vendor so `drydock tasks` labels Codex-subscription runs
  correctly. No Codex-specific meta type.

## Data flow

**Setup (once):** `codex login` (operator) → `drydock auth codex` copies the
credential into the host-only store.

**Per task:** broker mints `tok_`; VM Codex CLI (API-key mode) calls the gateway
with `tok_`; gateway resolves `OAuthCred.Current()` (refreshing within
`oauthRefreshMargin` of expiry and persisting the rotated snapshot), injects
`Bearer` + `chatgpt-account-id`, rewrites host/path to the Codex backend,
forwards, meters usage, enforces `task_max_requests` (HTTP 429 when exceeded).

## Validation spike (Task 1 — GO/NO-GO gate)

Opt-in integration test, run on the author's real `codex login`. Mirrors the
Claude Task 1.

1. Read `~/.codex/auth.json`; confirm exact shape (`tokens.access_token`,
   `tokens.refresh_token`, `tokens.account_id`, expiry/`last_refresh`).
2. Capture a **real** Codex request body (record what Codex CLI actually sends)
   so the replay uses the true wire shape, not a guess.
3. Stand up the gateway pointed at the Codex backend; mint a `tok_`; swap in the
   OAuth creds + `chatgpt-account-id` host-side; replay; assert **200 with a
   usable completion**.
4. Probe refresh: POST the OpenAI OAuth token endpoint with the Codex client id
   and the real refresh token; confirm the response shape (validated-by-typed-
   error acceptable if rate-limited, as in the Claude spike).

**GO** = 200 + completion through the gateway with only `tok_` in the VM, and a
refresh path validated at least by typed error. Record the confirmed constants,
endpoint, header set, any required `StripFields`/`BasePath`, and the `auth.json`
shape in the progress ledger for downstream tasks.

**NO-GO** (stop, report, no wasted build): the ChatGPT backend rejects the
standard Responses request shape in a way a proxy can't cleanly translate; or it
requires Codex-CLI-only signed/derived headers we cannot reproduce host-side; or
the credential cannot be used headlessly at all.

## Error handling & security invariants

- **Never** print or log the OAuth access token or refresh token. `account_id` is
  not a bearer secret but is not logged needlessly.
- Real credential never enters the VM. The red-team test asserts the access and
  refresh sentinels (and `account_id`) are absent from a VM env dump and that
  `tok_` is present.
- **api_key path unregressed:** the OAuth vendor is built only when
  `cfg.OpenAIAuth == "subscription"`; `OpenAIVendor()`/`StaticKey` path is
  byte-identical to today.
- Cred load failure / refresh failure → fail loud host-side with an actionable
  hint, never interpolating the token into the error.
- **Honest docs (no overclaim):** ChatGPT-subscription headless use likely
  brushes OpenAI's terms of service and hits rate limits sooner than interactive
  use; the operator assumes that risk, and drydock makes no claim the mode is
  sanctioned by OpenAI. The stored credential is broader than a scoped API key
  and not per-task revocable. No implied audit, no "production"/"race-clean"
  language; calibrate to working alpha. Mirror SECURITY.md / THREAT_MODEL.md /
  README the way the Claude feature did.

## Testing

- **Gateway unit:** `OpenAIOAuthVendor` injects `Bearer` + `chatgpt-account-id`
  and removes `X-Api-Key`; `refreshOpenAI` via `httptest` (request body fields,
  200 parse, non-200 error without leaking the token, empty-access-token guard).
- **Config:** `openai_auth` default/override/validate.
- **CLI:** `drydock auth codex` parser tests (parse `auth.json` → snapshot;
  empty-token error; token-free output).
- **Red-team (live):** `TestRedteam_*_CodexOAuthNeverInVM` — OAuth backend, mint
  grant, VM env dump asserts token sentinels + account_id absent, `tok_` present.
- **Per-task label:** a Codex-subscription run shows the subscription label in
  `drydock tasks`.
- **Spike:** opt-in integration test (Task 1).
- **Final:** whole-branch opus review, then a real end-to-end run on the
  author's ChatGPT plan (trivial + multi-turn task, request cap, doctor check,
  per-task label) before merge. Nothing pushed from the agent during e2e.

## Out of scope

- Building any OAuth/browser login flow (we piggyback on `codex login`).
- Changing the Claude subscription path.
- Non-macOS credential sourcing beyond reading `~/.codex/auth.json` (the same
  file path Codex CLI uses cross-platform; the spike confirms on macOS first).
