# Host-side egress + credential gateway (v2 hardening, laptop dev env)

**Status:** Design (pending user review)
**Date:** 2026-06-15
**Depends on:** the MVP (`docs/superpowers/specs/2026-06-14-isolated-claude-container-broker-design.md`, merged to `main`)

---

## 1. Goal & target environment

Get the **real API key out of the untrusted VM** and enforce the **egress allowlist by hostname
from a host-side process**, so the VM holds only a revocable per-task bearer token and reaches
exactly two host-local ports (the credential gateway and the egress proxy).

**Target environment is a local development environment on developer laptops** — a single developer
who is themselves the trust root; the untrusted party is repo code / dependencies / prompt-injection
running inside the VM. This constrains the design: **userspace, no-sudo mechanisms only; do not
mutate the laptop's system firewall (pf) or churn per-task networks.** Server-grade fleet hardening
is explicitly out of scope.

### Decisions locked during brainstorming

- **Gateway runs in-process** in `brokerd` (a goroutine + in-memory token map), not a separate binary.
- **Per-token enforcement = revoke + USD budget.** No short-TTL rotation (see §3).
- **Egress proxy = a userspace squid** (no sudo). **No pf, no per-task subnet churn.**
- **The egress pin is in-VM nft**, narrowed to "only the gateway and squid ports" — the MVP's
  existing mechanism, set as root pre-priv-drop with the agent non-root.
- **VM→gateway hop is plaintext HTTP** over the host-local vmnet (token in cleartext on that private
  link is acceptable; it is per-task and revocable).

### What we keep vs. drop vs. the server-grade version

- **Keep:** real key out of the VM; budget + revoke; hostname-based registry allowlist; DNS closure.
- **Drop (laptop-inappropriate):** pf system-firewall anchors (sudo, fragile across sleep/VPN/OS
  updates); per-task `container network create/delete` churn; the Phase-0 pf spike.
- **Accept:** the egress pin lives in-VM (nft). Defeating it needs a kernel LPE *inside* the VM —
  a proportionate residual risk for a single-developer laptop. (pf would have removed it; not worth
  the laptop cost.)

### Grounding (verified 2026-06-15)

- `ANTHROPIC_BASE_URL` overrides the API endpoint; `ANTHROPIC_AUTH_TOKEN` → `Authorization: Bearer
  <value>`; `ANTHROPIC_API_KEY` → `X-Api-Key`. The gateway hands the VM a bearer token, validates
  it, strips `Authorization`, sets `X-Api-Key: <real key>`, forwards to `api.anthropic.com`.
- `apple/container` 1.0.0 is installed on the dev host; VM boot, image build, and the in-VM nft
  firewall (with `--cap-add CAP_NET_ADMIN`) are verified working.

---

## 2. What this changes vs the MVP

| MVP (today) | After this subsystem |
|---|---|
| Real `ANTHROPIC_API_KEY` injected into the VM | Real key lives **only** in the broker host; VM gets a bearer token |
| Egress allowlist = in-VM nft matching **resolved IPs** | Allowlist = host **squid** matching **hostnames**; in-VM nft only pins to 2 host ports |
| VM resolves DNS (port 53 open) → DNS exfil channel | VM needs **no DNS**; gateway/squid resolve host-side; nft drops 53 |
| VM reaches `api.anthropic.com` + registries directly | VM reaches **only** `gateway:8088` and `squid:3128` |

---

## 3. Credential model (simplified from MVP spec §7)

"Scoped, short-lived key" was a mitigation for the key being *inside* the VM. The gateway removes
that premise:

- **Real key:** host-only (broker env). Rotated on normal host-secret hygiene, **not per task**.
- **Per-task bearer token:** the only credential in the VM. **Revoked on task completion and on
  timeout** (a one-line map delete — the real control). The gateway is in-process with an in-memory
  map, so a broker crash invalidates every token for free.
- **No short-TTL rotation.** At most a generous safety-net `expiry` (= task timeout + margin) to
  bound a token whose task hangs while the broker stays alive.
