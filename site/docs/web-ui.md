# Web UI

`drydock ui` serves a small web app over the broker socket: the same board,
approval gate, diff, and history you get from the CLI, in a browser. It binds to
loopback only and is gated by a one-time token.

## Launch

```bash
drydock ui                 # prints: UI ready: http://127.0.0.1:7878/#t=<token>
drydock ui --open          # also open it in your default browser
drydock ui --port 8080     # bind a different loopback port (default 7878)
```

`drydock start` must already be running; the UI is a thin client over the same
broker socket the CLI uses and keeps no state of its own. Open the printed URL;
the token rides in the `#t=` fragment, so it is never sent as a query parameter,
written to server logs, or leaked in `Referer` headers. The page reads it from
the fragment and sends it as a bearer token on every API call.

## What's in it

- **Board**: every live task as a card. Running tasks show turn count, cost,
  and the current action; a task awaiting you floats to the top with a prominent
  approval block.
- **Review**: open a task for its **Diff** and **Logs** (the agent transcript)
  in tabs. **Approve push** stays disabled until you've opened the diff, the
  same review-before-approve gate as the CLI; **Deny** takes a confirm. `Esc` or
  a backdrop click closes the overlay.
- **Submit**: start a task: repo, instruction, agent (`claude` / `codex` /
  `gemini` / `opencode`), and an optional model. The repo URL is validated as you type and
  recent repos are remembered.
- **History**: past runs from the audit dir: outcome, cost, and duration, each
  with its diff and logs. Outcome is `ok (N turns)` for a pushed or no-diff
  run, or a distinct `denied`/`cancelled`/`push failed`/`error` line
  otherwise, see [Push outcomes](submitting-tasks.html#push-outcomes). The
  board's "Just finished" rail marks the same split with an icon: ✓ for
  pushed/ok/no-diff, ✕ for error/push failed, and a neutral `∅` for
  denied/cancelled (neither succeeded nor failed: the task just didn't run).

On the board, when exactly one task is at a gate: `R` review · `A` approve · `D`
deny. `⌘/Ctrl+Enter` submits the form; `?` lists the shortcuts.

## Trust brief panel

Opening a review renders the task's **trust brief** above the diff: the same
broker-observed evidence `drydock inspect <id>` prints, so you can weigh the
diff without leaving the overlay. The panel shows the repo and base commit
(with `sensitive` / `auto-approve` chips where set), the runtime (agent,
vendor, model, image), the effective policy (budget, timeout, policy snapshot
hash), egress rules, broker-metered spend, and a diff summary — hash, size,
file/line counts, and any **FLAG** rows for structurally risky changes
(binaries, symlinks, exec bits, dependency manifests, lockfiles, CI
workflows, git metadata, submodule gitlinks). When an
[execution profile](submitting-tasks.html#execution-profiles-setup-per-repo)
or [verification](submitting-tasks.html#verification-optional-per-repo) is
configured, its block appears too: overall status, the VMs' capability
posture, and per-command exit codes and durations (the setup block first —
setup runs before the agent).

Everything in the panel is what the broker observed — none of it is the
agent's own account of what it did. It is read-only, fetched from the same
loopback-only, token-gated API as the diff, and a task recorded before briefs
existed simply shows "no trust brief recorded"; the diff still loads.

## Security

The server is **loopback-only** (`127.0.0.1`) and **token-gated**: every API
call must carry the token minted at launch. It drives the same broker socket the
CLI does, so the approval gate, audit trail, and [egress rules](egress.html) are
unchanged: the UI never widens what a task can reach or push. See the
[threat model](threat-model.html) for the guarantees it inherits.

Every response also carries a strict `Content-Security-Policy`
(`default-src 'self'` — no inline script, no external loads, no framing)
plus `X-Content-Type-Options: nosniff` and `X-Frame-Options: DENY`. That is
defense-in-depth behind the loopback bind and token, not a substitute for
them.

`--no-token` removes the gate for a trusted single-user machine. drydock prints a
warning when you use it, because then **any local process or web page can submit
tasks, approve pushes, and kill tasks** through the server. Don't pair it with
anything that exposes the port beyond loopback.
