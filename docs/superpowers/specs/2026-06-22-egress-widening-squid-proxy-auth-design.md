# Per-task egress widening via squid proxy auth

**Date:** 2026-06-22
**Status:** Approved (design)

## Problem

Per-task egress widening (`egress_extra` + the human approval gate) is
non-functional. The feature has a full apparatus — schema, `ValidateDomains`,
an approval gate (`gateEgressWiden`), audit persistence (`<id>.widen.json`) —
but an approved extra host **never becomes reachable** by the agent:

- Per-domain egress is enforced only by host-side squid, started **once** in
  `cmd/brokerd/main.go` via `netfw.StartSquid(... CompileSquidAllowlist(egCfg) ...)`
  with **default domains only**, and never reconfigured.
- The approve path's only output is `.task/allowlist.txt`
  (`egress.CompileAllowlist(cfg, extras)`), written into the VM work tree by
  `WriteTaskFiles`. **Nothing in the VM reads it** — `image/entrypoint.sh` runs
  `init-firewall.sh <GW> 8088 3128` (default-deny + gateway/proxy ports only).
- The VM default-denies all direct egress, so an approved host must be in
  squid's allowlist to be reachable. It never is.

Root cause (git archaeology): the broker's per-task allowlist was added
(`dff4f29`, 2026-06-15 18:03) **after** `init-firewall.sh` was simplified to
stop reading allowlist files (`9848c5e`, 2026-06-15 10:36). The producer
outlived its consumer.

**Severity:** Fail-closed — approved hosts stay *blocked*, so this is **not** a
sandbox escape; the deny/gate still works (agents cannot self-widen). It is a
**correctness + operator-trust bug**: a human approves access that silently
never materializes, and the task then fails to reach the host with a confusing
403. The threat model (A6) documents widening as a live capability.

## Goal

When an operator approves widening of host `H` for task `T`, the agent in
task `T`'s VM can reach `H` through squid — and **only** task `T` can, even
with other tasks running concurrently (**strict per-task isolation**, chosen
explicitly over broker-wide-during-task and serialize-widened-tasks).

## Non-goals

- Changing the default (non-widened) egress path in any observable way.
- Per-task scoping for the credential gateway (already solved via per-task
  bearer tokens).
- Widening the in-VM nft firewall (egress is funneled through squid; that is
  the enforcement point).

## Chosen approach: per-task proxy auth

Mirror the credential gateway's per-task model. The gateway mints a per-task
bearer token; squid gets a per-task **proxy credential**. Squid rules are
ordered so the default path is evaluated (and allowed) **before** any auth ACL,
so non-widened traffic is never challenged; only a request for an *extra* host
falls through to a per-task `proxy_auth` rule.

Rejected alternatives:
- **B. Per-task squid instance (widened tasks only).** Strict isolation by
  construction and leaves the common path untouched, but requires an **image
  change** to parameterize the proxy port in `entrypoint.sh`'s firewall rule and
  the injected proxy env → a new digest-pinned/threat-model-anchored image and
  re-test. Kept as the fallback if the proxy-auth spike (below) fails.
- **C. Post-start src-IP ACL.** Rejected: the approval gate fires *before* the
  container exists; there is a start-vs-first-request race; and it depends on
  unverified Apple-`container` IP stability/inspectability.

## Components

### netfw — squid control
A new `SquidController` (or extension of `Squid`) owning `runDir` + a `sync.Mutex`:
- `BaseConf(...)` renders `squid.conf` with the `auth_param` block and an
  `include <runDir>/task-acls/*.conf` line (see rule structure below).
- `AddTask(taskUser, secret string, domains []string) error` — writes the token
  line, the `<id>.domains` dstdomain file, and the `task-<id>.conf` fragment,
  then triggers reconfigure. Serialized by the mutex.
- `RemoveTask(taskUser string) error` — removes those files, reconfigure.
  Serialized.
- `reconfigure()` — runs `squid -k reconfigure -f <conf>`. The exec is an
  injectable package var (like `runCLI` in `internal/remote`) so tests capture
  the invocation without a live squid.
- Boot cleanup: clear any stale `task-acls/` and token file left by a
  hard-killed prior broker (mirrors existing stale-pid reaping in
  `reapStaleSquid`).

### Auth helper
Ship a tiny drydock basic-auth helper (≈15 lines) speaking squid's basic-auth
stdin/stdout protocol: read a `username password` line, respond `OK` / `ERR`
by checking the pair against the broker-written token file. This avoids
platform `crypt(3)` format fragility (macOS `crypt` is DES-only) and keeps the
per-task secret an ephemeral plaintext token the broker fully controls.

Decision: **custom minimal helper** over `basic_ncsa_auth` + hashing, for
portability and control. `basic_ncsa_auth` (present in Homebrew squid 7.6
`libexec`, verified) is the documented fallback if the custom helper proves
problematic.

### broker — lifecycle
For **widened tasks only** (i.e. `len(t.EgressExtra) > 0` and approval granted):
1. Mint a per-task proxy secret (`crypto/rand`); task-user = `task-<id>`.
2. `controller.AddTask(user, secret, extraHosts)` — registers + reconfigures
   **before** the container runs.
