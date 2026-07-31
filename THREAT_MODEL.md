# Threat model

drydock runs an LLM coding agent on hardware it doesn't trust the agent to
operate. This document is precise about what that buys you and what it does
not. It is the single source of truth for the security claims the rest of
the documentation makes.

## TL;DR

For people evaluating drydock who haven't read the full doc:

- **What we contain**: the agent process inside the per-task VM. Treat
  every tool call, file write, and HTTP request it makes as adversarial.
- **What we protect**: your real API key (Anthropic or OpenAI, whichever the task uses) never enters the VM,
  your host filesystem and git credentials (never mounted into the VM),
  egress (deny-by-default hostname allowlist enforced on the host).
- **What crosses back to the host**: a single `git diff`. Not commands,
  not files outside the work tree, not push events, just inert data
  the operator reviews before it lands anywhere. With the opt-in CI
  watch on (`ci.watch`, default off) one more thing crosses back, from
  GitHub rather than from the VM: a pushed PR's check names and check
  **conclusions**. Status, never log text — see N2 and N5.
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
  lib, `setpriv`/util-linux, `claude-code`, squid, the Debian base image). Trust the
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
| the agent CLI binary inside the VM (`claude-code`, `codex`, `gemini`, or `opencode`) | **untrusted** | runs the agent loop and the agent's tool calls |
| The staged repository contents | **untrusted** | may contain hostile files |
| The agent's outputs (tool calls, file writes, the captured diff) | **untrusted** | filtered, gated, and made visible before action |

## Attacks drydock defends against

Each claim below is backed by a test that runs the attack and asserts it
fails. **Reproduce:** `make redteam` executes the host-side attacks (A3–A6) and
watches them fail; the VM-backed claims (A1, A2, A7, A8, A9) run via
`make redteam-vm` on macOS / Apple silicon. With an installed build,
`drydock redteam` runs the VM-backed attacks (A1, A2, A7) live against your
own sandbox, no source checkout, no API spend. See
[`docs/ROADMAP.md`](docs/ROADMAP.md) for the full coverage map.

### A1. Agent reads a vendor API key from environment, config file, or memory

The VM is started with no copy of the real key, for either vendor. The
credential gateway holds the key on the host (Anthropic or OpenAI, whichever
the task uses) and issues a short-lived bearer token bound to a USD budget.
The VM sees only the bearer. Even if the agent exfiltrates the bearer, it
expires with the task and is capped by `DRYDOCK_TASK_BUDGET_USD`.

The real key can reach the gateway from two sources: the shell env
(`ANTHROPIC_API_KEY` / `OPENAI_API_KEY`) **or** the stored file
`~/.drydock/api-keys.env` (mode `0600`), which the broker reads at startup.
Neither source is ever passed into the VM, the A1 control is unchanged
regardless of which source the broker loaded the key from.

The gateway fronts several upstreams: `api.anthropic.com` (Claude Code),
`api.openai.com` (Codex), `generativelanguage.googleapis.com` (Gemini, native,
experimental), and any operator-configured OpenAI-compatible endpoint
(`opencode`). The real key for whichever vendor stays host-only in every case.
No new trust assumptions are introduced by additional vendors: the same
mint/validate/revoke cycle applies, and the VM still only ever sees a
budget-scoped bearer token (overshoot bounded by task_max_inflight, default 1)
(in `x-goog-api-key` for Gemini, `Authorization: Bearer` for the rest)
regardless of which upstream backs it.

**Implementation:** `internal/gateway/provider.go` injects only
`ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` (Claude) or
`OPENAI_BASE_URL`/`OPENAI_API_KEY` (Codex). The real key never crosses
the `container run` boundary. **Verified by**
`TestRedteam_A1_RealKeyNeverInVM` (inspects the VM env built from a real
gateway grant with a sentinel key).

### A2. Agent makes outbound calls to hostile or unintended hosts

