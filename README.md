# macagent

Run Claude Code as an autonomous coding agent on macOS, inside a per-task
hardware-isolated VM with deny-by-default egress and a host-side credential
gateway. The VM never sees your Anthropic key, can only reach an allowlisted
set of hosts, and the only thing that leaves it is a captured `git diff`.

The narrative explainer lives at [`site/index.html`](site/index.html). This
README is the operator's manual.

---

## What it is

- **brokerd** — a Go daemon you run on macOS. Stages the repo, mints a
  per-task budgeted credential, runs the sandbox VM, captures the diff, and
  pushes a branch on approval.
- **credential gateway** — a host-only HTTP service that proxies
  `api.anthropic.com` for the VM. The real API key lives only on the host.
  The VM uses a short-lived bearer token tied to a USD budget and TTL.
- **squid (userspace)** — a hostname-allowlisted forward proxy for registry
  egress (npm, pip). No TLS interception; squid resolves names on the host.
- **sandbox image** — a Linux VM image with `claude-code`, an `agent`
  user, and a root-installed `nft` default-deny output policy that allows
  only the gateway and the proxy.

What it is *not*: there is no per-task pf surgery on the host, no shared
volumes between VM and host, and no way for the VM to mint, refresh, or
exfiltrate the real API key.

---

## Architecture

```
                       host (macOS)
                  ┌───────────────────────────┐
   you ──POST──▶  │  brokerd  127.0.0.1:8765  │
                  │     │                     │
                  │     ▼ stage repo ──▶ /tmp │
                  │     │                     │
                  │     ▼ container run ──┐   │
                  │  ┌─────────────────┐  │   │
                  │  │ credential GW    │◀┼──┐│
                  │  │ 192.168.66.1:8088│  │  ││ api.anthropic.com
                  │  └─────────────────┘  │  │└──────────────────▶ Anthropic
                  │  ┌─────────────────┐  │  │
                  │  │ squid           │◀─┤  │  registry.npmjs.org, pypi.org …
                  │  │ 192.168.66.1:3128│ │  └──────────────────▶ allowed hosts
                  │  └─────────────────┘  │
                  │   bound to vmnet GW   │
                  └───────────────────────┼─┘
                                          │  vmnet (macagent-egress, 192.168.66.0/24)
                            ┌─────────────┴────────────┐
                            │  sandbox VM (per task)   │
                            │  192.168.66.x            │
                            │  • nft default-deny out  │
                            │  • allows only GW:8088   │
                            │             and GW:3128  │
                            │  • runs `claude --bare`  │
                            └──────────────────────────┘
```

Two egress lanes, both pinned to the vmnet gateway IP:

| Lane | Port | Used by | Auth |
|------|------|---------|------|
| credential gateway | 8088 | model API calls | bearer token minted per task, budget + TTL bound |
| squid forward proxy | 3128 | npm/pip & other allowlisted HTTPS | none; hostname allowlist |

All other VM egress (other ports, other IPs, DNS) is dropped by `nft` in the
VM. DNS being dropped is intentional — the VM relies on squid for name
resolution of allowed hosts; everything else has no way to resolve in the
first place.

---

## Prerequisites

```bash
# Apple's container runtime (https://github.com/apple/container)
brew install --cask container         # or follow upstream install
container system start --enable-kernel-install

# Userspace squid for the registry proxy
brew install squid

# Go (any 1.22+) to build brokerd
brew install go

# Create the stable egress network with the .1 gateway brokerd expects
container network create --subnet 192.168.66.0/24 macagent-egress

# Build the sandbox image
container build -t claude-sandbox:latest image/
```

### Sanity-check the network

```bash
container network inspect macagent-egress | jq .[].status
# ipv4Gateway must be 192.168.66.1 (matches MACAGENT_GW_IP default)
```

If the npm step of the image build fails with `ETIMEDOUT` against
`registry.npmjs.org`, it's a transient builder-VM network hiccup — rerun
`container build` once.

---

## First run (smoke test, no Anthropic spend)

This boots brokerd, brings up the anchor + squid + gateway listeners, and
exercises both egress lanes from a sandbox VM. The credential gateway
returns 401 to a placeholder key, which is the point — it proves the auth
path is live without spending tokens.

