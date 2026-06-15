# Isolated Claude Code on macOS via `apple/container` + a session broker

**Status:** Design (approved for implementation planning)
**Date:** 2026-06-14
**Author:** brainstormed with Claude Code

---

## 1. Goal & hard requirements

Run **Claude Code as an autonomous coding agent inside hardware-isolated Linux VMs** on
macOS (Apple silicon) via Apple's `container` tool. The agent reads/writes a repo and runs
arbitrary shell commands but must not reach the host filesystem, host secrets, or the open
internet beyond an explicit, config-defined allowlist.

Non-negotiable requirements (design around these):

1. **`git push` happens on the host, never in the sandbox.** The VM works against a staged
   copy of the repo; only a diff/bundle crosses back out. A trusted host-side step does the
   actual `git push` / PR creation with credentials the agent never sees.
2. **Egress is deny-by-default**, driven by a single source-of-truth config the broker
   compiles into per-VM enforcement. No secrets in URLs.
3. **The flow supports `claude --dangerously-skip-permissions`** for unattended runs. The VM
   boundary + egress allowlist are the containment that justifies the flag.

### Decisions locked during brainstorming

- **VM lifecycle:** warm pool (assign → run → wipe workdir → return; recycle after K/T).
- **Auth:** support both `ANTHROPIC_API_KEY` injection and OAuth, with the risk of each stated;
  recommend the gateway path (§7).
- **Egress:** minimal default (model API + registries, **no git host**); per-task widening
  allowed but gated by broker approval.
- **Broker shape:** Go daemon (HTTP API + warm pool + firewall compiler), with a
  weekend-buildable subset carved out.

---

## 2. Headline finding that shapes the design

`apple/container` has **no native per-VM egress allowlist**. `container network create` offers
`--internal` (fully air-gapped, all-or-nothing) and `--subnet`, but no "allow these
domains/ports" knob. **Requirement #2 cannot be met by a container flag — the broker must
implement egress filtering itself.** Two mechanisms, MVP then hardening:

- **MVP:** in-VM `nft` default-deny allowlist, installed as root in the entrypoint *before*
  dropping to the non-root agent (ports the Anthropic devcontainer `init-firewall.sh`).
- **Hardening (v2):** out-of-VM forward proxy — enforcement moves into the host TCB (§6).

### Verified CLI facts (2026-06)

- `container run`: `--memory`, `--cpus`, `--user name|uid[:gid]`, `--volume`, `--mount
  type=,source=,target=,readonly`, `--env key=value`, `--network name[,mac][,mtu]`,
  `--rm/--remove`, `--detach/-d`.
- `container cp <src> <dst>` with `container_id:/path` form.
- `container network create [--internal] [--subnet] [--subnet-v6]`; `network delete/list`.
- `claude`: `-p/--print`, `--output-format text|json|stream-json`, `--verbose`,
  `--include-partial-messages`, `--allowedTools`, `--permission-mode`, and **`--bare`**
  (skips OAuth/keychain/MCP/CLAUDE.md auto-discovery; auth must come from `ANTHROPIC_API_KEY`
  or `apiKeyHelper`) — the recommended mode for scripted/headless runs.
- Billing note: as of 2026-06-15, `claude -p` on a *subscription* draws from a separate
  monthly Agent SDK credit; the API-key/gateway path bills as API usage and supports
  workspace spend caps.

---

## 3. Threat model

**Adversary:** untrusted code running as the non-root agent user — from the repo, a malicious
dependency, or prompt-injection turning a fetched page into shell. With bypass mode, assume
arbitrary code with intent to exfiltrate and escape.

| Threat | Primary control | Backstop |
|---|---|---|
| Read host FS / secrets | Nothing host-side mounted; no `~/.ssh`, no cloud creds | Per-VM HW isolation |
| Exfil `~/.claude` / key | Egress deny-by-default | Scoped/revocable credential (§7) |
| Steal push creds | Push is host-side; VM has no git remote creds or egress | — |
| Tamper with firewall | nft set as root pre-priv-drop; agent has no sudo | Ephemeral VM; out-of-VM proxy (§6) |
| Persist across tasks | Workdir wipe + recycle after K/T | Periodic full teardown |
| Runaway resources | `--memory`/`--cpus` caps + broker wall-clock timeout | VM kill + recycle |
| VM/kernel escape | apple/container per-VM boundary | **Out of scope** |

