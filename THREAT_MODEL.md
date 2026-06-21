# Threat model

drydock runs an LLM coding agent on hardware it doesn't trust the agent to
operate. This document is precise about what that buys you and what it does
not. It is the single source of truth for the security claims the rest of
the documentation makes.

## TL;DR

For people evaluating drydock who haven't read the full doc:

- **What we contain**: the agent process inside the per-task VM. Treat
  every tool call, file write, and HTTP request it makes as adversarial.
- **What we protect**: your real API key — Anthropic or OpenAI, whichever the task uses — (never enters the VM),
  your host filesystem and git credentials (never mounted into the VM),
  egress (deny-by-default hostname allowlist enforced on the host).
- **What crosses back to the host**: a single `git diff`. Not commands,
  not files outside the work tree, not push events — just inert data
  the operator reviews before it lands anywhere.
- **What we don't claim**: defense against a host compromise, a kernel
  or `container` 0day (guest-to-host escape defeats drydock entirely),
  or attacks that need a malicious operator.
- **The one knob that bypasses the gate**: `--auto-approve` skips the
  diff-push review. Read the rest of this document before you enable
  it in batch runs.

The rest of this document is what those claims actually mean in code.

## In scope

The agent process running inside the per-task VM is **untrusted**. drydock
must contain its outputs and side-effects so that:

1. The host's Anthropic API key is never exfiltratable to the agent.
2. Agent network egress is limited to a stable, operator-chosen allowlist.
3. The only artifact the agent can deliver to the host is a git diff, and
   that diff cannot be applied to a remote without explicit operator
   approval.
4. Compromise of the agent VM (rogue tool calls, prompt injection,
   adversarial inputs in the staged repo) cannot persist past task end.

The agent is treated as an attacker for every assertion below. "An attacker
can do X" means: if a model + prompt combination could produce X, drydock
must either prevent X or make X visible to the operator.

## Out of scope

These are deliberately out of scope; addressing them requires controls
outside drydock:

- **Host compromise.** A host with malware can read keys, mount sockets,
  edit drydock binaries. drydock assumes the host is trusted.
- **Kernel/runtime 0day in Apple `container`** or in the underlying
  virtualization stack. A guest-to-host escape defeats drydock entirely.
  We pin a known-good `container` version and watch upstream.
- **Hostile staged code that runs on the host outside the VM.** The stage
  package neutralizes hooks and uses a separated `.git` directory; the
  `TestHostCommit_IgnoresPlantedHook` regression test enforces this. But
  a 0day in `git` or `gh` parsing a staged file is out of scope.
- **Supply-chain compromise of dependencies** (Apple `container`, Go std
  lib, `gosu`, `claude-code`, squid, the Debian base image). Trust the
  vendors or pin and audit.
- **Operator key hygiene.** A leaked `ANTHROPIC_API_KEY` defeats the
  gateway; drydock never sees the key after first read, but doesn't
  manage its lifecycle.
- **Multi-tenant isolation.** drydock is single-operator. The Unix socket
  default trusts the local user. No assumption is made about resisting
  other local users on the same host.

## Trust assumptions

| Component | Trust level | Why we trust it |
|---|---|---|
| brokerd binary | trusted | written by operator/project; runs as the operator |
| Host filesystem | trusted | drydock writes audit logs, staging, sockets here |
| Apple `container` runtime + vmnet | trusted | underlies the VM/network boundary |
| Squid + nft | trusted | enforce the network policy |
| `claude-code` or `codex` binary inside the VM | **untrusted** | runs the agent loop and the agent's tool calls |
| The staged repository contents | **untrusted** | may contain hostile files |
| The agent's outputs (tool calls, file writes, the captured diff) | **untrusted** | filtered, gated, and made visible before action |

## Attacks drydock defends against

Each claim below is backed by a test that runs the attack and asserts it
fails. **Reproduce:** `make redteam` executes the host-side attacks (A3–A6) and
watches them fail; the VM-backed claims (A1, A2, A7) run via
`make redteam-vm` on macOS / Apple silicon. With an installed build,
`drydock redteam` runs the VM-backed attacks (A1, A2, A7) live against your own
sandbox — no source checkout, no API spend. See
[`docs/ROADMAP.md`](docs/ROADMAP.md) for the full coverage map.

### A1. Agent reads a vendor API key from environment, config file, or memory

