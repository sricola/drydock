# drydock

Run Claude Code as an autonomous coding agent on macOS, inside a per-task
hardware-isolated VM with deny-by-default egress and a host-side credential
gateway. The VM never sees your Anthropic key, can only reach an allowlisted
set of hosts, and the only thing that leaves it is a captured `git diff`
that you approve before it touches origin.

The narrative explainer lives at [`site/index.html`](site/index.html); the
precise security claims live in [`THREAT_MODEL.md`](THREAT_MODEL.md). This
README is the operator's manual.

**Status:** working prototype. Tested against `container` v1.0.x on Apple
silicon. Single-task, single-operator. Production-readiness is honest work
ahead; the [threat model](THREAT_MODEL.md) is precise about what claims
hold today.

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
   you ──POST──▶  │  brokerd  unix sock      │
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
                                          │  vmnet (drydock-egress, 192.168.66.0/24)
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
container network create --subnet 192.168.66.0/24 drydock-egress

# Build the sandbox image
container build -t claude-sandbox:latest image/
```

### Sanity-check the network

```bash
container network inspect drydock-egress | jq .[].status
# ipv4Gateway must be 192.168.66.1 (matches DRYDOCK_GW_IP default)
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
go build -o /tmp/drydock  ./cmd/drydock
ANTHROPIC_API_KEY=sk-ant-fake \
DRYDOCK_NETWORK=drydock-egress \
DRYDOCK_GW_IP=192.168.66.1 \
SANDBOX_IMAGE=claude-sandbox:latest \
  /tmp/brokerd &

# Expected log lines:
#   container CLI v1.x.x (supported)
#   network anchor up on drydock-egress
#   squid listening on 192.168.66.1:3128
#   gateway listening on 192.168.66.1:8088
#   brokerd listening on unix:///tmp/drydock.sock (gateway …, squid …)

# 2. From a throwaway VM on drydock-egress, install the firewall pin and probe
container run --rm --network drydock-egress --cap-add CAP_NET_ADMIN \
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
container ls -a | grep drydock       # drydock-anchor should be gone
```

If those three lines match, every plumbing surface — anchor, gateway,
squid, in-VM `nft` — is working.

---

## Submitting a task

`POST /tasks` against the brokerd socket. The broker stages the repo, mints
a budgeted credential, runs `claude --bare` inside a fresh VM, captures the
diff, **blocks on operator approval**, and pushes a branch on approve.

```bash
curl -sS --unix-socket /tmp/drydock.sock http://_/tasks \
  -H 'content-type: application/json' \
  -d '{
        "repo_ref":    "git@github.com:your-org/your-repo",
        "instruction": "Add a one-line comment to README.md explaining the project.",
        "egress_extra": [],
        "sensitive":   false,
        "auto_approve": false
      }'
```

While that request hangs, brokerd logs:

```
task <id> awaiting approval (N bytes, diff at /tmp/broker/audit/<id>.diff)
  — run: drydock approve <id> | drydock deny <id>
```

In another shell, review the diff and decide:

```bash
drydock pending           # list awaiting tasks
less /tmp/broker/audit/<id>.diff
drydock approve <id>      # … or  drydock deny <id>
```

The original `POST /tasks` request unblocks immediately and returns the
push outcome. Setting `"auto_approve": true` on the task skips the gate —
use it for trusted batch runs and know what you're opting out of. The
[threat model](THREAT_MODEL.md) is explicit about what the gate
protects against.

`repo_ref` must be a `github.com` URL (`https://github.com/…`,
`git@github.com:…`, or `ssh://git@github.com/…`). Local paths are
rejected up front because the staging clone would inherit a filesystem
`origin` and the host-side `gh pr create` can't open a PR against it.

Response shape:

```json
{ "task_id": "ab12cd34ef56", "branch": "agent/ab12cd34ef56", "pushed": true }
```

…or, if there was no diff:

```json
{ "task_id": "ab12cd34ef56", "diff": "", "pushed": false }
```

Each task spends real Anthropic tokens against `ANTHROPIC_API_KEY`, capped
at `DRYDOCK_TASK_BUDGET_USD` (default $2.00) and `30m` wall-clock by
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
| `DRYDOCK_NETWORK` | `drydock-egress` | vmnet network name (must already exist) |
| `DRYDOCK_GW_IP` | `192.168.66.1` | The `.1` of the network's subnet; gateway + squid bind here |
| `DRYDOCK_TASK_BUDGET_USD` | `2.0` | Hard ceiling per task; gateway rejects once exhausted |
| `STAGE_ROOT` | `/tmp/broker/stage` | Per-task work tree (wiped on completion) |
| `AUDIT_ROOT` | `/tmp/broker/audit` | Per-task `<id>.jsonl` log of the VM's stream-json output |
| `SQUID_RUN_DIR` | `/tmp/broker/squid` | squid pid/conf/cache.log + compiled allowlist |
| `BROKER_SOCKET` | `/tmp/drydock.sock` | Unix socket brokerd listens on by default (mode 0600) |
| `BROKER_ADDR` | *(unset)* | Set to `host:port` to expose brokerd over TCP instead. Loud startup warning when used; the threat model treats the TCP listener as an operator-trust boundary |