**Out of scope:** hypervisor/kernel zero-day escape, base-image supply-chain compromise,
malicious broker code (broker is the TCB), compromised Anthropic endpoint.

---

## 4. Reference architecture

```
┌──────────────────────── HOST (Trusted Computing Base) ─────────────────────────┐
│  brokerd (Go)                                                                    │
│   ├── HTTP API:  POST /tasks {repo_ref, instruction, egress_extra?, sensitive?}  │
│   ├── Pool Manager:   warm[N] VMs, lease/return, recycle after K|T               │
│   ├── Firewall Compiler:  egress.yaml ─▶ allowlist + rules (nft MVP / squid v2)  │
│   ├── Credential Broker:  scoped token / gateway (§7)                            │
│   ├── Stager:   git clone (host) ─▶ stage in / collect diff out                  │
│   ├── Auditor:  capture stream-json, per-command log, replay                     │
│   └── Approval Gate:  pause before push / before egress widening                 │
│   git (host creds) ── push / gh pr create   ◀── diff bundle                      │
│                                                                                  │
│  ╞════════════════ TRUST BOUNDARY (only diffs cross inward → push) ═════════════╡│
│                                                                                  │
│  Warm pool of per-task Linux VMs (hardware-isolated)                             │
│   ┌────────────────────────────────────────────────────────────────────────┐   │
│   │ entrypoint as ROOT → install egress filter → drop priv → exec as agent:  │   │
│   │   claude --bare -p "<task>" --dangerously-skip-permissions \             │   │
│   │          --output-format stream-json --verbose                          │   │
│   │ workdir /work (scratch, wiped on return); NO host mounts of secrets      │   │
│   │ egress: model API + registries only (no git host)                       │   │
│   └────────────────────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────────────────────┘
```

**Boundary in one sentence:** untrusted code runs only inside the VM; the only thing crossing
back to the host is a diff (data, not commands), and the only thing injected inward is a
scoped token + the task.

---

## 5. Variants & recommendation

1. **Fully-ephemeral per-task VM** — `run --rm` per task. Max isolation, no state bleed,
   dodges the memory caveat; pays full cold boot per task.
2. **Broker-mediated warm pool (RECOMMENDED)** — N pre-booted VMs, workdir-wipe between tasks,
   recycle after K/T. Near-zero task latency, leak bounded, isolation via reset discipline.
3. **Persistent dev-VM** — one long-lived VM. Fastest/simplest but task N sees task N-1
   residue and memory grows unbounded. Avoid for autonomous/untrusted runs.

**Recommendation:** Variant 2, with a `sensitive: true` task flag that drops to Variant 1
(fresh `--rm` VM) for high-sensitivity work.

---

## 6. Egress enforcement

### 6.1 MVP — in-VM `nft` allowlist

`entrypoint.sh` runs as root, installs an `nft` default-deny output policy that allows only
DNS + the allowlisted domains' resolved IPs on :443, then `gosu agent` execs claude. The agent
is non-root with no sudo, so it cannot flush nft. **Weakness:** enforcement lives inside the
untrusted VM (safe only because it is set pre-priv-drop; a kernel LPE would defeat it). DNS
must be allowed for in-VM resolution → a DNS exfil channel exists.

### 6.2 Hardening (v2) — out-of-VM forward proxy

Move enforcement into the TCB. Run **squid as a host process, per VM**, bound to the vmnet
gateway. Three layers, two in the TCB:

| Layer | Where | Enforces | Role |
|---|---|---|---|
| **pf** (host) | TCB | VM subnet reaches nothing except `gateway:3128` | Primary — survives a rooted VM |
| **squid** (host) | TCB | which *domains* proxied traffic may reach | Primary — the allowlist itself |
| **nft** (in-VM) | VM | only egress is the proxy; no DNS | Belt-and-suspenders |

