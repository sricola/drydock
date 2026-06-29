# Web UI

`drydock ui` serves a small web app over the broker socket — the same board,
approval gate, diff, and history you get from the CLI, in a browser. It binds to
loopback only and is gated by a one-time token.

## Launch

```bash
drydock ui                 # prints: UI ready: http://127.0.0.1:7878/#t=<token>
drydock ui --open          # also open it in your default browser
drydock ui --port 8080     # bind a different loopback port (default 7878)
```

`drydock start` must already be running — the UI is a thin client over the same
broker socket the CLI uses and keeps no state of its own. Open the printed URL;
the token rides in the `#t=` fragment, so it is never sent as a query parameter,
written to server logs, or leaked in `Referer` headers. The page reads it from
the fragment and sends it as a bearer token on every API call.

## What's in it

- **Board** — every live task as a card. Running tasks show the agent, turn
  count, cost, and current action; a task awaiting you floats to the top with a
  prominent approval block.
- **Review** — open a task for its **Diff** and **Logs** (the agent transcript)
  in tabs. **Approve push** stays disabled until you've opened the diff — the
  same review-before-approve gate as the CLI; **Deny** takes a confirm. `Esc` or
  a backdrop click closes the overlay.
- **Submit** — start a task: repo, instruction, agent (`claude` / `codex` /
  `opencode`), and an optional model. The repo URL is validated as you type and
  recent repos are remembered.
- **History** — past runs from the audit dir: outcome, cost, and duration, each
  with its diff and logs.

On the board, when exactly one task is at a gate: `R` review · `A` approve · `D`
deny. `⌘/Ctrl+Enter` submits the form; `?` lists the shortcuts.

## Security

The server is **loopback-only** (`127.0.0.1`) and **token-gated** — every API
call must carry the token minted at launch. It drives the same broker socket the
CLI does, so the approval gate, audit trail, and [egress rules](egress.html) are
unchanged: the UI never widens what a task can reach or push. See the
[threat model](threat-model.html) for the guarantees it inherits.

`--no-token` removes the gate for a trusted single-user machine. drydock prints a
warning when you use it, because then **any local process or web page can submit
tasks, approve pushes, and kill tasks** through the server. Don't pair it with
anything that exposes the port beyond loopback.