The VM is started with no copy of the real key — for either vendor. The
credential gateway holds the key on the host (Anthropic or OpenAI, whichever
the task uses) and issues a short-lived bearer token bound to a USD budget.
The VM sees only the bearer. Even if the agent exfiltrates the bearer, it
expires with the task and is capped by `DRYDOCK_TASK_BUDGET_USD`.

The gateway now fronts two upstreams: `api.anthropic.com` (Claude Code tasks)
and `api.openai.com` (Codex tasks). The real key for whichever vendor stays
host-only in both cases. No new trust assumptions are introduced by the second
vendor: the same mint/validate/revoke cycle applies; the VM still only ever
sees a budget-capped bearer token regardless of which upstream backs it.

**Implementation:** `internal/gateway/provider.go` injects only
`ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` (Claude) or
`OPENAI_BASE_URL`/`OPENAI_API_KEY` (Codex). The real key never crosses
the `container run` boundary. **Verified by**
`TestRedteam_A1_RealKeyNeverInVM` (inspects the VM env built from a real
gateway grant with a sentinel key).

### A2. Agent makes outbound calls to hostile or unintended hosts

In-VM `nft` policy (set by root before `gosu agent`) drops all output
except loopback, established/related, and `${gateway-IP}:{8088,3128}`.
DNS itself is dropped — the VM has no way to resolve a name except by
asking squid, which only resolves hosts on the operator's allowlist.

**Implementation:** `image/init-firewall.sh` plus `internal/netfw`'s
hostname allowlist compiler. The agent user has no `CAP_NET_ADMIN`, so
it cannot rewrite the rules. **Verified by**
`TestRedteam_A2_EgressToHostileHostBlocked` (HTTPS, raw DNS, and direct-IP
escapes all blocked).

### A3. Agent plants a git hook in the work tree so the host-side commit runs hostile code on the host

The staging package keeps `.git` outside the mounted path. Host-side git
operations use `--git-dir=<host-only>` and run with
`core.hooksPath=/dev/null` and `core.fsmonitor=false`, including under
`gh pr create`. A planted `.git/hooks/pre-commit` in the work tree never
runs on the host.

**Implementation:** `internal/stage/stage.go`. Enforced by
`TestHostCommit_IgnoresPlantedHook`.

### A4. Agent leaks the staged prompt or compiled allowlist into the PR

The `.task/` control directory is excluded from the captured diff by the
`stageAll` step. Even an agent that writes inside `.task/` cannot push
its contents.

**Implementation:** `internal/stage/stage.go::CaptureDiff`. Enforced by
`TestCaptureDiff_ExcludesTaskDir`.

### A5. Agent crafts a diff that looks benign but contains a malicious
change

The diff-push gate (default-deny, `auto_approve` opt-in only) requires an
operator to inspect the diff before it reaches origin. The persisted
`AUDIT_ROOT/<task>.diff` plus the full `stream-json` trace in
`<task>.jsonl` give the reviewer everything needed. Once approved, the
host commits with hooks disabled and pushes; the agent cannot intercept.

**Implementation:** `internal/broker/broker.go::gatePush` plus
`cmd/drydock` (the operator CLI). `Task.AutoApprove` must be true on the
incoming JSON to skip the gate. **Verified by**
`TestRedteam_A5_GateBlocksUnapprovedPush`.

### A6. Agent attempts to widen its own egress for the current task

`per_task_widening.requires_approval: true` (the default in
`~/.drydock/egress.yaml`) routes `egress_extra` through the same human-
driven gate as A5. brokerd writes the requested hosts to
`AUDIT_ROOT/<id>.widen.json`, the task appears in `drydock pending`
under gate `egress` with the host list, and `drydock approve <id>` /
`drydock deny <id>` resolve it. Compilation of the per-task allowlist
happens **after** approval; a denied or cancelled task never reaches
squid with the extras. Setting `requires_approval: false` in the YAML
opts you out of the gate explicitly — the operator-trust-everything
mode for batch runs.

**Implementation:** `internal/broker/broker.go::gateEgressWiden`. **Verified
by** `TestRedteam_A6_EgressWidenDenied`.

### A7. Task state persists between tasks

Every task runs in a fresh VM (`--rm`). The host-side staging directory
is wiped (`defer st.Cleanup()`) on every code path. The anchor container
is unrelated to task state — it exists only to keep the vmnet gateway IP
up.