- **Why it beats in-VM nft:** the allowlist lives in the TCB, and filtering by hostname (the
  `CONNECT` target) means **name resolution happens at the proxy → the VM needs zero DNS egress
  → the DNS exfil channel closes.** Even a fully-rooted VM stays pinned to the proxy by pf and
  bounded to allowlisted domains by squid.
- **TLS:** CONNECT tunneling, **no interception** — squid allows/denies by hostname before the
  handshake; traffic stays end-to-end encrypted to Anthropic; no MITM CA in the image. A
  TLS-terminating proxy (inject CA, decrypt) buys path-level rules at the cost of a larger
  attack surface and decryptable model traffic — **don't**.
- **Proxy placement:** host process, not a container. A proxy *container* on an `--internal`
  net also has no external route; dual-homing it across two networks is unverified in v0.x.
  The host is already the TCB and already has internet.
- **Per-VM squid instance** so a per-task widened allowlist can't bleed into a concurrent task.

**v0.x unknown (spike before relying on this):** exact vmnet interface name, whether the host
NATs/forwards container egress on a path pf can filter, and `--internal` reachability
semantics. Fallback if pf-on-vmnet is unworkable: keep squid's TCB-side domain allowlist, rely
on in-VM nft "only proxy" for the routing pin (degrades the *pin* to the nft trust model, but
keeps the allowlist in the TCB).

### 6.3 Does the VM need git-host egress?

**No.** Push is host-side and the broker stages the repo in via `git clone` on the host +
`cp`/mount. The default allowlist is therefore: `api.anthropic.com` + package registries
(`registry.npmjs.org`, `.pypi.org`, `files.pythonhosted.org`) + (MVP only) DNS. No `github.com`.

---

## 7. Credential brokering

`ANTHROPIC_API_KEY` is a long-lived org/workspace credential; the API authenticates with
`x-api-key` (bearer-equivalent). **There is no native per-task / short-TTL token** — scoping
must be built. Three options, descending strength:

### Option A — Auth-injecting gateway (RECOMMENDED)

The VM holds **no Anthropic key**. A broker-held reverse proxy in front of `api.anthropic.com`
holds the real key and injects it outbound; the VM gets only an opaque per-task token.

```
VM:  ANTHROPIC_BASE_URL=http://<gateway>   ANTHROPIC_AUTH_TOKEN=tok_<opaque>
        │ Authorization: Bearer tok_...
        ▼
Gateway (TCB): validate tok → {real_key, expiry, budget, task_id}; reject if expired/over budget
        swap Bearer tok → x-api-key: <REAL_KEY>; decrement budget; forward
        ▼
     api.anthropic.com
```

Blast radius if the token leaks: usable only through your gateway, revoked instantly (delete
one map entry), capped by TTL + per-task budget. The real key never enters untrusted territory.
Reuses the §6.2 proxy infra (same principle: credential in TCB, VM holds a revocable token).
**Verify:** `claude-code` honors `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` and sends
`Authorization: Bearer` to the base URL.

### Option B — Admin-API minted workspace key

Broker holds an admin key (never in a VM); creates a workspace-scoped key with a spend limit
per task, injects it, disables it on task end. Real Anthropic-side spend cap, no gateway to
run; but it is create-then-disable (a leak works until revocation lands), and the scoped real
key sits in the VM. **Verify** key *creation* is exposed via the Admin API vs Console-only.

### Option C — Static broker key + workspace spend cap (weekend MVP)

One workspace key, injected per task, rotated on a schedule. No per-task isolation; a leak is
exposed until rotation. Acceptable only because the egress allowlist prevents exfiltration; the
**workspace spend limit is the hard backstop** — set it regardless of option.

| | Real key in VM? | Leak blast radius | Revoke speed | Build cost |
|---|---|---|---|---|
| **A — Gateway** | No | usable only via your gateway, until deleted; TTL+budget capped | instant | ~30 lines (reuses proxy) |
| **B — Admin-minted** | Yes (scoped) | workspace spend cap, until disable lands | seconds | Admin-API integration |
| **C — Static+cap** | Yes (shared) | workspace spend cap, until rotation | rotation interval | trivial |

**Recommendation:** ship C for the weekend (with a workspace spend limit), graduate to A in the
same v2 pass that adds the egress proxy — they are one piece of infrastructure.

