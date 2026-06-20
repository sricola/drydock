# Claude Subscription (OAuth) Auth — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator run drydock on a Claude Pro/Max subscription with no API key, while the real OAuth credential never enters the sandbox VM.

**Architecture:** The VM-facing protocol is unchanged — Claude Code still authenticates to the gateway with a minted bearer. The only change is host-side: the gateway injects an OAuth `Authorization: Bearer <access-token>` (kept fresh by the gateway) + a beta header instead of `X-Api-Key`. A new `Credential` abstraction replaces the static `Backend.RealKey`. The per-task USD budget is replaced by a request-count cap for subscription tasks.

**Tech Stack:** Go; existing `internal/gateway` reverse proxy, `internal/config`, `cmd/brokerd`, `cmd/drydock`.

## Global Constraints

- **Spike gate:** Task 1 (validation spike) is a go/no-go gate. Do **not** start Task 5+ until it passes. If its kill criteria hit, stop and report.
- **A1 invariant:** the real credential (OAuth access token AND refresh token) must never enter the VM. The VM-facing env stays exactly `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN=<minted bearer>`. No image/entrypoint changes.
- **Scope:** Claude (Anthropic) only. OpenAI/Codex stays API-key only — do not add `openai_auth`.
- **Auth model:** operator-global via `anthropic_auth: api_key | subscription` (default `api_key`).
- **Runaway control for subscription:** request-count cap + existing `task_timeout`. `task_budget_usd` is ignored for subscription tasks (never silently imply it caps spend).
- **Approach C:** bootstrap from the official `claude login`; do not reimplement interactive login. The gateway owns token refresh.
- **Credential storage:** drydock-owned copy at `~/.drydock/claude-oauth.json`, mode `0600`.
- **Honesty (docs):** no overclaiming. State the bigger blast radius and ToS/rate-limit caveats plainly.
- **Pinned OAuth constants** (`anthropicOAuthClientID`, `anthropicOAuthTokenURL`, `anthropicOAuthBeta`) are **determined by Task 1** and live in `internal/gateway/oauth_constants.go`. Later tasks reference them by name, never by guessed literal.

## File Structure

| File | Responsibility |
|---|---|
| `internal/gateway/oauth_constants.go` (new) | Pinned, Task-1-validated OAuth constants (client_id, token URL, beta header, refresh margin). |
| `internal/gateway/vendor.go` (modify) | `Credential` interface, `StaticKey`, `Backend.Cred`, `AnthropicOAuthVendor`. |
| `internal/gateway/oauth.go` (new) | `OAuthCred` (lazy refresh), `CredStore` (read/write the 0600 file), `refreshAnthropic`. |
| `internal/gateway/gateway.go` (modify) | Resolve `Cred.Current()` in the request path; lease `MaxRequests`/`Requests`; `Mint` gains `maxRequests`; 429 on cap. |
| `internal/gateway/provider.go` (modify) | `Provider.MaxRequests` field plumbed into `Mint`. |
| `internal/config/config.go` (modify) | `AnthropicAuth`, `TaskMaxRequests` + env overrides + seed template. |
| `cmd/brokerd/main.go` (modify) | Build the Anthropic backend per `anthropic_auth`; fail loud on missing subscription creds; plumb `task_max_requests`. |
| `cmd/drydock/auth.go` (new) | `drydock auth claude` — bootstrap creds from the host store into the drydock copy. |
| `cmd/drydock/doctor.go` (modify) | Subscription health check. |
| `cmd/drydock/tasks.go` (modify) | Cost column shows `subscription` for subscription tasks. |
| `cmd/drydock/main.go` (modify) | Register the `auth` subcommand + help. |
| `tests/integration/redteam_test.go` (modify) | A1 extended: OAuth tokens never in VM env. |
| `THREAT_MODEL.md`, `SECURITY.md`, `README.md` (modify) | N4 rewrite, blast-radius note, subscription install path. |

---

### Task 1: Validation spike (Deliverable 0) — GO/NO-GO GATE

This is a research/validation task, not red-green TDD. It proves the approach is even possible and produces the pinned constants every later task depends on. **It requires a real Claude Pro/Max account logged in via `claude login` on this host.**

