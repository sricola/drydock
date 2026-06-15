# drydock

Run Claude Code unattended on macOS without giving it your Anthropic key.
Each task runs in a per-task hardware-isolated VM with deny-by-default
egress; the model API is reached via a host-side credential gateway; the
only thing that leaves the sandbox is a `git diff` you approve before it
touches origin.

Design narrative: [`site/index.html`](site/index.html). Security claims:
[`THREAT_MODEL.md`](THREAT_MODEL.md).

## Install

```bash
brew install --cask container && container system start --enable-kernel-install
brew install squid go

git clone git@github.com:sricola/drydock && cd drydock
container network create --subnet 192.168.66.0/24 drydock-egress
container build -t claude-sandbox:latest image/
go build -o ./brokerd ./cmd/brokerd
go build -o ./drydock ./cmd/drydock
```

## Run

```bash
export ANTHROPIC_API_KEY=sk-ant-...
./brokerd &
```

Expected boot lines:

```
container CLI v1.x.x (supported)
network anchor up on drydock-egress
squid listening on 192.168.66.1:3128
gateway listening on 192.168.66.1:8088
brokerd listening on unix:///tmp/drydock.sock
```

## Submit a task

`POST /tasks` against the Unix socket. brokerd stages the repo, runs the
agent in a fresh VM, captures the diff, and **blocks until you approve**.

```bash
curl --unix-socket /tmp/drydock.sock http://_/tasks \
  -H 'content-type: application/json' \
  -d '{
        "repo_ref":     "git@github.com:your-org/your-repo",
        "instruction":  "Add a one-line comment to README.md explaining the project.",
        "auto_approve": false
      }'
```

While the request hangs, brokerd logs:

```
task <id> awaiting approval (N bytes, diff at /tmp/broker/audit/<id>.diff)
  — run: drydock approve <id> | drydock deny <id>
```

In another shell:

```bash
./drydock pending             # list awaiting tasks
less /tmp/broker/audit/<id>.diff
./drydock approve <id>        # … or: ./drydock deny <id>
```

The POST unblocks immediately with the push outcome. `repo_ref` must be a
`github.com` URL (https/ssh/scp form); local paths are rejected because
`gh pr create` can't resolve a filesystem origin. `"auto_approve": true`
skips the gate — see the threat model before using it.

## Egress policy

`config/egress.yaml` is the source of truth. The default:

```yaml
default:
  domains:
    - { host: api.anthropic.com,      ports: [443] }   # routed via gateway
    - { host: registry.npmjs.org,     ports: [443] }   # routed via squid
    - { host: pypi.org,               ports: [443] }   # routed via squid
    - { host: files.pythonhosted.org, ports: [443] }   # routed via squid
per_task_widening:
  requires_approval: true
```

`api.anthropic.com` is intentionally excluded from the squid allowlist —
it routes through the credential gateway, not the proxy. Per-task widening
via `egress_extra` is gated by an MVP auto-approve hook today (next
contribution surface). Restart brokerd after editing.

## Configuration

| Var | Default | Meaning |
|-----|---------|---------|
| `ANTHROPIC_API_KEY` | *(required)* | Real key; host-only |
| `EGRESS_CONFIG` | `config/egress.yaml` | Allowlist YAML |
| `SANDBOX_IMAGE` | `claude-sandbox:latest` | VM image |
| `DRYDOCK_NETWORK` | `drydock-egress` | vmnet network name |
| `DRYDOCK_GW_IP` | `192.168.66.1` | gateway + squid bind here |
| `DRYDOCK_TASK_BUDGET_USD` | `2.0` | per-task USD ceiling |
| `STAGE_ROOT` / `AUDIT_ROOT` / `SQUID_RUN_DIR` | `/tmp/broker/{stage,audit,squid}` | Per-task scratch |
| `BROKER_SOCKET` | `/tmp/drydock.sock` | Unix socket (mode 0600) |
| `BROKER_ADDR` | *(unset)* | Set `host:port` to expose over TCP (warns at boot) |

Gateway port `8088` and squid port `3128` are hard-coded in
`cmd/brokerd/main.go` and `image/entrypoint.sh`; change both together.

## Troubleshooting

| Symptom | First place to look |
|---|---|
| `192.168.66.1 never became bindable` | `container ls -a` (anchor running?), `container network inspect drydock-egress` (gateway IP?) |
| Image build fails on `npm install` | Transient registry timeout; rerun `container build` |
| Squid CONNECT 403 to an expected host | `cat /tmp/broker/squid/squid-allow.txt`; add via `egress.yaml` or `egress_extra` |
| Stale anchor after a crash | `container rm -f drydock-anchor`; next brokerd start does this for you |
| Gateway 401 | Key wrong or placeholder (`sk-ant-fake` is *expected* to 401) |
| VM reaches a host it shouldn't | Confirm `init-firewall.sh` ran inside the VM — overriding `--entrypoint` skips it |

Per-task stream-json from the agent lands in `$AUDIT_ROOT/<id>.jsonl`; the
diff lands in `$AUDIT_ROOT/<id>.diff`.

## Layout

```
cmd/brokerd/      # broker daemon
cmd/drydock/      # operator CLI (pending|approve|deny)
internal/
  broker/         # /tasks + admin handlers, approval gate
  creds/          # Grant/Provider interfaces
  egress/         # YAML loader + allowlist compilation
  gateway/        # credential gateway (mint/serve/account/revoke)
  netfw/          # squid conf + allowlist compiler
  runner/         # `container run` argv builder
  stage/          # work tree, host-side commit + push
image/            # Dockerfile, entrypoint.sh, init-firewall.sh
config/           # egress.yaml
site/             # narrative explainer + launch post
docs/superpowers/ # historical design specs
THREAT_MODEL.md   # what drydock defends — and doesn't
```

## Build, test, CI

```bash
go test -race ./...
go build ./cmd/...
```

GitHub Actions runs `go build`, `go test -race`, and `go vet` on every
push/PR. Integration (real `container` runtime) is macOS-only and lives
outside CI; the boot sequence above is the manual smoke test.

## Known gaps

- Egress widening still auto-approves; should share the diff-push gate.
- Pricing in `internal/gateway/pricing.go` is point-in-time.
- One task at a time per brokerd.
- No Slack/web approval adapters yet — only the local CLI.
- Apple `container` is v1.0; flag drift is the most likely breakage source.