### OAuth alternative (per the auth decision)

OAuth in a mounted `~/.claude` avoids API-key management but persists a longer-lived,
broader-scope credential inside an untrusted VM, `--bare` skips it, and it is harder to rotate —
a strictly worse exfil profile than a scoped token. Use only if subscription billing is
specifically wanted.

---

## 8. Bypass-mode safety envelope

What justifies `--dangerously-skip-permissions`: (1) per-VM hardware isolation, (2) egress
deny-by-default, (3) zero host secrets mounted, (4) ephemeral/recycled VM, (5) git diff as a
perfect undo (nothing merges without the host push step). **Residual risk:** in-VM destruction
within a task (fine — throwaway), credential exfil *if* the allowlist is widened (controlled by
the approval gate), VM escape (out of scope). In-VM tool prompts are redundant once the boundary
is real — turning them off removes friction, not containment.

---

## 9. Human-in-the-loop gates

In-VM prompts are off, so gates move to the broker:

- **Before push:** broker presents the diff; push only on approval (tiered — auto-push
  low-scope tasks, gate `sensitive`/widened ones).
- **Before egress widening:** any `egress_extra` beyond the default requires approval.
- **Before elevated-scope tasks:** `sensitive: true` forces a fresh `--rm` VM.

---

## 10. Observability

- Capture `--output-format stream-json` to a per-task audit log (tool calls, Bash commands,
  file writes, web fetches, subagent spawns).
- Per-command structured log keyed on task_id; session replay from the stream.
- Record the compiled allowlist + rendered rules per task for audit.

---

## 11. Failure modes

- **Hung agent:** broker wall-clock timeout (e.g. 30 min) → kill VM, recycle.
- **Crash / partial work:** diff is still collected; mark task failed, attach partial diff.
- **Dead VM:** pool health check evicts and recycles.
- **Broker restart:** pool state must be reconcilable (running containers discoverable via
  `container list`); in-flight tasks marked indeterminate, not silently retried.
- **Background tasks:** `claude -p` terminates background Bash ~5s after the final result; do
  not rely on long-lived in-VM daemons surviving the run.

---

## 12. MVP artifacts

### 12.1 `container run` (pooled path)

```bash
container run --rm \
  --name "task-${TASK_ID}" \
  --user agent \
  --memory 4G --cpus 4 \
  --network "egress-${VM_ID}" \
  --env ANTHROPIC_API_KEY="${SCOPED_KEY}" \
  --env TASK_PROMPT_FILE=/work/.task/prompt.txt \
  --mount type=bind,source="${STAGE_DIR}",target=/work,readonly=false \
  claude-sandbox:latest \
  /usr/local/bin/entrypoint.sh
```

Fallbacks: if per-VM bind-mount is flaky in v0.x, use `container cp ./stage/. task:/work` after
`create`/`start`. **Keep the broker + helper binaries out of `~/Documents` and `~/Desktop`** (a
known vmnet-creation bug).

### 12.2 Dockerfile

```dockerfile
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl jq nftables dnsutils ipset gosu \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code            # pin a version in real use
RUN useradd -m -u 10001 -s /bin/bash agent
COPY init-firewall.sh /usr/local/bin/init-firewall.sh
COPY entrypoint.sh    /usr/local/bin/entrypoint.sh
COPY allowlist.txt    /etc/sandbox/allowlist.txt        # compiled by broker
RUN chmod 0755 /usr/local/bin/*.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

```bash
# entrypoint.sh — root installs egress filter, THEN drops privileges
set -euo pipefail
/usr/local/bin/init-firewall.sh /etc/sandbox/allowlist.txt
cd /work
exec gosu agent claude --bare -p "$(cat "$TASK_PROMPT_FILE")" \
     --dangerously-skip-permissions \
     --output-format stream-json --verbose --include-partial-messages