**Files:**
- Create: `tests/integration/oauth_spike_test.go` (build tag `integration`)
- Create (output of this task): `internal/gateway/oauth_constants.go`

**Interfaces:**
- Produces: `anthropicOAuthClientID`, `anthropicOAuthTokenURL`, `anthropicOAuthBeta string` constants; a confirmed access-token request shape; a confirmed refresh-grant request shape.

- [ ] **Step 1: Locate and read the host Claude credentials.** Inspect `~/.claude/.credentials.json` (and, if absent, the macOS Keychain item Claude Code uses). Record the JSON shape: the access-token field, refresh-token field, expiry field, and any `client_id`/scope fields. Write down the exact key names.

- [ ] **Step 2: Identify the OAuth client + endpoints.** From the credential file and the public Claude Code distribution, determine the OAuth `client_id`, the token endpoint URL, and the `anthropic-beta` header value Claude Code sends on OAuth requests. Record candidates.

- [ ] **Step 3: Write the access probe.** In `oauth_spike_test.go`, write a test (skipped unless `DRYDOCK_OAUTH_SPIKE=1`) that reads the access token from the cred file and POSTs a minimal `/v1/messages` request to `https://api.anthropic.com` with `Authorization: Bearer <access>` + the candidate beta header + the minimal Claude-Code-shaped system prompt/identity, asserting a 200 with a completion.

```go
//go:build integration

package integration

// Run with: DRYDOCK_OAUTH_SPIKE=1 go test -tags=integration -run TestOAuthSpike ./tests/integration/ -v
// Requires `claude login` (Pro/Max) on this host.
func TestOAuthSpike_SubscriptionAccessThroughBearer(t *testing.T) {
    if os.Getenv("DRYDOCK_OAUTH_SPIKE") != "1" { t.Skip("set DRYDOCK_OAUTH_SPIKE=1 (needs a real Pro/Max login)") }
    access := readClaudeAccessToken(t)            // from ~/.claude/.credentials.json
    req := buildClaudeCodeShapedMessages(access)  // Bearer + beta header + minimal CC identity
    resp := do(t, req)
    if resp.StatusCode != 200 {
        t.Fatalf("KILL CRITERION: subscription access denied to a gateway-shaped request: %d\n%s", resp.StatusCode, bodyOf(resp))
    }
}
```

- [ ] **Step 4: Run the access probe.** `DRYDOCK_OAUTH_SPIKE=1 go test -tags=integration -run TestOAuthSpike_SubscriptionAccess ./tests/integration/ -v`. Expected: PASS (200 + completion). **KILL CRITERION:** if it 401/403s because subscription requires a native Claude Code OAuth session not reproducible via a forwarded request → STOP, the feature is infeasible as designed; report and do not continue.

- [ ] **Step 5: Write + run the refresh probe.** Add a test that performs the refresh grant (POST to the token endpoint with `client_id` + `refresh_token`), then repeats Step 3 with the **new** access token, asserting 200. Run it. Expected: PASS. **KILL CRITERION:** if refresh can't be performed → STOP; the bootstrapped token would expire with no way to renew, too weak to ship.

- [ ] **Step 6: Record the constants.** Create `internal/gateway/oauth_constants.go` with the validated values:

```go
package gateway

import "time"

const (
    anthropicOAuthClientID  = "<validated in Task 1>"
    anthropicOAuthTokenURL  = "<validated in Task 1>"
    anthropicOAuthBeta      = "<validated in Task 1>"
    oauthRefreshMargin      = 2 * time.Minute // refresh when the access token is within this of expiry
)
```

- [ ] **Step 7: Commit.**

```bash
git add tests/integration/oauth_spike_test.go internal/gateway/oauth_constants.go
git commit -m "spike: validate Claude subscription OAuth access + refresh through the gateway"
```

---

### Task 2: Credential abstraction (`StaticKey`, `Backend.Cred`)

Refactor the static `Backend.RealKey` into a `Credential` the gateway resolves per request. Behavior is unchanged (API-key path stays green); this just opens the seam.

**Files:**
- Modify: `internal/gateway/vendor.go`
- Modify: `internal/gateway/gateway.go`
- Test: `internal/gateway/vendor_test.go` (add)

