# Run unattended (launchd daemon)

`drydock daemon install` runs brokerd as a launchd LaunchAgent: it starts at
login, survives reboots, and restarts on crash. Tasks can be submitted while
you're away; approval gates queue (by default `approval_timeout: 0s` = wait
forever) until you pick them up with `drydock ui`, `drydock pending`, or
`drydock review`.

## ⚠️ Know the limit before you walk away

**There is no aggregate spend cap yet (ROADMAP 4.3)**: worst case is
`max_concurrent_tasks × task_budget_usd` per queue-drain on API keys.
Per-task controls are the only spend controls:

- `task_budget_usd`: USD ceiling per task (API-key mode)
- `task_max_requests`: request ceiling per task (the control that matters in
  subscription mode, where there is no metered spend)
- `task_timeout`: wall-clock ceiling per task

A submission loop gone wrong can start many tasks, each individually within
budget. Size `max_concurrent_tasks` and `task_budget_usd` with that in mind.

## Install

```bash
drydock daemon install
```

This preflights credentials **as launchd will see them**: launchd does not
inherit your shell, so `export ANTHROPIC_API_KEY=…` in `.zshrc` is invisible
to the daemon. Keys must be host-side: `drydock setup` stores them in
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
  rotation yet; prune it yourself if it grows).
- **Container runtime**: brokerd now ensures `container system start` at
  boot, so a reboot needs no manual step.
- Re-running `install` is the upgrade path: it rewrites the plist and
  restarts the job (e.g. after `make install` of a new build).

## Status / uninstall

```bash
drydock daemon status      # launchd state + broker socket health + log path
drydock daemon uninstall   # bootout + remove the plist
```

`drydock start` (foreground, ^C to stop) keeps working either way: the
flock in `~/.drydock/brokerd.lock` guarantees only one brokerd runs.