```bash
# 1. Build and run brokerd
go build -o /tmp/brokerd ./cmd/brokerd
ANTHROPIC_API_KEY=sk-ant-fake \
MACAGENT_NETWORK=macagent-egress \
MACAGENT_GW_IP=192.168.66.1 \
SANDBOX_IMAGE=claude-sandbox:latest \
  /tmp/brokerd &

# Expected log lines:
#   network anchor up on macagent-egress
#   squid listening on 192.168.66.1:3128
#   gateway listening on 192.168.66.1:8088
#   brokerd listening on 127.0.0.1:8765 (gateway …, squid …)

# 2. From a throwaway VM on macagent-egress, install the firewall pin and probe
container run --rm --network macagent-egress --cap-add CAP_NET_ADMIN \
  --entrypoint /bin/bash claude-sandbox:latest -c '
    /usr/local/bin/init-firewall.sh 192.168.66.1 8088 3128
    curl -sS --max-time 5 -o /dev/null -w "gateway:    %{http_code}\n" http://192.168.66.1:8088/
    curl -sS --max-time 8 -x http://192.168.66.1:3128 \
      -o /dev/null -w "registry:   %{http_code}\n" https://registry.npmjs.org/
    curl -sS --max-time 3 -o /dev/null -w "direct egress (expect 0): %{http_code}\n" https://example.com || true
'

# Expected:
#   gateway:    401
#   registry:   200
#   direct egress (expect 0): 000      ← name resolution dropped

# 3. Shut down
kill %1
container ls -a | grep macagent       # macagent-anchor should be gone
```

If those three lines match, every plumbing surface — anchor, gateway,
squid, in-VM `nft` — is working.

---

## Submitting a task

`POST /tasks` to `http://127.0.0.1:8765/tasks`. The broker stages the repo,
mints a budgeted credential, runs `claude --bare` inside a fresh VM, and
returns either a captured diff or a pushed branch.

```bash
curl -sS http://127.0.0.1:8765/tasks \
  -H 'content-type: application/json' \
  -d '{
        "repo_ref":    "/Users/you/code/your-repo",
        "instruction": "Add a one-line comment to README.md explaining the project.",
        "egress_extra": [],
        "sensitive":   false
      }'
```

Response shape:

```json
{ "task_id": "ab12cd34ef56", "branch": "agent/ab12cd34ef56", "pushed": true }
```

…or, if there was no diff:

```json
{ "task_id": "ab12cd34ef56", "diff": "", "pushed": false }
```

Each task spends real Anthropic tokens against `ANTHROPIC_API_KEY`, capped
at `MACAGENT_TASK_BUDGET_USD` (default $2.00) and `30m` wall-clock by
default. The credential gateway rejects model calls once the budget is hit.

### Widening egress per task

`egress_extra` is a list of `{host, ports}` objects added to the squid
allowlist for that task only. If `per_task_widening.requires_approval` is
true in `config/egress.yaml` (the default), brokerd consults its approval
hook before honoring the extras. The MVP hook auto-approves and logs to
stdout; replace `Broker.Approve` to gate on human review or a policy
engine.

### Approving the diff push

Once the VM exits cleanly, brokerd runs the diff through the same approval
hook (`"push diff"`). On approval, it commits as `agent/<task_id>` and
pushes to the repo's origin; on rejection, the diff is returned in the
response but never reaches the remote. **All staging directories are wiped
on task completion** — there is no host-visible scratch left behind.

---

## Egress policy: `config/egress.yaml`

```yaml
version: 1
default:
  allow_dns: true
  domains:
    - { host: api.anthropic.com,      ports: [443] }   # routed via gateway
    - { host: registry.npmjs.org,     ports: [443] }   # routed via squid
    - { host: pypi.org,               ports: [443] }   # routed via squid
    - { host: files.pythonhosted.org, ports: [443] }   # routed via squid
  cidrs: []
per_task_widening:
  requires_approval: true
```

- `api.anthropic.com` is **intentionally excluded** from the squid
  allowlist (see [`internal/netfw/netfw.go`](internal/netfw/netfw.go)) —
  it goes through the credential gateway, not the proxy. Listing it in
  config is what marks model-API egress as policy-allowed; the routing
  split is then handled in code.
- `allow_dns` is currently advisory; the in-VM nft pin is the source of
  truth and drops UDP/53 because squid does host-side resolution.
- New default hosts: edit the YAML and restart brokerd. squid re-reads its
  allowlist file at start.

---

## Configuration reference

All knobs are environment variables on the brokerd process.

| Var | Default | Meaning |
|-----|---------|---------|
| `ANTHROPIC_API_KEY` | *(required)* | Real key, lives only on the host; never reaches the VM |
| `EGRESS_CONFIG` | `config/egress.yaml` | Path to the YAML above |
| `SANDBOX_IMAGE` | `claude-sandbox:latest` | Image used for the anchor and every task VM |
| `MACAGENT_NETWORK` | `macagent-egress` | vmnet network name (must already exist) |
| `MACAGENT_GW_IP` | `192.168.66.1` | The `.1` of the network's subnet; gateway + squid bind here |
| `MACAGENT_TASK_BUDGET_USD` | `2.0` | Hard ceiling per task; gateway rejects once exhausted |
| `STAGE_ROOT` | `/tmp/broker/stage` | Per-task work tree (wiped on completion) |
| `AUDIT_ROOT` | `/tmp/broker/audit` | Per-task `<id>.jsonl` log of the VM's stream-json output |
| `SQUID_RUN_DIR` | `/tmp/broker/squid` | squid pid/conf/cache.log + compiled allowlist |
| `BROKER_ADDR` | `127.0.0.1:8765` | Where brokerd listens for `POST /tasks` |