**Interfaces:**
- Produces: `type Credential interface { Current() (string, error) }`; `type StaticKey string` with `Current()`; `Backend{Vendor Vendor; Cred Credential}` (replaces `RealKey string`).
- Consumes: existing `Vendor`, `Backend`, `vendorRT`, `director`.

- [ ] **Step 1: Write the failing test** in `internal/gateway/vendor_test.go`:

```go
func TestStaticKey_Current(t *testing.T) {
    var c Credential = StaticKey("sk-ant-abc")
    got, err := c.Current()
    if err != nil || got != "sk-ant-abc" {
        t.Fatalf("Current() = %q, %v; want sk-ant-abc, nil", got, err)
    }
}
```

- [ ] **Step 2: Run it — fails to compile** (`Credential`/`StaticKey` undefined). `go test ./internal/gateway/ -run TestStaticKey`.

- [ ] **Step 3: Add the abstraction** to `internal/gateway/vendor.go`:

```go
// Credential is the host-held secret the gateway injects upstream. Never seen by the VM.
type Credential interface{ Current() (string, error) }

// StaticKey is a fixed API key (the existing path).
type StaticKey string
func (k StaticKey) Current() (string, error) { return string(k), nil }
```

Change `Backend`:

```go
type Backend struct {
    Vendor Vendor
    Cred   Credential // was: RealKey string
}
```

- [ ] **Step 4: Thread it through the gateway** in `internal/gateway/gateway.go`. Change `vendorRT.realKey string` → `cred Credential`; in `New`, set `cred: b.Cred`. Resolve the secret in `ServeHTTP` (where errors can be reported) and stash it in context; `director` reads it:

```go
type vendorRT struct{ v Vendor; cred Credential; upstream *url.URL }

type reqCtx struct{ lease *Lease; secret string }

// in ServeHTTP, after a successful check():
vt := g.vendors[lease.Vendor]
secret, err := vt.cred.Current()
if err != nil { http.Error(w, "credential unavailable", http.StatusBadGateway); return }
ctx := context.WithValue(r.Context(), ctxKey{}, &reqCtx{lease: lease, secret: secret})
g.proxy.ServeHTTP(w, r.WithContext(ctx))

// director:
rc, _ := req.Context().Value(ctxKey{}).(*reqCtx)
if rc == nil { return }
vt, ok := g.vendors[rc.lease.Vendor]
if !ok { return }
req.URL.Scheme, req.URL.Host, req.Host = vt.upstream.Scheme, vt.upstream.Host, vt.upstream.Host
vt.v.Inject(req, rc.secret)

// meter: read rc.lease via the same reqCtx.
```

Update `New` and `contextWith`/`ctxKey` usage accordingly. Update any gateway test that constructs `Backend{... RealKey: ...}` to `Backend{... Cred: StaticKey(...)}`.

- [ ] **Step 5: Run the suite.** `go test ./internal/gateway/` — `TestStaticKey_Current` passes and existing gateway/provider tests stay green. Also `go build ./...` (call sites that build `Backend` now use `Cred`).

- [ ] **Step 6: Fix call sites.** `cmd/brokerd/main.go` builds `gateway.Backend{Vendor: …, RealKey: key}` → `Cred: gateway.StaticKey(key)`. `internal/gateway` tests likewise. Re-run `go build ./... && go test ./internal/gateway/`.

- [ ] **Step 7: Commit.** `git add internal/gateway cmd/brokerd && git commit -m "gateway: Credential abstraction (StaticKey) behind Backend.Cred"`

---

### Task 3: Request-count cap on the lease

Add a per-task request ceiling enforced by the gateway, plumbed from config through the provider. Primary runaway control for subscription tasks; defense-in-depth for all.

**Files:**
- Modify: `internal/gateway/gateway.go` (Lease, Mint, check)
- Modify: `internal/gateway/provider.go` (MaxRequests field)
- Test: `internal/gateway/gateway_test.go` (add)