3. Inject `HTTP_PROXY`/`HTTPS_PROXY=http://<user>:<secret>@<GW>:3128` into that
   VM's env (replacing the unauthed proxy URL for this task only).
4. `defer controller.RemoveTask(user)` on the same `defer` chain that revokes
   the gateway grant, so every exit path (success, deny, cancel, panic)
   deregisters.

Non-widened tasks: unchanged. Plain `HTTP_PROXY=http://<GW>:3128`, no token, no
fragment, never challenged.

## squid.conf rule structure

Base conf:
```
http_port <GW>:3128
auth_param basic program <helper> <runDir>/task-tokens
auth_param basic children 2
acl authed proxy_auth REQUIRED
acl default_dst dstdomain "<runDir>/squid-allow.txt"
acl SSL_ports port 443
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
http_access allow CONNECT default_dst SSL_ports   # common path: NO auth
http_access allow default_dst                     # plain GET defaults: NO auth
include <runDir>/task-acls/*.conf                  # per-task allow rules
http_access deny all
dns_nameservers 1.1.1.1 8.8.8.8
cache deny all
cache_log <runDir>/cache.log
access_log none
pid_filename <runDir>/squid.pid
forwarded_for delete
via off
```

Per-task fragment `task-acls/task-<id>.conf`:
```
acl u_<id> proxy_auth task-<id>
acl d_<id> dstdomain "<runDir>/task-acls/<id>.domains"
http_access allow CONNECT SSL_ports d_<id> u_<id>   # fast ACLs first, auth last
http_access allow d_<id> u_<id>
```

**ACL ordering is load-bearing.** `proxy_auth` is a "slow" squid ACL: when
evaluated it forces a 407 challenge before it can resolve. So the fast
`dstdomain` ACL (`d_<id>`) and `SSL_ports`/`CONNECT` must come **before**
`u_<id>` in each rule. Then a request whose host is *not* this task's extra
fails the rule on `d_<id>` and is never challenged; only a request whose host
*is* `d_<id>` proceeds to the `u_<id>` auth check. Without this ordering, every
non-default request from any task (including non-widened tasks and
genuinely-blocked hosts) would get a 407 instead of a clean deny-all 403 —
breaking the common path and the breach-demo's 403 assertion.

Ordering guarantees, end to end: a non-widened request to a default host
matches `allow ... default_dst` before any `proxy_auth` ACL is evaluated → no
407. A request to a host that is no task's extra fails every fragment on the
fast `d_<id>` check → clean deny-all 403, no challenge. A widened request to
*its own* extra host matches `d_<id>`, hits `u_<id>` → 407 → the VM (carrying
creds) retries → allowed for *its* domains only.

## Data flow (widened task)

```
approve gate (existing)
  └─ mint proxy secret
  └─ controller.AddTask(user, secret, extras)  → write token + domains + fragment → reconfigure
  └─ inject HTTPS_PROXY=http://user:secret@GW:3128 into THIS vm env
  └─ run container
  └─ defer controller.RemoveTask(user)         → remove files → reconfigure   (all exit paths)
```

## Concurrency & cleanup

- All `AddTask`/`RemoveTask` mutations (file writes + reconfigure) serialize
  behind one controller mutex. `squid -k reconfigure` is online, cheap, and
  idempotent.
- Cleanup rides the existing task `defer` chain (same site as grant revoke,
  `broker.go:389`), so panic/cancel still deregisters.
- Boot cleanup clears stale `task-acls/` + token file from a prior hard-killed
  broker.

## Error handling

- Any failure on the widening path (helper write, fragment write, reconfigure)
  aborts the task with a clear error **before** the container runs — fail-closed.
  The extra simply stays unreachable; the path never silently reports success.
- Reconfigure failure → task aborts rather than running with a dead credential.

## Testing

- **netfw (unit):** base-conf render; `AddTask`/`RemoveTask` file content +
  that reconfigure is invoked (injected exec, captured argv); auth-helper
  `OK`/`ERR` protocol against a token file (valid pair, wrong secret, unknown
  user).
- **broker (unit):** widened task registers then deregisters on **every** exit
  path (success, deny, cancel, error); non-widened task touches none of the
  squid-control surface; proxy env carries creds only for widened tasks.
- **integration (existing demo):** `redteam`/`breach` asserts an approved host
  becomes reachable, a non-approved host still 403s, and a second concurrent
  task cannot reach task A's approved host (cross-task isolation).

## First implementation gate (spike)

Step 1 of the plan stands up the base conf + helper + one static per-task
fragment and verifies **both** claude-code and codex send `Proxy-Authorization`
on a 407 for an extra host (Node HTTP clients generally honor proxy userinfo,
but this is unverified for these CLIs). If either fails, **stop** and implement
fallback **B** (per-task squid instance) instead. Everything after step 1
depends on this gate passing.

## Risks

- **Agent proxy-auth support** (gated by the spike above).
- **Reconfigure churn** under high widened-task concurrency — bounded by the
  mutex; reconfigure is cheap, but worth a load sanity check.
- **Helper path discovery** — the helper must be locatable next to the broker
  or shipped at a known path; the base conf references an absolute path.
