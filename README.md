<p align="center">
  <img src="site/logo-512.png" alt="drydock" width="160">
</p>

# drydock

<p align="center"><b>Let a coding agent run wild on your repo — without trusting it.</b></p>

<p align="center">
  <img alt="status: alpha" src="https://img.shields.io/badge/status-alpha-orange">
  <img alt="version" src="https://img.shields.io/github/v/tag/sricola/drydock?label=release&color=brightgreen">
  <img alt="platform" src="https://img.shields.io/badge/platform-macOS%2026%2B%20·%20Apple%20silicon-black">
  <img alt="license" src="https://img.shields.io/badge/license-MIT-blue">
</p>


drydock runs **Claude Code** or **OpenAI Codex** full-throttle on your own
repos, on your own Mac — no permission prompts, no babysitting. Each task runs
sealed in a throwaway VM, so the agent **can't touch your API key, can't reach
the open internet, and can't write to anything but a disposable copy**. The only
thing that ever comes back is a `git diff` — and nothing reaches your real code
until you approve it.

- **It never gets your key.** Your real API key stays on the host; the agent
  only ever sees a short-lived, budget-capped token.
- **It can't smuggle anything out.** The internet is deny-by-default — no
  exfiltrating your code, no calling home (you allow the package registries it
  needs, nothing else).
- **Nothing touches your repo until you say so.** You read the diff and approve
  it before it ever reaches `origin`.

Most agent tooling tries to keep the agent *well-behaved* — permission
prompts, output filters, policy. drydock takes the opposite stance: **contain
the blast radius** so a hostile agent (a poisoned repo, a malicious
dependency, a prompt-injection that turns a fetched URL into a shell command)
can't reach your key, your filesystem, your push credentials, or the open
internet — regardless of what it tries.

<p align="center">
  <img src="demo/breach.gif" alt="drydock running four real attacks from its threat model — each is contained live" width="820">
</p>

<p align="center"><sub><b>Don't take the threat model's word for it.</b> Every green above is a real <code>go&nbsp;test</code> red-team case that runs the actual attack and asserts it fails. Reproduce them yourself: <code>make&nbsp;redteam</code> — or watch all seven, including live VM isolation, with <code>make&nbsp;demo&nbsp;VM=1</code>.</sub></p>

> **Status: working alpha (v0.1.10).** The full task lifecycle works
> end-to-end — submit → isolated VM → gated diff → push — and drydock ships
> through a Homebrew tap. It is pre-1.0 and single-maintainer: only `main` is
> supported, behavior and config can change between minor versions, and it
> hasn't been hardened by real-world use. **There has been no third-party
> security audit** — the security model is written down in detail in the
> [threat model](THREAT_MODEL.md), so read that and decide for yourself
> before trusting it. **Hard requirement: macOS 26+ on Apple silicon** — it
> runs on Apple's `container` runtime (itself 1.0), so it won't run anywhere
> else.

Security claims: [`THREAT_MODEL.md`](THREAT_MODEL.md).  
Roadmap: [`docs/ROADMAP.md`](docs/ROADMAP.md) (security-credibility focus).  
Website: https://sricola.github.io/drydock/  

## Install

> **Are you eligible?** drydock runs **locally, on your own Mac** — not a cloud
> VM — so it needs **macOS 26+ on Apple silicon** (it builds on Apple's
> `container` runtime, which ships nowhere else). Check in one line:
>
> ```bash
> [ "$(uname -m)" = arm64 ] && [ "$(sw_vers -productVersion | cut -d. -f1)" -ge 26 ] \
>   && echo "eligible — Apple silicon, macOS 26+" \
>   || echo "not yet — drydock needs macOS 26+ on Apple silicon"
> ```

### Option A — Homebrew (recommended)

```bash
brew tap sricola/drydock
brew trust sricola/drydock     # personal taps require explicit trust
brew install drydock
drydock setup                  # installs container + squid, then runs init
```

`drydock setup` installs the two prerequisites (Apple's `container` runtime and
squid — prompting before each; pass `--yes` to skip), then runs `drydock init`
(container service, network, sandbox image, `~/.drydock` seed). Pulls a
pre-built Apple-silicon binary from the latest tagged release (currently
`v0.1.10`); no Go toolchain required.

Prefer to install the prerequisites yourself? `brew install --cask container`
and `brew install squid`, then `drydock init`.