```

```bash
# init-firewall.sh — nft default-deny + allowlist (MVP egress)
set -euo pipefail
ALLOW="$1"
nft flush ruleset
nft add table inet fw
nft add chain inet fw out '{ type filter hook output priority 0; policy drop; }'
nft add rule inet fw out ct state established,related accept
nft add rule inet fw out oifname "lo" accept
nft add rule inet fw out udp dport 53 accept
nft add rule inet fw out tcp dport 53 accept
nft add set inet fw allow4 '{ type ipv4_addr; flags interval; }'
while read -r host port; do
  [ -z "$host" ] && continue
  for ip in $(getent ahostsv4 "$host" | awk '{print $1}' | sort -u); do
    nft add element inet fw allow4 "{ $ip }"
  done
done < "$ALLOW"
nft add rule inet fw out ip daddr @allow4 tcp dport '{ 443 }' accept
```

### 12.3 Egress config (source of truth)

```yaml
# egress.yaml
version: 1
default:
  allow_dns: true
  domains:
    - { host: api.anthropic.com,   ports: [443] }
    - { host: registry.npmjs.org,  ports: [443] }
    - { host: pypi.org,            ports: [443] }
    - { host: files.pythonhosted.org, ports: [443] }
  cidrs: []
  # no github.com — push is host-side, repo staged in. VM needs no git egress.
per_task_widening:
  requires_approval: true
```

Compiler: merge `default` + approved `egress_extra` → `allowlist.txt` (`host port` lines) for
the nft MVP, or squid `dstdomain` allowlist + pf `pass` rule for the v2 proxy. Domains are
resolved at VM boot inside `init-firewall.sh` (MVP) or by squid (v2). v2 closes DNS egress.

### 12.4 v2 proxy artifacts

```squid
# squid.conf (per VM)
http_port 192.168.66.1:3128
acl allowed   dstdomain "/etc/squid/sandbox/allow-66.txt"
acl SSL_ports port 443
acl CONNECT   method CONNECT
http_access deny CONNECT !SSL_ports
http_access deny CONNECT !allowed
http_access allow CONNECT allowed SSL_ports
http_access allow allowed
http_access deny all
dns_nameservers 1.1.1.1 8.8.8.8
cache deny all
forwarded_for delete
via off
```

```pf
# /etc/pf.anchors/sandbox — Layer 1
pass  out quick proto tcp from 192.168.66.0/24 to 192.168.66.1 port 3128
block drop out quick from 192.168.66.0/24 to any
```

```bash
# in-VM v2 env + shrunken nft
export HTTPS_PROXY=http://192.168.66.1:3128
export HTTP_PROXY=http://192.168.66.1:3128
export NO_PROXY=127.0.0.1,localhost
nft add rule inet fw out ip daddr 192.168.66.1 tcp dport 3128 accept   # no :53 rule
```

### 12.5 brokerd (Go) — core flow

```go
type EgressDomain struct{ Host string; Ports []int }
type EgressConfig struct {
    Default struct{ AllowDNS bool; Domains []EgressDomain; CIDRs []string } `yaml:"default"`
    PerTaskWidening struct{ RequiresApproval bool } `yaml:"per_task_widening"`
}
type Task struct{ RepoRef, Instruction string; EgressExtra []EgressDomain; Sensitive bool }

func compileAllowlist(cfg EgressConfig, extra []EgressDomain) []byte {
    var b bytes.Buffer
    for _, d := range append(cfg.Default.Domains, extra...) {
        for _, p := range d.Ports { fmt.Fprintf(&b, "%s %d\n", d.Host, p) }
    }
    return b.Bytes()
}