In-VM `nft` policy (set by root before `drop-agent.sh` drops to the agent user via `setpriv`) drops all output
except loopback, established/related, and `${gateway-IP}:{8088,3128}`.
DNS itself is dropped, the VM has no way to resolve a name except by
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
`<task>.jsonl` give the reviewer everything needed. When the repo has a
`verify.repos` entry, the reviewer additionally gets broker-observed
verification results in the trust brief (`drydock inspect <id>`):
host-approved commands run against a sealed export of the staged tree,
each in a fresh VM with no credentials and a deny-all nft pin (loopback
only — strictly tighter than the agent's allowlist) that root installs
before the same setpriv privilege drop A2 relies on, so repo code cannot
flush it. Verdicts are the exit codes the broker observes; nothing the
verify VM prints can alter a status, and at push time the staged tree is
re-hashed and must equal the verified tree or the push fails closed.
`required: true` blocks the push on any status but `passed`. The verifier
VM's containment is claimed and tested separately as A9. Once approved,
the host commits with hooks disabled and pushes; the agent cannot
intercept.

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
opts you out of the gate explicitly: the operator-trust-everything
mode for batch runs.

A **bounded CI retry** (`ci.max_attempts`, N4) preserves its parent's
`egress_extra` — it redoes the same work and needs the same reach — and
**re-poses this gate from scratch**. The gate is per task and keys only on that
task's own `egress_extra` plus `requires_approval`; nothing carries a prior
approval forward, so one human "yes" can never authorize egress for a chain of
unattended runs.

**Implementation:** `internal/broker/gates.go::gateEgressWiden`. **Verified
by** `TestRedteam_A6_EgressWidenDenied`,
`TestRedteam_A6_AutoApproveCannotBypassWideningGate`, and
`TestRedteam_A6Retry_ChildReposesTheEgressWideningGate`.

### A7. Task state persists between tasks

Every task runs in a fresh VM (`--rm`). The host-side staging directory
is wiped (`defer st.Cleanup()`) on every code path. The anchor container
is unrelated to task state, it exists only to keep the vmnet gateway IP
up.

**Verified by** `TestRedteam_A7_NoStatePersistsBetweenTasks` (a marker
written by one task is absent in the next) plus the host-side
`TestCleanup_*` stage tests.

**A bounded CI retry does not weaken this and takes no exception.** A retry
(`ci.max_attempts`, off by default) is a new task on a fresh VM that re-clones
the repository's **default branch HEAD**; the previous attempt's work crosses
over only as capped, sanitized text inside the new instruction (N2), never as a
base branch, a mounted tree, or a clone ref. Basing a retry on the previous
`agent/<id>` branch *would* have needed a new sub-clause here — it is an
agent-written tree entering a fresh VM, exactly what the cache carve-out below
turns on excluding — which is why it is not done.
**Verified by** `TestRedteam_A7Retry_ChildInheritsTextOnlyNeverTheParentsTreeOrBranch`,
a host-side test that drives the real dispatcher and asserts the retry's stage
root is its own and its clone ref is the parent's repo ref unchanged.

**The one persistence exception — the dependency cache — is not agent
state.** Repos that opt in (`profiles.repos.<key>.cache: true`) get a
persistent dependency cache mounted at `/deps`. It does not weaken the
claim above, because nothing the agent writes can enter it: entries are
produced only by the trusted setup phase (host-configured commands running
with no credentials, before the agent VM exists), the mount into the
**agent** VM always carries `readonly` (`runner.BuildRunArgs`), and the
store is host-managed and content-addressed — each entry is keyed by repo
identity + lockfile digests + setup-command digest + sandbox image +
architecture (`internal/depcache`), so entries are isolated per key and
two repos never share one, even with identical lockfiles (and a repo
without a lockfile gets no cache at all). Entries hold what the setup
phase's package managers write there (npm/Go/pip/cargo caches), and no
credential can enter them — a property that holds by composition, not
because the paths are mechanically payload-only: the setup VM that
populates the cache carries no registry credentials (its env is
proxy/gateway vars only), so none are ever written. `CARGO_HOME` in
particular is cargo's config-and-credential home, not a pure payload
cache, but nothing in the bearer-free setup env can write a credential
there — and the agent VM, the only credential-carrying VM, mounts
`/deps` read-only. Operational caveat, not a
security one: with `/deps` read-only in the agent VM, a package manager
that tries to write its cache mid-agent-run (e.g. installing a package
the cache does not carry) hits a read-only filesystem error — the cache
is a speedup, never a correctness dependency; a miss (or caching off)
simply re-fetches through squid.

**Verified by** `TestRedteam_A7Cache_AgentCannotWriteReadOnlyCache` (the
production agent argv carries the readonly `/deps` mount; in-VM write
attempts as root and as the dropped agent user are all rejected and the
host cache dir stays empty) and `TestRedteam_A7Cache_DistinctKeysIsolate`
(distinct keys — including two repos with identical lockfiles — resolve
to distinct entry dirs; a marker under one is invisible under the other).

### A8. Agent floods /work to exhaust host disk

A task's `/work` is bounded twice (F-04). On macOS the stage root is a
size-capped APFS sparse image (`stage_quota_gb`, default 8 GiB, 0 disables
it): a hostile in-VM agent writing through `/work` hits the image's
filesystem wall (ENOSPC) no matter how fast it writes. A polling guard (4
GiB total bytes, 200k files, 2 GiB host free floor, checked every 2s) sits
on top as the early-cancel layer: it stops a task cleanly before it reaches
the quota and is the only layer on non-macOS builds, where the image is a
no-op. File-count exhaustion within an attached image is still bounded
only by the soft polling guard, an image full of many small files can hit
the 200k-file cap before it hits the byte quota.

The same polling guard also spans the setting_up stage: setup VMs run
untrusted repo code (npm postinstall, pip setup.py) with `/work` mounted
rw and — when caching is active — the rw `/deps` mount, so a setup
command that crosses the stage caps or drives host free space below the
floor (through either mount) is cancelled mid-flight and the task fails
closed as `setup_failed`. A hostile install script cannot fill the host
disk during setup, and the dependency cache can never fill the disk (A8
extended to the persistent store).

**Implementation:** `internal/stage/quota_darwin.go` (`AttachQuota`,
`QuotaImagePath`) plus `internal/broker/stagesize.go` (the polling guard).
**Verified by** `TestRedteam_A8_WorkQuotaHardBound` (an in-VM `dd` flood
hits "No space left on device" and the backing image stays within slack of
the quota) plus `TestQuota_HardBoundENOSPC`.

### A9. Verifier VM exfiltrates credentials or reaches the network

The post-run verifier (A5) runs repo-controlled commands, so its VM is
held to a posture strictly tighter than the agent's: it receives no
gateway bearer, no proxy env, and no credentials of any kind
(`runner.VerifySpec` carries no env beyond `HOME`); root installs a
deny-all nft pin (loopback only — no gateway, no squid) before the same
setpriv privilege drop A2 relies on, so repo code runs unprivileged and
cannot flush it. Verdicts are the exit codes the broker observes; the
`.verify.log` is display-only. The verify VM is `--rm` (A7 holds) and its
`/work` is the sealed tree export inside the per-task quota image (A8
holds).

**Implementation:** `internal/runner/runner.go::BuildVerifyArgs` (the
deny-all pin + privilege drop) plus `internal/broker/verify.go` (no
credential env, broker-observed exit codes). **Verified by**
`TestRedteam_V1_VerifierVMHasNoNetworkAndNoCredentials` (as the dropped
agent user: no credential material in the environment, HTTPS/DNS/gateway
all blocked, `nft flush` denied, `$HOME` writable).

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
untrusted contributors with extra suspicion. Issue-sourced instructions
(`drydock submit --issue`) are attacker-influenced the same way — anyone
who can file an issue is writing prompt text — and the human plan+diff
gates are the boundary there too.

**CI check names and conclusions are a third documented source of the same
kind** (`ci.watch`, off by default). When the watch is on, the broker reads a
pushed PR's checks off GitHub; each check's **name** crosses into the broker
alongside the broker-observed conclusion. A repository's own workflow file
chooses those names, so a check name is attacker-influenced text in exactly the
way issue text is. Three properties bound it: it is sanitized at ingestion
(C0/C1 control characters, DEL, and Unicode bidi/format overrides stripped;
length-capped, and the retained list itself is capped), it **decides nothing**
— every control-flow decision derives only from the broker-observed
conclusion, the same class of fact as the verifier's exit codes — and in B1
nothing carried it onward: what is persisted and displayed is broker-authored
state, a PR number, and a broker-authored reason string. This is a new
*source*, not a new trust boundary, and no new claim is made: the human
plan+diff gates are still the boundary.

**The broker fetches no CI log text at all — in B1 or B2.** The only host calls
on the watch path are `gh pr checks --json bucket,name,state` and
`gh pr view --json statusCheckRollup`; no log, URL, output, description, or
annotation field is ever requested, and the summary types carry no field that
could hold log content. That is worth stating explicitly because an earlier
draft of this document anticipated B2 shipping fenced CI **log** text; it does
not, and the field list above is the enforcement.

**What B2 does carry into an agent's prompt** (`ci.max_attempts > 0`, off by
default) is two things, and both are attacker-influenceable:

- the **failed check names and conclusions** described above — a repository's
  own workflow file chooses the names;
- the **previous attempt's own diff**, which is agent-written text.

**Exactly one attempt's worth of each, at every depth of a chain**, and the
singular matters: a retry's instruction is the operator's ORIGINAL task plus one
CI-evidence section plus one prior-diff section — never the parent's assembled
instruction plus another pair. The original is carried forward on its own field
(`Task.RootInstruction`, broker-owned, zeroed by `POST /queue`) precisely so it
can be. Two things rest on that. The untrusted text an agent sees is bounded by
the section caps *regardless of depth* rather than by the caps times the chain
length — hop 10 is the same size as hop 1, which is also what makes the
documented `ci.max_attempts` ceiling of 10 reachable at all. And the fence claim
below is true at every depth: an instruction that inherited its parent's would
carry earlier hops' genuine `BEGIN`/`END` pairs, which are in neither the token's
preimage nor its containment check, so it would announce "these tokens and no
others" over a document containing several other perfectly genuine-looking ones.
Pinned by `TestBuildRetryTask_InstructionSizeDoesNotGrowWithChainDepth`,
`TestBuildRetryTask_FenceClaimHoldsAtEveryDepth`, and
`TestBuildRetryTask_CarriesOnlyTheMostRecentPriorDiff`.