**Verified by** `TestRedteam_A7_NoStatePersistsBetweenTasks` (a marker
written by one task is absent in the next) plus the host-side
`TestCleanup_*` stage tests.

## Attacks drydock does NOT defend against

These are real and the operator must be aware:

### N1. Operator approves a malicious diff

The gate exists, but a hurried operator can still approve a diff that
contains a subtle backdoor. **Reviewers should diff like they would for
any external contributor.** drydock makes review possible; it does not
make it automatic.

### N2. Prompt injection from staged repo files

A `.cursorrules`, `AGENTS.md`, or hostile docstring inside the staged
repo can influence the agent's actions for that task. drydock does
**not** filter staged content. Treat task results from repos with
untrusted contributors with extra suspicion.

### N3. Side-channel data exfiltration via the diff

A clever agent can encode information (e.g. an exfiltrated bearer
token) into the diff itself — file names, whitespace patterns,
comment text. drydock makes this visible to the operator at review
time, but does not detect it programmatically.

### N4. Cost exhaustion and runaway tasks

**`api_key` mode (default).** The per-task USD ceiling (`task_budget_usd` /
`DRYDOCK_TASK_BUDGET_USD`) caps spend but does not cap usefulness. An agent
that burns $2 on no-op API calls hits the cap and produces no diff. Operators
should monitor `costUSD` in `<task>.jsonl` and treat repeated zero-diff runs
as a flag.

`task_budget_usd` is a **soft cap**: the gateway meters a request's cost only
once its response completes, so a single in-flight request can overshoot by
its own cost before the next one is refused (`402`). Within a task the agent
calls the API sequentially, so the overshoot is bounded by one call — but a
single deliberately oversized call can exceed the budget in one shot. Set the
budget with that headroom in mind.

**`subscription` mode (`anthropic_auth: subscription`).** When
`anthropic_auth: subscription` is set, drydock routes through the operator's
personal Claude Pro/Max subscription. The credential gateway holds the OAuth
access and refresh tokens host-side and issues per-task bearers as usual (A1
still holds), but **the USD budget cap does not apply** — there is no spend to
meter. The runaway controls are:

- `task_max_requests` — hard ceiling on the number of API round-trips the
  gateway will allow for a single task before returning `429`. Set this
  explicitly; there is no equivalent to the API-key budget sentinel.
- `task_timeout` — wall-clock ceiling (default `30m`), unchanged.

Without `task_max_requests` a subscription task can burn through a large
fraction of the subscription's rate limit before `task_timeout` fires.
Operators running batch jobs should set both.

**`subscription` mode (`openai_auth: subscription`).** When
`openai_auth: subscription` is set, drydock routes Codex tasks through the
operator's personal ChatGPT subscription via the Codex backend
(`chatgpt.com/backend-api/codex`). The credential gateway holds the OAuth
access token, refresh token, and account id host-side and issues per-task
bearers as usual (A1 still holds), but **the USD budget cap does not apply** —
there is no spend to meter. The runaway controls are identical:

- `task_max_requests` — hard ceiling on the number of API round-trips the
  gateway will allow for a single task before returning `429`. Set this
  explicitly when using subscription mode.
- `task_timeout` — wall-clock ceiling (default `30m`), unchanged.

Without `task_max_requests` a Codex subscription task can burn through a large
fraction of the subscription's rate limit before `task_timeout` fires.
Operators running batch jobs should set both.

### N5. Compromise of the host's git remote credentials

`gh` on the host uses the operator's GitHub credentials to push and
open PRs. drydock does not isolate these. An attacker who can run
`drydock approve` can push to any repo the operator's `gh` token can
reach.

### N6. Local attacker on the same host

The default Unix socket at `$TMPDIR/drydock-$UID/drydock.sock` is mode `0600`, owned by
the operator. Another user on the same host cannot reach it. But a
process running as the operator can. drydock assumes the host's user
boundary is the relevant trust boundary.

### N7. Apple `container` runtime escapes

A guest-to-host escape in the VM stack defeats every claim above. We
pin a tested version and recommend upgrading promptly when upstream
publishes security advisories.

## Residual risk summary

- **You must review every diff before approving.** This is the only place
  human judgment is load-bearing.
- **You must keep your host clean.** No drydock defense survives host
  compromise.
- **You must pin and update `container`, `claude-code`, and `codex`.** All
  move fast; drydock's claims hold only against the versions it was tested
  against.

If you find a residual that isn't covered here, open an issue. The model
moves; this document moves with it.
