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
2. **Extra header + a server-enforced header allowlist.** It sends a
   `chatgpt-account-id` header (alongside `Authorization: Bearer <oauth access
   token>`), AND the Codex backend **whitelists the `originator` and
   `User-Agent`** — wrong values return a hard **403** (Cloudflare). Accepted
   originators are `codex_cli_rs` / `codex_vscode` / `codex_sdk_ts` / `Codex*`.
3. **Request/response shape differences are real, not hypothetical.** The Codex
   backend **requires `store: false` and `stream: true`** (forcing `store:true`
   → 400), and streams SSE that can diverge from the public Responses-API SSE
   contract. A standard `api.openai.com/v1/responses` request is therefore not
   byte-identical.

So the gateway must **translate** an `api.openai.com`-style request into a
Codex-backend request. The saving grace of Approach A: because the VM runs the
**real Codex CLI** (not a hand-built request), that CLI — even in API-key mode —
should already emit `originator: codex_cli_rs`, a `Codex*` User-Agent, and
`store:false`/`stream:true`. So a **pass-through** proxy that *preserves* those
headers and body fields may need zero surgery. The entire go/no-go question of
the validation spike (Task 1) is whether that pass-through actually works, i.e.
whether the VM's api-key-mode Codex emits native-Codex requests the backend
accepts once we swap auth host-side.

The wire facts below are corroborated by multiple third-party clients that
already run this path (opencode, openclaw, pi-mono, llm-via-codex) — the
architecture is **proven feasible**, not speculative — but the exact constants,
header set, and `auth.json` shape **MUST still be confirmed by the spike against a
real `codex login`** before any downstream task relies on them.

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
  `chatgpt-account-id: <accountID>`, removing any inbound `X-Api-Key`. It must
  **preserve** the inbound `originator` and `User-Agent` headers that the VM's
  Codex CLI sends (do not overwrite them — wrong values → 403). The spike
  confirms whether any of these must be *set* by the gateway vs merely passed
  through.
- Reuse `parseOpenAIUsage` and `OpenAIPrices()` for display only (see metering
  caveat below).
- **`account_id` handling (corrected).** `account_id` is **not** a stable
  top-level field: `chatgpt_account_id` is a claim *inside the JWT*
  (`access_token`/`id_token`), and OpenAI's refresh endpoint has a documented
  habit of returning refreshed tokens **without** re-asserting it — which breaks
  naive refreshers after the first refresh (~10 days). The mitigation is exactly
  capture-once: brokerd reads `account_id` from the **original** login (decoded
  JWT claim or `auth.json`) at backend construction and the `Inject` closure
  injects that captured value on every request, independent of token rotation.
  **No change to the `Credential` interface.** Because of the refresh-strips-
  account-id failure mode, the spike MUST assert that a request using a
  **post-refresh** access token together with the captured account_id still
  returns 200 (not merely that refresh returns a token).

If the spike shows a path remap is required (e.g. `/v1/responses` →
`/backend-api/codex/responses`), add a minimal `BasePath` field to `Vendor`,
joined in the proxy director — not a per-vendor hack. If the spike shows the
gateway must coerce body fields the VM's Codex does **not** already send
(e.g. force `store:false`), reuse/extend the existing `StripFields` mechanism
into a small field-set/strip step — but first confirm whether native Codex
already sends them (expected yes), in which case no body surgery is needed.

Rejected alternatives: **B** — a separate Codex backend with its own proxy logic
(duplicates metering/cap/budget plumbing); **C** — enriching the `Credential`
interface to return token + headers (speculative generality for one non-rotating
header; churns the interface the Claude path just stabilized).

## Components (files)

All mirror the Claude path unless noted.