Both are dropped into the retry task's instruction, control-character
sanitized (C0, C1, DEL, and Unicode format/bidi characters stripped; invalid
UTF-8 dropped rather than replaced — note that invisibles *outside* category
Cf, such as the Hangul fillers and variation selectors, are deliberately NOT
stripped, because no line short of banning non-ASCII can be drawn there and
combining marks are ordinary content in most scripts), byte-capped with an
explicit truncation marker, and fenced under `### BEGIN/END UNTRUSTED <KIND> <token>` delimiters
whose token is derived by SHA-256 from **every** body in the instruction drydock
did not author itself — both fenced sections *and* the carried original
instruction — and proven to occur in none of them, so neither input can
terminate its own section, or the other one's, with a delimiter a reader would
believe. (Deriving each token from its own body alone was not enough: both
tokens are deterministic and both are announced in the preamble, so the
agent-written diff could compute and plant the *genuine* CI-OUTPUT token, and
vice versa.) The salt search that guarantees absence is bounded; exhausting it
would refuse the retry rather than ship an instruction whose fence claim was
false. The assembled instruction is
hashed into `InstructionSHA256` like every other instruction: provenance, not
filtration.

**None of that is a security boundary, and it is not claimed as one.** An LLM
reading fenced text may still be steered by it. The claims that *are* made, and
are mechanically tested, are narrower: the fence is not trivially defeated
(`TestCIRetryAdversarial_UntrustedTextCannotTerminateItsOwnSection` drives fence
delimiters, forged section-end headings, `SYSTEM:`- and tool-call-shaped
payloads, nested fences, bidi overrides, zero-width characters, and
astral-plane runes at a cap boundary through both channels;
`TestBuildRetryTask_ForgedCrossSectionFenceIsImpossible` adds the cross-section
case, replanting each announced token in the *other* channel), and **containment
does not depend on the fence holding**
(`TestCIRetryAdversarial_HostileTextChangesNoControlField` asserts that every
control-bearing field of the enqueued child — the decision itself, the bound,
`auto_approve`, `sensitive`, the repository, the attempt depth — is identical
whether the CI text is benign or maximally hostile). The boundary is the human
diff gate, which a retry always re-poses because `auto_approve` is
force-cleared on every child.

**Why this takes no A7 exception.** A retry is a new task on a fresh VM that
re-clones the repository's **default branch HEAD**. The previous attempt's work
crosses over only as the capped text above — never as a base branch, a mounted
tree, or a clone ref. Nothing agent-written becomes a base tree, so the retry
sits outside A7's single carve-out (the read-only dependency cache) rather than
widening it, and B2 adds **no new A-claim**. Pinned host-side, without a VM, by
`TestRedteam_A7Retry_ChildInheritsTextOnlyNeverTheParentsTreeOrBranch`, which
drives the real dispatcher and asserts the child's stage root is its own and
its clone ref is the parent's repo ref unchanged — **no reference to the
parent's branch or tree** anywhere in its persisted record: no `agent/<parent>`,
no base ref, no fragment, no mount.

The child does of course reference the parent *as an id*, deliberately and in
two places: `retry_of` on its persisted task, and a `- prior task: <id>` line in
its instruction. That is the chain link every surface follows, not a leak — an
id names a record, and naming one gives a fresh VM no access to the tree that
record produced.

### N3. Side-channel data exfiltration via the diff

A clever agent can encode information (e.g. an exfiltrated bearer
token) into the diff itself: file names, whitespace patterns,
comment text. drydock makes this visible to the operator at review
time, but does not detect it programmatically.

### N4. Cost exhaustion and runaway tasks