The PR/MR adapters call `gh`, `glab`, or `tea` — install whichever your repos
use, and run their respective `auth login` before submitting a task.

### Option B — Build from source

```bash
brew install go
git clone https://github.com/sricola/drydock && cd drydock
make install                             # PREFIX=/usr/local by default
make install PREFIX=$HOME/.local         # …or a user-owned prefix
drydock init
```

Either way, `drydock init` walks the remaining prereqs (container
service, `drydock-egress` network, sandbox + anchor images) and reports
per-step status. Idempotent — re-run any time.

## Run

At least one vendor key is required. Both are host-only — they never go to
disk and never enter the VM:

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # required for Claude Code tasks
export OPENAI_API_KEY=sk-...          # required for Codex tasks
drydock start              # foreground; ^C to stop. backgrounds via & or your launchd plist.
```

### Use your Claude subscription (no API key)

If you have a **Claude Pro or Max subscription**, you can run Claude Code
tasks without an `ANTHROPIC_API_KEY`. macOS only; requires the `claude` CLI.

```bash
# 1. Log in to your Claude account (opens a browser)
claude login

# 2. Copy the credential into drydock's store (~/.drydock/claude-oauth.json, mode 0600)
drydock auth claude

# 3. Tell drydock to use subscription auth — either in config.yaml:
#      anthropic_auth: subscription
#    or as an env override:
export DRYDOCK_ANTHROPIC_AUTH=subscription

# 4. Start the broker
drydock start
```

**Important limits.** The USD budget (`task_budget_usd`) does not apply in
subscription mode — there is no spend to meter. To prevent a runaway task
from consuming your subscription's rate limit, set `task_max_requests` in
`~/.drydock/config.yaml`.
`task_timeout` still applies as a wall-clock backstop. Note: the cap stops
*inference* the moment it's hit (the gateway returns HTTP&nbsp;429), but Claude
Code retries a rejected request with backoff before giving up — so a capped
task can spin for a minute or two before it exits with an error.

The credential stored at `~/.drydock/claude-oauth.json` is a full-account
OAuth token — broader than a scoped API key and not per-task revocable. It
never enters the VM, but keep it protected. See [SECURITY.md](SECURITY.md)
for the full blast-radius note. Headless use of a personal subscription may
also brush against Anthropic's terms of service and hit rate limits sooner
than interactive use; the operator assumes that risk.

Quick liveness:

```bash
drydock status
# brokerd     up
# in flight   0 running · 0 awaiting egress · 0 awaiting diff · 0 pushing
# tasks       0 total · 0 in last 24h
# audit dir   ~/.drydock/audit
```

## Submit a task

First time? Walk through [`examples/hello-task.md`](examples/hello-task.md) —
a copy-paste first task that exercises every layer, fits inside the
default budget, and tells you exactly what each step proves.

In one shell, fire the task. **It blocks until the agent runs and you
approve the diff** (typical: a few seconds to a few minutes, plus your
review time):

```bash
drydock submit \
  --repo git@github.com:your-org/your-repo \
  --instruction "Add a one-line comment to README.md explaining the project."
```

A macOS notification fires when the diff is ready. In another shell:

```bash
drydock pending               # awaiting tasks (egress + diff gates both shown)
drydock review <id>           # diff in $PAGER, then prompt y/N — the one-shot path
                              # ─ or, step by step ─
less ~/.drydock/audit/<id>.diff
drydock approve <id>          # … or: drydock deny <id>
```

The submit shell unblocks with the push outcome:

```
task ab12cd34: pushed agent/ab12cd34 (github)
```

### Operator surface (other shell)

```bash
drydock status                # brokerd up?, breakdown (running · egress · diff · pushing)
drydock tasks                 # recent runs: id, age, duration, cost, outcome
drydock logs <id> [-f]        # stream-json audit (use -f to follow)
drydock kill <id>             # cancel the in-flight task (VM down + gate unblocked)
drydock doctor                # smoke-test the sandbox setup (no API spend)
drydock redteam               # run live containment attacks on your own sandbox (no API spend)
```

`drydock redteam` is the proof you can run yourself: it boots throwaway VMs on
your Mac and runs the real attacks behind A1 (the vendor key never enters the
VM), A2 (egress to non-allowlisted hosts is blocked), and A7 (no state survives
between tasks), printing a pass/fail table. Don't trust the threat model —
check it.

<p align="center">
  <img src="demo/img/redteam.png" alt="drydock redteam — A1/A2/A7 containment attacks run live against the sandbox and all pass" width="760">
</p>

### Submit variations

```bash
# Use OpenAI Codex instead of Claude Code for this task
drydock submit --repo … --instruction "…" --agent codex

