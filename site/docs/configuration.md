# Configuration

`drydock init` creates `~/.drydock/` (mode `0700`) and seeds two files:

| Path | What |
|---|---|
| `~/.drydock/config.yaml` | Operator settings (network, gateway IP, budget, timeout, concurrency, paths, listener, behavior flags) |
| `~/.drydock/egress.yaml` | The allowlist: hosts and ports the sandbox may reach (see [Egress](egress.html)) |

Both are seeded from defaults the first time; `drydock init` never overwrites
them. **Env vars win over file values**, so existing scripts keep working. Edit
`config.yaml` and re-run `drydock start`.

The vendor keys (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GEMINI_API_KEY`) are
intentionally **not** in these files; they live in your shell env, or at
`~/.drydock/api-keys.env` (mode `0600`), read host-side and never passed into
the VM. All three keys are recognized automatically; no extra config is needed
to declare them.

## Common settings

| Field (`config.yaml`) | Env override | Default | Meaning |
|---|---|---|---|
| `anthropic_auth` | `DRYDOCK_ANTHROPIC_AUTH` | `api_key` | `api_key` uses `ANTHROPIC_API_KEY`; `subscription` uses `~/.drydock/claude-oauth.json` |
| `openai_auth` | `DRYDOCK_OPENAI_AUTH` | `api_key` | `api_key` uses `OPENAI_API_KEY`; `subscription` uses `~/.drydock/codex-oauth.json` |
| `default_agent` | `DRYDOCK_DEFAULT_AGENT` | `claude` | Agent when `--agent` is omitted (`claude` \| `codex` \| `gemini` \| `opencode`) |
| `default_model` | `DRYDOCK_DEFAULT_MODEL` | *(empty)* | `--model` fallback for **Claude Code and Codex only**; empty = the agent picks. Not applied to `gemini` (uses its own `gemini-2.5-pro` default) or `opencode` (uses `openai_compat.model`). |
| `task_budget_usd` | `DRYDOCK_TASK_BUDGET_USD` | `2.0` | Per-task USD soft cap, metered post-hoc; overshoot bounded to `task_max_inflight` in-flight requests (default 1); set `max_request_cost_usd` for a reservation-backed bound (`api_key` mode only; unused in subscription mode) |
| `task_max_inflight` | `DRYDOCK_TASK_MAX_INFLIGHT` | `1` | Concurrent gateway requests admitted per task lease; bounds budget overshoot to this many in-flight requests; `0` disables the cap |
| `max_request_cost_usd` | `DRYDOCK_MAX_REQUEST_COST_USD` | `0` (disabled) | Worst-case USD reserved per in-flight request so concurrent requests cannot admit past the budget; `0` disables (post-hoc metering only) |
| `task_max_requests` | `DRYDOCK_TASK_MAX_REQUESTS` | `0` (falls closed to a built-in default of 1000) | Hard cap on API round-trips per task; the primary runaway control in subscription mode |
| `aggregate_budget_usd` | `DRYDOCK_AGGREGATE_BUDGET_USD` | `0` (disabled) | Cross-task USD ceiling per `api_key` provider over `aggregate_window`; `0` disables the cap; subscription mode is out of scope (bounded per-task by `task_max_requests`) |
| `aggregate_window` | `DRYDOCK_AGGREGATE_WINDOW` | `24h` | Rolling window for the aggregate cap; `0` = total since brokerd boot, resets on restart |
| `global_budget_usd` | `DRYDOCK_GLOBAL_BUDGET_USD` | `0` (disabled) | **Global usage ceiling, USD limb.** Cumulative broker-metered USD across **all** vendors and both auth modes over `global_window`; `0` disables it. See [The global usage ceiling](#the-global-usage-ceiling-opt-in-off-by-default) |
| `global_max_tasks` | `DRYDOCK_GLOBAL_MAX_TASKS` | `0` (disabled) | **Global usage ceiling, task limb.** Cumulative task **starts** across all vendors and both auth modes over `global_window`; `0` disables it. Must be `>= max_concurrent_tasks` when set (the pair is refused at load) |
| `global_window` | `DRYDOCK_GLOBAL_WINDOW` | `24h` | Rolling window for **both** global limbs; `0s` = total, nothing ages out — and unlike `aggregate_window` it is durable across restarts |
| `task_timeout` | n/a | `30m` | Wall-clock per task |
| `approval_timeout` | n/a | `0s` | Auto-deny a task left at an approval gate after this long; `0` = wait forever (right for interactive use; set for unattended runs). **Must be non-zero when `ci.max_attempts > 0`** — brokerd refuses the pair at load, because an unattended retry child holds a concurrency slot across the gate it re-poses |
| `max_concurrent_tasks` | `DRYDOCK_MAX_CONCURRENT_TASKS` | `2` | Excess POSTs to `/tasks` get HTTP 503 |
| `notifications` | `DRYDOCK_NO_NOTIFY=1` (off) | `true` | macOS notifications on pending approval |
| `push_max_retries` | `DRYDOCK_PUSH_MAX_RETRIES` | `3` | Transient push failures (network errors) to retry with exponential backoff before giving up; `0` disables transient retry |
| `push_retry_backoff` | `DRYDOCK_PUSH_RETRY_BACKOFF` | `1s` | Base delay for push retry backoff (`backoff * 2^n`); `0` disables the delay between retries |
| `push_fresh_branch_tries` | `DRYDOCK_PUSH_FRESH_BRANCH_TRIES` | `2` | Alternate remote branch names (`agent/<id>-2`, `-3`, ...) to try when a branch-name collision is detected; `0` disables fresh-branch recovery |

## The global usage ceiling (opt-in, off by default)

Every other spend control in drydock is **per task** or **per vendor**. The
global usage ceiling is neither: it bounds the daemon as a whole, across every
vendor and both auth modes, over one rolling window.

```yaml
global_budget_usd: 25       # cumulative broker-metered USD across ALL vendors
global_max_tasks:  40       # cumulative TASK STARTS across all vendors
global_window:     24h      # the window both limbs are measured over
```

Both default to `0`, which is **off**: with neither set, brokerd opens no
ledger, creates no file under `audit_root`, and every admission path behaves
exactly as it did before this feature existed.

### Why there are two limbs

They measure different things because **not every lane has dollars to measure**.

- `global_budget_usd` counts **broker-metered USD** — the figure the credential
  gateway parses out of proxied response bodies. It is meaningful wherever
  metering is real: `api_key` lanes, and `openai_compat` lanes that have a
  `prices` table.
- `global_max_tasks` counts **task starts**. It works everywhere, because a task
  start is an event the broker itself causes. This is the limb that actually
  bounds **subscription mode**, where there is no USD to meter at all, and it is
  the backstop for every case where the dollar figure can be under-reported (see
  [what it does not cover](#what-the-ceiling-does-not-cover)).

Retries and their parents count alike against both — a retry is a task start
like any other.

### How it interacts with `aggregate_budget_usd`

They are independent and both may be set. **The stricter answer wins**: a task
start is refused if either says so. Otherwise they differ in every dimension:

| | `aggregate_budget_usd` | `global_budget_usd` / `global_max_tasks` |
|---|---|---|
| scope | one vendor (N vendors ⇒ N × the number) | all vendors, both auth modes |
| auth modes | `api_key` only | both |
| storage | in memory | durable under `audit_root` |
| restart | total mode resets to $0 | survives, deliberately |
| on "I can't tell" | admits (fail-open) | **refuses (fail-closed)** |
| currencies | USD only | USD **and** task starts |

### What an operator sees when it trips

The ceiling refuses task **starts**. It never kills a running task: the money is
already spent, and terminating in-flight work would leave half-finished trees
for no saving.

- **`drydock submit` / `POST /tasks`** → **HTTP 402** with the reason. An
  over-cap refusal names the limb, both numbers and the window — the USD one
  also gives the remaining headroom, the task one gives how many starts are
  recorded versus still in flight. A refusal the ceiling could not *evaluate*
  names the limb that is enforced and what went wrong instead, since there are
  no numbers to report.
- **A queued item at the dispatcher** → it **parks**. It stays `queued` with its
  attempt count untouched and dispatches on its own once the window rolls or an
  in-flight task finishes.
- **An automatic CI retry** → **dropped** to `dead_letter` rather than parked,
  with the reason in that item's `last_error` (the `REASON` column of
  `drydock queue list`). An unattended item parked at a ceiling would dispatch
  hours later against a base that has moved on. A retry the ceiling could not
  *measure* — as opposed to one it measured and refused — **parks** instead, so
  a transient fault never destroys unattended work.

Fail-closed means the ceiling **refuses** whenever it cannot answer, and it is
gated **per limb** — which is deliberate, not a weakness:

- A ledger that **cannot be read at all** (a symlink where the file should be,
  a permission failure, a file past the read cap) refuses **both** limbs: the
  starts we never read are missing from the count as surely as the dollars.
- A **corrupt line** refuses the **USD** limb only. The line is quarantined, not
  dropped, and it still counts as one task **start** — it exists because a task
  ran — so the start count is complete while the spend total is a lower bound.
  Refusing both here would let one bad byte brick an install that is not even
  using the dollar limb, permanently in total mode. Damage that cannot be
  identified as a single entry (a destroyed checkpoint, or a region that took
  its line breaks with it) refuses **both**, because then the start count really
  is a lower bound too.
- An **agent that will not resolve** refuses.
- An **absent** ledger admits, and that is not a hole: an install that has never
  run a task has provably spent nothing and started nothing. Empty is a fact,
  not an "I don't know". (Deleting the ledger is therefore also how an operator
  clears an exhausted total-mode ceiling — see below.)

The refusal text says which limb is enforced and what clears it; in total mode
it says plainly that the condition does not age out on its own.

### Reading the headroom

```console
$ drydock stats

global ceiling (the last 24h0m0s):
  spend:  $12.50 of $50.00 broker-metered — $37.50 left
  starts: 7 of 20 — 13 left (6 recorded, 1 in flight)
```

The section appears only when the ceiling is on **and** brokerd is reachable —
the rest of `drydock stats` reads the audit dir directly and works with the
daemon stopped, but headroom is live state, and three of its numbers (in-flight
starts, the degraded flags, the verdict) exist only in the running process.

`in flight` is the count of starts this process has admitted that have not
reached their terminal yet, so they are not in the durable ledger. They are
included in the total because that is the number the ceiling is actually
comparing against.

Extra lines appear when they apply:

- `DEGRADED: …` — a number above is a **lower bound**, not a measurement.
  Ledger damage that could not be read is reported rather than silently rounded
  down. It is why the limb that number belongs to refuses — *when that limb is
  armed*: with only `global_max_tasks` set you can see a degraded **spend**
  total and no refusal, because the start count is unaffected.
- `BLOCKED: …` — the exact refusal a task start would receive right now,
  produced by the enforcement path itself rather than recomputed.

The same data is served as JSON by `GET /admin/ceiling` (same listener and auth
as every other `/admin/*` route — the `0600` unix socket, or the
loopback-guarded TCP wrap; nothing here is reachable from a sandbox VM) and by
`drydock stats --json` under `global_ceiling`.

### What the ceiling does not cover

The USD limb can only count dollars the broker measured. It under-counts when:

- spend is metered **after** a task's broker result row (a late in-flight
  completion) — the same post-hoc bound `task_budget_usd` carries;
- a response's usage block exceeds the **1 MiB parse buffer**;
- the route is **batch-style** and usage is not in the proxied response at all.
  For the built-in vendors this is closed rather than open: the gateway's route
  allowlist deliberately omits `/v1/messages/batches` (F-03) and answers it
  `403`, so it is never proxied. The gap is on **`openai_compat`** lanes, whose
  route allowlist is unrestricted — a batch-style route there is proxied and
  meters nothing;
- an `openai_compat` lane is configured with **no `prices`**, so it meters at
  **$0 by construction**;
- the lane is **subscription**, where there is no USD to meter.

`global_max_tasks` is the backstop for every one of those: it counts events, not
dollars, so no metering gap can under-report it. If you run subscription or
unpriced `openai_compat` lanes, set the **task limb** — the dollar limb cannot
help you there.

### Only broker-metered spend counts

Neither limb ever reads a `total_cost_usd` an agent printed. The USD figure is
the gateway lease's own metering, recorded host-side; the ledger lives under
`audit_root` with `0600` permissions in a `0700` directory and is never read or
written by anything inside a VM. An agent cannot inflate the ceiling to deny
service, and it cannot deflate it to keep spending.

The same rule now holds for every surface that *displays* spend. `drydock
stats`, `drydock tasks` and the web UI history table read the broker-authored
`src=="broker"` audit row; the web UI's **push-approval gate** shows the live
gateway lease's own figure, published on the task state by the broker at gate
entry, so there is nothing for it to parse at all. Where the only figure that
exists is one the agent reported (a task still running, say), it is shown but
explicitly marked as agent-reported, and never added to a spend total.

### `global_window: 0` (total mode)

Nothing ages out. Unlike `aggregate_window: 0`, which is in-memory and resets on
every restart, this is **durable** — that is the point, since a crash loop that
reset the ceiling would be the hole rather than the feature. The consequence is
worth knowing before you set it: **an exhausted ceiling stays exhausted across
reboots** until you raise a limb or remove the ledger file. brokerd warns at
boot when a limb is armed in total mode.

## Diff policy: caps, blocked paths, second-look

The optional `diff_policy` block in `config.yaml` constrains the diff a task
may propose. It is enforced host-side by brokerd against the broker-computed
diff facts (the same analysis the trust brief reports), so nothing the agent
prints can influence it. All checks are off by default (zero values / empty
lists).

```yaml
diff_policy:
  max_files_changed: 0        # fail closed when the diff changes more files than this (0 = no cap)
  max_lines_changed: 0        # fail closed when added+deleted lines exceed this (0 = no cap)
  blocked_paths: []           # e.g. ["**/*.pem", ".github/workflows/**"] — touching one fails the task
  second_look_paths: []       # e.g. ["**/Dockerfile"] — approver must acknowledge each flagged category
```

Two distinct mechanisms:

- **Caps and blocked paths fail closed, before review.** A diff that exceeds
  `max_files_changed` / `max_lines_changed`, or touches any `blocked_paths`
  pattern, ends the task with outcome `policy_blocked` **before it reaches
  the approval gate** — there is nothing to approve, and `--auto-approve`
  does not bypass it. The captured diff and trust brief are preserved in the
  audit dir for inspection. A diff too large to fully analyze (files omitted
  past the tracking bound) also fails closed whenever a content-based policy
  (`blocked_paths` / `max_lines_changed`) is configured, rather than letting
  unanalyzed files slip past it.
- **Second-look paths still reach review, but approving requires
  acknowledgment.** When a diff touches a `second_look_paths` pattern, the
  broker computes the affected risk-flag categories (the trust brief's FLAG
  kinds, e.g. `ci-workflow`, `lockfile`, `exec-bit`) and refuses any approve
  that does not explicitly acknowledge every one of them: `drydock approve
  <id> --acknowledge <category>` (repeatable; alias `--ack`), the per-category
  prompts in `drydock review`, or the checkboxes in the web UI. An approve
  with missing acknowledgments is refused (HTTP 422 naming the missing
  categories) and **the task stays pending** — there is no path on which an
  under-acknowledged approve pushes. Acknowledgments are a human-gate
  feature: `--auto-approve` tasks skip the gate entirely, so
  `second_look_paths` does not apply to them — anything that must be
  impossible to push belongs in `blocked_paths`.

Path patterns are `**`-aware repo-relative globs: `*` matches within a path
segment, `**` crosses segments. Write `dir/**` to cover everything under
`dir` — a trailing-slash pattern like `dir/` matches nothing and is rejected
at config load. There is no env override for this block; configure it in
`config.yaml` (it participates in `drydock policy explain`'s divergence
check like any other field).

## Execution profiles: setup and readiness (per repo)

The optional `profiles` block in `config.yaml` gives a repository a **setup
phase**: commands drydock runs against the task's live work tree **before
the agent starts** — dependency install, code generation — plus readiness
commands that gate the run. Keys must be the canonical `host/owner/repo`
form (a non-canonical key is a config error, not a silent never-match):

```yaml
profiles:
  repos:
    "github.com/you/yourrepo":
      setup:
        - ["npm", "ci"]
      readiness:
        - ["node", "--version"]
      timeout: 10m      # per command; 0 = the default (10m)
      cache: false      # true = opt in to the persistent per-repo dependency cache
```

What the phase guarantees:

- **Host config only.** The commands live in this file on your machine; the
  sandboxed agent can never edit its own setup phase.
- **Fail closed before any API spend.** Setup and readiness run before the
  agent VM boots. Any command that fails, times out, or errors ends the task
  with outcome `setup_failed` — the agent VM never boots, no credential is
  ever injected into any VM, and zero API budget is spent.
- **No credentials, egress-only network.** Setup VMs get the squid egress
  proxy (so `npm ci` can reach your allowlisted registries) but no model
  gateway and no API bearer.
- **Same workspace the agent gets.** Commands run in fresh sandbox VMs (the
  agent's image) against the live per-task work tree, so what setup installs
  is exactly what the agent sees. The per-task stage is wiped at cleanup;
  setup runs from scratch each task unless the repo opts into the
  [dependency cache](#persistent-dependency-cache-opt-in-per-repo) below.
- **Self-contained commands.** Each command is its own argv in its own VM
  run: shell state (`cd`, exported variables, activated virtualenvs) does
  **not** carry between commands. Write each entry to stand alone.

Verdicts are the process exit codes the broker observes — nothing a command
prints can flip a status. The task shows a `setting_up` stage while the
phase runs, and the per-command evidence (status, exit codes, durations)
lands in the trust brief (`drydock inspect <id>`); the combined output is
kept display-only at `~/.drydock/audit/<id>.setup.log`. See
[Submitting tasks](submitting-tasks.html#execution-profiles-setup-per-repo)
for the full behavior.

## Persistent dependency cache (opt-in, per repo)

Set `cache: true` in a repo's profile to reuse setup's dependency downloads
across tasks instead of re-fetching everything each run. Package-manager
caches (npm, Go modules, pip, cargo) are pointed at `/deps`, a host-side
store under `cache_root` (default `~/.drydock/cache`, env
`DRYDOCK_CACHE_ROOT`) bounded by `cache_quota_gb` (default 20 GiB, env
`DRYDOCK_CACHE_QUOTA_GB`; least-recently-used entries are evicted past the
bound, and `0` disables the cache entirely, even for repos with
`cache: true`).

The semantics that keep it safe:

- **Read-only to the agent.** Only setup VMs mount `/deps` read-write; the
  agent VM's mount is strictly read-only, so agent-run code can never poison
  dependencies that later tasks consume. The A7 claim in the
  [threat model](threat-model.html) is unchanged — agent-writable state
  still never persists between tasks — and the `TestRedteam_A7Cache_*`
  red-team tests enforce it.
- **No cross-repo sharing.** Entries are content-addressed: keyed by repo
  identity + lockfile digests + the setup commands + the sandbox image +
  architecture. Two repos never share an entry, even with identical
  lockfiles, and any change to a key input simply misses to a fresh entry.
- **Lockfile required (fails closed to "no cache").** A repo without a
  recognized lockfile runs uncached — an unpinned dependency set has no
  stable content identity to cache under. The trust brief records
  `disabled: no lockfile` so you can see why.
- **Payloads only, never auth.** The cache holds package payloads; registry
  credentials and tokens live in the VM's ephemeral HOME and never touch
  `/deps`.
- **A speedup, never a correctness dependency.** A miss (or caching off)
  simply re-fetches through the squid proxy. One caveat of the read-only
  agent mount: if the *agent itself* installs a package the cache does not
  already carry, its package manager may fail writing the read-only cache
  dir (`EROFS`). Keep setup responsible for installing dependencies — that
  is what the phase is for — or leave caching off for repos where the agent
  routinely installs new packages mid-task.

Cache participation lands in the trust brief's setup block (`drydock
inspect <id>`): `hit`/`miss`/`disabled: no lockfile` plus the entry's key
prefix.

## Host-side CI observation (opt-in, off by default)

The optional `ci` block lets brokerd follow a pushed PR's continuous
integration, record what it observed on the queue item, and — separately
opt-in — enqueue a bounded number of retry tasks when it observes a failure. It
is **off by default**: a stock install writes no marker, starts no watch
goroutine, makes no API call on a timer, and never retries anything.

```yaml
ci:
  watch:         false          # enable the watch (default false = off)
  poll_interval: 60s            # watch tick; 0 = built-in default (60s), minimum 10s
  watch_timeout: 90m            # absolute per-PR deadline; 0 = built-in default (90m); must exceed max(2 x poll_interval, 5m)
  max_attempts:  0              # bounded retry on an observed CI failure; 0 = off (>0 requires a non-zero approval_timeout)
```

| Field (under `ci:`) | Env override | Default | Meaning |
|---|---|---|---|
| `watch` | `DRYDOCK_CI_WATCH=1` | `false` | Enable the host-side CI watch. Only the exact value `1` enables it via env |
| `poll_interval` | `DRYDOCK_CI_POLL_INTERVAL` | `60s` | Watch tick. One GitHub API call per watched PR per tick; `0` = the built-in default, minimum `10s` |
| `watch_timeout` | `DRYDOCK_CI_WATCH_TIMEOUT` | `90m` | Per-PR deadline, absolute and anchored at push, so a restart cannot extend it; `0` = the built-in default. Must exceed the dispatch floor `max(2 × poll_interval, 5m)` — a shorter window makes every watch dead-letter, so the pair is rejected at load |
| `max_attempts` | `DRYDOCK_CI_MAX_ATTEMPTS` | `0` (off) | Bounded retry on an **observed** CI failure. Counts retries, so `2` means at most two extra tasks in a chain. Each is a **new** task with a fresh full `task_budget_usd`, so one chain's worst case is `max_attempts × task_budget_usd` on top of the original. Capped at `10`, and `10` is genuinely reachable — a retry's instruction is your original task plus one attempt's evidence, so its size does not grow with chain depth |

### What the watch does, and what it does not

- **It observes check conclusions.** With `watch: true`, a queued task no
  longer finishes at the push: the item moves to `awaiting_ci` and brokerd
  polls that PR's checks until they resolve. Terminals are `completed` (every
  check passed, *or* the PR has no checks configured), `ci_failed` (the broker
  **observed** a check fail), and `dead_letter` when the watch ended with *no*
  conclusion — a timeout, repeated read failures, or a marker lost to a
  restart. An unobserved outcome never reads as success. The verdict is
  surfaced on the **queue** — `drydock queue list` and `GET /queue` — and in
  the raw audit record; `drydock tasks`, `drydock stats`, and the web UI
  classify a task from its own terminal row and keep reading `ok` for a clean
  push regardless of its CI. The full state table is in
  [Submitting tasks](submitting-tasks.html#watching-the-pr-s-ci-opt-in-off-by-default).
- **It does not read CI logs.** Only conclusions are fetched. No CI log text
  is retrieved, parsed, stored, or displayed anywhere on this path, and
  nothing a repository's workflow prints can influence any decision. Check
  *names* are recorded (a repo's workflow file chooses them, so they are
  sanitized at ingestion) and they decide nothing.
- **It retries only when you ask it to, and only a bounded number of times.**
  `max_attempts` needs `watch: true` to do anything: the retry decision is only
  ever reached from a terminal CI observation, so a non-zero `max_attempts`
  with the watch off is inert. With `max_attempts: 0` (the default) a CI
  failure is recorded and never acted on. Above `0`, an **observed** check
  failure enqueues one **new** task — never a re-run of the old one — carrying
  the prior attempt's diff and the failed check names forward as capped,
  fenced, untrusted text. **Exactly one attempt's worth, at every depth**: the
  retry instruction is your original task plus one evidence section plus one
  prior-diff section (the most recent), not the parent's instruction plus
  another pair — so hop 10 is the same size as hop 1 and a chain never grows its
  own prompt out of the task-body cap. Every other ending (`timed_out`, gave-up,
  "no checks", and of course a pass) retries **nothing**: they are not evidence
  of a failing build. A plan-only parent and a synchronous `drydock submit`
  task are never retried either.
- **What it costs.** Each attempt mints a **fresh full `task_budget_usd`** — it
  does not share the parent's, which would 402 mid-run against money already
  spent — so the worst case for one chain is `max_attempts × task_budget_usd`
  **on top of** the parent's own budget. With `task_budget_usd: 2.00` and
  `max_attempts: 3`, one persistently failing task can cost $8.00 rather than
  $2.00. That is why the default is `0` and the ceiling is `10`. When
  `aggregate_budget_usd` is set and its window is exhausted, a retry is
  **refused outright rather than parked** (with the reason recorded): an
  operator's own queued item waits for the window because a human is waiting
  for it, but a broker-initiated retry must not sit queued for hours and then
  dispatch unattended against a base that has moved on. That holds at both
  ends — the decision declines to enqueue, and a child whose cap exhausts in
  the gap before dispatch is dropped to `dead_letter` rather than parked (with
  the reason in that item's `last_error`, shown by `drydock queue list`'s
  `REASON` column).

  **With no `aggregate_budget_usd`, both of those refusals are inert**, and the
  per-chain product above is then the only bound on retry spend from that cap —
  which is also `api_key`-mode-only by design, so it is absent in subscription
  mode, and caps nothing across chains running at once. brokerd warns at boot
  when `max_attempts > 0` with no aggregate cap; it does not refuse the pair,
  because that would make retry unusable in subscription mode.

  **`global_max_tasks` is the bound that covers the concurrent-chains case**
  (see [the global usage ceiling](#the-global-usage-ceiling-opt-in-off-by-default)):
  it counts task starts across every chain, every vendor and both auth modes, so
  ten overnight chains share one allowance instead of having ten. A retry it
  refuses is declined at the decision or dropped at the dispatcher, exactly like
  the aggregate cap's. It is off by default too.
- **It requires a non-zero `approval_timeout`, and the pair is refused at
  load.** A retry child is the daemon's only *unattended* task author, and it
  holds one of your `max_concurrent_tasks` slots for its entire life — the
  human diff gate it always re-poses included. With `approval_timeout: 0`
  ("wait forever") two PRs failing CI overnight fill the default cap of two
  slots with children parked at gates nobody is at, and every task submitted
  afterwards sits `queued` indefinitely. Restarting brokerd *does* clear the
  held slots — the semaphore is in-process and the boot resume of a parked gate
  does not take one — but the daemon is meant to run for weeks, and "restart it"
  is not a plan for an unattended overnight queue.
- **A retry never bypasses the diff gate.** The child is an ordinary queued
  task: its own slot, its own credential lease, its own VM, its own verify, and
  its own human approval — `auto_approve` is force-cleared on it even when the
  parent had it set, and `sensitive` is preserved and never downgraded.
- **Every attempt's diff is cumulative, on purpose.** The retry re-clones the
  default branch rather than building on the previous attempt's branch, so each
  gate shows the **full** change against the default branch. A delta-based
  retry would ask you to approve attempt 2 without attempt 1's hunks in front
  of you while the push still pushes the whole tree; worse, it would silently
  **downgrade second-look acknowledgments** (attempt 1 touches
  `.github/workflows/**` and needs a `ci-workflow` ack, attempt 2's delta
  touches only `src/` and needs none, and the workflow change rides along), and
  it would make `max_lines_changed` / `max_files_changed` per-attempt so an
  over-cap change could split into N under-cap attempts.
- **Each attempt opens its OWN pull request**, and closing the superseded one
  is the operator's job — drydock never closes a PR, because that is a write
  against your remote nobody approved.
- **The bound is on disk, so a crash cannot launder it.** The attempt counter
  lives in the persisted queue record and its CI marker, never in memory, and
  the decision runs only after the parent's terminal reached disk. A brokerd
  killed mid-decision can end a chain one attempt short; it can never restart
  one, mint two children for one parent, or extend a chain past
  `max_attempts`. `attempt` and `retry_of` are broker-owned — `POST /queue`
  zeroes both — and a conclusion replayed after a crash or a failed queue write
  carries the check rollup the marker persisted with it, so a retry can never
  be authorized by an observation that cannot say what failed. Lowering `max_attempts` mid-chain stops the chain at the next
  decision without unwinding an already-enqueued retry; raising it lets an
  in-flight chain continue to the new bound.
- **Follow a chain** with the `RETRY` column in `drydock queue list` (attempt
  depth plus abbreviated `<-` parent / `->` child ids), or with `attempt`,
  `retry_of`, and `retry_task_id` on `GET /queue` and on the audit's
  `ci_observation` record — which also carries a `retry_detail` string naming
  the reason whenever the decision was reached and refused (the bound, the
  spend cap, a refused build, or an enqueue that failed). It is empty whenever
  the decision was never reached: the retry is off, the observation was not a
  failure, the parent's terminal did not land, the task was a synchronous
  `drydock submit` with no durable queue item (so every non-queued task), a
  child was already recorded, or brokerd was shutting down.
- **It holds no concurrency slot and keeps no stage.** The watch is a
  separate bounded poll, so other tasks keep dispatching normally.
- **It waits before believing a non-failing answer.** CI dispatch is
  asynchronous and incremental, so a poll right after a push sees only part of
  what will run. An empty check list means *not yet*; a list where everything
  is green means *the first workflow finished*. Both are kept as `pending`
  until `max(2 × poll_interval, 5m)` has elapsed since the push — raising
  `poll_interval` therefore raises that floor too, and `watch_timeout` must
  exceed it (the pair is validated at load). Only after the floor do "this PR
  has no checks" and "this PR passed" become conclusions. An observed
  **failure** is conclusive immediately.
- **It only watches `github.com`.** The host is pinned inside the API call
  (see below), so a task on a GitHub Enterprise host is never watched: it
  pushes and completes on the unwatched path, and is never dead-lettered for
  it.

### It puts your `gh` credential on a timer

This is the one thing to weigh before enabling it. Until now every host
`gh`/`git` call drydock made was operator-initiated — a push and a PR open,
both downstream of your approval at the diff gate. The watch is not: it runs
every `poll_interval` for up to `watch_timeout` with no human in the loop.

The calls are **read-only** (`gh pr checks --json` and `gh pr view --json`;
no write subcommand is reachable from the watch path), the GitHub host is
**pinned in the flag value** so an exported `GH_HOST` cannot redirect the
credential elsewhere, and the environment is the same curated env every other
host CLI call gets. See [N5 in the threat model](threat-model.html) for the
full statement.

**To turn it off:** set `ci.watch: false` (or delete the block — the default
is off) and restart brokerd with `drydock start`. To keep the observation but
stop the retry, set `ci.max_attempts: 0` instead. Make sure `DRYDOCK_CI_WATCH`
and `DRYDOCK_CI_MAX_ATTEMPTS` are not exported in the daemon's environment; env
wins over the file. `drydock policy explain` shows which layer set it, and
whether the running daemon agrees. An item already parked in `awaiting_ci`
when the watch is switched off is terminated honestly at the next boot
(`dead_letter`, "no CI conclusion was observed") rather than left hanging.

## Bring your own model

`opencode` reaches any OpenAI-compatible endpoint via the `openai_compat` block
in `config.yaml` (or the `drydock setup` wizard). There is **no env override**;
configure it in the file. The real key is referenced by env-var **name**, never
stored here.

| Key (under `openai_compat:`) | Meaning |
|---|---|
| `base_url` | Endpoint host, e.g. `https://generativelanguage.googleapis.com` (empty = disabled; https, or http only for `localhost`) |
| `base_path` | Path joined onto the request, e.g. `/v1beta/openai` |
| `api_key_env` | **Name** of the host env var holding the real key (e.g. `GEMINI_API_KEY`) |
| `model` | Model id passed to the agent, e.g. `gemini-2.5-pro` |
| `prices` | Optional `{<model>: {input, output}}` USD per 1M tokens; enables USD budgeting, omit to rely on `task_max_requests` |

**Streaming and USD metering.** Streaming `chat/completions` responses commonly
omit token usage unless the client explicitly requests it (via
`stream_options.include_usage`). drydock does not inject that option, so a
*streamed* task against a priced `openai_compat` endpoint may be metered at $0
against `task_budget_usd`: the response completes but carries no usage to bill.
The usage-independent backstop is `task_max_requests`: it counts every API
round-trip regardless of whether the upstream reports usage. Set
`task_max_requests` for any `openai_compat` lane where streaming is expected.

**`prices` and the `"default"` row.** The `prices` map is keyed by model id. If
a task uses a model not explicitly listed and no `"default"` row exists, drydock
has no price to apply and meters that call at $0, so the USD budget will never
trip for that model. Add a `"default"` entry to catch unlisted models:

```yaml
openai_compat:
  prices:
    my-model: {input: 1.00, output: 3.00}
    default:  {input: 1.00, output: 3.00}  # fallback for any unlisted model
```

See [Bring your own model](models.html) for worked examples.

## Native Gemini

`--agent gemini` (`default_agent: gemini`) uses Google's native Gemini API
directly. No `openai_compat:` block is needed; just set `GEMINI_API_KEY` in
your env or `~/.drydock/api-keys.env`. `GEMINI_API_KEY` is a recognized key
automatically. There is no subscription mode for Gemini; API key is the only
auth path.

```yaml
default_agent: gemini          # make Gemini the default (defaults to gemini-2.5-pro)
```

`default_model` does not affect Gemini; pick a non-default Gemini model per task
with `--model gemini-2.5-flash`.

## Advanced: runtime, paths, listener

| Field (`config.yaml`) | Env override | Default | Meaning |
|---|---|---|---|
| `network` | `DRYDOCK_NETWORK` | `drydock-egress` | vmnet network name |
| `gateway_ip` | `DRYDOCK_GW_IP` | `192.168.66.1` | Gateway + squid bind here |
| `sandbox_image` | `SANDBOX_IMAGE` | `drydock-sandbox:latest` | Per-task agent VM image |
| `anchor_image` | `DRYDOCK_ANCHOR_IMAGE` | `drydock-anchor:latest` | Minimal image holding the vmnet gateway IP |
| `stage_root` / `audit_root` / `squid_run_dir` | `STAGE_ROOT` / `AUDIT_ROOT` / `SQUID_RUN_DIR` | `~/.drydock/{stage,audit,squid}` | Per-task scratch (audit dir `0700`; log + diff `0600`) |
| `cache_root` | `DRYDOCK_CACHE_ROOT` | `~/.drydock/cache` | Persistent per-repo dependency caches for profiles with `cache: true` |
| `cache_quota_gb` | `DRYDOCK_CACHE_QUOTA_GB` | `20` | Total disk bound (GiB) for the dependency cache; LRU-evicted; `0` disables caching entirely |
| `broker.socket` | `BROKER_SOCKET` | `$TMPDIR/drydock-$UID/drydock.sock` | Unix socket (parent dir `0700`, socket `0600`) |
| `broker.addr` | `BROKER_ADDR` | *(empty)* | `host:port` to expose over TCP (**no auth**; see [SECURITY.md § TCP exposure](https://github.com/sricola/drydock/blob/main/SECURITY.md#tcp-exposure-brokeraddr--broker_addr)) |
| `log_json` | `DRYDOCK_LOG_JSON=1` | `false` | Force JSON logs even on a TTY |
| `strict_container_version` | `DRYDOCK_STRICT_CONTAINER_VERSION=1` | `false` | Fail closed when `container`'s major drifts from the tested range |
| n/a | `EGRESS_CONFIG` | `~/.drydock/egress.yaml` | Path override for the egress YAML |

Gateway port `8088` and squid port `3128` are hard-coded in
`cmd/brokerd/main.go` and `image/entrypoint.sh`; change both together.

## Inspecting the effective policy

With env vars layered over `config.yaml` layered over built-in defaults, it is
easy to lose track of which value actually won. `drydock policy explain`
resolves the config exactly like `drydock start` does and prints every setting
with the layer that supplied it:

```text
SETTING          VALUE             SOURCE
Network          drydock-egress    default
GatewayIP        192.168.66.1      default
TaskBudgetUSD    5                 config.yaml
MaxConcurrent    4                 env:DRYDOCK_MAX_CONCURRENT_TASKS
...

daemon: in sync — the running brokerd resolved this same policy
```

The `SOURCE` column is one of three values, resolved per field (env >
`config.yaml` > default):

- `default` — the built-in default is in effect.
- `config.yaml` — the file set this field to a non-default value.
- `env:VAR` — the named env var overrode it (the row tells you exactly what
  to unset).

**The daemon verdict.** brokerd resolves its policy once, at boot. When it is
reachable, `policy explain` compares your shell's resolution against the
daemon's:

- `in sync` — the running daemon resolved this same policy.
- `DIVERGENT` — the daemon is running an older resolution (you edited
  `config.yaml` or changed env since it started); the differing fields are
  listed as `local=... live=...`. Restart brokerd to pick up the changes.
- `LIVE POLICY UNVERIFIED` — brokerd isn't reachable; only the local table is
  shown, and no sync claim is made.

`--json` emits the machine-readable form: `{"local": {fields, hash}, "live":
{fields, hash} | null, "in_sync": bool | null}` (`live`/`in_sync` are `null`
when the daemon could not be asked). Both `hash` values cover only the divergence-comparison subset (connection fields `broker.socket` / `broker.addr` are excluded), so clients should compare the two `hash` values to each other or check `in_sync` rather than recomputing a hash over the full `fields` list.

Two semantics worth knowing:

- **Source attribution is value-based.** A `config.yaml` line that sets a
  field to its built-in default shows as source `default` — the layers agree,
  so the lowest one is credited. Only a non-default file value reads
  `config.yaml`.
- **Connection settings don't count as divergence.** `broker.socket` /
  `broker.addr` (`BROKER_SOCKET` / `BROKER_ADDR`) describe how the CLI
  *reaches* the daemon, not policy the daemon enforces. They appear in the
  table like any other field but are excluded from the in-sync/DIVERGENT
  verdict, so dialing a specific daemon via `BROKER_ADDR` doesn't trip a
  spurious divergence.