See the [security defaults](https://sricola.github.io/drydock/docs/security-defaults.html)
page for the full table of shipped bounds (budget, request caps, timeouts,
disk quota), generated from code, and the test that enforces each one.

**The global usage ceiling (`global_budget_usd` / `global_max_tasks`, opt-in,
OFF by default).** Every other bound on this page is per-task or per-vendor. The
global ceiling is the only one that is neither: it bounds the daemon as a whole,
across every vendor and both auth modes, over a rolling `global_window`
(default `24h`). It exists because bounded CI retry made drydock spend money
unattended, and the honest answer to "what stops a retry storm?" was *nothing
global* — `aggregate_budget_usd` is per-vendor (so with N vendors the real
ceiling is N × the configured number), `api_key`-mode only, in-memory, and
fail-open at every broker-side check.

It has **two limbs**, either of which refuses a task start:

- **`global_budget_usd`** — cumulative **broker-metered** USD across all
  vendors. "Broker-metered" is exact: the figure is the gateway lease's own
  `SpentUSD`, parsed host-side out of proxied response bodies. An agent-reported
  `total_cost_usd` never reaches it, and cannot inflate or deflate it.
- **`global_max_tasks`** — cumulative **task starts** across all vendors and
  both auth modes. Retries and their parents count alike.

What it does, precisely:

- **It refuses task STARTS. It never kills a running task.** The three admission
  points are `POST /tasks` (which returns **402** with the reason and the
  current headroom), the queue dispatcher, and the CI-retry gate. At the
  dispatcher a human-submitted item **parks** (it stays `queued`, its attempt
  count untouched, and it dispatches when the window rolls); a **retry** over
  the cap is **dropped** to `dead_letter` rather than parked, because an
  unattended item parked at a ceiling would dispatch hours later against a base
  that has moved on. A retry the ceiling could not *measure* — as opposed to one
  it measured and refused — parks instead, so a transient fault does not destroy
  unattended work. Money already spent is already spent; terminating in-flight
  work would create half-finished trees for no saving.
- **It fails CLOSED.** This is the deliberate break with every existing spend
  check. If the durable ledger cannot be read, is corrupt, or the agent cannot
  be resolved, the start is **refused**. For a ceiling, "I don't know" must mean
  "no" — an unattended loop that cannot be measured is exactly the thing to
  stop. `aggregate_budget_usd` keeps its existing fail-open behavior; the two
  coexist and the stricter answer wins.
- **It is durable.** The ledger lives under `audit_root` (host-only, `0600` in a
  `0700` directory, never read or written by anything in a VM) and survives
  restart in both window modes. A crash loop cannot reset it, which is a hole
  the in-memory `aggregate_window: 0` still has.
- **Headroom is visible.** `GET /admin/ceiling` and `drydock stats` report spend
  and starts against each limb, the window, in-flight starts not yet recorded,
  and whether either number is degraded.

**What it does NOT cover.** The USD limb can only count dollars the broker
actually measured, so it under-counts in exactly these cases:

- **Spend metered after a task's broker result row** — a request still in flight
  when the task terminates. The same post-hoc bound `task_budget_usd` carries.
- **Responses whose usage block exceeds the 1 MiB parse buffer.** Beyond that
  the usage is not read and the request meters at $0.
- **Batch-style routes** (`/v1/messages/batches` and equivalents) where usage is
  not in the response the broker proxies at all.
- **`openai_compat` lanes configured with no `prices`**, which meter at **$0 by
  construction** — there is no rate table to price tokens against.
- **Subscription lanes**, where there is no USD to meter at all.

None of these is fixed by the USD limb, and pretending otherwise would be the
dangerous claim. **`global_max_tasks` is the backstop for all of them**: it
counts an event the broker itself causes — a task start — rather than dollars a
response reported, so no metering gap, missing price table, oversized usage
block or batch route can under-report it. That is why the ceiling has two limbs
rather than one, and why an operator running subscription or unpriced
`openai_compat` lanes should set the task limb rather than the dollar one.

Stated plainly, because it is the change that matters most here: **before this,
subscription lanes had NO cross-task bound of any kind** — only per-task limits
(`task_max_requests`, one in-flight request, `task_timeout`) plus
`max_concurrent_tasks`. Nothing bounded cumulative usage over a day, which is
the likeliest configuration for an unattended install. They have one now, if the
operator sets `global_max_tasks`.

Residual, stated without softening:

- **Both limbs default to `0` (off).** A stock install has exactly the bounds it
  had before: no ledger file is created and no admission path changes.
- The USD limb is **soft in the same post-hoc sense as `task_budget_usd`**: an
  admitted task's spend is unknown until it ends, so the ceiling can be crossed
  by at most `max_concurrent_tasks × task_budget_usd`. The task limb is **hard**
  — the start is claimed in the same critical section as the check, so
  concurrent admissions cannot race through it.
- A wall-clock jump **forward** would move a rolling window past every recorded
  entry. In-process jumps are detected against monotonic time and corrected; a
  clock change made while brokerd is **down** is not detectable (a monotonic
  reading does not survive a reboot) and is logged loudly at open rather than
  refused, because refusing it would also refuse every legitimately idle
  install.
- `global_window: 0` (total mode) never ages anything out, and unlike
  `aggregate_window: 0` it is durable — an exhausted ceiling stays exhausted
  across restarts until an operator raises a limb or removes the ledger file.
  brokerd warns at boot when a limb is armed in total mode.

This stays an **N4 mitigation rather than a new A-claim**, deliberately. The A
claims are containment boundaries: an agent attempts something and drydock's
isolation stops it, each verified by an adversarial red-team probe. The global
ceiling is a **safety control on operator-authorized work** — it bounds a loop
the operator asked for, and the "attacker" in the retry-storm scenario is a
failing test suite, not the agent. The agent cannot evade it (the ledger is
host-only and reads only broker-metered spend, which is claim **A1**'s property,
not a new one), but non-evadability of a budget is not a containment boundary.
Its enforcement is pinned by the generated
[security defaults](https://sricola.github.io/drydock/docs/security-defaults.html)
table, which now carries a row per limb naming the test that enforces it.

**Operator-facing spend is broker-observed.** Related, and worth stating here
because it is a trust property rather than a bound: every surface that shows a
dollar figure — `drydock stats`, `drydock tasks`, the web UI's history table and
its **push-approval gate** — now reads the broker-authored `src=="broker"` audit
row, which carries the gateway lease's own metering. The agent's stdout is
copied verbatim into the audit stream, so a `total_cost_usd` it printed is
untrusted text; it is still displayed where it is the only figure that exists
(a task with no broker terminal row yet), but it is explicitly marked as
agent-reported and is never summed into a spend total. The push gate previously
rendered exactly that untrusted number beside the Approve button.

**`api_key` mode (default).** The per-task USD ceiling (`task_budget_usd` /
`DRYDOCK_TASK_BUDGET_USD`) caps spend but does not cap usefulness. An agent
that burns $2 on no-op API calls hits the cap and produces no diff. Operators
should monitor `costUSD` in `<task>.jsonl` and treat repeated zero-diff runs
as a flag.

`task_budget_usd` is a **soft cap**: the gateway meters a request's cost only
once its response completes, so a request that is in flight has not yet been
charged. A hostile in-VM agent holds its own task bearer and could once fire
many requests concurrently to ride past the cap before any of them completed.
Admission now serializes each task's lease to `task_max_inflight` concurrent
requests (default 1), so the worst-case overshoot is bounded: at most
`task_max_inflight` requests in flight at once, times the cost of the largest
single request, not an unbounded number of concurrent calls.
`max_request_cost_usd` adds a reservation-backed bound on top of that: it
reserves that worst-case amount per in-flight request against the budget at
admission, so a concurrent request cannot be admitted past the ceiling at
all. Across concurrent tasks, the aggregate ledger's residual overshoot is
one in-flight request per concurrent task, bounded by `max_concurrent_tasks`.
Set the budget (and, for adversarial workloads, `max_request_cost_usd`) with
that headroom in mind.

**`subscription` mode (`anthropic_auth: subscription`).** When
`anthropic_auth: subscription` is set, drydock routes through the operator's
personal Claude Pro/Max subscription. The credential gateway holds the OAuth
access and refresh tokens host-side and issues per-task bearers as usual (A1
still holds), but **the USD budget cap does not apply**, there is no spend to
meter. The runaway controls are:

- `task_max_requests`: hard ceiling on the number of API round-trips the
  gateway will allow for a single task before returning `429`. Set this
  explicitly; there is no equivalent to the API-key budget sentinel.
- `task_timeout`: wall-clock ceiling (default `30m`), unchanged.
- `global_max_tasks`: the only CROSS-TASK bound available here (see the global
  usage ceiling above). Off by default. Both of the controls above are
  per-task; without this one nothing bounds cumulative usage over a day.

Without `task_max_requests`, brokerd applies a built-in default cap of 1,000
requests, so a subscription task is bounded by that; set `task_max_requests`
explicitly to raise or lower it. Operators running batch jobs should set both.

**`subscription` mode (`openai_auth: subscription`).** When
`openai_auth: subscription` is set, drydock routes Codex tasks through the
operator's personal ChatGPT subscription via the Codex backend
(`chatgpt.com/backend-api/codex`). The credential gateway holds the OAuth
access token, refresh token, and account id host-side and issues per-task
bearers as usual (A1 still holds), but **the USD budget cap does not apply**,
there is no spend to meter. The runaway controls are identical:

- `task_max_requests`: hard ceiling on the number of API round-trips the
  gateway will allow for a single task before returning `429`. Set this
  explicitly when using subscription mode.
- `task_timeout`: wall-clock ceiling (default `30m`), unchanged.
- `global_max_tasks`: the only CROSS-TASK bound available here (see the global
  usage ceiling above). Off by default.

Without `task_max_requests`, brokerd applies a built-in default cap of 1,000
requests, so a Codex subscription task is bounded by that; set
`task_max_requests` explicitly to raise or lower it. Operators running batch
jobs should set both.

**CI retry chains (`ci.max_attempts`).** The CI watch shipped in increment B1
adds **no model spend whatsoever**: it observes check conclusions host-side and
enqueues nothing. Increment B2's bounded retry does add spend, and it is the
first thing in drydock that spends money **unattended, on a timer, without a
human in the loop at the moment of the decision**. The arithmetic, stated
plainly:

A retry is a new task with a new credential lease and a **fresh full
`task_budget_usd`** — it deliberately does not share the parent's budget,
because sharing it would make a retry 402 mid-run against a budget the parent
already spent. So with `ci.max_attempts > 0` the worst case for a single
failing task is

```
max_attempts × task_budget_usd    on top of the parent attempt's own task_budget_usd
```

— `task_budget_usd: 2.00` with `max_attempts: 3` is a **$8.00** ceiling for one
task, not $2.00. Multiply by however many tasks fail CI in a window.

The bounds on that:

- `ci.max_attempts` defaults to **0 (retry off)**, and is inert unless
  `ci.watch` is also on (the decision is only reachable from a terminal CI
  observation). Config validation rejects anything above **10**, so a typo
  cannot authorize an arbitrarily deep chain of real spend.
- The chain's depth is persisted on the task and mirrored on its durable
  `<id>.ci.json` marker, never held in memory, and the retry decision runs only
  after the parent's terminal reached disk. A crash or restart therefore cannot
  launder the bound in the expensive direction: the one crash window that
  exists loses a child rather than duplicating one. A negative `attempt` on a
  submitted body — the only value that could buy extra hops — is clamped to 0.
  Neither `.queue.json` nor `.ci.json` is pruned by `drydock prune`, so an
  age-based sweep cannot reset a counter mid-chain either.
- At most **one child per parent, ever**, enforced by two independent durable
  anchors on the broker-owned queue item (a replay check and a first-writer-wins
  `retry_task_id`), neither of which anything inside a task VM can write.
- `aggregate_budget_usd`, where set, applies across the whole chain, and a
  retry blocked by it is **refused outright rather than parked** — an
  operator's own queued item waits for the rolling window because a human is
  waiting for it, whereas a broker-initiated retry that nobody asked for must
  not sit queued for hours and then dispatch against a base that has moved on.
  The refusal is recorded on the audit's `ci_observation` row.

  **Where neither that cap nor the global ceiling is set, the per-chain product
  above is the ONLY spend bound there is.** The two retry-specific aggregate-cap
  refusals — the decision's and the dispatcher's drop — test a cap that is
  unwired when `aggregate_budget_usd` is `0` (the default) and is
  `api_key`-mode-only by design, so it is absent entirely in subscription mode,
  and neither bounds the number of chains running CONCURRENTLY:
  `max_attempts × task_budget_usd` is per failing task, and ten tasks failing
  CI overnight is ten of them. brokerd warns at boot when `ci.max_attempts > 0` with no
  aggregate cap; it does not refuse, because refusing would make the feature
  unusable in the auth mode most unattended installs run.

  **`global_max_tasks` is what actually bounds the concurrent-chains case**, and
  it is the reason the global ceiling was built: it counts task starts across
  every chain, every vendor and both auth modes, so ten overnight chains consume
  one shared allowance rather than ten independent ones. A retry the ceiling
  refuses is declined at the decision (recorded on the `ci_observation` row) or
  dropped to `dead_letter` at the dispatcher, matching the aggregate cap's
  refuse-rather-than-park rule. It is also off by default.
- Only an **observed** check failure retries. A watch that timed out, gave up,
  or found no checks configured retries nothing: spending a fresh budget on
  "we could not tell" is the failure mode this rule exists to prevent.

**`openai_compat` lane (`api_key` mode).** USD metering depends on the upstream
reporting token usage in its response. Streaming `chat/completions` responses
commonly omit usage, so a streamed task may be metered at $0 and never trip
`task_budget_usd`. `task_max_requests` is the usage-independent per-task cap and should
be set for any `openai_compat` lane where streaming is expected;
`global_max_tasks` is its cross-task counterpart, and is the only global bound
that holds for a lane configured with no `prices` (which meters at $0 by
construction, so `global_budget_usd` can never trip on it).

### N5. Compromise of the host's git remote credentials

`gh` on the host uses the operator's GitHub credentials to push and
open PRs. drydock does not isolate these. An attacker who can run
`drydock approve` can push to any repo the operator's `gh` token can
reach.

**With `ci.watch` enabled, that credential is also used on a timer.** State
this plainly: until increment B1, every host `gh`/`git` invocation was
operator-initiated — a push and a PR open, both downstream of a human
approval at the diff gate. The CI watch is the first use that is not. brokerd
polls each watched PR every `ci.poll_interval` (default `60s`) for up to
`ci.watch_timeout` (default `90m`) with no human in the loop, so an operator
who enables it is consenting to unattended, recurring use of their own GitHub
credential. That is a real change in posture, not a detail.

What bounds it:

- **Read-only.** The watch path issues exactly two commands,
  `gh pr checks <n> --repo <pinned> --json bucket,name,state` and
  `gh pr view <n> --repo <pinned> --json statusCheckRollup`. No write
  subcommand is reachable from it.
- **Host-pinned.** `github.com` is hard-pinned inside the `--repo` flag value,
  so an exported `GH_HOST` cannot aim the credential at another host while
  every persisted record still says github.com.
- **Curated env.** The watch uses the same curated environment every other
  host CLI call gets, never `os.Environ()`.
- **Bounded.** Each watch runs against an absolute deadline anchored at push
  time, which a restart or a crash loop cannot extend, and gives up after a
  small number of consecutive read failures.
- **Off by default.** `ci.watch: false` (the shipped default) means no marker
  is written, no goroutine runs, and no `gh` call is made on a timer.

What it does **not** change: the credential is still the operator's, still
un-isolated, and everything N5 already said about an attacker who can run
`drydock approve` is unchanged. The watch grants the sandboxed agent no new
reach — it runs host-side, on a PR number and owner/repo validated at PR-open
time, and the agent VM has no path to either the credential or the watch.

### N6. Local attacker on the same host

The default Unix socket at `$TMPDIR/drydock-$UID/drydock.sock` is mode `0600`, owned by
the operator. Another user on the same host cannot reach it. But a
process running as the operator can. drydock assumes the host's user
boundary is the relevant trust boundary.

The optional web UI (`drydock ui`) adds a loopback HTTP surface over the
same admin actions (submit, approve/deny, kill, audit reads). It binds
`127.0.0.1` only, requires a per-session bearer token (constant-time
compare; carried in the URL fragment so it never hits server logs), and
rejects non-loopback `Host`/`Origin` headers, the browser is the one
operator-level process that runs attacker-supplied code, and these checks
stop a hostile web page from driving the API via DNS rebinding. Submissions
through the UI refuse `auto_approve`, and audit reads reject symlinks.
`--no-token` removes the token gate with a loud warning; use it only on
single-user machines. The trust boundary itself is unchanged: a native
process running as the operator could already reach the broker socket
directly. Verified by: `internal/webui` tests (`TestAuth`,
`TestHostCheck`, `TestNonLoopbackOriginRejected`,
`TestConstantTimeTokenCompare`, `TestSymlinkRejected`,
`TestSubmitRejectsAutoApprove`).

**The durable state files under the audit dir are trusted for their contents,
and this is a host-filesystem assumption rather than a check.** With `ci.watch`
on, `<id>.ci.json` is the watch's entire cross-restart state: a process running
as the operator that hand-edits one to `"state": "passed"` short-circuits that
task to `completed` with zero `gh` calls and no CI ever observed. The same
applies to `<id>.queue.json` and `<id>.gate.json`. What is enforced is
structural, not semantic: the audit dir is `0700` and each marker `0600`, every
read refuses symlinks (`O_NOFOLLOW`), every write is atomic (temp + rename), the
task id is validated against its shape before any path is built (no traversal),
and a marker whose body names a different task id than its filename is
discarded rather than acted on. Contents are trusted because an attacker who can
write these files is already the operator — the boundary N6 has always drawn.

### N7. Apple `container` runtime escapes

A guest-to-host escape in the VM stack defeats every claim above. We
pin a tested version and recommend upgrading promptly when upstream
publishes security advisories.

## Residual risk summary

- **You must review every diff before approving.** This is the only place
  human judgment is load-bearing.
- **You must keep your host clean.** No drydock defense survives host
  compromise.
- **You must pin and update `container` and the agent CLIs** (`claude-code`,
  `codex`, `gemini`, `opencode`). All move fast; drydock's claims hold only
  against the versions it was tested against (the image pins each one).

If you find a residual that isn't covered here, open an issue. The model
moves; this document moves with it.
