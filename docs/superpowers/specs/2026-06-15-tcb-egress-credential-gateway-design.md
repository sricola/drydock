# TCB-side egress + credential gateway (v2 hardening)

**Status:** Design (pending user review)
**Date:** 2026-06-15
**Depends on:** the MVP (`docs/superpowers/specs/2026-06-14-isolated-claude-container-broker-design.md`, merged to `main`)

---

## 1. Goal

Move both the **real API key** and the **egress allowlist** out of the untrusted VM and into the
host TCB. After this subsystem, the VM holds only a revocable per-task bearer token and can reach
exactly two host-local ports (the credential gateway and the egress proxy). This closes the two
biggest residual risks in the MVP: the API key sits in the VM as an env var (exfiltratable if
egress ever widens), and the egress allowlist is enforced *inside* the untrusted VM (in-VM nft).

### Decisions locked during brainstorming

- **Gateway runs in-process** in `brokerd` (a goroutine + in-memory token map), not a separate binary.
- **Per-token enforcement = revoke + USD budget.** No short-TTL rotation (see §3).
- **Scope = gateway + out-of-VM egress proxy** (squid + pf), bundled — enforcement fully in the TCB.
- **VM→gateway hop is plaintext HTTP** over the host-local vmnet (token in cleartext on that
  private link is acceptable; it is per-task and revocable).
- **Phase-0 pf/vmnet spike first**, with a **gateway-only fallback** if host-pf filtering of the
  VM subnet proves unworkable on `apple/container` 1.0.0.

### Grounding (verified 2026-06-15)

- `ANTHROPIC_BASE_URL` overrides the API endpoint (route through a proxy/gateway).
- `ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer <value>`.
- `ANTHROPIC_API_KEY` is sent as `X-Api-Key`.
  → The gateway hands the VM a bearer token via `ANTHROPIC_AUTH_TOKEN`, validates it, strips
  `Authorization`, sets `X-Api-Key: <real key>`, and forwards to `api.anthropic.com`.
- `apple/container` 1.0.0 is installed on the dev host; VM boot, image build, and the in-VM nft
  egress firewall (with `--cap-add CAP_NET_ADMIN`) are all verified working. The networking
  primitives (`container network create --subnet`, per-VM vmnet) can therefore be spiked locally.

---

## 2. Why this, and what it changes vs the MVP

| MVP (today) | After this subsystem |
|---|---|
| Real `ANTHROPIC_API_KEY` injected into the VM as env | Real key lives **only** in the broker host; VM gets a bearer token |
| Egress allowlist enforced by **in-VM nft** (untrusted territory) | Allowlist enforced by **host squid + pf** (TCB) |
| VM resolves DNS (port 53 open) → DNS exfil channel | VM needs **no DNS**; gateway/squid resolve host-side |
| VM reaches `api.anthropic.com` and registries directly | VM reaches **only** `gateway:8088` and `squid:3128` |

---

## 3. Credential model (simplified from spec §7)

The original "scoped, short-lived key" framing was a mitigation for the key being *inside* the VM.
The gateway removes that premise, so the model simplifies:

- **Real key:** host-only (broker env). Rotated on normal host-secret hygiene, **not per task**.
- **Per-task bearer token:** the only credential in the VM. **Revoked on task completion and on
  timeout** (a one-line map delete — the real control). Because the gateway is in-process with an
  in-memory map, a broker crash invalidates every token for free.
- **No short-TTL rotation.** At most a generous safety-net `expiry` (= task timeout + margin) to
  bound a token whose task hangs while the broker stays alive; this is a backstop, not a scheme.
- **USD budget is retained** — it controls runaway *cost* from a misbehaving agent, which is
  orthogonal to leaks/rotation.

**Blast radius if the token leaks anyway:** usable only through your gateway (worthless elsewhere),
capped by the per-task USD budget, dead the moment the task ends. The real key is never exposed.

---

## 4. Architecture / data flow