# Long prompt from a file
drydock submit --repo … --instruction-file ./task.md

# Pipe from stdin
echo "Refactor the egress compiler" | drydock submit --repo … -

# Pick a specific model (overrides default_model in config)
drydock submit --repo … --instruction "…" --model claude-sonnet-4-6

# Skip the approval gate (trusted batch run; see threat model)
drydock submit --repo … --instruction "…" --auto-approve

# Request additional egress (host:port[,port], repeatable; gated)
drydock submit --repo … --instruction "…" \
  --egress-extra internal.example.com:443 \
  --egress-extra files.example.com:443,8443

# Scripting — emit the raw response shape
drydock submit --repo … --instruction "…" --json | jq .branch
```

If you'd rather hit the HTTP API directly:

```bash
SOCK=$TMPDIR/drydock-$(id -u)/drydock.sock
curl --unix-socket "$SOCK" http://_/tasks \
  -H 'content-type: application/json' \
  -d '{ "repo_ref": "git@github.com:o/r", "instruction": "..." }'
```

Notifications opt-out: `DRYDOCK_NO_NOTIFY=1`.

### Platform selection

`repo_ref` must be a git URL (`https://`, `git@`, or `ssh://`); local
paths are rejected because adapters can't operate on filesystem origins.
The PR/MR adapter is chosen by `platform`:

- `"platform": "github"` → `gh pr create --head <branch> --fill` (needs `gh` authed)
- `"platform": "gitlab"` → `glab mr create --fill --yes` (needs `glab` authed)
- `"platform": "gitea"` (alias `forgejo`) → `tea pr create --head <branch>` (needs `tea` authed)
- `"platform": "none"` → push only; no PR/MR
- *omitted* → hostname autodetect (`github.com`, `gitlab.com`, `gitea.com`/`codeberg.org`; else push-only — covers Bitbucket and other self-hosted)

Self-hosted GitLab and Gitea need explicit `"platform"`. Bitbucket has no
widely-adopted CLI to wrap and falls back to push-only; contributions
welcome. The push response includes `"platform"` so the caller can see
which adapter ran. `"auto_approve": true` skips the gate — see the threat
model before using it.

## Where config lives

`drydock init` creates `~/.drydock/` at mode `0700` and seeds two files:

| Path | What |
|---|---|
| `~/.drydock/config.yaml` | Operator settings (network name, gateway IP, per-task budget + timeout, max concurrent tasks, paths, broker listener, behavior flags) |
| `~/.drydock/egress.yaml` | Squid + gateway allowlist (hosts and ports the sandbox may reach) |

Both files are seeded from defaults the first time; `drydock init` never
overwrites them. Env vars still win over file values (e.g.
`BROKER_ADDR=…` in the shell overrides `broker.addr` in the YAML), so
existing scripts keep working. The vendor keys (`ANTHROPIC_API_KEY` /
`OPENAI_API_KEY`) are intentionally **not** in either file — by design,
they never go to disk.

## Egress policy

`~/.drydock/egress.yaml` is the source of truth (seed template lives at
`$HOMEBREW_PREFIX/share/drydock/config/egress.yaml`). The default:

```yaml
default:
  domains:
    - { host: api.anthropic.com,      ports: [443] }   # routed via gateway
    - { host: api.openai.com,         ports: [443] }   # routed via gateway
    # JavaScript
    - { host: registry.npmjs.org,     ports: [443] }   # routed via squid
    # Python
    - { host: pypi.org,               ports: [443] }   # routed via squid
    - { host: files.pythonhosted.org, ports: [443] }   # routed via squid
    # Go module ecosystem
    - { host: proxy.golang.org,       ports: [443] }   # routed via squid
    - { host: sum.golang.org,         ports: [443] }   # routed via squid
per_task_widening:
  requires_approval: true
```