**Interfaces:**
- Produces: `Lease.MaxRequests int`, `Lease.Requests int`; `Gateway.Mint(vendor string, budgetUSD float64, maxRequests int, ttl time.Duration)`; `Provider.MaxRequests int`.
- Consumes: existing `Mint`, `check`, `Provider.Mint`.

- [ ] **Step 1: Write the failing test** in `internal/gateway/gateway_test.go`:

```go
func TestRequestCap_RejectsOverLimit(t *testing.T) {
    g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
    tok, _ := g.Mint("anthropic", 100, 2, time.Hour) // maxRequests = 2
    if _, s := g.check(tok); s != 0 { t.Fatalf("req1 rejected: %d", s) }
    if _, s := g.check(tok); s != 0 { t.Fatalf("req2 rejected: %d", s) }
    if _, s := g.check(tok); s != http.StatusTooManyRequests {
        t.Fatalf("req3 status = %d, want 429", s)
    }
}

func TestRequestCap_ZeroMeansUnlimited(t *testing.T) {
    g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
    tok, _ := g.Mint("anthropic", 100, 0, time.Hour) // 0 = unlimited
    for i := 0; i < 50; i++ {
        if _, s := g.check(tok); s != 0 { t.Fatalf("req %d rejected: %d", i, s) }
    }
}
```

- [ ] **Step 2: Run — fails** (Mint signature mismatch / 429 not returned). `go test ./internal/gateway/ -run TestRequestCap`.

- [ ] **Step 3: Implement.** Add `MaxRequests, Requests int` to `Lease`. Change `Mint` to accept `maxRequests int` and set it on the lease. In `check`, after the budget check, enforce + increment under the existing lock:

```go
if l.MaxRequests > 0 && l.Requests >= l.MaxRequests {
    return nil, http.StatusTooManyRequests
}
l.Requests++
return l, 0
```

