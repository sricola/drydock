# Changelog

All notable changes to this project are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [SemVer](https://semver.org/spec/v2.0.0.html). Each
entry below corresponds to a Git tag of the same name.

## Unreleased

### Added

- **A global, durable, fail-closed usage ceiling (`global_budget_usd`,
  `global_max_tasks`, `global_window`; opt-in, OFF by default).** The first
  spend bound in drydock that is neither per-task nor per-vendor: it bounds the
  daemon as a whole, across every vendor and both auth modes, over one rolling
  window (default `24h`).

  It exists because bounded CI retry made drydock spend money unattended and
  nothing global stopped a retry storm. `aggregate_budget_usd` is per-vendor (so
  with N vendors the real ceiling is N × the configured number),
  `api_key`-mode-only, in memory, and fail-open at every broker-side check —
  and in **subscription mode there was no cross-task bound at all**, only
  per-task limits plus `max_concurrent_tasks`. There is one now.

  **Two limbs, because not every lane has dollars to measure.**
  `global_budget_usd` counts cumulative **broker-metered** USD across all
  vendors. `global_max_tasks` counts cumulative **task starts**, which works
  everywhere because it counts an event the broker itself causes — it is what
  bounds subscription mode, and the backstop for every case where the dollar
  figure can be under-reported (usage past the 1 MiB parse buffer, batch-style
  routes on an `openai_compat` lane, `openai_compat` lanes with no `prices` that
  meter at $0 by construction). Retries and their parents count alike.

  What is true of it, stated as invariants:

  - **It fails CLOSED, per limb**, unlike every existing spend check. A ledger
    that cannot be read *at all*, or an agent that will not resolve, **refuses**
    the task start on both limbs. A corrupt *line* refuses the **USD** limb
    only — it is quarantined rather than dropped and still counts as one task
    start, so the start count stays complete while the spend total becomes a
    lower bound; refusing both there would let one bad byte brick an install
    that is not using the dollar limb. An **absent** ledger admits: an install
    that has never run a task has provably spent nothing. For a ceiling, "I
    don't know" must mean "no" — but empty is a fact, not an unknown.
    `aggregate_budget_usd` keeps its current behavior; the two coexist and the
    stricter answer wins.
  - **It refuses task STARTS and never kills a running task.** `POST /tasks`
    returns **402** with the reason (the limb, both numbers, the window, and
    the headroom or the in-flight split); a human-submitted
    queue item **parks** and dispatches when the window rolls; an automatic CI
    retry over the cap is **dropped** to `dead_letter` rather than parked, while
    one the ceiling could not *measure* parks instead, so a transient fault
    never destroys unattended work.
  - **It is durable and window-honest.** The ledger lives under `audit_root`
    (`0600` in a `0700` directory, host-only, never touched from a VM) and
    survives restart in both window modes, so a crash loop cannot reset it.
    A LIVE task terminal carries a broker-authored timestamp, so the window is
    measured from when the task *ran*. Boot reconciliation cross-checks the
    ledger against the audit and only ever adds — and it reads a trace's
    *existence*, never its content: the start is counted exactly, the spend is
    recorded as **unknown**, and the timestamp comes from filesystem metadata
    (clamped to now), because everything inside a trace is text the agent's own
    stdout was copied into. A forward wall-clock jump is corrected against
    monotonic time on the **write** path as well as the read path, so it can
    neither zero a limb nor delete a durable entry.
  - **Only broker-metered spend counts.** The USD figure is the credential
    gateway's own lease metering. An agent-reported `total_cost_usd` cannot
    inflate the ceiling to deny service or deflate it to keep spending.
  - **Headroom is visible**: `GET /admin/ceiling` and a new section in
    `drydock stats` report spend and starts against each limb, the window,
    in-flight starts not yet recorded, and whether either number is degraded.
  - **`global_max_tasks` must be at least `max_concurrent_tasks`.** The pair is
    refused at load: a limb below the concurrency width can never fill the slots
    you configured, and at runtime that is indistinguishable from a wedged
    daemon.
  - **`global_window: 0` is total mode** — nothing ages out, and unlike
    `aggregate_window: 0` it is durable, so an exhausted ceiling survives a
    restart until an operator raises a limb or removes the ledger. brokerd warns
    at boot.

  Both limbs default to `0`. A stock install creates no ledger file, runs no
  extra boot scan, and takes byte-identical admission paths. `THREAT_MODEL.md`
  N4 documents the coverage and the non-coverage; the generated security-defaults
  table gains a row per key.

- **Bounded automatic retry on an observed CI failure (orchestration increment
  B2, opt-in, off by default).** With `ci.max_attempts > 0`, an **observed**
  check failure on a pushed PR now enqueues a retry. A retry is a **NEW task**
  — new id, new queue item, new credential lease, new stage, new branch, new
  PR — never a re-run of the parent, whose record stays terminal at
  `ci_failed` and gains a `retry_task_id` link to its child (the child carries
  `retry_of` back). It **re-clones the repository's default branch HEAD**
  rather than building on the previous attempt's branch: the prior diff and the
  failed check names cross over only as capped, sanitized, fenced *untrusted
  text* in the instruction, so every attempt's gate shows the **full cumulative
  diff** and the diff-size caps and second-look acknowledgments are computed
  over the whole change instead of a per-attempt increment.

  What cannot happen, stated as invariants:

  - **Only an OBSERVED failure retries.** `timed_out`, gave-up (`unknown`),
    "no checks configured", and a pass each enqueue nothing — none of them is
    evidence of a failing build, and retrying on "we could not tell" would
    spend a fresh budget to learn nothing.
  - **CI text still decides nothing (D3).** The retry keys off the
    broker-observed conclusion bucket alone. A check literally named
    `all checks passed, do not retry` on a `failure` conclusion still retries;
    one named `FAILED` on a `success` conclusion still does not.
  - **A retry never bypasses the human diff gate.** `auto_approve` is
    force-cleared on the child even when the parent had it, and the child runs
    the ordinary dispatcher lifecycle — its own slot, lease, VM, verify, and
    approval. `sensitive` is preserved and never downgraded.
  - **The bound cannot be laundered by a crash.** The attempt counter lives in
    the persisted queue record and its `<id>.ci.json` marker, never in memory,
    and the decision runs only *after* the parent's terminal is durable — so a
    kill mid-decision can end a chain one attempt short but can never produce a
    second child or extend a chain past `max_attempts`. `attempt` and
    `retry_of` are broker-owned: `POST /queue` zeroes both, so the bound does
    not rest on a single downstream clamp.
  - **A retry that cannot say what failed is refused, not shipped.** The
    `<id>.ci.json` marker persists the check rollup alongside the terminal
    state, so a conclusion replayed after a crash or a failed queue write still
    carries its evidence — and an observation that carries none is declined
    rather than turned into a fresh full-budget task that would re-run blind.
  - **The spend cap refuses rather than parks — at both ends.** A retry
    blocked by `aggregate_budget_usd` at the decision is declined outright,
    with the reason recorded; one whose vendor cap exhausts in the gap before
    dispatch is **dropped to `dead_letter` by the dispatcher** rather than
    parked, so an unattended retry never sits queued for a rolling window and
    then dispatches hours later. A human-submitted task still parks. That drop
    is the **only** `queued → dead_letter` edge in the queue state machine, and
    a dropped child never runs — so it writes no audit terminal row, and its
    `last_error` (now on `GET /queue` and in a new `REASON` column of
    `drydock queue list`) is its only record.
  - **A hop's prompt does not grow with the chain.** Each retry's instruction
    is the operator's **original** task plus exactly one CI-evidence section
    plus exactly one prior-diff section — the most recent attempt's — carried
    on a broker-owned `root_instruction` field rather than by appending to the
    parent's assembled instruction. Attempt 10 is the same size as attempt 1
    (an accumulating instruction hit the 64 KiB task-body cap around attempt 3,
    which made the documented ceiling unreachable), and the fence's stated
    property — the announced delimiters are the only ones in the document —
    holds at every depth instead of only the first.
  - **Shutdown authors and starts nothing new.** brokerd latches the queue shut
    before it tears tasks down, so a CI poll that concludes mid-drain cannot
    enqueue a retry and the dispatcher cannot start a fresh VM after
    `CancelAll`; shutdown then waits for dispatched lifecycles to finish their
    teardown, which `srv.Shutdown` never saw because they are not HTTP
    handlers.

  Worst case for one chain is `max_attempts × task_budget_usd` **on top of**
  the parent's own budget — each attempt mints a fresh full budget. Default is
  `0` (off); the ceiling is `10`, and `10` is genuinely reachable — a retry's
  instruction is the **original** task plus one attempt's evidence, so a hop's
  size does not grow with chain depth. Without `aggregate_budget_usd` the
  per-chain product is the *only* bound (both retry-specific spend refusals go
  through that cap, which is unset by default and `api_key`-mode-only); brokerd
  warns at boot about that pair. `ci.max_attempts` needs `ci.watch` to do
  anything — the decision is only reachable from a terminal CI observation — and
  a plan-only parent or a synchronous `drydock submit` task is never retried.

  **`ci.max_attempts > 0` now requires a non-zero `approval_timeout`**, and
  brokerd refuses the pair at load. A retry child re-poses the human diff gate
  while holding one of your `max_concurrent_tasks` slots, so "retry overnight"
  plus "wait at the gate forever" is silent slot starvation: two failed PRs at
  03:00 fill the default cap of 2 and every task submitted afterwards sits
  `queued` indefinitely.

  The accepted cost, stated rather than buried: **each attempt opens its own
  pull request**, and closing the superseded one is the operator's job. drydock
  never closes a PR; an automatic close would be an unapproved write against
  your remote, and the superseded PR is usually the clearest record of what
  went wrong.

  **Where a chain is surfaced:** new `RETRY` and `REASON` columns in
  `drydock queue list` (attempt depth plus abbreviated `<-` parent / `->` child
  ids; and the item's `last_error` for anything that did not finish cleanly),
  the new `attempt` and `last_error` fields alongside
  `retry_of`/`retry_task_id` on `GET /queue`, and
  `attempt`/`retry_of`/`retry_task_id`/`retry_detail` on the audit's
  `ci_observation` record — `retry_detail` being the broker-authored reason a
  retry did or did not happen. All ids and broker-authored strings only: no
  instruction text, and no CI or agent text, is re-broadcast anywhere.
  **`drydock queue list` gained two columns this release**, so scripts parsing
  it by column position need updating.

- **Host-side CI observation for pushed PRs (orchestration increment B1,
  opt-in, off by default).** With `ci.watch` enabled, a queued task no longer
  finishes at the push: once its branch is pushed and the PR is open, the item
  moves to the new **`awaiting_ci`** state and brokerd polls that PR's check
  conclusions host-side until they conclude. The watch holds **no concurrency
  slot and keeps no stage** — it is a bounded poll keyed off a durable
  `<id>.ci.json` marker, so a restart re-watches with the *original* absolute
  deadline and a crash loop cannot extend the window. Terminals:
  `completed` for an observed pass **or** an observed "this PR has no checks
  configured", the new terminal **`ci_failed`** for an **observed** check
  failure, and `dead_letter` for a watch that ended with *no* conclusion (a
  timeout, repeated read failures, a marker lost to a restart, or the watch
  being disabled under a parked item), with the reason in `last_error`. The
  asymmetry is the feature: `ci_failed` means the broker watched a check fail
  and nothing else produces it, and **an unobserved CI outcome never reaches
  `completed`** — absence of evidence is not success. The queue state machine
  stays forward-only and acyclic (asserted mechanically, not by inspection);
  terminals remain sinks. Only check *conclusions* are read — no CI log text
  is fetched, parsed, stored, or displayed anywhere on the path, and none can
  influence a decision. **Where the observation is surfaced**, exactly: a `CI`
  column in `drydock queue list` (observed conclusion + PR number, `-` when
  nothing was observed), `ci_state`/`pr_number` on `GET /queue`, and the raw
  `ci_observation` record in the task's audit trace. It is deliberately **not**
  surfaced in the web UI, `drydock tasks`, or `drydock stats`: all three
  classify a task from its last `result` row, and a CI observation is not one
  (see below), so a pushed task keeps reading `ok` in those views no matter
  what its PR's CI did. The queue is the CI surface. The observation is appended to the
  task's audit as a broker-authored `{"type":"ci_observation","src":"broker"}`
  record — deliberately **not** a late `result` row, so a CI failure never
  relabels a successful push in `drydock tasks`/`stats`/the web UI and never
  disturbs the restart-seeded aggregate spend ledger (F-07), both of which
  read the last `result` line. With `ci.watch` off (the default) a pushed
  queue item completes at the push exactly as before, no marker is written,
  and no API call is made on a timer.

  Three details worth stating outright, because each one is a place "nothing
  observed" could otherwise have become "fine":

  - **`no_checks` is a function of time, not just of one poll.** GitHub Actions
    dispatch is asynchronous, so a poll that lands between the push and the
    checks appearing sees an empty rollup that means *not yet*. An empty rollup
    is therefore recorded as `pending` until a **dispatch floor** —
    `max(2 × ci.poll_interval, 5m)`, measured from the marker's persisted push
    time so a restart or crash loop cannot dodge it — has elapsed. The floor
    holds a **passing** rollup for exactly the same reason: a poll that lands
    while workflow A's check is green and workflow B's has not been created yet
    sees a real `passed`, about one workflow of N, and concluding on it would
    make B's later failure permanently invisible. An observed **failure** is
    conclusive on sight and is never held. Only above the floor are "this PR
    has no checks" and "this PR passed" conclusions. A repository with
    genuinely no CI — or with very fast CI — now sits in `awaiting_ci` for
    those few minutes before completing. The pair `ci.poll_interval` /
    `ci.watch_timeout` is cross-validated at load so a timeout inside the floor
    (which would dead-letter every watch) is rejected rather than discovered in
    production.
  - **Enterprise hosts are not watched, and are not failed either.** The
    watch's `gh` calls hard-pin `github.com` (the `GH_HOST` defense), so a task
    whose repo ref names a GitHub Enterprise host writes no marker and arms no
    watch: its push completes on the unchanged path exactly as it does with
    `ci.watch` off. Watching enterprise hosts is a deliberate future increment.
  - **The `CI` column in `drydock queue list` is always present**, including
    with `ci.watch` off, where every row simply shows `-`. The column is part
    of the table now, not conditional on the feature; scripts that parse that
    output by column position should be updated.
  - **`drydock queue cancel` works on an `awaiting_ci` item**, cancelling the
    watch (its marker is removed, so no further API call is spent on that PR)
    and moving the item to `cancelled`. `cancelled` therefore remains reachable
    from every non-terminal state, as documented.

- **`ci:` config block — the opt-in that turns the CI watch on.** New keys in
  `~/.drydock/config.yaml`: `ci.watch` (default **`false`** — the feature ships
  off), `ci.poll_interval` (`60s`, minimum `10s`), `ci.watch_timeout` (`90m`,
  minimum `1m`, an absolute deadline anchored at push so a restart cannot
  extend it), and `ci.max_attempts` (`0` = retry off, capped at `10`), which
  drives the bounded retry above. Each
  has an env override (`DRYDOCK_CI_WATCH=1`, `DRYDOCK_CI_POLL_INTERVAL`,
  `DRYDOCK_CI_WATCH_TIMEOUT`, `DRYDOCK_CI_MAX_ATTEMPTS`) and a row in
  `drydock policy explain`, so which layer armed the watch is always visible.
  Out-of-range values are rejected at load rather than silently clamped, and
  the `max_attempts` ceiling exists because a retry chain's worst case is
  `max_attempts × task_budget_usd` — each attempt mints a fresh full budget.
  **Threat model updated, not weakened:** enabling `ci.watch` puts the host's
  `gh` credential on a **timer** where it was previously only
  operator-initiated (N5), and CI check names join staged files and issue text
  as a documented untrusted *source* (N2). No new containment claim is made
  and no exception is taken; B1 fetches no CI log text at all.

- **Durable task queue (orchestration increment A).** `drydock queue add`
  takes the same flags as `submit` but returns the moment brokerd has
  durably persisted the task; it runs unattended when a concurrency slot
  frees. Items move through a broker-enforced state machine
  (`queued → preparing → running → verifying → awaiting_review →
  completed | dead_letter`, plus a `queued → dead_letter` edge with exactly
  one writer — see the CI-retry entry above — and with `cancelled` reachable
  from every non-terminal state) and are stored as atomically-written, 0600,
  symlink-refusing files in the audit dir, so the queue **survives brokerd
  restarts**: at boot, still-`queued` items re-dispatch, items parked at the
  diff-approval gate resume, and an item that was mid-flight when the daemon
  died is dead-lettered — never run a second time. The dispatcher shares
  `max_concurrent` with synchronous submits (one pool, never over-committed)
  and **parks** a *human-submitted* vendor item while that vendor's aggregate
  spend cap is exhausted (parked ≠ failed; a park is not an attempt) — a
  broker-initiated CI retry is dropped instead, per the entry above. A dispatched item
  runs the exact synchronous lifecycle — setup profiles, verification, diff
  policy, and the approval gate unless `--auto-approve`. Surfaces:
  `drydock queue add/list/cancel` + `POST/GET /queue`,
  `POST /queue/cancel/{id}`; the terminal metrics row records the enqueue →
  dispatch wait as `stage_ms.queued` (omitted for synchronous tasks, whose
  rows are byte-shape unchanged); `drydock stats` gains a queue-wait
  p50/p95 line and the `completed`/`dead_letter` outcome vocabulary, which
  `drydock tasks` and the web UI render too.

- **Persistent per-repo dependency cache (opt-in).** `cache: true` in a
  repo's execution profile reuses setup's dependency downloads (npm, Go
  modules, pip, cargo) across tasks: a host-side, content-addressed store
  under `cache_root` (bounded by `cache_quota_gb`, default 20 GiB,
  LRU-evicted; env `DRYDOCK_CACHE_ROOT` / `DRYDOCK_CACHE_QUOTA_GB`) is
  mounted at `/deps`, with entries keyed by repo identity + lockfile
  digests + setup commands + sandbox image + architecture — no cross-repo
  sharing ever, and no cache at all for a repo without a lockfile (an
  unpinned dependency set has no stable content identity to cache under;
  the brief records `disabled: no lockfile`). Setup VMs mount the entry
  **read-write**; the agent VM's mount is strictly **read-only**, so
  agent-run code can never poison dependencies later tasks consume — the
  threat model's A7 claim (agent-writable state never persists) is
  unchanged, now stated with the cache as its one documented,
  trusted-setup-produced exception and enforced by the new
  `TestRedteam_A7Cache_*` red-team tests (`make redteam-vm`). Eviction
  never removes an entry a live task has mounted, and `cache_quota_gb`
  joins the [security-defaults table](https://sricola.github.io/drydock/docs/security-defaults.html).
  The trust brief's setup block records the evidence
  (`hit`/`miss`/`disabled: no lockfile` + the entry's key prefix) and
  `drydock inspect` renders it as a `cache` line. The cache is a speedup,
  never a correctness dependency: a miss re-fetches through squid; note
  that an agent installing a package the cache lacks may hit the
  read-only `/deps` (keep dependency installs in setup, or leave caching
  off for such repos).

- **GitHub issue ingestion + a plan-mode scope gate.** `drydock submit
  --issue <url>` turns a GitHub issue into the task instruction: the issue
  is fetched **host-side** via your authenticated `gh` (curated environment,
  never a full env copy; strict URL parse, so a hostile URL is rejected
  before any argv is built), `--repo` is derived from the URL when omitted,
  and only the issue *text* — title, labels, and a rune-safe size-capped
  body — enters the prompt; your GitHub credentials never reach the sandbox.
  Adding `--plan` (or the sugar `drydock plan <issue-url>`) makes it a
  **plan-only run**: the sandbox gets `DRYDOCK_MODE=plan` and the image
  entrypoint prepends a plan preamble steering the agent to write
  `/work/.task/plan.md` instead of code, while the broker enforces the hard
  guarantee — a plan run terminates with the new outcome `planned` *before*
  the verify/push logic, so nothing can be pushed regardless of what the
  agent left in the tree. The plan is captured (symlink-refusing,
  size-capped) as `~/.drydock/audit/<id>.plan.md`; `drydock inspect` renders
  the issue provenance, the planned indicator, and the plan inline;
  `drydock tasks`/`stats` count `planned` as a first-class outcome; and the
  persisted invocation carries `plan_only`/`issue_url`, so `drydock retry`
  of a plan run re-plans instead of silently escalating to an implementing
  run. Issue text is untrusted input (anyone who can file an issue is
  writing prompt text — see the threat model's N2); the human plan+diff
  gates remain the boundary. The workflow: `--issue … --plan` → review →
  `--issue …` to implement (see
  [Submitting tasks](https://sricola.github.io/drydock/docs/submitting-tasks.html)).

- **Execution profiles — a host-configured setup phase that fails closed
  before any model spend.** A new optional `profiles.repos` block in
  `config.yaml` gives a repository `setup` commands (dependency install,
  code generation) plus `readiness` commands, run in fresh sandbox VMs
  against the task's **live** work tree **before the agent starts** — what
  setup installs is exactly what the agent sees. The commands live in host
  config only (the sandboxed agent can never edit its own setup phase), each
  is a self-contained argv (shell state does not carry between commands),
  and the per-task stage is wiped at cleanup (repos can opt into the
  persistent dependency cache above). Setup VMs get squid-only egress — allowlisted registries are
  reachable, but no model gateway and **no credentials**: any command
  failure, timeout, or infra error ends the task with the new outcome
  `setup_failed` while the agent VM has never booted and no API bearer was
  ever injected into any VM, so a broken workspace costs $0 of model budget.
  Verdicts are the exit codes the broker observes (nothing a command prints
  can flip a status), recorded per command in the trust brief's new setup
  block — rendered by `drydock inspect` and the web UI's brief panel above
  verification — with the combined output kept display-only at
  `~/.drydock/audit/<id>.setup.log`. The phase surfaces as a `setting_up`
  stage in the submit stream, `drydock status`, and the web UI; `drydock
  stats` counts `setup_failed` as a first-class outcome; each command is
  bounded by `profiles.repos.*.timeout` (default 10m, on the
  [security-defaults table](https://sricola.github.io/drydock/docs/security-defaults.html));
  and the metrics row's `stage_ms` now records `.setup` with `preparing`
  ending where setup begins, so the stages partition the task's wall-clock
  without double-counting.

- **`drydock doctor --repo <path>` — repo preflight with no API spend and no
  container boot.** Diagnoses one local repo for drydock readiness before the
  first submit: repo size against the effective per-task stage cap (the
  tighter of `stage_quota_gb` and the stage guard's defaults), language
  toolchains (from dependency manifests) against what the sandbox image ships
  (node/python/go), package-registry hosts — ecosystem defaults plus any
  custom registry assigned in `.npmrc`/`.yarnrc`/`Cargo.toml`/`pip.conf` —
  against the egress allowlist, and repo files that already collide with
  `diff_policy.blocked_paths`/`second_look_paths`. Warnings (missing
  toolchain, unallowlisted registry, second-look collisions) are advisory and
  exit 0; blockers (repo over the stage cap, files matching `blocked_paths`)
  exit 1. The scan is one bounded walk that never follows symlinks (registry
  config files are opened `O_NOFOLLOW` too) and never echoes file contents —
  registry configs can hold auth tokens; only `url.Parse`-extracted hostnames
  appear in the output. Plain `drydock doctor` is unchanged.

- **`diff_policy` — host-side caps, blocked paths, and second-look
  acknowledgments for the diff a task proposes.** A new optional
  `config.yaml` block, enforced by brokerd against the broker-computed diff
  facts (the same analysis the trust brief reports; nothing the agent prints
  can influence it). `max_files_changed` / `max_lines_changed` / a
  `blocked_paths` glob list fail a violating task **closed** with outcome
  `policy_blocked` before it ever reaches the approval gate — `--auto-approve`
  does not bypass them, a diff too large to fully analyze fails closed when a
  content-based policy is configured, and the captured diff plus trust brief
  are preserved for inspection (`drydock tasks` / `stats` show it as
  **policy blocked**). A separate `second_look_paths` list keeps the diff
  reviewable but requires the approver to explicitly acknowledge each touched
  risk-flag category: brokerd refuses any approve whose
  `{"acknowledge":[...]}` body doesn't cover the requirement (HTTP 422 naming
  the missing categories; the task stays pending), a brokerd restart
  recomputes the requirement from the persisted diff, and denies never need
  acknowledgment. The requirement surfaces everywhere an approval does:
  `drydock pending` marks such tasks `SECOND-LOOK[...]`, `drydock review`
  prompts per category before approving (any refusal denies), `drydock
  approve <id> --acknowledge <category>` (repeatable; alias `--ack`) is the
  explicit path — a refused approve prints the missing categories and the
  corrected command — and the web UI's review overlay renders a checkbox per
  required category, keeping the Approve button disabled until all are
  checked. Glob patterns are `**`-aware repo-relative paths, validated at
  config load (`dir/**`, not `dir/`), and the block participates in
  `drydock policy explain`'s divergence check.

- **Trust brief panel in the web UI.** Opening a review now renders the
  task's trust brief above the diff — the same broker-observed evidence
  `drydock inspect` prints: repo and base commit, runtime, effective policy,
  egress rules, broker-metered spend, the diff summary with its risk FLAG
  rows, and the verification block when configured. Served read-only at
  `GET /api/brief/{id}` through the same loopback-only, token-gated boundary
  as the diff (symlink-refusing open, no directory traversal). Every field is
  sanitized before display (control and bidirectional-override characters
  stripped, length-capped) and rendered via `textContent` only; tasks
  recorded before briefs existed show a muted "no trust brief recorded" line
  and the diff still loads. As defense-in-depth behind the loopback bind and
  bearer token, every UI response now also carries a strict
  `Content-Security-Policy` (`default-src 'self'`; no inline script, no
  external loads, no framing), `X-Content-Type-Options: nosniff`, and
  `X-Frame-Options: DENY`.

- **`drydock policy explain` — where did this setting come from, and is the
  daemon running it?** A new read-only subcommand prints every config field's
  effective value with the layer that supplied it (`default`, `config.yaml`,
  or `env:VAR`, resolved per field with the exact guards the loader uses, so
  a set-but-invalid env var is correctly attributed to the layer beneath it).
  When brokerd is reachable, the CLI compares its resolution against the
  policy the daemon resolved at boot (served by a new read-only
  `GET /admin/policy`): `in sync`, or `DIVERGENT` with the differing fields
  listed — the "I edited config.yaml but the daemon never restarted" failure
  made visible. With brokerd down, a `LIVE POLICY UNVERIFIED` banner replaces
  the verdict rather than implying one. `--json` emits the machine-readable
  form. The connection settings (`broker.socket`/`broker.addr`) are shown in
  the table but excluded from the divergence verdict: they describe how the
  CLI reaches the daemon, not policy the daemon enforces, so dialing a remote
  daemon via `BROKER_ADDR` doesn't read as drift. Field values are
  rendered as summaries (never raw secrets), and the endpoint recomputes
  nothing at request time.

- **Independent verification stage.** A new opt-in `verify.repos` config
  block runs host-approved commands (build, tests, lint) against a sealed
  export of the agent's staged tree before the approval gate — each command
  in a fresh VM with no credentials and a deny-all firewall pin (loopback
  only, installed by root before the privilege drop, so repo code cannot
  remove it). Verdicts are the exit codes the broker observes; nothing the
  verify VM prints can influence a status, and at push time the staged tree
  is re-hashed and must still equal the verified tree or the push fails
  closed. `required: false` (default) is advisory; `required: true` blocks
  the push on any status but `passed` — a timeout or infra error reads
  `inconclusive`, never passed, and blocks too (outcome `verify_failed`).
  Each command is bounded by a per-command timeout (default 10m); verifier
  output is capped at the same limit as the per-task host output cap, from
  its own separate budget. A `verify_failed` task persists the same trust
  brief and audit `.diff` evidence a gated task would, so the `drydock
  inspect` hint always has something to show.

- **Verification surfaced to operators.** The trust brief gains a populated
  verification block, and `drydock inspect` renders it: overall status, the
  VM's capability posture (network denied, no credentials), the verified
  tree hash, per-command `argv → exit N (duration)` lines, and the
  display-only `<id>.verify.log` path. The submit stream, `drydock status`,
  and the web UI board all show the new `verifying` stage (with Kill
  available), the CLI reports `verify_failed` terminals with an inspect
  hint, and the audit metrics row records `stage_ms.verifying`. Operator
  docs cover the config block, the advisory/required semantics, and restart
  behavior (no mid-verify resume; re-verify only via `drydock retry`); the
  security-defaults page gains the verify-timeout row.

### Fixed

- **Operator-facing spend is now broker-observed everywhere (threat model N4).**
  `audit.TotalCost` read the last `total_cost_usd` in a task's audit trace
  **without filtering `src`**, and the agent's stdout is copied into that trace
  verbatim — so a number an in-VM agent printed could reach an operator as
  though the broker had measured it. Three consequences, all fixed:

  - The web UI's **push-approval gate** rendered exactly that untrusted figure
    beside the Approve button — it fetched the task's log stream and took the
    last `total_cost_usd` on any line. It now shows the broker's own gateway
    lease metering, published onto the task state at gate entry so the page has
    nothing to parse, and distinguishes "no USD metering on this lane" from
    "not measured" instead of printing a bare `$0.00`.
  - `drydock stats` summed it into the **spend total**. The total is now
    broker-metered only; a figure that exists solely because an agent reported
    it is listed separately and labelled, never dropped silently.
  - Worst of the three: six of the broker's own synthetic terminal rows
    (`planned`, `policy_blocked`, `push_failed`, `verify_failed`,
    `setup_failed`, and the resumed terminal)
    copied that number into a row stamped `src:"broker"` — **laundering** it
    into the one field the aggregate-cap restart seed, boot reconciliation and
    every downstream `src=="broker"` filter trust. Every one of those rows now
    carries the gateway lease's figure, and `audit.TotalCost` has been removed
    outright so the shortcut cannot come back.

  `drydock tasks` and the web UI history table likewise read the broker row, and
  mark an agent-reported cost as such rather than presenting it as measured.
  A structural test now pins the class shut: every `src:"broker"` result row in
  `internal/broker` is parsed out of the package and its `total_cost_usd` must
  come from the gateway lease, so a new terminal path cannot reintroduce the
  laundering.

## v0.6.7 (2026-07-28)

A patch release closing the findings from the v0.6.5 release QA pass. Three
command-line and UI fixes to argument handling, terminal outcome taxonomy for
denied and cancelled tasks, and web UI render stability and polish. One
operator-visible change on upgrade: `drydock tasks`, `drydock stats`, and the
web UI history now show denied and cancelled tasks with their real outcome
instead of rendering them as successes or generic errors.

### Security

- **argv[0] is now provably constant in the PR/MR shell-out.** `runCLI`
  (internal/remote) took the vendor CLI name inside the same variadic argv
  slice as user-influenced values (PR title/body), so static analysis could
  not prove the executable path was never attacker-influenced. The CLI name
  is now a separate parameter that every adapter (github.go, gitlab.go,
  gitea.go) passes as a compile-time literal ("gh"/"glab"/"tea"); every
  user-influenced value still only ever appears as the value following a
  flag, never as a bare positional. Closes the CodeQL go/command-injection
  alert on this shell-out. No behavior change.

### Fixed

- **Shutdown-parked tasks no longer read as cancelled while awaiting approval
  after a restart.** A task at the diff-approval gate when brokerd shuts down
  is only paused, not cancelled: it resumes alive at the next boot. The
  gateShutdown branch previously recorded outcome=cancelled on the metrics
  row, so `drydock tasks` and the web UI History showed it as terminal for
  the whole time it sat parked. It now records no outcome at all, so readers
  fall back to the result row, matching how a resumed approval-timeout is
  already handled. The gate-cause-to-outcome mapping that used to be
  duplicated (and disagree) between the live push path and the resume path
  is now a single shared, unit-tested helper.

- **Resumed gate tasks never flash as running.** Boot reconciliation used to
  register a resumed awaiting-approval task as StageRunning, then correct it
  to StagePending under a second lock acquisition, and the HTTP admin
  listener is already serving during reconciliation. A reader could observe
  the wrong stage in between. Registration now records the correct initial
  stage in one critical section, so a resumed task is never observable as
  running.

- **The history endpoint halves its audit tail I/O.** `drydock tasks`
  (summarize) and the web UI's `/api/history` each independently tail-read
  an audit file's result row and its metrics row. Both now do a single tail
  read and scan once for both rows.

- **Terminal-outcome and confirm-key hardening.** A non-empty broker
  metrics-row outcome (denied, cancelled, error) now consistently refines a
  result row's coarse classification, but never overrides "push_failed",
  the one key that already carries more specific push-time information (a
  push_failed result row now always stays push_failed). The web UI's
  egress-deny and push-deny buttons on the board card use distinct confirm
  keys, so arming one no longer shares state with the other. Documented that
  `brokerd --version` / `--help` print and exit rather than booting the
  daemon.

- **brokerd now handles --version, --help, and unknown arguments (#199).**
  `brokerd --version` and `brokerd --help` print their usual text and exit 0;
  unknown arguments print an error with usage and exit 2, instead of silently
  booting the daemon. `__squid-authhelper` is preserved as a special case to
  avoid breaking the squid helper invocation. Added argvAction function for
  testable argv dispatch with unit coverage.

- **Denied and cancelled tasks now show their real outcome in the UI and CLI
  (#200).** The broker-authored terminal metrics row now carries an outcome
  field (pushed, denied, cancelled, push_failed, error, no_diff), so `drydock
  tasks`, `drydock stats`, and the web UI history correctly classify tasks
  denied at the approval gate or killed mid-run (previously both rendered as
  "ok" or "error" indistinguishably). Also fixes the "Just finished" icon
  defaulting to a checkmark for unrecognized outcomes (interrupted, a broker
  crash recovery line, rendered as a success). `healthz` now counts a resumed
  awaiting-approval task as pending_approval instead of running. `drydock kill`
  on an unknown task id exits 1 instead of 0.

- **The web UI confirm button armature now survives board repaints (#201).**
  The Deny/Kill button's two-press confirm state persists through polling
  rebuilds for its full 3-second window by keying the armed state on task id
  and action in a module-level map with expiry and separate timers. The card
  and review overlay buttons now use distinct keys, so opening Review while the
  card's Deny is armed no longer defeats the two-press confirm on the overlay.
  Also included: the UI serves a favicon at /favicon-32.png; board log polling
  no longer console-errors on expected 404s after submit; the history DUR column
  humanizes durations like the CLI (39m12s, not 2352s).

## v0.6.6 (2026-07-28)

An operator-hardening release: push credentials are now proven at submit
time instead of discovered at end-of-task push time, and host-side git can
never prompt or hang. Two operator-visible changes on upgrade: a task
against a repo you cannot push to fails at submit (deliberately, with no
opt-out; the diff-without-push workflow against read-only remotes no
longer runs), and SSH runs in BatchMode unless you have your own
`GIT_SSH_COMMAND` or `core.sshCommand` transport, which is respected.

### Added

- **Push-credential preflight at submit.** Before the sandbox boots,
  drydock proves write auth against the task's repo with a
  `git push --dry-run` on the exact branch it would later push; a
  failure ends the task at accept time with a classified reason
  (`push preflight failed (auth): ...`) and an actionable hint, so a
  missing credential can no longer cost a full agent run. There is no
  opt-out: a task against a repo you cannot push to now fails at
  submit. `drydock doctor` gains a generic push-credential check.

- **Release QA gate for the installed artifact.** `make release-qa`
  (`tests/release/qa.sh`) runs a black-box pass over the installed
  `drydock`/`brokerd` as an operator would drive them: CLI contract and
  exit codes, doctor, the live red team, brokerd and squid lifecycle,
  and the web UI auth boundary, with opt-in `LIVE=`/`DAEMON=1` phases
  for real approve/deny/kill task runs and the launchd round trip. It
  complements `make release-preflight` (which proves the source tree)
  and slots in after the brew bump; docs at `site/docs/release-qa.md`
  include the manual browser checklist.

### Changed

- **Host-side git is now strictly non-interactive.** Every git
  invocation (clone, diff, push, and the env handed to gh/glab) runs
  with terminal prompts disabled and, unless `GIT_SSH_COMMAND` is set
  by the operator, SSH in BatchMode; a credential gap fails in
  milliseconds instead of prompting on stdin or wedging under launchd.

### Fixed

- **Claude Code's Bash tool works inside the sandbox again (#198).**
  The claude and codex entrypoint branches leaked root's `HOME=/root`
  through the setpriv privilege drop, so Claude Code's per-session
  `~/.claude/session-env` mkdir failed with EACCES on every shell
  command; the agent limped along on read/edit tools while builds,
  tests, and installs silently broke. Both branches now run with
  `HOME=/home/agent` (as gemini and opencode already did), and
  `drydock doctor` gains an "agent HOME writable" check that probes
  through the real privilege drop, so a regression fails doctor
  instead of degrading tasks. Run `drydock init` after upgrading to
  rebuild the image.

## v0.6.5 (2026-07-27)

A feature release landing roadmap item 4.7 (observability): per-task run
metrics recorded in the audit stream and a `drydock stats` command that
aggregates them. Fully backward compatible; pre-upgrade audit files keep
working, with timing columns reported as absent. Two cosmetic
operator-visible changes: `drydock tasks` shows `ok (N turns)` for new runs
now that `num_turns` carries the real gateway-admitted request count, and
submit-stream events carry an RFC 3339 `ts` field.

### Added

- **Run metrics + `drydock stats` (roadmap 4.7).** Each task's audit stream
  now ends with a broker-authored `{"type":"metrics"}` row (stage durations,
  egress/approval gate waits, admitted request count, diff size, spend,
  egress-widen outcome), stream events carry an RFC 3339 `ts`, and the
  result row's `num_turns` now reports the gateway-admitted request count.
  `drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]`
  aggregates the audit dir into outcome rates, duration and gate-wait
  percentiles, spend, and egress-widen frequency, with graceful fallback for
  pre-upgrade audit files.

## v0.6.4 (2026-07-26)

A security-hardening release closing the two residual findings from the July
2026 red-team review (F-04, F-09). The review itself is now committed under
docs/audits/ with a closure addendum mapping every finding to its fix. No new
features. One operator-visible change on upgrade: each task's stage dir is
bound by a hard disk quota, `stage_quota_gb` (default 8 GiB); raise it for
tasks that legitimately stage more.

### Security

- **The sandbox image's apt and npm dependency graphs are date-pinned
  (F-09).** apt installs resolve against a dated snapshot.debian.org
  archive and every npm resolution is cut at a pinned --before date, so
  two builds of the same commit produce the same dependency set; the npm
  --before date moves deliberately via the weekly bump lane, while the
  Debian snapshot is bumped by hand.
- **Each task's stage dir is now a size-capped APFS sparse image, making the
  /work disk bound hard (F-04).** A hostile in-VM agent writing through
  /work previously raced a 2-second polling guard and could overshoot by
  its write rate; it now hits the image's filesystem wall (ENOSPC) at
  `stage_quota_gb` (default 8 GiB). The polling guard remains as the
  early-cancel layer. VM-verified by the new A8 red-team test.

### Docs

- The July 2026 red-team review is committed at docs/audits/, with a closure addendum mapping every finding to its fix.

## v0.6.3 (2026-07-23)

A follow-up security-hardening release closing the residual items from the
v0.6.2 red-team verification pass (V-01, F-02, F-03, F-05, F-07, F-08, V-02).
Every fix ships with an exploit-first regression test, and the whole branch was
independently reviewed, mutation-tested, and run through the host and VM-backed
red-team suites. No new features. The project status graduates from alpha to
beta: the containment is now continuously red-teamed and adversarially
reviewed, while it stays pre-1.0 with no third-party security audit. Two
operator-visible default changes on upgrade: `task_max_inflight` defaults to 1 (concurrent gateway requests per
task serialize; raise it or set `DRYDOCK_TASK_MAX_INFLIGHT` if a
subagent-parallel task stalls on 429s), and `task_max_requests: 0` now falls
closed to a built-in 1000-request cap instead of unlimited.

### Security

- **An oversize review diff now fails the task closed instead of being
  truncated (V-01, High).** A staged diff over 32 MiB used to be truncated
  and the task allowed to continue with a partial review; the operator could
  approve a diff that hid content past the cutoff. Staging now fails the task
  closed when the diff exceeds the cap.
- **Both config loaders reject a trailing YAML document (F-08 residual).** A
  config file with a second `---`-delimited document after the first was
  silently ignored by one loader and honored by the other, letting a stray or
  injected second document change behavior unnoticed; both loaders now reject
  it.
- **The gateway caps concurrent in-flight requests per task lease (F-05/F-02,
  High).** `task_max_inflight` (default 1) serializes each task's admitted
  requests, bounding budget overshoot to at most `task_max_inflight`
  concurrent requests instead of an unbounded number a hostile in-VM agent
  could otherwise fire before any of them completed.
- **The gateway blocks the Batches API and matches inference routes
  exactly (F-03 residual).** The per-vendor route allowlist previously
  matched by prefix and missed the Batches API; routes are now matched
  exactly against the pinned CLI's inference routes.
- **`task_max_requests: 0` falls closed to a built-in default cap (F-02
  defense in depth).** Leaving the per-task request cap unset used to mean
  no cap in every auth mode; it now falls closed to a built-in default of
  1000 requests per task, set explicitly to change the bound.
- **The broker authors a terminal result on every sandbox exit path (F-07
  residual).** Some `runSandbox` exit paths could leave a task without a
  broker-authored terminal result; every exit path now produces one.
- **The release workflow verifies a preflight receipt before tagging (V-02).**
  `tag-release` and `release.yml` now enforce a preflight receipt, closing a
  path to an accidental release bypassing the pre-release checks.
- **The gateway releases a request's budget reservation on every exit path,
  not only on metering (review).** When `max_request_cost_usd` was set, a
  request whose upstream round trip failed before a response body skipped
  metering and leaked its reservation, so a flaky upstream could make a lease
  deny its own budget despite near-zero real spend; the reservation is now
  released alongside the in-flight slot on success, credential failure, and
  upstream error alike.

## v0.6.2 (2026-07-12)

A security-hardening release closing the findings from a red-team review of
v0.6.1. Every code fix ships with an exploit-first regression test; the
containment fixes were independently reviewed. No new features.

### Security

- **Hostile repositories can no longer overwrite host files (F-01, Critical).**
  A staged repo that committed `.task` or `.task/prompt.txt` as a symlink could
  make the trusted broker truncate an arbitrary operator-writable file (e.g.
  `~/.zshrc`, `~/.ssh/authorized_keys`) before the VM even booted. WriteTaskFiles
  now refuses a symlinked `.task` and opens the prompt with `O_NOFOLLOW`; two A3
  red-team cases reproduce and gate it.

- **The gateway credential is scoped to inference routes (F-03, High).** After
  bearer admission the gateway forwarded any path/method to the vendor origin
  with the real key injected, so a task could drive file upload/download/delete,
  fine-tuning, and other control-plane routes that response metering never sees.
  A per-vendor route allowlist (segment-boundary matched) now returns a
  gateway-local 403 for anything outside the pinned CLI's inference routes,
  without contacting upstream; path traversal (literal and encoded `..`) is
  rejected.

- **The optional TCP broker resists DNS rebinding (F-06, High).** When
  `broker.addr` is set, the listener now rejects any request whose Host is not
  literal loopback or whose Origin (when present) is not loopback, closing the
  browser-driven submit/approve/kill path. This is not authentication (a
  non-browser local process still has access; SECURITY.md documents the scope).

- **A task can no longer exhaust host resources (F-04, High).** Bounded task
  stdout+stderr (cancels the task past a 256 MiB budget), a streaming-capped diff
  capture (bounds broker memory), and a per-task `/work` guard (cancels on a
  4 GiB / 200k-file stage or when host free space drops below a floor), plus a
  submit-time host-free-space preflight.

- **Broker-authored, unforgeable cost accounting (F-07, Medium).** The broker
  now writes a `src:"broker"` terminal result for every agent from the
  gateway-metered spend; the displayed cost/outcome and the aggregate-cap seed
  trust only that record, so a compromised agent CLI can no longer forge a cheap
  `total_cost_usd` to understate the rolling cap after a restart.

- **Config typos fail closed (F-08, Medium).** Both `config.yaml` and
  `egress.yaml` now decode with `KnownFields(true)`, so a misspelled security
  control (`aggregate_budget_usd`, `max_request_cost_usd`, ...) is a hard startup
  error naming the field instead of a silent no-op to a weaker default. **Upgrade
  note:** a config carrying a since-removed key now errors with a clear message;
  drop the named key.

- **Bounded gateway buffers (F-05, Low-Med).** The proxy request body is capped
  (16 MiB) and the usage-metering buffers are bounded (1 MiB each), so an in-VM
  agent cannot pressure gateway memory with a giant or newline-free body.

- **Sandbox supply-chain hardening (F-09, Medium).** The Go toolchain tarball is
  verified against its pinned per-arch SHA-256 before the root extraction, and
  every GitHub Action is pinned to a full commit SHA.

- **Concurrent per-task budget overshoot documented (F-02, Medium).** With
  `max_request_cost_usd` unset (default), the gateway meters post-hoc, so a
  hostile agent firing concurrent requests can overshoot `task_budget_usd` by
  roughly in-flight-count times per-request cost (bounded by the per-task cap and
  the built-in 1,000-request backstop). Set `max_request_cost_usd` to bound the
  concurrent case precisely; the threat model and config now describe this.

### Documentation

- **Operator claims reconciled with the code (F-10).** Corrected the
  `task_budget_usd` "hard ceiling" wording, the stale `no-aggregate-cap` daemon
  notes, the `gosu` -> `setpriv` privilege-drop description, and the subscription
  request-cap default in the threat model.

## v0.6.1 (2026-07-12)

### Documentation

- **Validated against Apple `container` 1.1.0.** The runtime was upgraded from
  1.0.0 to 1.1.0 and the sandbox image rebuilt; `make redteam-vm` (VM-backed
  A1/A2/A7 containment) passes, so drydock's `container run` path is confirmed
  unchanged. No code change was needed: the version gate keys on the major
  (`supportedContainerMajor = "1"`), which already accepts all 1.x. README and
  the gate comment updated to record the validation.

- **Egress doc now covers the plain-HTTP vs HTTPS-CONNECT edge (ROADMAP
  4.10 landed).** The "Plain HTTP vs HTTPS (the CONNECT edge)" subsection
  documents what squid actually enforces: CONNECT tunnels are locked to port
  443 and trust the allowlisted host at the host level (any path, no TLS
  inspection); plain HTTP is denied by default because the default allowlist
  is 443-only (fail-closed); widening a host to port 80 allows cleartext HTTP
  through squid; a widened non-443 port permits plain-HTTP traffic but not a
  CONNECT tunnel; enforcement granularity is host and port, not path. The
  section also restates the SSRF guard scope and the nft firewall guarantee
  (no bypass path). No silent gaps.

### Added

- **Scheduled agent-CLI bump automation (ROADMAP 4.6).** A weekly GitHub
  Actions workflow (`agent-cli-bump.yml`) fetches the latest npm version for
  each pinned CLI (`@anthropic-ai/claude-code`, `@openai/codex`,
  `opencode-ai`, `@google/gemini-cli`) and proposes a bump PR when any is
  strictly newer than the current Dockerfile pin. The PR body notes that a
  GITHUB_TOKEN-authored PR does not auto-trigger CI, and that the reviewer
  must run the red-team gate (`make redteam` and `make redteam-vm`) before
  merging. A new `cmd/cli-bump` tool implements the version-check and
  Dockerfile-rewrite logic, which is unit-tested independently of the
  workflow.

- **In-flight reservation: per-request cost ceiling (ROADMAP 4.15).** Closes
  the concurrent-bypass hole from #139: previously N pipelined requests could
  all admit at spend=0, letting concurrent tasks overshoot the budget before
  any stream completed. A new config field `max_request_cost_usd` (env
  `DRYDOCK_MAX_REQUEST_COST_USD`, default `0`) sets a worst-case USD amount
  reserved against the lease budget while each request is in flight; the
  reservation is released and reconciled with actual metered cost when the
  stream ends. `0` (default) disables the reservation and keeps the existing
  post-hoc metering behavior, so the change is fully backward compatible.

- **Sandbox image picks up Debian security fixes on rebuild (ROADMAP 4.13).**
  `apt-get upgrade -y` now runs inside the Dockerfile before the package
  install block, so every image rebuild (whether triggered by `drydock setup`
  or the daily `image-scan` CI job) pulls in current Debian point-release
  security fixes without waiting for a base-image digest bump or a lucky
  manual rebuild. The base image remains digest-pinned as a reproducible
  starting point (bumped deliberately, per the Phase 2.5 convention); the
  tradeoff accepted is that exact bit-reproducibility of the installed package
  set is given up in favour of security currency, which is appropriate for a
  locally-built sandbox runtime rather than a user-verified released artifact.
  The daily `image-scan.yml` CI (grype + `cmd/cve-gate` against
  `image/cve-allowlist.yaml`) already rebuilds and gates on CVEs; `apt-get
  upgrade` makes each rebuild current. The anchor image (`image/anchor/`) is
  unchanged: it ships `FROM scratch` with a single static Go binary and has
  nothing to upgrade.

- **Resume awaiting-approval tasks across restart (ROADMAP 4.14).** A task
  blocked at the diff-approval gate now survives a brokerd restart. A durable
  gate marker (`<id>.gate.json`) is written when a task enters the push gate;
  the stage dir's cleanup is skipped on graceful shutdown and the marker is left
  on disk; boot reconciliation skips reaping any stage dir whose id has a marker.
  At boot, brokerd re-registers each marked task as pending and resumes it:
  `drydock approve <id>` after a restart pushes the surviving branch with no
  agent re-run and no additional spend (push auth via `PushEnv`, no model
  credential needed). If the stage did not survive (a task from before this
  feature, or a manually removed stage), brokerd appends an `interrupted`
  terminal line to the audit and preserves the `.diff` for `drydock retry <id>`.
  A false `ok` is never written for a task that did not push. The gate marker is
  idempotent: if brokerd shuts down again while a resumed task is re-awaiting,
  the marker persists for the next boot. Scope: the push (diff-approval) gate;
  the egress-widen gate is unaffected (no completed work to preserve there).

- **Push partial-failure recovery (ROADMAP 4.2).** The approved-diff push now
  reports one clean terminal outcome and recovers from recoverable failures.
  Push errors are classified into five reasons: `transient` (network), `auth`,
  `protected`, `non_fast_forward`, and `unknown`. Transient failures are retried
  with exponential backoff (up to `push_max_retries` attempts, default `3`;
  base delay `push_retry_backoff`, default `1s`); branch-name collisions are
  retried to a fresh remote name (up to `push_fresh_branch_tries` alternates,
  default `2`); auth/protected/unknown stop immediately. All three config fields
  have env overrides (`DRYDOCK_PUSH_MAX_RETRIES`, `DRYDOCK_PUSH_RETRY_BACKOFF`,
  `DRYDOCK_PUSH_FRESH_BRANCH_TRIES`) and setting any to `0` disables that
  recovery path. A single-ref push is atomic, so `push_failed` guarantees
  nothing landed on the remote; the captured diff is preserved in the audit for
  every outcome. `drydock tasks` now shows "push failed" for a failed push
  (previously it showed the agent outcome, e.g. "ok", leaving the state
  ambiguous). `push_failed` is retry-safe: `drydock retry <id>` re-runs under a
  new id and never collides with the failed attempt.

- **Aggregate budget cap (`aggregate_budget_usd` / `aggregate_window`).**
  Two new config fields bound cross-task USD spend per `api_key` provider:
  `aggregate_budget_usd` (env `DRYDOCK_AGGREGATE_BUDGET_USD`, default `0`
  = disabled) sets a USD ceiling per vendor; `aggregate_window` (env
  `DRYDOCK_AGGREGATE_WINDOW`, default `24h`) sets the rolling window length,
  where `0` means total since brokerd boot (no time decay, resets on restart).
  Enforcement is two-layer: the gateway's `admit()` rejects requests with HTTP
  402 once a vendor's windowed spend reaches the cap (halting an already-running
  task on its next request), and the broker pre-checks at `POST /tasks` so an
  over-cap submission fails cleanly at submit time rather than starting a doomed
  task. In rolling mode, the ledger is seeded at boot from audit files within the
  window, so the cap survives a brokerd restart. The cap applies to `api_key`-mode
  vendors only; subscription mode is out of scope (bounded per-task by
  `task_max_requests`). Closes ROADMAP 4.3.

## v0.6.0 (2026-07-09)

A security-hardening release from a full production-readiness review of the
code and product. Every containment claim that was previously "enforced by
default kernel behavior" is now explicitly enforced *and* tested.

### Security

- **The agent can no longer regain `CAP_NET_ADMIN` after the privilege drop,
  now enforced and tested.** The sandbox dropped to the `agent` user with
  `gosu`, which changes UID only, leaving the capability bounding set intact; a
  stray SUID binary or config regression could have let the agent flush the nft
  egress pin. The drop now uses `setpriv` with `--no-new-privs` and an emptied
  `--bounding-set`/`--inh-caps`, all SUID/SGID bits are stripped from the image,
  and the in-VM firewall is applied as one atomic `nft -f` transaction with
  input/forward default-drop (was output-only). A new red-team test runs *as the
  dropped agent* and asserts `nft flush` returns EPERM and egress stays blocked,
  the load-bearing half of the A2 claim, previously untested. `gosu` is gone,
  which also retires its 35-entry go1.19 CVE-allowlist cluster.
- **Subscription/priceless lanes are no longer unbounded.** With no USD budget
  (subscription auth, or an `openai_compat` lane with no prices) the only
  runaway control was `task_max_requests`, which defaults to unlimited, so a
  looping task could drain a real Claude/ChatGPT subscription. A 0 there now
  fails closed to a built-in per-task request cap. Negative `openai_compat`
  prices (which disabled the USD budget entirely) are rejected at config load.
- **SSRF guard on the egress proxy.** squid resolves allowlist hostnames on the
  host, outside the VM's nft pin; an allowlisted or widened name pointing at a
  private/loopback/link-local/metadata IP (or via DNS rebinding) could reach
  host-local services or the LAN. squid now denies those destination ranges
  before the allowlist.
- **Loopback-only admin bind.** The broker's admin routes (approve/deny/kill)
  refused nothing on a TCP bind; a non-loopback `broker.addr` is reachable from
  the sandbox VM, which could self-approve its own push. A non-loopback bind is
  now refused fail-closed (loopback TCP and the unix socket are unaffected).
- **Audit-trail durability + symlink-safety.** The trace is the source of truth
  for outcome/cost but was neither `fsync`'d nor `O_NOFOLLOW`-opened on the
  paths that mattered; a crash could lose the terminal result line and a planted
  symlink could redirect a read/write. Both are fixed.
- **Supply-chain + image hardening.** The anchor image base is digest-pinned
  (matching the sandbox image); per-task credential config files are written
  `0600`; the squid proxy-auth secret is compared in constant time; and
  `cve-gate` now surfaces High/Critical CVEs that ship without an upstream fix
  instead of passing them silently.

### Added

- **`drydock retry <id>`**: re-run a prior task from the invocation the broker
  now records in its trace (repo, prompt, agent, model, platform, egress,
  draft), without reconstructing the `submit` by hand. It re-enters the approval
  gate (`auto_approve` is not carried over).
- **`drydock cancel <id>`**: an alias for `kill`.

### Fixed

- **`drydock deny` prints "denied", not "denyd".**
- **`drydock logs <id> -f` no longer dies when the trace doesn't exist yet**:
  it polls for the file, fixing the reattach hint `submit` prints on `^C`.
  `logs` also parses `-f` with a proper flag set (order-independent).
- **`drydock ui -h`** shows curated help like every other subcommand.
- The orphan-VM reaper matches the exact `task-<32hex>` name (was a loose
  `task-` substring) and no longer fuzzy-`pkill`s squid.

## v0.5.2 (2026-07-09)

### Fixed

- **A running brokerd self-heals when its subscription token is refreshed
  out-of-band.** In subscription mode the OAuth token rotates on every refresh.
  A second process sharing the credential file (most often `drydock doctor`,
  which validates by refreshing, but also `drydock auth` or a second broker)
  would rotate the token the long-running brokerd held in memory, wedging every
  task on `502 credential unavailable` until brokerd was restarted. Worse, the
  error told you to run `drydock doctor`, which is exactly what triggered it.
  brokerd now reloads the credential from disk when a refresh fails and adopts
  the rotated token, recovering without a restart. A genuinely dead token still
  fails (no masking, no retry loop). Covers both Claude and Codex subscription.

- **`drydock deny` prints "denied", not "denyd".** The confirmation used a
  blanket `%sd` suffix that misspelled the past tense of `deny`.

## v0.5.1 (2026-07-08)

### Fixed

- **Homebrew installs can build images again.** Apple `container` ships an
  empty build context when the context path traverses a symlink, and the
  Homebrew layout always does (`share/drydock` → `Cellar/...`), so every fresh
  brew install failed `drydock setup`/`init` at the sandbox image build with
  `failed to calculate checksum … not found` on files present on disk.
  `findImageDir` now hands `container build` a fully resolved path. The
  empty-context hint also names the symlink cause (with the resolved path)
  when it applies, instead of blaming the runtime generically.

- **Loopback-DNS breakage diagnosed instead of mystifying.** When the host's
  resolvers are loopback proxies (Cloudflare WARP, dnscrypt, some VPNs),
  Apple `container` VMs get no DNS at all, image builds die at `apt-get`
  with `Temporary failure resolving …` while raw egress works. The build
  failure now gets a hint naming the cause and the fix
  (`container builder start --dns 1.1.1.1`), and `drydock doctor` warns
  up front (advisory, not a failure; existing images keep working).

## v0.5.0 (2026-07-08)

### Added

- **Unattended operation: the drydock daemon (`drydock daemon`).** `drydock
  daemon install` runs brokerd as a launchd LaunchAgent
  (`so.sri.drydock.brokerd`): it starts at login, survives reboots, and
  restarts on crash (`KeepAlive {SuccessfulExit: false}`; a crash restarts
  brokerd and triggers boot reconciliation; `drydock daemon uninstall` keeps it
  down). Install preflights credentials **as launchd will see them**: launchd
  never inherits your shell, so a key that lives only in a shell `export` fails
  the preflight by name; keys must be in `~/.drydock/api-keys.env` or OAuth
  files. If a foreground `drydock start` holds the lock, install refuses to
  claim success instead of reporting a healthy socket it doesn't own. brokerd
  now ensures `container system start` at boot, so a reboot needs no manual
  step. Logs append to `~/.drydock/logs/brokerd.log`. `drydock daemon status`
  reports launchd state, socket health, and the log path. **⚠️ There is no
  aggregate spend cap yet (ROADMAP 4.3)**: before walking away, size
  `max_concurrent_tasks`, `task_budget_usd`, `task_max_requests`, and
  `task_timeout` with a queue-drain worst case in mind; see the
  [daemon docs](https://sricola.github.io/drydock/docs/daemon.html).

- **CI CVE-scanning of the sandbox image.** A daily scheduled workflow
  Docker-builds the sandbox image and scans it with a pinned grype; a new
  tested gate (`cmd/cve-gate`) fails the run on fixable High/Critical CVEs
  unless covered by a live entry in `image/cve-allowlist.yaml`; every
  exception carries a reason and an expiry, expired entries stop suppressing,
  and allowlist edits themselves trigger the scan so suppression attempts are
  conspicuous in review. CI also now proves the egress deny-by-default
  property against a real squid on every PR (allowed host reaches upstream,
  no-auth → 407, non-allowlisted host → 403) instead of substring-matching the
  generated config.

- **Native Gemini vendor (`--agent gemini`), experimental.** Google Gemini
  models get a native lane: Google `x-goog-api-key` auth brokering, a
  `usageMetadata` token-metering parser, and a Gemini price table for
  `task_budget_usd`. Set `GEMINI_API_KEY` (host env or `api-keys.env`); API-key
  auth only, no subscription mode. Default model `gemini-2.5-pro`; override with
  `--model gemini-2.5-flash` or `gemini-2.5-flash-lite`. **Not yet verified
  end-to-end:** the gateway/parser/pricing have CI unit coverage, but the full
  in-sandbox run (A1/A2 red-team + a real metered task) is macOS-gated and has
  not been executed; treat as experimental until `make test-integration`
  passes on macOS with a real key. ROADMAP 3B stays open until then.

### Fixed

- **Squid access-log growth is now bounded.** v0.4.0 enabled the egress access
  log for forensics but never rotated it. The generated squid config now caps
  retained generations (`logfile_rotate 10`) and brokerd rotates the log daily,
  serialized behind the same lock as reconfigures so a rotation can't race a
  widening.

- **Wrong "brokerd not running" hint for config-only TCP.** With `broker.addr`
  set to TCP in `config.yaml` (no `BROKER_ADDR` env), a dial failure printed
  the unix-socket hint (`run drydock start`). The hint now resolves the address
  the same way the client does (env → config).

- **Cleartext-key warning for `openai_compat` over `http://`.** A non-loopback
  `http://` base URL would send the real API key in cleartext; config
  validation now warns. Loopback (a local model) stays quiet.

## v0.4.0 (2026-07-02)

### Added

- **Local web UI (`drydock ui`).** A loopback-only browser app (task board,
  diff review, one-click approve/deny, and run history) served over the broker
  socket. Navigate to the printed URL; the one-time token in the URL fragment
  gates access (never sent to the server). `--open` launches the default browser
  automatically; `--no-token` disables the gate for trusted local setups.
  `drydock init`, `drydock status`, and `drydock pending` surface the
  `drydock ui` hint when a task is waiting at the approval gate.

- **Bring your own model: OpenAI-compatible lane.** Any endpoint that speaks
  the OpenAI wire protocol (Google Gemini, OpenRouter, Ollama, LM Studio, vLLM,
  …) can now be wired as a drydock agent. Add an `openai_compat:` block to
  `~/.drydock/config.yaml` with the base URL, optional path, the *name* of the
  env var holding the real key (never the key itself), and the model id; set
  `default_agent: opencode`. Tasks then route through the **opencode** agent
  against your endpoint via the same credential gateway, per-task token, and USD
  metering as Claude Code and Codex. Optional `prices:` sub-keys enable dollar
  metering for the model. The setup wizard prompts for an OpenAI-compatible
  endpoint on first run. `drydock doctor` verifies the lane and red-team A1 (key
  isolation) covers it: the real host key never enters the VM.

- **Agent-written PR title and body.** After a successful push the broker sends
  the agent-produced title and description to the remote's PR-open call instead
  of a generic placeholder. `drydock submit --draft` opens the PR as a draft.
  `drydock doctor` (and `drydock submit`) preflight the git-host auth (`gh`,
  `glab`, `tea`) before attempting a push, surfacing auth failures early.

### Changed

- **Provider registry.** The CLI, config validator, setup wizard, `drydock
  start`, `drydock doctor`, and `drydock auth` all read from a single
  `Provider{Agent, Vendor, AuthModes}` table instead of hardcoded per-vendor
  branches. Adding a new agent is a registry row, not a five-file edit. No
  behavior change for Claude Code or Codex.

- **Relicensed Apache 2.0** (was MIT). All source files and the repository
  `LICENSE` reflect the change.

### Fixed

- **Per-domain egress port enforcement.** The egress allowlist now enforces the
  port alongside the hostname: a domain without an explicit port is constrained
  to the default for its scheme; one with an explicit port (e.g.
  `registry.npmjs.org:443`) blocks any other port. IP literals in the allowlist
  are now rejected at validation time; only hostnames are accepted, closing a
  bypass where a numeric `1.2.3.4` entry could evade the hostname check. The
  squid access log is now written to `~/.drydock/squid/access.log` so failed
  egress attempts are inspectable without running `drydock doctor`.

- **Crash-recovery reconciliation.** A `brokerd` killed mid-task previously left
  orphaned VMs, stale stage dirs, and tasks stuck at `running?` forever. Boot
  reconciliation now enforces single-instance via `flock`
  (`~/.drydock/brokerd.lock`) so a second daemon cannot clobber a live task;
  reaps orphaned `task-*` VMs and squid processes; sweeps stale stage dirs; and
  resolves crash-interrupted tasks to a distinct `interrupted` outcome in
  `drydock tasks` instead of `running?`.

- **Security hardening (UI + audit).** The web UI one-time token is verified in
  constant time (no timing oracle on the gate secret). Audit log reads
  (`drydock tasks`, web UI history) use `O_NOFOLLOW` so a symlink planted inside
  the audit dir cannot redirect a read outside it. The history diff overlay in
  the web UI is read-only to prevent accidental in-browser edits.

- **`opencode` surfaced consistently.** `drydock init`, `drydock status`,
  `drydock pending`, and `drydock doctor` now show the opencode lane alongside
  Claude Code and Codex rather than silently omitting it.

### Docs

- **Docs site.** Operator documentation is now a Go-native rendered HTML site
  with a shared design system, dark mode, and a sidebar; see
  [sricola.github.io/drydock/docs](https://sricola.github.io/drydock/docs/).
  New pages: Models / Bring your own model (OpenAI-compatible endpoint setup)
  and Web UI. Existing pages updated for v0.4.0 features throughout.

## v0.3.0 (2026-06-22)

### Added

- **First-run setup wizard.** On a fresh install with a TTY, `drydock setup`
  runs an interactive wizard: pick your agent (Claude Code / OpenAI Codex /
  both), choose subscription or API-key auth per agent, and optionally store an
  API key host-side. `--reconfigure` re-runs it; non-TTY runs and existing
  configs without `--reconfigure` keep the previous static seed behavior.
- **Host-side API-key store (`~/.drydock/api-keys.env`, mode 0600).** API keys
  can be persisted host-side so the broker finds them across shells; they are
  never copied into the sandbox VM. A non-empty environment variable still
  overrides the stored value, and `drydock doctor` reports each key's source
  (env / api-keys.env / none).
- **Live progress streaming for `drydock submit`.** Instead of blocking silently
  for the whole run, the submit shell now streams phase updates in real time:
  `preparing → running → awaiting approval → pushing`. Each phase prints a
  one-liner as it starts; no polling needed.
- **Visible approval gate.** When the agent finishes and the diff is ready, the
  submit shell prints the diff size and the exact commands to act on it
  (`drydock review <id>` / `drydock approve <id>` / `drydock deny <id>`), so
  you no longer have to switch shells and guess.
- **Actionable boot-failure output.** A sandbox that fails to boot now reports
  the real error (e.g. `entrypoint.sh: DRYDOCK_GW_IP: missing gateway ip`) and
  suggests `drydock doctor`, instead of the opaque `task failed: exit status 1`.
- **Richer completion summary.** The final line now shows branch, platform,
  diffstat (files, insertions, deletions), wall-clock duration, and cost,
  e.g. `✓ pushed agent/7f3a… (github) · 4 files +120/-8 · 2m18s · $0.11`.
- **`--quiet` flag for `drydock submit`.** Suppresses all progress output and
  prints only the final outcome line, useful in scripts that capture the result
  but don't want interleaved status noise.
- **`--json` now streams raw NDJSON events** as the task runs (one JSON object
  per line), replacing the previous single-object response. Pipe to `jq -c` to
  process events incrementally or filter the terminal `result` event for the
  branch name.

### Fixed

- **Per-task egress widening is now actually enforced.** Approved
  `--egress-extra` hosts previously never became reachable: the host-side squid
  proxy (the per-domain egress enforcement point) was started once with the
  default allowlist and never reconfigured, so an operator could approve a host
  and the agent would still be blocked. Widening now provisions a per-task squid
  proxy credential (mirroring the credential gateway's per-task token model) that
  authorizes only that task's extra hosts, with strict isolation from other
  concurrent tasks. The default (non-widened) egress path is byte-for-byte
  unchanged, and the squid run dir / broker path are validated to fail fast on
  whitespace that would silently break the proxy config.
- **The setup wizard never echoes a pasted secret.** Terminal echo is reliably
  disabled while reading an API key; if echo cannot be disabled, the wizard
  refuses to read the key in plaintext rather than risk showing it on screen.
- **Hardening: pager invocation, OAuth expiry, API-key store.** `drydock review`
  passes the diff path to `$PAGER` as a positional argument, so a path with
  spaces or shell metacharacters can neither break nor inject into the command;
  OAuth `expires_in` is clamped to avoid an int64 duration overflow on a hostile
  or buggy token endpoint; and the API-key store's load and write now agree on
  the recognized key set. Adds test coverage for the GitHub/GitLab/Gitea remote
  adapters.
- **Squid pid and image-build robustness.** A stale `squid.pid` left by a
  hard-killed broker is now self-healed on start/stop, and a `container build`
  that ships an empty context produces an actionable message instead of an
  opaque failure.

### Notes

- **Version-skew caveat.** An old `drydock submit` binary (pre-streaming)
  talking to a new brokerd will print raw NDJSON to the terminal instead of
  rendered progress. Because brokerd and the CLI ship as one binary, this only
  affects a stale separate install; `drydock version` should match on both sides.

## v0.2.0 (2026-06-21)

### Added

- **Run Claude Code on your Claude subscription, no API key.** With a Claude
  Pro or Max plan: `claude login` → `drydock auth claude` →
  `anthropic_auth: subscription`. The OAuth credential is held host-side
  (`~/.drydock/claude-oauth.json`, mode 0600), kept fresh by the gateway, and
  **never enters the VM**: the sandbox still sees only a per-task token. A live
  red-team test asserts the access and refresh tokens are absent from the VM
  environment.
- **Run OpenAI Codex on your ChatGPT subscription, no API key.** The parallel
  path: `codex login` → `drydock auth codex` → `openai_auth: subscription`. The
  gateway injects the real OAuth token plus the `chatgpt-account-id` header and
  routes to the Codex backend; the access token, refresh token, and account id
  all stay host-side and never reach the VM (covered by a live red-team test).
- **Per-task request cap (`task_max_requests`).** The USD budget doesn't apply
  in subscription mode, so this caps how many upstream requests one task may
  make: the gateway returns HTTP 429 once the cap is hit. Works as defense in
  depth for API-key tasks too.
- **`drydock auth claude` / `drydock auth codex`**: copy the subscription
  credential from the vendor CLI's login into drydock's host-only store. Status
  output is token-free.
- **`drydock doctor`** validates a configured subscription token (loads it and
  refreshes if near expiry): `claude subscription` / `codex subscription` →
  token valid, skipped in API-key mode.

### Changed

- `drydock tasks` labels subscription runs as `subscription` in the cost column
  (recorded per task at run time) instead of a misleading `$0.0000`.
- CI now runs `staticcheck` (with a matching `make lint` target), alongside the
  existing `go vet` and `govulncheck`.

### Docs

- Site and README spell out the agent × auth matrix: Claude Code and OpenAI
  Codex, each usable with an API key **or** a subscription.
- README / SECURITY / THREAT_MODEL document the subscription blast radius (a
  full-account OAuth token, broader than a scoped key and not per-task
  revocable) and the ToS/rate-limit caveat for headless use.
- README logo is now visible in GitHub dark mode.

## v0.1.10 (2026-06-19)

### Added

- **`drydock setup`**: one command from install to ready. It installs the
  Homebrew prerequisites (Apple `container`, squid), prompting before each
  (`--yes` to skip), then runs `drydock init`. Previously `init` only *checked*
  the prerequisites and exited if they were missing, so the happy path is now
  `brew install drydock` → `drydock setup` instead of a multi-step prerequisite
  hunt. Without a TTY and without `--yes`, it prints the install command and
  stops rather than silently modifying the system.

### Changed

- `drydock doctor` gives an **actionable message when Codex is missing** from
  the sandbox image: it names the offending image and points at the fix (run
  `drydock init` to rebuild, or correct a stale `sandbox_image`) instead of
  dumping a raw `/bin/sh: codex: not found`. The usual cause is a config that
  predates the v0.1.5 `claude-sandbox` → `drydock-sandbox` rename.

### Fixed

- Website: selected text is legible inside the dark terminal/code blocks. The
  global `::selection` rule had forced selected text to the dark page color,
  rendering it invisible (dark-on-dark) when highlighted in a terminal block.

### Docs

- README: a real `drydock redteam` screenshot beside the command, showing the
  A1/A2/A7 containment attacks passing live against the sandbox.

## v0.1.9 (2026-06-19)

### Added

- **`drydock redteam`** runs drydock's live containment attacks against your
  own sandbox (on your own Mac, on your actual image) and prints a pass/fail
  table, so you can verify containment yourself instead of trusting the threat
  model. It runs the VM-backed attacks behind **A1** (the real vendor key never
  enters the VM), **A2** (egress to non-allowlisted hosts is blocked), and
  **A7** (no state persists between tasks). No API spend; the attacks inspect
  the VM env, egress, and filesystem; they never call a model.
- **Breach demo**: `demo/breach.sh` (`make demo`, `make demo VM=1`) is a
  narrated runner that executes the real `THREAT_MODEL.md` red-team tests and
  shows them contain actual attacks; every green is a live `go test` pass, not a
  scripted print. Recorded GIFs ship in `demo/` and lead the README and website.

### Fixed

- The `log_json`, `strict_container_version`, and `notifications` keys in
  `~/.drydock/config.yaml` were parsed but never honored; brokerd read each
  one's `DRYDOCK_*` env var directly at the point of use, so setting the YAML
  key did nothing. Config is now loaded first and drives logging, the
  container-version check, and notifications. The env vars keep working (config
  still folds them on top, env winning), so precedence is unchanged.

### Changed

- Front-door copy leads with the local-first stance (*run coding agents on your
  own Mac like you assume they're already hacked*) across the README tagline
  and the site hero. The README install now opens with a one-line macOS-26 /
  Apple-silicon eligibility self-check so users self-qualify in seconds.

## v0.1.8 (2026-06-19)

A hardening release: reliability, performance, and supply-chain work from an
internal audit. No new user-facing features; behavior is unchanged except where
noted.

### Fixed / reliability

- **Graceful shutdown.** On `SIGINT`/`SIGTERM`, brokerd now cancels every
  in-flight task (each tears down its own VM and answers the client), drains
  the HTTP server, stops squid + the anchor, and removes the socket, instead
  of `os.Exit`-ing and orphaning running VMs until the next boot.
- **HTTP server timeouts** (`ReadHeaderTimeout` + `IdleTimeout`) on the broker
  and gateway listeners blunt slow-loris / idle-keepalive abuse without cutting
  off long agent streams or the blocking approval wait.
- Task IDs and gateway tokens **fail closed** if `crypto/rand` ever fails
  (no zero-filled IDs/tokens); failed VM force-deletes and squid stops are now
  logged instead of silently swallowed.

### Performance

- The credential gateway **meters usage by streaming** the response line by
  line and keeping only the few usage-bearing events, instead of buffering the
  whole multi-MB SSE body. Peak memory drops from O(body) to O(one line).
- `drydock tasks` reads each audit log's **tail** (not the whole trace) and
  sorts from cached `mtime` (no per-comparison `stat`).

### Supply chain

- `govulncheck` runs in CI and fails the build on a known dependency
  vulnerability; the sandbox base image is **pinned by digest**.
- Release **binaries are byte-for-byte reproducible**; each release publishes a
  per-binary checksum and `make verify-build` re-derives it.

### Notes

- `DRYDOCK_TASK_BUDGET_USD` is documented as a **soft cap** (post-paid): a
  single in-flight request can overshoot by its own cost before the next is
  refused. See `THREAT_MODEL.md` N4.

## v0.1.7 (2026-06-19)

### Security / credibility

- **Adversarial red-team harness.** Every `THREAT_MODEL.md` claim (A1–A7) is
  now backed by a test that runs the actual attack and asserts containment.
  `make redteam` runs the host-side claims (A3–A6, also in CI); `make
  redteam-vm` runs the VM-backed claims (A1, A2, A7) on macOS / Apple silicon.
  THREAT_MODEL carries a `Verified by:` link for each.
- **Verifiable releases.** Each tagged release now ships a CycloneDX SBOM, a
  keyless **cosign** signature, and **SLSA build provenance** (built by the
  release workflow from the tagged commit), all attached to the GitHub
  release. See `SECURITY.md` "Verifying a release"; `make sbom` generates the
  SBOM locally.

### Changed

- Removed the dead `Broker.Approve` hook (it was set but never called; the
  real gates are `gatePush` / `gateEgressWiden`). Internal cleanup, no
  behavior change.

## v0.1.6 (2026-06-19)

### Added

- **`drydock prune --older-than DUR [--keep-last N] [--yes]`** deletes old
  per-task audit artifacts (`<id>.jsonl` / `.diff` / `.widen.json`) from the
  audit dir, which previously grew unbounded. Dry-run by default (prints what
  it would remove + bytes freed); `--older-than` is required so it can never
  prune everything by accident; `--keep-last` always retains the N
  most-recent tasks. Only touches files matching the task-artifact pattern.
- `brokerd` warns at boot when `default_agent`'s vendor has no API key
  configured; tasks that don't pass `--agent` would otherwise be rejected
  at submit time with no upfront signal.

## v0.1.5 (2026-06-18)

### Added

- **OpenAI Codex as a second agent.** Tasks choose their agent with
  `drydock submit --agent claude|codex`; the operator default is set via
  `default_agent` (config) / `DRYDOCK_DEFAULT_AGENT` (env), default
  `claude`. The credential gateway gained a vendor registry: the real key
  for whichever vendor (Anthropic or OpenAI) stays **host-only**, the VM
  only ever sees a budget-capped bearer token, and per-task USD metering +
  revoke apply to both. `brokerd` now accepts **at least one** of
  `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`. `drydock tasks` shows
  duration + metered cost + outcome for Codex tasks too.

### Changed

- **The sandbox image is renamed `claude-sandbox` → `drydock-sandbox`**
  and now ships both the Claude Code and Codex CLIs; `entrypoint.sh`
  dispatches on `DRYDOCK_AGENT`. Re-run `drydock init` to rebuild (it
  detects the stale image); set `SANDBOX_IMAGE` if you had pinned the old
  name. `api.openai.com` is added to the default egress allowlist and,
  like `api.anthropic.com`, routes through the gateway rather than squid.

### Fixed

- `drydock start` now accepts either vendor key; it previously refused
  to start without `ANTHROPIC_API_KEY`, blocking Codex-only operation
  even though `brokerd` itself accepted either key.

### Notes

- Codex routes through the gateway via a generated `model_provider`
  config written by the entrypoint (Codex ignores `OPENAI_BASE_URL`); the
  real key still never enters the VM. The OpenAI entries in the budget
  gate's pricing table are approximate: a safety cap, not a billing
  source of truth.

## v0.1.4 (2026-06-17)

### Changed

- **State dirs default to `~/.drydock/{stage,audit,squid}`** instead of
  `/tmp/broker/{stage,audit,squid}`. Audit history was silently
  evictable on `/tmp` (tooling, OS upgrades, disk pressure all treat it
  as scratch). Existing operators upgrading from < v0.1.4 still see
  pre-existing `/tmp/broker/audit` history in `drydock tasks` while it
  exists; a legacy fallback path triggers when the new default is
  empty. The seeded config now uses `~/.drydock/...`; tilde-expansion
  is applied at load time.

### Fixed

- **`drydock submit` propagates ^C as a request cancellation.** The
  comment claimed this for a while; the code used `context.Background()`
  so ^C just orphaned the CLI while brokerd kept running. Now wired
  through `signal.NotifyContext`.
- **Friendlier "brokerd not running" messages.** `drydock submit`,
  `approve`, `deny`, `pending`, `kill`, and `status` all detect the
  missing-socket / connection-refused case and print
  `brokerd not running, start it in another shell with drydock start`
  instead of the raw Go HTTP transport error. `drydock kill`
  specifically used to say "no such task" without ever asking brokerd.

### Added

- README: `--model` flag and `default_model` config field documented.
- README: Egress section reflects the Go module proxies and notes which
  runtimes ship in the sandbox image (Node 22, Python 3.11, Go 1.26).
- README: `brew tap` install snippet now includes `brew trust
  sricola/drydock` (personal taps require explicit trust on newer brew).
- `SECURITY.md`: dedicated "TCP exposure" section spelling out that
  `broker.addr` / `BROKER_ADDR` has no built-in auth and naming the
  acceptable deployment patterns (loopback + SSH, mTLS reverse proxy).
- `examples/hello-task.md`: a copy-paste-ready first task that fits the
  default $2 budget and exercises every layer of the boundary.
- `CHANGELOG.md`: this file.
- `THREAT_MODEL.md` opens with a five-bullet TL;DR so the security
  posture is scannable without reading the full doc.

## v0.1.3 (2026-06-17)

### Added

- **`drydock doctor`**: no-API-spend smoke command that checks sandbox
  image freshness (`DRYDOCK_GW_IP` baked in), VM boot, and that the nft
  egress pin enforces (non-allowlisted host blocked). ~5s; gives
  operators a way to validate setup before paying for an API call.
- **`drydock submit --model <id>`**: per-task model passthrough to
  `claude --model`. Empty falls back to `default_model` in
  `~/.drydock/config.yaml`, then to claude's own default.
- **`default_model` in `~/.drydock/config.yaml`** (also
  `DRYDOCK_DEFAULT_MODEL` env override): operator-level fallback model
  for tasks that don't pass `--model`.
- **Python 3.11 and Go 1.26 in the sandbox image** alongside Node 22.
  Go is installed from the upstream tarball; Debian's `golang-go` is
  too old to be useful. Image grows ~265MB.
- **`proxy.golang.org` and `sum.golang.org` in the default egress
  allowlist** so `go mod download` works inside the sandbox.
- **macOS + Apple-silicon preflight in `drydock init`**: fails loudly
  with a one-line reason on non-darwin, non-arm64, or macOS < 26
  instead of a cryptic downstream `container build` error.
- **Embedded version**: `drydock version` reports `git describe`
  output on source builds (`v0.1.2-10-g26adad8`) instead of `"dev"`.
- **Pricing table covers the 4.x families** (Opus, Sonnet, Haiku) with
  an Opus-priced default fallback so unknown new releases can't undercount
  budgets.

### Fixed

- **`--help` on every subcommand.** Previously `drydock approve --help`
  would try to approve a task literally named `"--help"`; same trap on
  `kill`, `review`, `logs`, etc. Now per-subcommand help intercepts
  before any positional consumption.
- **`drydock init` rebuilds stale sandbox images.** If the entrypoint
  still reads `MACAGENT_GW_IP` (pre-rename layer cache), init detects
  it, deletes the image, and rebuilds `--no-cache`.
- **Failed tasks resolve to `error`, not `running?`.** When a task's
  container exits non-zero before claude can write a terminal `result`
  event, brokerd appends a synthetic result row so `drydock tasks`
  shows the right outcome.
- **`brokerd` upgrade nudge.** `drydock init` checks existing
  `~/.drydock/egress.yaml` for the hosts that ship in the current
  release; missing entries get a one-shot copy-paste hint at the end
  of init.

## v0.1.2 (2026-06-16)

### Changed

- Operator config moved to `~/.drydock/{config,egress}.yaml`, seeded
  by `drydock init`, never overwrites operator edits. Env vars
  (`DRYDOCK_*`, `BROKER_*`) still override file values, so existing
  scripts keep working. `ANTHROPIC_API_KEY` stays env-only by design.

## v0.1.1 (2026-06-16)

(early packaging pass; see git history)

## v0.1.0 (2026-06-16)

Initial public release of drydock, hardware-isolated sandbox for
autonomous coding agents on macOS. Per-task Apple `container` VM,
host-side credential gateway (real key never enters the VM), userspace
squid for hostname-based egress allowlist, host-side `git push`. See
`THREAT_MODEL.md` for the security claims this all backs.