- **`internal/gateway/codex_oauth.go`** *(new)* — `refreshOpenAI(refreshToken)
  (CredSnapshot, error)` hitting the OpenAI OAuth token endpoint with the Codex
  client id. Reuse `OAuthCred`, `CredSnapshot`, `FileCredStore` unchanged. Since
  `account_id` is captured once (from the original login's JWT) and injected
  independent of token rotation, it is stored as a separate field on the
  persisted store / passed to `OpenAIOAuthVendor(accountID)` — **not** threaded
  through `CredSnapshot` (which stays the shared Claude/Codex token-pair type).
  `refreshOpenAI` returns only the rotated token pair + expiry.
- **`internal/gateway/codex_constants.go`** *(new)* — `openaiOAuthClientID`,
  `openaiOAuthTokenURL` (`var` for test overrideability, as with Anthropic), the
  Codex backend base URL/path, and any required static headers. The spike pins
  these; current best-known starting values to verify (corroborated by
  third-party clients): token endpoint `https://auth.openai.com/oauth/token`,
  authorize `https://auth.openai.com/oauth/authorize`, client_id
  `app_EMoamEEZ73f0CkXaXp7hrann`, backend `https://chatgpt.com/backend-api/codex`,
  `auth.json` shape `{auth_mode, last_refresh, tokens:{access_token, id_token,
  refresh_token}}` with `account_id` derived from the JWT `chatgpt_account_id`
  claim (NOT a sibling field).
- **`internal/gateway/vendor.go`** — add `OpenAIOAuthVendor(accountID string)`.
  Add `BasePath` to `Vendor` only if the spike requires a path remap.
- **`cmd/brokerd/main.go`** — build the OpenAI backend per `cfg.OpenAIAuth`:
  `subscription` → load `~/.drydock/codex-oauth.json` (die-with-hint on load
  error) + `OpenAIOAuthVendor(accountID)` + `NewOAuthCred` (refresh =
  `refreshOpenAI`) + `Budget = math.MaxFloat64`; `api_key` path **byte-for-byte
  unchanged**. Boot guard treats a Codex subscription as satisfying the OpenAI
  side. Every Provider keeps `MaxRequests = cfg.TaskMaxRequests`.
- **`cmd/drydock/auth.go`** — `drydock auth codex`: read `~/.codex/auth.json`,
  parse `tokens.{access_token,id_token,refresh_token}` + expiry, **derive
  `account_id` from the JWT `chatgpt_account_id` claim** (decode the JWT payload;
  do not log the decoded claims — they carry account_id/plan/org), write
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
- **Metering caveat (no new file).** The chatgpt.com Codex stream may not carry
  usage in the shape `parseOpenAIUsage` expects, so token counts could parse to
  zero/garbage. In subscription mode budget is a no-op anyway, so `drydock tasks`
  must show the **`subscription`** cost label (driven by `drydock_meta`, same as
  Claude) rather than a misleading `$0.0000`. The spike records whether usage is
  parseable; if not, we simply don't display a dollar figure for these runs.

### How the VM's Codex is pointed at the gateway (api-key mode)

Codex CLI prefers a ChatGPT login over an API key when **both** are present
(`openai/codex#2733`). So in subscription mode the VM must be staged with **no
`~/.codex/auth.json`** and configured for api-key mode against the gateway:
`OPENAI_API_KEY=tok_…` plus a gateway `base_url` (env `OPENAI_BASE_URL` or a
`model_provider` entry in the VM's `config.toml`). The spike confirms the exact
knob and confirms that api-key-mode Codex still emits the native
`originator`/`User-Agent`/`store:false`/`stream:true` the backend requires.

## Data flow

**Setup (once):** `codex login` (operator) → `drydock auth codex` copies the
credential into the host-only store.

**Per task:** broker mints `tok_`; VM Codex CLI (API-key mode) calls the gateway
with `tok_`; gateway resolves `OAuthCred.Current()` (refreshing within
`oauthRefreshMargin` of expiry and persisting the rotated snapshot), injects
`Bearer` + `chatgpt-account-id`, rewrites host/path to the Codex backend,
forwards, meters usage, enforces `task_max_requests` (HTTP 429 when exceeded).

## Validation spike (Task 1 — GO/NO-GO gate)

Run on the author's real `codex login`. Mirrors the Claude Task 1, but with a
**cheap step 0 first** so we never invest in gateway scaffolding before the core
question is answered.

**Step 0 — ~10-minute `curl` replay (do this BEFORE any code).** Read the
`access_token` and the JWT `chatgpt_account_id` from `~/.codex/auth.json`; `curl`
a minimal request to `https://chatgpt.com/backend-api/codex/responses` with
`Authorization: Bearer <access_token>`, `chatgpt-account-id: <id>`,
`originator: codex_cli_rs`, a `Codex*` `User-Agent`, and a body with
`store:false`/`stream:true`. A **200 + a streamed completion** answers the whole
feasibility question. Tokens are read into shell vars and **never echoed**;
`curl -v` is avoided so the `Authorization` header isn't printed. If this 200s,
the rest is plumbing; if it 4xx/403s, we triage which header/field is wrong
before writing anything.

1. Capture a **real** Codex request body (record what the actual Codex CLI sends,
   including its exact `originator`/`User-Agent`/`store`/`stream`) so the replay
   uses the true wire shape, not a guess.
2. Stand up the gateway pointed at the Codex backend; mint a `tok_`; swap in the
   OAuth creds + `chatgpt-account-id` host-side, **preserving** the CLI's
   `originator`/`User-Agent`; replay; assert **200 with a usable completion**.
3. Probe refresh: POST the OpenAI OAuth token endpoint with the Codex client id
   and the real refresh token. **Then make a fresh backend request using the
   post-refresh access token + the captured account_id and assert it still 200s**
   — this guards the documented "refresh strips `chatgpt_account_id`" failure
   mode (a request that 200s pre-refresh but 401/403s post-refresh is a NO-GO
   until mitigated). Validated-by-typed-error acceptable only for the token POST
   if rate-limited, never as a substitute for the post-refresh request check.

**GO** = (a) 200 + completion through the gateway with only `tok_` in the VM,
(b) the `originator`/`User-Agent` allowlist satisfied by pass-through, (c) a
**post-refresh** request still 200s. Record the confirmed constants, endpoint,
full header set, any required body coercion (`store:false`), `BasePath`, and the
`auth.json`/JWT shape in the progress ledger for downstream tasks.

**NO-GO** (stop, report, no wasted build): the backend 403s on
`originator`/`User-Agent` we can't reproduce host-side; or it rejects the request
shape in a way a proxy can't cleanly coerce; or refresh permanently breaks the
account_id binding with no host-side mitigation; or the credential cannot be used
headlessly at all.

## Error handling & security invariants

- **Never** print or log the OAuth access token, `id_token`, or refresh token.
  The access/id tokens are JWTs whose decoded payload carries `account_id`,
  `plan_type`, and org — **never log the decoded JWT claims** either. `account_id`
  is not a bearer secret but is not logged needlessly.
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

- **Gateway unit:** `OpenAIOAuthVendor` injects `Bearer` + `chatgpt-account-id`,
  removes `X-Api-Key`, and **preserves** inbound `originator`/`User-Agent`;
  `refreshOpenAI` via `httptest` (request body fields, 200 parse, non-200 error
  without leaking the token, empty-access-token guard).
- **Config:** `openai_auth` default/override/validate.
- **CLI:** `drydock auth codex` parser tests (parse `auth.json` + decode JWT
  `chatgpt_account_id` claim → snapshot+account_id; empty-token error; token-free
  output; malformed-JWT error).
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