The squid and gateway port numbers (`3128`, `8088`) are hard-coded in
`cmd/brokerd/main.go` — change them there and in the entrypoint's
`init-firewall.sh` invocation together.

---

## Operational notes

### Boot order matters

The vmnet gateway IP only exists while a container is attached to the
network. brokerd therefore:

1. Removes any stale `drydock-anchor` container.
2. Starts a fresh `drydock-anchor` (the sandbox image running
   `sleep infinity`) on `drydock-egress`. The vmnet plugin brings up
   `192.168.66.1` on the host.
3. Polls `net.Listen` on `:3128` and `:8088` until they're bindable
   (interface up).
4. Launches squid bound to `192.168.66.1:3128`.
5. Launches the credential gateway on `192.168.66.1:8088`.
6. Starts the HTTP API on `/tmp/drydock.sock` (or `BROKER_ADDR`).

If brokerd exits non-cleanly (`SIGKILL`, panic), the anchor container can
survive. The next start removes it; you can also clean up manually with
`container rm -f drydock-anchor`.

### Cleanup on SIGTERM

`SIGTERM`/`SIGINT` triggers: stop squid, remove the anchor. The credential
gateway and HTTP server exit with the process. Task VMs always run with
`--rm` plus a `container delete --force task-<id>` backstop in case the
context times out.

### Where to look when things break

| Symptom | First place to look |
|---------|---------------------|
| brokerd exits at startup with `ANTHROPIC_API_KEY must be set` | `env \| grep ANTHROPIC` |
| `192.168.66.1 never became bindable` | `container ls -a` — anchor running? `container network inspect drydock-egress` — gateway IP correct? |
| Image build fails on `npm install` | Transient registry timeout; rerun `container build` |
| Gateway returns 401 | Check `ANTHROPIC_API_KEY`; the placeholder `sk-ant-fake` is *expected* to 401 |
| Squid CONNECT 403 to a host you expected to work | `cat /tmp/broker/squid/squid-allow.txt`; not on the list → add to `config/egress.yaml` or pass via `egress_extra` |
| VM can reach a host it shouldn't | `container run --network drydock-egress … nft list ruleset` — confirm `init-firewall.sh` actually ran (only happens with the default entrypoint, not when you override with `/bin/sh`) |
| `container system start` hangs on a Y/n prompt | Re-run with `--enable-kernel-install` |

Per-task audit logs land in `$AUDIT_ROOT/<task_id>.jsonl` and contain the
full `claude --bare` stream-json output. Useful for debugging both agent
behavior and gateway accounting.

---

## Repo layout

```
cmd/brokerd/         # the broker daemon — composes everything below
cmd/drydock/         # operator CLI (drydock pending|approve|deny)
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
THREAT_MODEL.md      # precise security claims (what drydock defends; what it doesn't)
docs/superpowers/    # design specs and execution plans (historical context)
```

---

## Building from source

```bash
go test ./...                       # all unit tests, no host/network deps
go build -o brokerd ./cmd/brokerd
go build -o drydock ./cmd/drydock
```

There are no external Go dependencies beyond `gopkg.in/yaml.v3`.
CI runs `go build`, `go test -race`, and `go vet` on every push/PR
(see [`.github/workflows/test.yml`](.github/workflows/test.yml));
integration testing requires Apple's `container` runtime and lives
outside CI.

---

## Limitations and roadmap

- Egress widening (`egress_extra`) still goes through the MVP auto-approve
  hook (it just logs and returns true). Diff push is now gated by a real
  human; egress widening should join that flow next.
- Pricing in `internal/gateway/pricing.go` is point-in-time; update it
  when Anthropic publishes new rates or when you add models. The
  `DefaultPrices()` constructor is the single source.
- One task at a time per brokerd process. Concurrency would require a
  task queue in front of the HTTP handler and a way to label the
  approval prompt with task context (per-task gateway leases already
  exist).
- Apple's `container` is v1.0 but the surface still moves; flag drift
  (e.g. `--cap-add`, `network create`, mount option parsing) is the
  most likely breaking source. drydock fails closed when `container`
  isn't installed and warns when the major version drifts from the
  tested range.
- No Slack/web approval adapters yet — only the local `drydock` CLI
  against the Unix socket. Adapters are the obvious next contribution
  surface.