In `provider.go`, add `MaxRequests int` to `Provider` and pass it: `tok, err := p.GW.Mint(p.Vendor, b, p.MaxRequests, ttl)`. (Provider.Mint's own signature is unchanged, so `prov.Mint(1)` callers in tests/broker are unaffected.)

- [ ] **Step 4: Run — passes.** `go test ./internal/gateway/`. Fix any other direct `g.Mint(` callers for the new arg (`grep -rn 'GW.Mint\|\.Mint(' internal cmd tests`).

- [ ] **Step 5: Commit.** `git add internal/gateway && git commit -m "gateway: per-task request-count cap (429), plumbed via Provider.MaxRequests"`

---

### Task 4: Config — `anthropic_auth` + `task_max_requests`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (add)

**Interfaces:**
- Produces: `Config.AnthropicAuth string` (yaml `anthropic_auth`, default `"api_key"`); `Config.TaskMaxRequests int` (yaml `task_max_requests`, default `0`); env overrides `DRYDOCK_ANTHROPIC_AUTH`, `DRYDOCK_TASK_MAX_REQUESTS`.

- [ ] **Step 1: Write the failing test** in `internal/config/config_test.go`:

```go
func TestConfig_AnthropicAuthAndMaxRequests(t *testing.T) {
    t.Setenv("DRYDOCK_ANTHROPIC_AUTH", "subscription")
    t.Setenv("DRYDOCK_TASK_MAX_REQUESTS", "150")
    c, err := Load("/nonexistent.yaml") // defaults + env
    if err != nil { t.Fatal(err) }
    if c.AnthropicAuth != "subscription" { t.Errorf("AnthropicAuth=%q", c.AnthropicAuth) }
    if c.TaskMaxRequests != 150 { t.Errorf("TaskMaxRequests=%d", c.TaskMaxRequests) }
}

func TestConfig_AnthropicAuthDefaultsToApiKey(t *testing.T) {
    c, _ := Load("/nonexistent.yaml")
    if c.AnthropicAuth != "api_key" { t.Errorf("default AnthropicAuth=%q, want api_key", c.AnthropicAuth) }
}
```

- [ ] **Step 2: Run — fails** (fields undefined). `go test ./internal/config/ -run TestConfig_AnthropicAuth`.

- [ ] **Step 3: Implement.** Add to the `Config` struct: `AnthropicAuth string \`yaml:"anthropic_auth"\`` and `TaskMaxRequests int \`yaml:"task_max_requests"\``. In the defaults constructor set `AnthropicAuth: "api_key"`, `TaskMaxRequests: 0`. In the env-override block add `DRYDOCK_ANTHROPIC_AUTH` (string) and `DRYDOCK_TASK_MAX_REQUESTS` (int via `strconv.Atoi`, ignore parse error). In `validate()`, require `AnthropicAuth ∈ {api_key, subscription}`. Add both keys to the seed template with comments.

- [ ] **Step 4: Run — passes.** `go test ./internal/config/`.

- [ ] **Step 5: Commit.** `git add internal/config && git commit -m "config: anthropic_auth + task_max_requests"`

---

### Task 5: `AnthropicOAuthVendor` (OAuth injection)

**Depends on Task 1 constants.**

**Files:**
- Modify: `internal/gateway/vendor.go`
- Test: `internal/gateway/vendor_test.go`

**Interfaces:**
- Produces: `func AnthropicOAuthVendor() Vendor` — `Inject` sets `Authorization: Bearer <secret>` + `anthropic-beta: anthropicOAuthBeta`, removes `X-Api-Key`.
- Consumes: `AnthropicVendor()`, `anthropicOAuthBeta` (Task 1).

- [ ] **Step 1: Write the failing test:**

```go
func TestAnthropicOAuthVendor_Inject(t *testing.T) {
    r, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
    r.Header.Set("X-Api-Key", "leftover")
    AnthropicOAuthVendor().Inject(r, "oauth-access-123")
    if r.Header.Get("X-Api-Key") != "" { t.Error("X-Api-Key not removed") }
    if r.Header.Get("Authorization") != "Bearer oauth-access-123" { t.Errorf("Authorization=%q", r.Header.Get("Authorization")) }
    if r.Header.Get("anthropic-beta") != anthropicOAuthBeta { t.Errorf("beta=%q", r.Header.Get("anthropic-beta")) }
}
```

- [ ] **Step 2: Run — fails** (`AnthropicOAuthVendor` undefined).

- [ ] **Step 3: Implement** in `vendor.go`:

```go
func AnthropicOAuthVendor() Vendor {
    v := AnthropicVendor()
    v.Inject = func(r *http.Request, secret string) {
        r.Header.Del("X-Api-Key")
        r.Header.Set("Authorization", "Bearer "+secret)
        r.Header.Set("anthropic-beta", anthropicOAuthBeta)
        if r.Header.Get("anthropic-version") == "" { r.Header.Set("anthropic-version", "2023-06-01") }
    }
    return v
}
```

- [ ] **Step 4: Run — passes.** `go test ./internal/gateway/ -run TestAnthropicOAuthVendor`.

- [ ] **Step 5: Commit.** `git add internal/gateway && git commit -m "gateway: AnthropicOAuthVendor (Bearer + beta header)"`

---

### Task 6: `OAuthCred` + `CredStore` + refresh

**Depends on Task 1 constants.**

**Files:**
- Create: `internal/gateway/oauth.go`
- Test: `internal/gateway/oauth_test.go`

**Interfaces:**
- Produces: `type OAuthCred` implementing `Credential` (refreshes near expiry, persists rotation); `func NewOAuthCred(snap CredSnapshot, store CredStore) *OAuthCred`; `type CredSnapshot struct{ Access, Refresh string; Expiry time.Time }`; `type CredStore interface{ Load() (CredSnapshot, error); Save(CredSnapshot) error }`; `func FileCredStore(path string) CredStore`; `func refreshAnthropic(refresh string) (CredSnapshot, error)`.
- Consumes: `Credential` (Task 2), `anthropicOAuthClientID`, `anthropicOAuthTokenURL`, `oauthRefreshMargin` (Task 1).

- [ ] **Step 1: Write the failing test** (mock the refresh func so no network):

```go
func TestOAuthCred_RefreshesWhenExpiring(t *testing.T) {
    store := &memStore{}
    c := &OAuthCred{snap: CredSnapshot{Access: "old", Refresh: "r1", Expiry: time.Now().Add(30 * time.Second)}, store: store,
        refresh: func(r string) (CredSnapshot, error) {
            if r != "r1" { t.Fatalf("refresh used %q", r) }
            return CredSnapshot{Access: "new", Refresh: "r2", Expiry: time.Now().Add(time.Hour)}, nil
        }}
    got, err := c.Current()
    if err != nil || got != "new" { t.Fatalf("Current=%q,%v want new", got, err) }
    if store.saved.Refresh != "r2" { t.Errorf("rotated refresh not persisted: %q", store.saved.Refresh) }
}

func TestOAuthCred_NoRefreshWhenFresh(t *testing.T) {
    c := &OAuthCred{snap: CredSnapshot{Access: "tok", Expiry: time.Now().Add(time.Hour)},
        refresh: func(string) (CredSnapshot, error) { t.Fatal("should not refresh"); return CredSnapshot{}, nil }}
    got, _ := c.Current()
    if got != "tok" { t.Errorf("Current=%q want tok", got) }
}
```

(`memStore` is a test double implementing `CredStore`.)

- [ ] **Step 2: Run — fails** (types undefined).

- [ ] **Step 3: Implement** `internal/gateway/oauth.go`: `CredSnapshot`, `CredStore` + `FileCredStore` (read/write JSON at `0600`), `OAuthCred{mu, snap, store, refresh}` with `Current()` (refresh when `time.Until(snap.Expiry) <= oauthRefreshMargin`, persist, return access), `NewOAuthCred`, and `refreshAnthropic` (POST `anthropicOAuthTokenURL` with `grant_type=refresh_token`, `client_id=anthropicOAuthClientID`, `refresh_token`; parse access/refresh/expires_in). Per the validated shape from Task 1.

- [ ] **Step 4: Run — passes.** `go test ./internal/gateway/ -run TestOAuthCred`.

- [ ] **Step 5: Commit.** `git add internal/gateway && git commit -m "gateway: OAuthCred with lazy refresh + FileCredStore"`

---

### Task 7: brokerd wiring (build the Anthropic backend per `anthropic_auth`)

**Files:**
- Modify: `cmd/brokerd/main.go`
- Test: manual/integration (brokerd has no unit suite; covered by boot behavior + Task 10).

**Interfaces:**
- Consumes: `cfg.AnthropicAuth` (Task 4), `cfg.TaskMaxRequests` (Task 4), `gateway.AnthropicOAuthVendor` (Task 5), `gateway.NewOAuthCred`/`FileCredStore`/`CredSnapshot` (Task 6), `gateway.StaticKey` (Task 2), `Provider.MaxRequests` (Task 3).

- [ ] **Step 1: Implement the backend selection** where backends are built today:

```go
var backends []gateway.Backend
switch cfg.AnthropicAuth {
case "subscription":
    store := gateway.FileCredStore(filepath.Join(config.Dir(), "claude-oauth.json"))
    snap, err := store.Load()
    if err != nil { die("anthropic_auth=subscription but no usable credentials — run `drydock auth claude`", "err", err) }
    backends = append(backends, gateway.Backend{Vendor: gateway.AnthropicOAuthVendor(), Cred: gateway.NewOAuthCred(snap, store)})
default: // api_key
    if anthropicKey != "" {
        backends = append(backends, gateway.Backend{Vendor: gateway.AnthropicVendor(), Cred: gateway.StaticKey(anthropicKey)})
    }
}
if openaiKey != "" {
    backends = append(backends, gateway.Backend{Vendor: gateway.OpenAIVendor(), Cred: gateway.StaticKey(openaiKey)})
}
```

Adjust the existing "at least one key" guard so `anthropic_auth=subscription` satisfies the Anthropic side. Set `Provider.MaxRequests = cfg.TaskMaxRequests` on each provider. For a subscription provider, set its `Budget` to a large value (e.g. `math.MaxFloat64`) so only the request cap bites.

- [ ] **Step 2: Build + boot in api_key mode.** `go build ./... && make redteam` — existing host-side suite stays green (no behavior change for API-key path).

- [ ] **Step 3: Boot in subscription mode (manual).** With `claude-oauth.json` present (Task 8) and `DRYDOCK_ANTHROPIC_AUTH=subscription`, `drydock start` boots and logs the Anthropic backend as subscription. With the file absent, it dies with the `drydock auth claude` hint.

- [ ] **Step 4: Commit.** `git add cmd/brokerd && git commit -m "brokerd: build Anthropic backend per anthropic_auth; plumb task_max_requests"`

---

### Task 8: `drydock auth claude` (bootstrap creds)

**Files:**
- Create: `cmd/drydock/auth.go`
- Modify: `cmd/drydock/main.go` (dispatch + subHelp + usage)
- Test: `cmd/drydock/auth_test.go`

**Interfaces:**
- Produces: `func runAuth(args []string)`; pure helper `func parseClaudeCreds(raw []byte) (gateway.CredSnapshot, error)` (maps the host file shape from Task 1 → `CredSnapshot`).
- Consumes: `gateway.CredSnapshot`/`FileCredStore` (Task 6), `config.Dir()`.

- [ ] **Step 1: Write the failing test** for the pure parser (using the field names recorded in Task 1):

```go
func TestParseClaudeCreds(t *testing.T) {
    raw := []byte(`{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":1750000000000}}`)
    snap, err := parseClaudeCreds(raw)
    if err != nil { t.Fatal(err) }
    if snap.Access != "a1" || snap.Refresh != "r1" { t.Fatalf("snap=%+v", snap) }
}

func TestParseClaudeCreds_NotLoggedIn(t *testing.T) {
    if _, err := parseClaudeCreds([]byte(`{}`)); err == nil { t.Error("want error for empty creds") }
}
```

(Field names above are illustrative — replace with the exact shape recorded in Task 1, Step 1.)

- [ ] **Step 2: Run — fails** (`parseClaudeCreds` undefined).

- [ ] **Step 3: Implement** `auth.go`: `parseClaudeCreds`; `runAuth(["claude"])` locates the host cred file (`~/.claude/.credentials.json`, else Keychain), parses it, writes `CredSnapshot` via `FileCredStore(config.Dir()/claude-oauth.json)`, prints status (`authenticated · token valid for Nm`); on no login, prints "run `claude login` first". Add `--status` to report without re-copying. Register `auth` in `main.go` dispatch + `subHelp` + `usage`.

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/ -run TestParseClaudeCreds`. `go build ./...`. `drydock auth claude --help` works.

- [ ] **Step 5: Commit.** `git add cmd/drydock && git commit -m "cli: drydock auth claude — bootstrap subscription creds"`

---

### Task 9: `doctor` + `tasks` display

**Files:**
- Modify: `cmd/drydock/doctor.go`, `cmd/drydock/tasks.go`
- Test: `cmd/drydock/tasks_test.go` (add)

**Interfaces:**
- Consumes: `cfg.AnthropicAuth`, `gateway.FileCredStore`/`OAuthCred` (Task 6).

- [ ] **Step 1: Write the failing test** for the tasks cost-cell formatter:

```go
func TestCostCell_Subscription(t *testing.T) {
    if got := costCell(true /*subscription*/, 0); got != "subscription" {
        t.Errorf("costCell=%q want subscription", got)
    }
}
```

(Extract the existing cost formatting in `tasks.go` into `costCell(subscription bool, usd float64) string`.)

- [ ] **Step 2: Run — fails.**

- [ ] **Step 3: Implement.** `costCell` returns `"subscription"` when subscription, else the existing `$x.xxxx`. `tasks.go` passes whether the run was subscription (derivable from config at display time). In `doctor.go`, when `cfg.AnthropicAuth == "subscription"`, add a step: load `claude-oauth.json`, call `OAuthCred.Current()` once (forces refresh if needed) → `step("claude subscription", ok, "token valid")`. No API spend beyond the refresh.

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/`.

- [ ] **Step 5: Commit.** `git add cmd/drydock && git commit -m "cli: doctor subscription check + tasks shows 'subscription'"`

---

### Task 10: Red-team — OAuth tokens never in the VM env

**Files:**
- Modify: `tests/integration/redteam_test.go`

**Interfaces:**
- Consumes: the existing A1 test (`TestRedteam_A1_RealKeyNeverInVM`), `gateway` minting with an OAuth backend.

- [ ] **Step 1: Add an A1 variant** that builds a gateway with an `AnthropicOAuthVendor` + an `OAuthCred` carrying sentinel access/refresh tokens, mints a grant, runs the same `env`/`/proc/self/environ` dump in the VM, and asserts **neither** sentinel (access nor refresh) appears, while a `tok_` bearer does:

```go
func TestRedteam_A1_OAuthTokensNeverInVM(t *testing.T) {
    requireContainer(t)
    const access, refresh = "sk-ant-oat-SENTINEL-ACCESS", "sk-ant-oat-SENTINEL-REFRESH"
    gw, _ := gateway.New(gateway.Backend{Vendor: gateway.AnthropicOAuthVendor(),
        Cred: gateway.NewOAuthCred(gateway.CredSnapshot{Access: access, Refresh: refresh, Expiry: time.Now().Add(time.Hour)}, gateway.NoopStore{})})
    // mint, build VM env from grant.EnvVars(), run env dump, assert sentinels absent + tok_ present
}
```

(Add a `NoopStore` test helper or reuse `memStore`.)

- [ ] **Step 2: Run** (macOS + sandbox): `make redteam-vm` — passes; the sentinels are absent.

- [ ] **Step 3: Commit.** `git add tests/integration && git commit -m "redteam: A1 — OAuth access+refresh tokens never enter the VM"`

---

### Task 11: Docs — threat model, security, README

**Files:**
- Modify: `THREAT_MODEL.md`, `SECURITY.md`, `README.md`

- [ ] **Step 1: THREAT_MODEL N4 rewrite.** State that with `anthropic_auth: subscription` there is **no USD cap**; the runaway controls are the **request-count cap (`task_max_requests`)** and the **wall-clock `task_timeout`**. Keep the API-key budget behavior as-is for `api_key`.

- [ ] **Step 2: SECURITY.md blast-radius note.** Add: subscription mode keeps a **full-account OAuth token + refresh token** host-side at `~/.drydock/claude-oauth.json` (`0600`). It never enters the VM (A1 holds), but it is broader than a scoped API key and is not per-task revocable like a minted bearer. Note the **ToS/rate-limit** caveat: automating a personal subscription headlessly may brush against terms and hit limits sooner; the operator assumes that risk.

- [ ] **Step 3: README install path.** Add a "Use your Claude subscription (no API key)" subsection: `claude login` → `drydock auth claude` → set `anthropic_auth: subscription` (or `DRYDOCK_ANTHROPIC_AUTH=subscription`) → `drydock start`. State plainly that the USD budget doesn't apply; set `task_max_requests`.

- [ ] **Step 4: Commit.** `git add THREAT_MODEL.md SECURITY.md README.md && git commit -m "docs: Claude subscription auth — N4, blast radius, install path"`

---

## Self-Review

**Spec coverage:** Credential abstraction (T2) ✓; AnthropicOAuthVendor (T5) ✓; OAuthCred + refresh + store (T6) ✓; request cap (T3) ✓; config (T4) ✓; brokerd wiring (T7) ✓; `drydock auth claude` (T8) ✓; doctor + tasks (T9) ✓; storage/rotation contract (T6/T8 via FileCredStore + drydock-owned copy) ✓; validation spike as gate (T1) ✓; threat-model delta (T11) ✓; red-team OAuth sentinel (T10) ✓. No spec section is unaddressed.

**Placeholder scan:** The only deferred values are the three OAuth constants and the host cred-file field names — both are explicit **outputs of Task 1** (a spike whose nature is discovery), referenced by name thereafter, not guessed literals. This is a real task dependency, not a TODO.

**Type consistency:** `Credential.Current() (string, error)` used in T2/T6/T7; `Backend.Cred` everywhere (T2 onward); `Gateway.Mint(vendor, budget, maxRequests, ttl)` in T3 matches its caller `Provider.Mint` (T3) and any test; `CredSnapshot{Access,Refresh,Expiry}` consistent across T6/T8/T10; `anthropicOAuthBeta`/`anthropicOAuthClientID`/`anthropicOAuthTokenURL`/`oauthRefreshMargin` defined in T1, consumed in T5/T6. Consistent.