```
┌───────────────── HOST (TCB) ─────────────────────────────────────────────┐
│ brokerd                                                                   │
│  ├─ POST /tasks (existing)                                                │
│  ├─ gateway goroutine — binds <vmnet-gw>:8088                            │
│  │    token → lease{ realKey, budgetUSD, spentUSD, expiry }             │
│  │    Bearer tok ─► validate(exists, !expired, spent<budget)           │
│  │    strip Authorization; set X-Api-Key=REAL, anthropic-version       │
│  │    reverse-proxy ─► HTTPS api.anthropic.com                          │
│  │    tee response, parse usage (SSE-aware) ─► spentUSD += cost(model)  │
│  ├─ squid (host proc) — binds <vmnet-gw>:3128                           │
│  │    CONNECT allowlist (npm/pypi…) by hostname, no TLS interception    │
│  └─ pf anchor — block <vm-subnet> → * except :8088 and :3128            │
│                                                                           │
│  ╞══ trust boundary ════════════════════════════════════════════════════╡│
│  VM (subnet fixed via `container network create --subnet`)              │
│   ANTHROPIC_BASE_URL=http://<vmnet-gw>:8088   ANTHROPIC_AUTH_TOKEN=tok  │
│   HTTPS_PROXY=http://<vmnet-gw>:3128          (no real key, no DNS)     │
│   nft (belt): allow only →:8088 and →:3128                             │
└───────────────────────────────────────────────────────────────────────────┘
```

Two distinct host proxies, by necessity:
- **Gateway** is an L7 *reverse* proxy specific to the model API — it terminates the connection so
  it can swap the credential and read usage for the budget. VM→gateway is plaintext HTTP; gateway→
  Anthropic is real HTTPS.
- **Squid** is a *forward* CONNECT proxy for everything else (registries) — it filters by hostname
  with **no TLS interception**, so it never sees payloads and needs no injected CA.

---

## 5. Components (each independently testable)

### 5.1 `internal/gateway`
- `type Lease struct { RealKey string; BudgetUSD, SpentUSD float64; Expiry time.Time }`
- `type Gateway struct { ... }` with an in-memory `map[string]*Lease` under a mutex.
- Admin (in-process Go API, not HTTP): `Mint(budgetUSD float64, ttl time.Duration) (token string)`,
  `Revoke(token string)`.
- HTTP handler: extract Bearer token → `validate` (exists, `now < Expiry`, `SpentUSD < BudgetUSD`)
  → reject with 401/402 otherwise → strip `Authorization`, set `X-Api-Key` + `anthropic-version`
  → `httputil.ReverseProxy` to `https://api.anthropic.com` → `ModifyResponse` tees the body and
  parses usage (handle both single-JSON and SSE `message_start`/`message_delta` events) →
  `SpentUSD += cost(model, inputTokens, outputTokens)`.
- `cost` uses a small static `model → {inputPer1M, outputPer1M}` price table.
- **Budget enforcement is request-gated:** the gateway can't abort an in-flight stream, so it
  rejects the *next* request once `SpentUSD ≥ BudgetUSD`. Documented limitation.
- Binds the vmnet gateway IP (from §6), not `0.0.0.0`.

### 5.2 `creds` refactor (targeted improvement to the existing seam)
Replace the current `Provider.Mint(ttl) (Token, error)` with a grant that carries the env to inject,
so static-key and gateway providers are interchangeable:
```go
type Grant interface {
    EnvVars() []string   // e.g. ["ANTHROPIC_API_KEY=sk-…"] or
                         //      ["ANTHROPIC_BASE_URL=http://…:8088", "ANTHROPIC_AUTH_TOKEN=tok_…"]
    Revoke() error
}
type Provider interface { Mint(budgetUSD float64) (Grant, error) }
```
- `StaticProvider` (MVP, unchanged behavior) returns a grant with `ANTHROPIC_API_KEY`.
- `GatewayProvider{GW *gateway.Gateway, BaseURL string}` registers a lease and returns a grant with
  `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`; `Revoke()` deletes the lease.