- **USD budget retained** — controls runaway *cost*, orthogonal to leaks/rotation.

**Blast radius if the token leaks:** usable only through your gateway, capped by the per-task USD
budget, dead the moment the task ends. The real key is never exposed.

---

## 4. Architecture / data flow

```
┌───────────────── HOST (laptop, userspace, no sudo) ──────────────────────┐
│ brokerd                                                                   │
│  ├─ POST /tasks (existing)                                                │
│  ├─ gateway goroutine — binds <vmnet-gw-ip>:8088                         │
│  │    token → lease{ realKey, budgetUSD, spentUSD, expiry }             │
│  │    Bearer tok ─► validate(exists, !expired, spent<budget)           │
│  │    strip Authorization; set X-Api-Key=REAL, anthropic-version       │
│  │    reverse-proxy ─► HTTPS api.anthropic.com                          │
│  │    tee response, parse usage (SSE-aware) ─► spentUSD += cost(model)  │
│  └─ squid (userspace host proc) — binds <vmnet-gw-ip>:3128              │
│       CONNECT allowlist (npm/pypi…) by hostname, no TLS interception    │
│                                                                           │
│  ╞══ trust boundary ════════════════════════════════════════════════════╡│
│  VM (on a stable shared network; nft is the pin)                        │
│   ANTHROPIC_BASE_URL=http://<vmnet-gw-ip>:8088  ANTHROPIC_AUTH_TOKEN=tok│
│   HTTPS_PROXY=http://<vmnet-gw-ip>:3128         (no real key, no DNS)   │
│   nft (root, pre-priv-drop): allow egress ONLY →:8088 and →:3128;      │
│                              drop 53 and everything else                │
└───────────────────────────────────────────────────────────────────────────┘
```

Two distinct host proxies, by necessity:
- **Gateway** — an L7 *reverse* proxy for the model API; it terminates the connection so it can swap
  the credential and read usage for the budget. VM→gateway is plaintext HTTP; gateway→Anthropic is
  real HTTPS.
- **Squid** — a *forward* CONNECT proxy for everything else (registries); filters by hostname with
  **no TLS interception**, so it never sees payloads and needs no injected CA.

The in-VM nft pin (allow only the two host ports) is what guarantees the VM can't bypass the proxies.
It is set as root in the entrypoint before dropping to the non-root agent, who cannot flush it.

---

## 5. Components (each independently testable)

### 5.1 `internal/gateway`
- `type Lease struct { RealKey string; BudgetUSD, SpentUSD float64; Expiry time.Time }`
- `type Gateway struct { ... }` with an in-memory `map[string]*Lease` under a mutex.
- Admin (in-process Go API): `Mint(budgetUSD float64, ttl time.Duration) (token string)`,
  `Revoke(token string)`.
- HTTP handler: extract Bearer token → `validate` (exists, `now < Expiry`, `SpentUSD < BudgetUSD`)
  → reject (401 unknown/expired, 402 over-budget) → strip `Authorization`, set `X-Api-Key` +
  `anthropic-version` → `httputil.ReverseProxy` to `https://api.anthropic.com` → `ModifyResponse`
  tees and parses usage (single-JSON and SSE `message_start`/`message_delta`/final) →
  `SpentUSD += cost(model, in, out)` from a small static price table.
- **Budget is request-gated:** can't abort an in-flight stream, so it rejects the *next* request
  once `SpentUSD ≥ BudgetUSD`. Documented limitation.
- Binds the vmnet gateway IP (§6), not `0.0.0.0`.

### 5.2 `creds` refactor (targeted improvement to the existing seam)
Replace `Provider.Mint(ttl) (Token, error)` with a grant that carries the env to inject, so static
and gateway providers are interchangeable:
```go
type Grant interface {
    EnvVars() []string   // ["ANTHROPIC_API_KEY=sk-…"]  OR
                         // ["ANTHROPIC_BASE_URL=http://…:8088", "ANTHROPIC_AUTH_TOKEN=tok_…"]
    Revoke() error
}
type Provider interface { Mint(budgetUSD float64) (Grant, error) }
```
- `StaticProvider` (MVP) → grant with `ANTHROPIC_API_KEY`.
- `GatewayProvider{GW *gateway.Gateway, BaseURL string}` → registers a lease, grant carries
  `ANTHROPIC_BASE_URL`+`ANTHROPIC_AUTH_TOKEN`; `Revoke()` deletes the lease.