func handleTask(w http.ResponseWriter, r *http.Request) {
    var t Task; _ = json.NewDecoder(r.Body).Decode(&t)
    if len(t.EgressExtra) > 0 && cfg.PerTaskWidening.RequiresApproval {
        if !approvalGate("widen egress", t.EgressExtra) { http.Error(w, "denied", 403); return }
    }
    taskID := newID()
    stage := filepath.Join("/tmp/broker/stage", taskID)

    run("git", "clone", "--depth", "1", t.RepoRef, stage)          // stage IN on host
    os.MkdirAll(filepath.Join(stage, ".task"), 0o755)
    os.WriteFile(filepath.Join(stage, ".task/prompt.txt"), []byte(t.Instruction), 0o644)
    os.WriteFile(filepath.Join(stage, ".task/allowlist.txt"), compileAllowlist(cfg, t.EgressExtra), 0o644)

    vm := pool.Lease(t.Sensitive)                                  // WEEKEND: always --rm
    defer pool.Return(vm, taskID)                                  // wipe /work, recycle K/T

    key := creds.MintScoped(15 * time.Minute)                      // WEEKEND: static key from file
    defer creds.Revoke(key)

    logf := openAudit(taskID)
    cmd := exec.Command("container", "run", "--rm",
        "--name", "task-"+taskID, "--user", "agent",
        "--memory", "4G", "--cpus", "4", "--network", vm.Net,
        "--env", "ANTHROPIC_API_KEY="+key.Value,
        "--env", "TASK_PROMPT_FILE=/work/.task/prompt.txt",
        "--mount", "type=bind,source="+stage+",target=/work,readonly=false",
        "claude-sandbox:latest", "/usr/local/bin/entrypoint.sh")
    cmd.Stdout, cmd.Stderr = io.MultiWriter(logf, parseStream(taskID)), logf
    ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
    defer cancel()
    if err := runCtx(ctx, cmd); err != nil { http.Error(w, "task failed", 500); return }

    diff := captureDiff(stage)                                     // data crosses the boundary
    if !approvalGate("push diff", diff) { writeJSON(w, map[string]any{"diff": diff, "pushed": false}); return }

    branch := "agent/" + taskID
    run("git", "-C", stage, "checkout", "-b", branch)
    run("git", "-C", stage, "commit", "-am", "agent: "+t.Instruction)
    run("git", "-C", stage, "push", "origin", branch)              // HOST creds VM never saw
    run("gh", "pr", "create", "--head", branch, "--fill")
    writeJSON(w, map[string]any{"branch": branch, "pushed": true})
}
```

**Weekend-buildable subset:** skip the pool (always `--rm`), skip scoped-key minting (static key
from a file), skip the proxy (in-VM nft). Keeps the full security spine — boundary, egress
allowlist, no host secrets in-VM, host-side push — in one afternoon. Layer pool + gateway +
out-of-VM proxy as v2.

---

## 13. `apple/container` vs the Docker devcontainer

| Dimension | Anthropic devcontainer (Docker) | This design (apple/container) |
|---|---|---|
| Isolation | Shared host kernel (namespaces) | **Per-VM hardware isolation** — core primitive |
| Egress allowlist | `init-firewall.sh` in-container | Same script, **no native flag**; in-VM nft or out-of-VM proxy |
| Memory | Returned to host on free | **Freed pages NOT returned** → recycle pooled VMs after K/T |
| Networking | Docker bridge | Per-VM vmnet; **`~/Documents`/`~/Desktop` helper-binary bug** |
| Maturity | Mature | **v0.x** — bind-mount/cp, network create may be flaky |
| Persistent volumes | Named volumes | Host scratch + `cp`/bind; no Docker volume semantics |

---

## 14. Open questions / residual risks

1. **`HTTPS_PROXY` honored?** (#1 spike for the v2 proxy.) Verify `claude-code` (undici),
   `npm`, `pip` all route through the proxy; undici historically did not auto-read proxy env.
   Anything that opens a raw socket fails closed (blocked by pf), so it breaks rather than
   tunnels — each tool's proxy must be configured explicitly.
2. **pf ↔ vmnet integration** (v0.x unknown) — confirm the host forwards VM egress on a
   filterable path; fallback in §6.2.
3. **Scoped-key mechanism** — resolved in §7 (gateway recommended); verify
   `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` behavior and Admin-API key creation.
4. **DNS-as-exfil** in the MVP (port 53 open) — closed by the v2 proxy; accept for MVP.
5. **Domain fronting / shared-CDN** — SNI-based allow can be abused if an allowlisted CDN
   honors a mismatched Host header; largely dead on modern CDNs.
6. **No payload inspection** (no-MITM proxy) — exfil inside an allowed channel is undetectable;
   fundamental, accept.
7. **v0.x bind-mount/cp reliability** under concurrent pool load — unverified; spike.
8. **Approval-gate UX vs unattended** — tier gates (auto low-scope, gate sensitive/widened).
9. **Warm-pool reset correctness** — the lease/return/recycle state machine and broker-restart
   reconciliation need their own detailed design (next depth area).
