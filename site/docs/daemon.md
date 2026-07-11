# Run unattended (launchd daemon)

`drydock daemon install` runs brokerd as a launchd LaunchAgent: it starts at
login, survives reboots, and restarts on crash. Tasks can be submitted while
you're away; approval gates queue (by default `approval_timeout: 0s` = wait
forever) until you pick them up with `drydock ui`, `drydock pending`, or
`drydock review`.

## ⚠️ Know the limits before you walk away

Per-task controls bound each individual run:

- `task_budget_usd`: USD ceiling per task (API-key mode)
- `task_max_requests`: request ceiling per task (the primary control in
  subscription mode, where there is no metered spend)
- `task_timeout`: wall-clock ceiling per task

To bound spend across tasks, set `aggregate_budget_usd` (and optionally
`aggregate_window`). The gateway enforces a rolling per-provider USD ceiling:
once a vendor's windowed spend reaches the cap, new requests for that vendor
are rejected (HTTP 402), and submitting a new task fails immediately at
submit time rather than starting a doomed task. Set it to the maximum you are
comfortable spending per provider over the window before walking away.

`aggregate_budget_usd` applies to `api_key`-mode vendors only. Subscription
mode has no metered USD spend; subscription runaway stays bounded per-task by
`task_max_requests`.

Size `max_concurrent_tasks` and `task_budget_usd` with a queue-drain worst case
in mind: absent an aggregate cap, worst-case burn is
`max_concurrent_tasks × task_budget_usd` per drain.

## Install

```bash
drydock daemon install
```

This preflights credentials **as launchd will see them**: launchd does not
inherit your shell, so `export ANTHROPIC_API_KEY=…` in `.zshrc` is invisible
to the daemon. Keys must be host-side; `drydock setup` stores them in
`~/.drydock/api-keys.env` (mode 0600), or use `drydock auth claude|codex`
for subscription mode. If your only credential is a shell export, install
fails and names it.

Then it writes `~/Library/LaunchAgents/so.sri.drydock.brokerd.plist`,
bootstraps it, and waits for brokerd to answer on its socket.

Details that matter:

- **Crash restart, clean stop stays down.** `KeepAlive {SuccessfulExit:
  false}`: a crash restarts brokerd (boot reconciliation then reaps any
  orphans); `drydock daemon uninstall` keeps it down.
- **Logs**: `~/.drydock/logs/brokerd.log` (stdout+stderr, appended; no
  rotation yet, so prune it yourself if it grows).
- **Container runtime**: brokerd now ensures `container system start` at
  boot, so a reboot needs no manual step.
- Re-running `install` is the upgrade path: it rewrites the plist and
  restarts the job (e.g. after `make install` of a new build).

## Restart and awaiting-approval tasks

A task blocked at the diff-approval gate survives a brokerd restart:

- When a task enters the gate, brokerd writes a durable gate marker
  (`<id>.gate.json`) in the audit dir.
- On a graceful shutdown, the stage dir is preserved (its cleanup is
  skipped) and the marker is left on disk. Boot reconciliation skips
  reaping any stage dir whose id has a gate marker, so the work tree is
  intact when brokerd comes back up.
- At boot, brokerd re-registers each marked task as pending and resumes it.
  Running `drydock approve <id>` after a restart pushes the surviving
  branch exactly as it would have before the restart: no agent re-run, no
  additional API spend. Push auth goes through git remote credentials
  (`PushEnv`), not the model credential.
- If the stage did not survive (for example, a task that was awaiting
  approval before this feature was deployed, or one whose stage dir was
  removed manually), brokerd appends an `interrupted` terminal line to the
  audit instead. The `.diff` is preserved, so `drydock retry <id>` still
  works. A false `ok` is never written for a task that did not push.
- If brokerd shuts down again while a resumed task is still waiting at the
  gate, the marker is preserved for the next boot (idempotent).

## Status / uninstall

```bash
drydock daemon status      # launchd state + broker socket health + log path
drydock daemon uninstall   # bootout + remove the plist
```

`drydock start` (foreground, ^C to stop) keeps working either way; the
flock in `~/.drydock/brokerd.lock` guarantees only one brokerd runs.