- Broker injects `grant.EnvVars()` and `defer grant.Revoke()`.

### 5.3 `internal/netfw`
- Compiles `egress.yaml` → a squid `dstdomain` allowlist file; starts/reloads the userspace squid
  bound to the vmnet gateway IP.
- Ensures a **single stable network** exists (created once, not per task) with a known subnet so the
  gateway IP is deterministic; VMs attach to it. No pf, no per-task create/delete.
- The model-API host (`api.anthropic.com`) is removed from the VM-facing allowlist entirely — the VM
  reaches the gateway, and only the gateway (host, unrestricted) reaches Anthropic.

### 5.4 Image / entrypoint changes
- Broker injects `grant.EnvVars()` (base-url + token, or static key) plus
  `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`.
- `init-firewall.sh` shrinks to the pin: allow egress only to `<vmnet-gw-ip>:8088` and `:3128`,
  **drop DNS**. (No per-host IP resolution loop anymore.)
- Verify `npm`/`pip` honor `HTTP(S)_PROXY` (or set their proxy explicitly) — §8.

---

## 6. Networking on a laptop

No pf and no per-task network churn. Instead:
- Create **one stable network** at setup (`container network create --subnet 192.168.<n>.0/24
  macagent-egress`); the gateway (host) IP is the `.1` of that subnet. Tasks attach to it.
- `brokerd` binds the gateway and squid to that `.1` address; the VM reaches them at
  `192.168.<n>.1:8088` / `:3128`.
- The **only** real unknown is small and no-sudo: *can a host process bind the vmnet gateway IP and
  be reached from inside a VM on that network, and what is that IP* (discover via
  `container network inspect`). A quick spike on the dev host answers it before building `netfw`. If
  binding the vmnet gateway IP turns out not to work, the fallback is to bind the gateway/squid on a
  host loopback/LAN IP the VM can reach and point the env/nft at that — still userspace, no pf.

---

## 7. Testing

- **gateway (unit, no network):** token validate / expired / over-budget; header swap
  (`Authorization` stripped, `X-Api-Key` set); usage→cost parsing vs a single-JSON fixture and an
  SSE fixture; stub upstream via `httptest.Server`.
- **creds (unit):** `StaticProvider` and `GatewayProvider` both satisfy `Grant`/`EnvVars`; gateway
  grant `Revoke()` makes a subsequent gateway call 401.
- **netfw (unit):** `egress.yaml` → expected squid allowlist text; deterministic gateway-IP
  derivation from the configured subnet.
- **host integration (this box):** the §6 bind/reachability spike; then end-to-end — from inside the
  VM, the model call via the gateway succeeds, a non-allowlisted host is blocked, and **a direct
  `https://api.anthropic.com` from the VM is blocked** (proving the VM only reaches the gateway).

---

## 8. Open risks / to verify during build

1. **Bind/reachability of the vmnet gateway IP** (§6 spike). Fallback noted there.
2. **`HTTPS_PROXY` honored** by `npm`/`pip` in the image. Raw-socket tools fail closed (nft blocks
   them) — they break rather than tunnel; configure each tool's proxy explicitly.
3. **SSE usage parsing** — confirm the exact event shape claude-code's API calls produce; the budget
   reads `usage` from `message_start`/`message_delta`/final message.
4. **Plaintext VM→gateway hop** carries the bearer token in cleartext on the vmnet. Acceptable
   (host-local, per-task, revocable).
5. **Budget is request-gated, not mid-stream** — one long response can overshoot by a single
   request. Acceptable; documented in §5.1.
6. **In-VM nft is the egress pin** — defeating it needs a kernel LPE inside the VM. Accepted for a
   single-developer laptop (§1).