The squid and gateway port numbers (`3128`, `8088`) are hard-coded in
`cmd/brokerd/main.go` — change them there and in the entrypoint's
`init-firewall.sh` invocation together.

---

## Operational notes

### Boot order matters

The vmnet gateway IP only exists while a container is attached to the
network. brokerd therefore:

1. Removes any stale `macagent-anchor` container.
2. Starts a fresh `macagent-anchor` (the sandbox image running
   `sleep infinity`) on `macagent-egress`. The vmnet plugin brings up
   `192.168.66.1` on the host.
3. Polls `net.Listen` on `:3128` and `:8088` until they're bindable
   (interface up).
4. Launches squid bound to `192.168.66.1:3128`.
5. Launches the credential gateway on `192.168.66.1:8088`.
6. Starts the HTTP API on `127.0.0.1:8765`.

If brokerd exits non-cleanly (`SIGKILL`, panic), the anchor container can
survive. The next start removes it; you can also clean up manually with
`container rm -f macagent-anchor`.

### Cleanup on SIGTERM

`SIGTERM`/`SIGINT` triggers: stop squid, remove the anchor. The credential
gateway and HTTP server exit with the process. Task VMs always run with
`--rm` plus a `container delete --force task-<id>` backstop in case the
context times out.

### Where to look when things break

| Symptom | First place to look |
|---------|---------------------|
| brokerd exits at startup with `ANTHROPIC_API_KEY must be set` | `env \| grep ANTHROPIC` |
| `192.168.66.1 never became bindable` | `container ls -a` — anchor running? `container network inspect macagent-egress` — gateway IP correct? |
| Image build fails on `npm install` | Transient registry timeout; rerun `container build` |
| Gateway returns 401 | Check `ANTHROPIC_API_KEY`; the placeholder `sk-ant-fake` is *expected* to 401 |
| Squid CONNECT 403 to a host you expected to work | `cat /tmp/broker/squid/squid-allow.txt`; not on the list → add to `config/egress.yaml` or pass via `egress_extra` |
| VM can reach a host it shouldn't | `container run --network macagent-egress … nft list ruleset` — confirm `init-firewall.sh` actually ran (only happens with the default entrypoint, not when you override with `/bin/sh`) |
| `container system start` hangs on a Y/n prompt | Re-run with `--enable-kernel-install` |

Per-task audit logs land in `$AUDIT_ROOT/<task_id>.jsonl` and contain the
full `claude --bare` stream-json output. Useful for debugging both agent
behavior and gateway accounting.

---

## Repo layout

```
cmd/brokerd/         # the single binary — composes everything below
internal/
  broker/            # /tasks handler, approval gates, runner orchestration
  creds/             # Grant / Provider interfaces for credential injection
  egress/            # YAML config loader, allowlist compilation
  gateway/           # credential gateway (mint/serve/account/revoke), pricing
  netfw/             # squid conf generator, allowlist compiler, gateway IP derivation
  runner/            # `container run` argv builder
  stage/             # per-task work tree, host-side commit + push
image/               # Dockerfile + entrypoint.sh + init-firewall.sh
config/egress.yaml   # default allowlist
site/                # narrative explainer (for engineers)
docs/superpowers/    # design specs and execution plans (historical context)
```

---

## Building from source

```bash
go test ./...                       # all unit tests, no host/network deps
go build -o brokerd ./cmd/brokerd
```

There are no external Go dependencies beyond `gopkg.in/yaml.v3`.

---

## Limitations and roadmap

- The MVP approval hook auto-approves both egress widening and diff
  pushes. Wire `Broker.Approve` to your review path before treating any
  push as trusted.
- Pricing in `internal/gateway/pricing.go` is point-in-time; update it
  when Anthropic publishes new rates or when you add models. The
  `DefaultPrices()` constructor is the single source.
- One task at a time per brokerd process. Concurrency would require
  per-task gateway leases (already there) plus a queue in front of the
  HTTP handler.
- Apple's `container` is v1.0 but the surface still moves; flag drift
  (e.g. `--cap-add`, `network create`) is the most likely breaking source.
