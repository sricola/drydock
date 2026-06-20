# Claude subscription (OAuth) auth — design

**Status:** approved design → pending spec review → implementation plan
**Date:** 2026-06-20
**Scope:** Claude Pro/Max only (Codex/ChatGPT-backend deferred)

## Goal

Let an operator run drydock on their **Claude Pro/Max subscription with no API
key**, without breaking drydock's core property: **the real credential never
enters the sandbox VM.**

The win is UX and cost: people who already pay for Pro/Max can run agents
through drydock without setting up (and paying for) separate API billing.

## Decisions locked during brainstorming

1. **Scope:** Claude Pro/Max first. Codex / ChatGPT-backend sign-in is an
   explicit non-goal for this work (different backend, different request shape —
   a separate later project).
2. **Runaway control:** the per-task **USD budget cap does not apply** to a
   flat-rate subscription. Replace it with a **request-count cap + the existing
   wall-clock timeout** for subscription tasks.
3. **Auth model:** **operator-global** (one auth mode per vendor for the whole
   broker), not per-task.
4. **Credential approach:** **C** — bootstrap from the official `claude login`
   (don't reimplement the interactive OAuth/PKCE login); the **gateway owns
   token refresh**.
5. **Sequencing:** a **validation spike (Deliverable 0) is a go/no-go gate**
   before the full build.

## Background: how auth works today

drydock's credential gateway (`internal/gateway`) is a host-side reverse proxy
in front of `api.anthropic.com` / `api.openai.com`:

- `Vendor{Name, BaseURL, Inject(r *http.Request, realKey string), ParseUsage, Prices}`
  describes one upstream. `AnthropicVendor().Inject` sets `X-Api-Key: <realKey>`
  + `anthropic-version`; `OpenAIVendor().Inject` sets `Authorization: Bearer`.
- `Backend{Vendor, RealKey string}` pairs a vendor with the **real key, held
  host-only**.
- `Gateway.Mint(vendor, budget, ttl)` issues a per-task **bearer token** tied to
  a `lease{budget, spent, vendor}`. The VM only ever receives
  `ANTHROPIC_BASE_URL=http://<gw>` + `ANTHROPIC_AUTH_TOKEN=<bearer>` (via
  `gateway.Provider` → `grant.EnvVars()`).
- On each proxied request the gateway validates the bearer's lease, **swaps in
  the real key host-side** (`Vendor.Inject`), forwards over TLS, and meters
  usage against the lease's USD budget. `Revoke` is a map delete.

The real key never enters the VM. Subscription auth must keep that invariant.

## Core design

### The VM side does not change

Claude Code in the VM keeps talking to the gateway with a minted bearer
(`ANTHROPIC_AUTH_TOKEN`), exactly as today. The **only** change is what the
gateway injects upstream: instead of `X-Api-Key: <api-key>`, it injects
`Authorization: Bearer <oauth-access-token>` + the OAuth beta header. The OAuth
token lives host-side; Claude Code never knows it's subscription-backed.

This means: **no entrypoint changes, no image changes, no change to what the VM
can see.** The blast-radius story for the VM is identical.

### 1. Credential abstraction (`internal/gateway`)

Replace the static `Backend.RealKey string` with a credential the gateway reads
at injection time:

```go
// Credential is the host-held secret the gateway injects upstream. Never
// exposed to the VM.
type Credential interface {
    // Current returns the secret to inject right now, refreshing it if needed.
    Current() (string, error)
}

type Backend struct {
    Vendor Vendor
    Cred   Credential // was: RealKey string
}

// StaticKey is the existing API-key path.
type StaticKey string
func (k StaticKey) Current() (string, error) { return string(k), nil }
```

The gateway's request path changes from reading `backend.RealKey` to:

```go
secret, err := backend.Cred.Current()
if err != nil { /* 502 to the VM; log host-side; do not leak detail */ }
backend.Vendor.Inject(r, secret)
```

`StaticKey` makes this a no-op for the existing API-key path.

### 2. `OAuthCred` (the subscription credential)

```go
type OAuthCred struct {
    mu        sync.Mutex
    access    string
    refresh   string
    expiry    time.Time
    store     CredStore     // persists rotated refresh token
    refreshFn func(refresh string) (access, newRefresh string, expiry time.Time, err error)
}

func (c *OAuthCred) Current() (string, error) {
    c.mu.Lock(); defer c.mu.Unlock()
    if time.Until(c.expiry) > refreshMargin { // e.g. 2m
        return c.access, nil
    }
    a, r, e, err := c.refreshFn(c.refresh)
    if err != nil { return "", fmt.Errorf("oauth refresh failed (re-run `drydock auth claude`): %w", err) }
    c.access, c.refresh, c.expiry = a, r, e
    _ = c.store.Save(c.snapshot()) // persist the rotated refresh token
    return c.access, nil
}
```

- `refreshFn` performs the OAuth **refresh grant** (a single POST to Anthropic's
  token endpoint with the pinned `client_id` + `refresh_token`). The endpoint,
  `client_id`, and beta header are **pinned constants determined by Deliverable
  0** and treated as reverse-engineered (see Risks).
- Refresh is lazy (on `Current()` near expiry) and serialized by the mutex.
- The rotated refresh token is persisted immediately so a broker restart resumes.

### 3. `AnthropicOAuthVendor` (`internal/gateway/vendor.go`)

Same `BaseURL` (`https://api.anthropic.com`) and `ParseUsage` as `AnthropicVendor`,
but a different `Inject`:

```go
func AnthropicOAuthVendor() Vendor {
    v := AnthropicVendor()
    v.Inject = func(r *http.Request, secret string) {
        r.Header.Del("X-Api-Key")
        r.Header.Set("Authorization", "Bearer "+secret)
        r.Header.Set("anthropic-beta", anthropicOAuthBeta) // pinned const, from D0
        if r.Header.Get("anthropic-version") == "" {
            r.Header.Set("anthropic-version", "2023-06-01")
        }
    }
    return v
}
```

`ParseUsage`/`Prices` are retained for **request/usage visibility** (the gateway
can still count tokens for `drydock tasks`), but **not** for budget enforcement.

### 4. Request-count cap (replaces the USD cap for subscription tasks)

Add to the gateway lease:

```go
type lease struct {
    // ... existing fields (vendor, budget, spent) ...
    maxRequests int // 0 = unlimited
    requests    int
}
```

- On each proxied request, increment `requests`; if `maxRequests > 0 &&
  requests > maxRequests`, reject with **HTTP 429** (and the task fails out
  cleanly — same shape as a budget exhaustion today).
- `Mint` gains a `maxRequests` parameter (plumbed from config). It applies to
  **all** tasks (defense-in-depth), but is the **primary** runaway control for
  subscription tasks where the USD budget is meaningless.
- The existing per-task `task_timeout` (wall-clock) is unchanged and is the
  second control.

### 5. Configuration (`internal/config`)

```yaml
# --- Anthropic auth ---
anthropic_auth:  api_key        # api_key | subscription   (operator-global)
# --- Per-task limits ---
task_max_requests:  0           # 0 = unlimited; the runaway cap for subscription
```

- Env overrides: `DRYDOCK_ANTHROPIC_AUTH`, `DRYDOCK_TASK_MAX_REQUESTS` (follows
  the existing override convention).
- `task_budget_usd` is **ignored for subscription tasks** (document this; do not
  silently imply it still caps spend).
- OpenAI/Codex stays `api_key` only for now (no `openai_auth` key yet).

### 6. brokerd boot (`cmd/brokerd/main.go`)

```
if anthropic_auth == "subscription":
    creds := loadClaudeOAuth()           // from ~/.drydock/claude-oauth.json
    backends += Backend{AnthropicOAuthVendor(), NewOAuthCred(creds, store)}
else:
    require ANTHROPIC_API_KEY
    backends += Backend{AnthropicVendor(), StaticKey(key)}
```

- Boot **fails loud** if `anthropic_auth: subscription` but no usable cred file
  (point at `drydock auth claude`), mirroring the existing "no key" failure.
- At least one of (Anthropic subscription | Anthropic key | OpenAI key) is still
  required, as today.

### 7. Credential storage & the refresh-token rotation gotcha

- **Storage:** drydock keeps its **own** copy at `~/.drydock/claude-oauth.json`
  (mode `0600`), seeded by `drydock auth claude` from the host's
  `~/.claude/.credentials.json` (or Keychain). drydock refreshes against **its
  own copy** thereafter and never writes back to the `claude` CLI's file.
- **Why a separate copy:** OAuth refresh tokens are single-use and rotate.
  Sharing one refresh token between the `claude` CLI and drydock means whichever
  refreshes first invalidates the other. A drydock-owned copy keeps drydock's
  refresh chain independent **after the first refresh**. The initial token is
  still shared, so:
  - **Documented contract:** after `drydock auth claude`, drydock owns this
    account's session for drydock's purposes. If you log in again elsewhere with
    the same account, re-run `drydock auth claude` to re-sync.
- This trade-off (vs. a dedicated drydock OAuth login) is accepted to avoid
  reimplementing the interactive login. Revisit if it proves painful.

### 8. Operator surface

- **`drydock auth claude`** — locates the `claude` login creds, validates them
  (present, parseable, not refresh-dead), copies them to
  `~/.drydock/claude-oauth.json` (`0600`), prints status. Does **not**
  reimplement login — if the operator isn't logged in, it tells them to run
  `claude login` first. `--status` to just report.
- **`drydock doctor`** — in subscription mode, add a check: cred file present +
  a successful token refresh (or "valid for Nm"). No API spend.
- **`drydock tasks`** — the cost column shows `—` / `subscription` for
  subscription tasks; optionally show request count (`k/N reqs`).

## Deliverable 0 — the validation spike (go/no-go gate)

Build **nothing else** until this passes. A standalone, integration-tagged
script/test that, against a **real Pro/Max account**:

1. Reads the operator's Claude OAuth creds.
2. Issues **one** request to `api.anthropic.com` with `Authorization: Bearer
   <access-token>` + the candidate beta header, in the **request shape Claude
   Code actually sends** (the minimal system-prompt / client identity Claude
   Code uses), and asserts a **200 with a real completion** — i.e. subscription
   access is granted to a gateway-forwarded, Claude-Code-shaped request.
3. Performs the **refresh grant** against the candidate token endpoint +
   `client_id`, and asserts the new access token also returns a 200.

**Success criteria:** both (2) and (3) succeed. → proceed to the full build, with
the endpoint/`client_id`/beta header pinned as constants.

**Kill criteria:** (2) fails because subscription access requires the request to
originate from a native Claude Code OAuth session (not reproducible through the
gateway) → the feature is **infeasible as designed**; stop and report, do not
build the rest. (3) failing alone → tokens can't be kept alive → fall back to
"works until the bootstrapped access token expires," which is too weak to ship.

This is the single highest-risk unknown; isolating it first caps the downside.

## Data flow (subscription mode)

```
operator: claude login        (once, host, official CLI)
operator: drydock auth claude (copies creds → ~/.drydock/claude-oauth.json, 0600)
brokerd boot: load OAuth creds → Backend{AnthropicOAuthVendor, OAuthCred}
per task:  Mint(vendor, maxRequests, ttl) → VM gets ANTHROPIC_BASE_URL + bearer
per request (VM → gateway):
    validate lease → enforce request-count cap
    secret = OAuthCred.Current()        # refresh if near expiry; persist rotation
    AnthropicOAuthVendor.Inject(r, secret)   # Bearer + beta header
    forward → api.anthropic.com (real TLS, host→Anthropic)
    count request; (optionally parse usage for display)
task end / timeout: Revoke(bearer)
```

## Security & threat-model delta

- **`THREAT_MODEL.md` N4 (budget) rewrite:** for subscription auth there is **no
  USD cap**. The runaway controls are the **request-count cap** and the
  **wall-clock timeout**. State this plainly; do not imply USD capping still
  applies.
- **Bigger blast radius (new `SECURITY.md` note):** the host now holds a
  **full-account OAuth token + refresh token**, not a scoped/budget-capped API
  key. It **still never enters the VM** (the core property holds), but if the
  **host** is compromised this is worse than a scoped key, and the underlying
  account credential is not per-task revocable the way a minted gateway bearer
  is. The `~/.drydock/claude-oauth.json` file is `0600`.
- **ToS / rate limits (honest note):** automating a personal subscription
  headlessly may brush against Anthropic's terms and will hit subscription rate
  limits sooner than API usage. The operator assumes that risk. drydock makes no
  claim that this is sanctioned; it states the limitation.
- **No new VM-visible surface:** the VM-facing protocol is byte-for-byte the same
  as the API-key path, so A1 (real key never in VM) is unchanged — verified by
  the existing red-team test against a sentinel, which should be extended to
  cover the OAuth path (assert the access token / refresh token never appear in
  the VM env).

## Out of scope

- Codex / ChatGPT-backend sign-in (different endpoint + request shape).
- Reimplementing the interactive OAuth/PKCE login (we delegate to `claude login`).
- Per-task auth selection (operator-global only).
- Multi-account / account switching.
- Keeping `task_budget_usd` meaningful for subscription tasks.

## Risks & open questions

1. **(Highest) Subscription access through the gateway** — resolved by
   Deliverable 0. If Anthropic requires a native Claude Code OAuth session that
   can't be reproduced via a forwarded request, the feature dies here.
2. **Reverse-engineered OAuth mechanics** — the token endpoint, `client_id`, and
   beta header are not a stable public API. We pin them and treat upstream
   changes as expected maintenance (a `doctor` check surfaces breakage fast).
3. **Refresh-token rotation conflict** with the `claude` CLI — mitigated by a
   drydock-owned copy (§7); documented contract, accepted trade-off.
4. **Rate limits** — a runaway or parallel tasks can exhaust subscription limits;
   the request cap helps but the upstream limit is Anthropic's. Document.
5. **Credential-file location/format drift** — `~/.claude/.credentials.json` vs.
   Keychain across Claude Code versions; `drydock auth claude` must handle both
   and fail with a clear message on an unknown shape.

## Testing strategy

- **Unit:** `StaticKey.Current` (trivial); `OAuthCred.Current` refresh logic with
  a mocked `refreshFn` (no-refresh-when-fresh, refresh-when-expiring, rotation
  persisted, refresh-error message); request-count lease enforcement (under,
  at, over the cap → 429); config parsing for `anthropic_auth` +
  `task_max_requests` + env overrides; backend construction per `anthropic_auth`.
- **Gateway swap test:** a fake upstream asserting the OAuth `Inject` sets
  `Authorization: Bearer` + beta header and removes `X-Api-Key` (and the inverse
  for the API-key path) — no live calls.
- **Red-team extension:** A1 sentinel test extended so the OAuth **access and
  refresh tokens** never appear in the VM env.
- **Integration (live, needs a real subscription):** Deliverable 0; plus a
  doctor-style "subscription healthy" check.

## File-level change map

- `internal/gateway/vendor.go` — `Credential` interface, `StaticKey`,
  `Backend.Cred`, `AnthropicOAuthVendor`.
- `internal/gateway/oauth.go` (new) — `OAuthCred`, `refreshFn`, `CredStore`,
  pinned constants.
- `internal/gateway/gateway.go` — request path reads `Cred.Current()`; lease
  gains `maxRequests`/`requests`; `Mint` gains `maxRequests`; 429 on cap.
- `internal/gateway/provider.go` — plumb `maxRequests` into `Mint`.
- `internal/config/config.go` — `anthropic_auth`, `task_max_requests` + env
  overrides + seed template.
- `cmd/brokerd/main.go` — build the Anthropic backend per `anthropic_auth`;
  fail-loud on missing subscription creds.
- `cmd/drydock/auth.go` (new) — `drydock auth claude`.
- `cmd/drydock/doctor.go` — subscription health check.
- `cmd/drydock/tasks.go` — cost column shows `subscription` / request count.
- `THREAT_MODEL.md`, `SECURITY.md`, `README.md` — N4 rewrite, blast-radius note,
  subscription install path.
- `tests/integration/` — Deliverable 0 spike; OAuth sentinel red-team extension.