The sandbox image ships **Node 22, Python 3.11, and Go 1.26** so JS,
Python, and Go tasks work without operator customization. Other
toolchains can be added by extending `image/Dockerfile` and rebuilding
via `make image` (or `drydock init`, which detects stale images and
rebuilds).

`api.anthropic.com` and `api.openai.com` are intentionally excluded from
the squid allowlist — they route through the credential gateway, not the
proxy. Per-task widening
via `egress_extra` goes through the same human-driven gate as the diff
push (when `per_task_widening.requires_approval: true`, which is the
default): brokerd blocks the request, writes the requested hosts to
`AUDIT_ROOT/<id>.widen.json`, and shows the task in `drydock pending`
under gate `egress`. Approve with `drydock approve <id>` once you've
reviewed the request. Restart brokerd after editing the default
allowlist.

## Configuration

The canonical location is `~/.drydock/config.yaml` — seeded by `drydock
init` with the defaults below as a commented template. Edit and re-run
`drydock start`. Env vars still override file values for ops/scripting:

| Field (`config.yaml`) | Env override | Default | Meaning |
|---|---|---|---|
| — | `ANTHROPIC_API_KEY` | *(at least one required)* | Real Anthropic key; **host-only**, never goes to disk |
| — | `OPENAI_API_KEY` | *(at least one required)* | Real OpenAI key; **host-only**, never goes to disk |
| `anthropic_auth` | `DRYDOCK_ANTHROPIC_AUTH` | `api_key` | How to authenticate to Anthropic; `api_key` (default) uses `ANTHROPIC_API_KEY`; `subscription` uses the OAuth credential at `~/.drydock/claude-oauth.json` — run `drydock auth claude` first |
| `default_agent` | `DRYDOCK_DEFAULT_AGENT` | `claude` | Agent to use when `--agent` is not passed; allowed values: `claude` \| `codex` |
| `network` | `DRYDOCK_NETWORK` | `drydock-egress` | vmnet network name |
| `gateway_ip` | `DRYDOCK_GW_IP` | `192.168.66.1` | gateway + squid bind here |
| `sandbox_image` | `SANDBOX_IMAGE` | `drydock-sandbox:latest` | per-task agent VM image |
| `anchor_image` | `DRYDOCK_ANCHOR_IMAGE` | `drydock-anchor:latest` | minimal sleep-forever image holding the vmnet gateway IP |
| `task_budget_usd` | `DRYDOCK_TASK_BUDGET_USD` | `2.0` | per-task USD ceiling (`api_key` mode only; not used in `subscription` mode) |
| `task_max_requests` | `DRYDOCK_TASK_MAX_REQUESTS` | *(none)* | hard ceiling on API round-trips per task; the primary runaway control in `subscription` mode |
| `max_concurrent_tasks` | `DRYDOCK_MAX_CONCURRENT_TASKS` | `2` | excess POSTs to `/tasks` get HTTP 503 |
| `task_timeout` | — | `30m` | wall-clock per task |
| `default_model` | `DRYDOCK_DEFAULT_MODEL` | *(empty)* | agent `--model` fallback for tasks that don't pass `--model` (passed to `claude --model` or `codex exec --model`); empty = the agent picks |
| `stage_root` / `audit_root` / `squid_run_dir` | `STAGE_ROOT` / `AUDIT_ROOT` / `SQUID_RUN_DIR` | `~/.drydock/{stage,audit,squid}` | per-task scratch (audit dir is `0700`, audit log + diff are `0600`). Pre-v0.1.4 used `/tmp/broker/`; `drydock tasks` and friends still surface that history while it exists. |
| `broker.socket` | `BROKER_SOCKET` | `$TMPDIR/drydock-$UID/drydock.sock` | Unix socket (per-user parent dir at `0700`, socket at `0600`) |
| `broker.addr` | `BROKER_ADDR` | *(empty)* | set `host:port` to expose over TCP (warns at boot — **no auth**; see [SECURITY.md § TCP exposure](SECURITY.md#tcp-exposure-brokeraddr--broker_addr)) |
| `notifications` | `DRYDOCK_NO_NOTIFY=1` (off) | `true` | macOS notifications on pending approval |
| `log_json` | `DRYDOCK_LOG_JSON=1` | `false` | force JSON logs even on a TTY (default: terse text on TTY, JSON otherwise) |
| `strict_container_version` | `DRYDOCK_STRICT_CONTAINER_VERSION=1` | `false` | fail closed when `container` major drifts from the tested range |
| — | `EGRESS_CONFIG` | `~/.drydock/egress.yaml` | path override for the egress YAML |

Gateway port `8088` and squid port `3128` are hard-coded in
`cmd/brokerd/main.go` and `image/entrypoint.sh`; change both together.

## Troubleshooting

| Symptom | First place to look |
|---|---|
| `192.168.66.1 never became bindable` | `container ls -a` (anchor running?), `container network inspect drydock-egress` (gateway IP?) |
| Image build fails on `npm install` | Transient registry timeout; rerun `container build` |
| Squid CONNECT 403 to an expected host | `cat ~/.drydock/squid/squid-allow.txt`; add via `egress.yaml` or `egress_extra` |
| Stale anchor after a crash | `container rm -f drydock-anchor`; next brokerd start does this for you |
| Gateway 401 | Key wrong or placeholder (`sk-ant-fake` is *expected* to 401) |
| VM reaches a host it shouldn't | Confirm `init-firewall.sh` ran inside the VM — overriding `--entrypoint` skips it |

Per-task stream-json from the agent lands in `$AUDIT_ROOT/<id>.jsonl`; the
diff lands in `$AUDIT_ROOT/<id>.diff`.

## Layout

```
cmd/brokerd/      # broker daemon
cmd/drydock/      # operator CLI (init|start|submit|status|tasks|pending|review|approve|deny|kill|prune|logs)
internal/
  broker/         # /tasks + admin handlers, approval + egress gates, concurrency, cancellation
  creds/          # Grant/Provider interfaces
  egress/         # YAML loader + allowlist compilation + host/port validation
  gateway/        # credential gateway (mint/serve/account/revoke), constant-time token check
  netfw/          # squid conf + allowlist compiler
  remote/         # PR/MR adapters: github (gh), gitlab (glab), gitea (tea), push-only
  runner/         # `container run` argv builder
  sockpath/       # shared per-uid socket path discovery for brokerd + CLI
  stage/          # work tree, host-side commit + push, curated adapter env
image/            # drydock-sandbox (hosts claude-code + codex): Dockerfile + entrypoint.sh + init-firewall.sh
image/anchor/     # drydock-anchor: FROM scratch + static Go sleep binary
tests/integration # //go:build integration — boots brokerd against real container CLI
config/           # egress.yaml
site/             # narrative explainer + launch post
docs/superpowers/ # historical design specs
LICENSE           # MIT
SECURITY.md       # how to report a security bug + documented residuals
THREAT_MODEL.md   # what drydock defends — and doesn't
Makefile          # build, install, test, test-integration, image, image-anchor, network, init, clean
```

## Build, test, CI

```bash
make build              # bin/brokerd, bin/drydock
make test               # go test -race ./...
make image              # both images
make image-sandbox      # per-task agent image
make image-anchor       # minimal anchor image (FROM scratch + static binary)
make test-integration   # boot brokerd as subprocess; macOS only, needs container runtime
```

GitHub Actions runs `go build`, `go test -race`, and `go vet` on every
push/PR. Integration (`make test-integration`) requires `container` and
is macOS-only — runs locally, not in CI. No real Anthropic or OpenAI spend.

## Known gaps

- Pricing in `internal/gateway/pricing.go` covers Anthropic 4.x families (Opus, Sonnet, Haiku) and OpenAI GPT-5/o4 families, each with a high-end default fallback; the budget gate is a safety cap, not a billing source of truth. Bump when either vendor publishes new rates.
- Audit dir (`~/.drydock/audit/`) has no automatic retention. Run `drydock prune --older-than DUR [--keep-last N]` to delete old `<id>.{jsonl,diff,widen.json}` artifacts (dry-run unless `--yes`); brokerd-side auto-retention isn't wired up yet.
- Up to `DRYDOCK_MAX_CONCURRENT_TASKS` tasks in flight per brokerd (default 2); raise on bigger hardware.
- No Slack/web approval adapters yet — only the local CLI + macOS notifications.
- Bitbucket PR/MR opening: push-only fallback (no widely-adopted CLI to wrap). Contribution slot.
- Apple `container` is v1.0; flag drift is the most likely breakage source. `DRYDOCK_STRICT_CONTAINER_VERSION=1` fails closed on drift.