- Broker injects `grant.EnvVars()` into the container and `defer grant.Revoke()`.

### 5.3 `internal/netfw`
- Compiles `egress.yaml` → squid `dstdomain` allowlist file + a pf anchor.
- Manages a per-task network with a deterministic subnet:
  `container network create --subnet 192.168.<n>.0/24 egress-<id>`; gateway/squid bind `…<n>.1`;
  pf blocks `192.168.<n>.0/24 → * except :8088,:3128`.
- Lifecycle: create network + write configs + start squid + load pf anchor on lease; reverse on
  teardown (`pfctl -a … -F`, stop squid, `container network delete`).

### 5.4 Image / entrypoint changes
- Broker injects `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` (from the grant) and
  `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`.
- `init-firewall.sh` shrinks to a belt rule: allow egress only to `<vmnet-gw>:8088` and `:3128`,
  **drop DNS**. pf is the primary pin; nft is defense-in-depth.
- Verify `npm`/`pip` honor `HTTP(S)_PROXY` (or set their proxy explicitly) — see §8.

---

## 6. Networking + Phase-0 spike + fallback

The one unverified assumption is **host pf filtering of the VM subnet** on `apple/container` 1.0.0:
1. Does `container network create --subnet` deterministically set the gateway IP to `…1`?
2. Can host processes bind that vmnet gateway IP and be reached from inside the VM?
3. Can host **pf** filter/forward the VM subnet's egress on a path we can write rules for?

**Phase 0 of the plan is a spike on this host** that answers all three with real commands before
any gateway/squid/pf code is written.

**Fallback if pf-on-vmnet is unworkable:** ship **gateway-only**. The real key still leaves the VM
(the biggest win), the gateway still enforces budget + revoke, and the **routing pin degrades to
in-VM nft** ("allow only the gateway IP:port"). The egress proxy / DNS-closure parts wait for a
later networking pass. This fallback is a first-class outcome, not a failure.

---

## 7. Testing

- **gateway (unit, no network):** token validate / expired / over-budget rejection; header swap
  (`Authorization` stripped, `X-Api-Key` set); usage→cost parsing against a single-JSON fixture and
  an SSE fixture; uses a stub upstream (`httptest.Server`).
- **creds (unit):** `StaticProvider` and `GatewayProvider` both satisfy `Grant`/`EnvVars`; gateway
  grant `Revoke()` invalidates the token (a subsequent gateway call 401s).
- **netfw (unit):** `egress.yaml` → expected squid allowlist + pf anchor text; deterministic subnet
  derivation.
- **host integration (this box):** Phase-0 spike; then end-to-end — from inside the VM, the model
  call via the gateway succeeds, a non-allowlisted host is blocked, **and a direct
  `https://api.anthropic.com` from the VM is blocked** (proving the VM only reaches the gateway).

---

## 8. Open risks / to verify during build

1. **pf-on-vmnet** (the Phase-0 spike). Fallback in §6.
2. **`HTTPS_PROXY` honored** by `npm`/`pip` in the image (undici/tooling differences). Anything that
   opens a raw socket fails closed (pf/nft block it) — so it breaks rather than tunnels; configure
   each tool's proxy explicitly.
3. **SSE usage parsing** — confirm the exact event shape claude-code's API calls produce; the budget
   depends on reading `usage` from `message_start`/`message_delta`/final message.
4. **Plaintext VM→gateway hop** carries the bearer token in cleartext on the vmnet. Acceptable
   (host-local, per-task, revocable); revisit only if the vmnet is considered shared.
5. **Budget is request-gated, not mid-stream** — a single very long response can overshoot the
   budget by one request. Acceptable; documented in §5.1.
6. **Per-task squid/network churn** under load — fine at low volume; revisit with the warm pool.
