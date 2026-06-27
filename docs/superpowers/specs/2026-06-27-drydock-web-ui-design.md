# drydock Web UI — Design Spec

**Status:** Approved (brainstorming) — 2026-06-27
**Goal:** A local web UI that is a complete browser alternative to the `drydock` CLI: submit tasks, monitor them live, review the push diff and approve/deny/kill, approve egress-widening, and browse history — without changing brokerd's security model.

## Summary of decisions

| Decision | Choice |
|---|---|
| Primary purpose | Full control center (submit + monitor + approve/deny/kill + egress + history) |
| Access model | `drydock ui` loopback-only server, token-gated; brokerd stays socket-only (unchanged) |
| Frontend stack | Vanilla HTML/CSS/JS, `go:embed`'d, no npm/bundler/framework |
| v1 scope | All four: Live board, Diff review + approve/deny/kill, Submit, History |
| Real-time | Polling (~1.5s); no brokerd streaming endpoint |
| Live cost | Out of v1 — cost shown at completion + in History (parsed from audit); board shows stage + elapsed only |
| Diff rendering | Client-side unified-diff coloring (split on `+`/`-`/`@@`); no vendored syntax highlighter |

## Background — what already exists (do not change)

- **brokerd is already an HTTP/JSON server** over a per-user unix socket (optional TCP via `broker.addr`, which is explicitly no-auth — the UI does NOT use it). Routes (`cmd/brokerd/main.go:318-325`):
  - `POST /tasks` (submit; registers the task immediately, then blocks until terminal outcome)
  - `POST /admin/approve/{id}`, `POST /admin/deny/{id}`, `POST /admin/kill/{id}`
  - `GET /admin/pending` (→ `[]string` of task ids awaiting a gate)
  - `GET /admin/tasks` (→ `[]TaskState`), `GET /healthz`
- **`TaskState`** (`internal/broker/broker.go:181`): `id, repo, instruction, stage, started_at, egress_extra`. **No cost field.**
- **Stages** (`broker.go:172-175`): `awaiting_egress`, `running`, `awaiting_approval` (push gate), `pushing`.
- **The push diff is persisted to disk**, not served over HTTP: `<audit_root>/<id>.diff` (mode 0600), written at `broker.go:741-744`; `drydock review` reads it directly (`cmd/drydock/review.go`, `diffPath()` at `cmd/drydock/util.go:58`).
- **The audit log** `<audit_root>/<id>.jsonl` is the per-task event stream the broker writes live (`accepted` → `stage` → … → `result`). Its terminal `result` line carries `outcome`, `total_cost_usd`, `duration_ms`. `drydock tasks` parses these (`cmd/drydock/tasks.go`: `auditResult`, `auditMeta`, `runTasks`); `auditCost()` at `internal/broker/stream.go:85`.
- The CLI talks to brokerd as an HTTP client over the socket (`cmd/drydock/client.go`).

The key architectural fact: **the UI server runs on the same machine as brokerd and the audit dir, so it can read diffs/logs/history directly and only needs to proxy the socket for live state + actions.** brokerd needs no changes.

## Architecture

```
browser ──TCP 127.0.0.1:PORT──▶ drydock ui server ──┬─ unix socket ─▶ brokerd   (live state + actions)
        (token-gated)                                └─ reads <audit_root>/*.{jsonl,diff}  (diffs, logs, history)
```

The `drydock ui` server is a localhost-only HTTP server that (a) serves the embedded SPA, (b) proxies a small allow-listed set of brokerd endpoints over the unix socket, and (c) reads the audit directory directly for diffs, logs, and history. brokerd is untouched; no TCP exposure of brokerd is involved.

## Components

### 1. `cmd/drydock/ui.go` — the `drydock ui` command

```
drydock ui [--port N] [--open] [--no-token]
```

- Resolves the audit root and socket path from config (same resolution the other CLI commands use).
- Mints a random token (32 bytes hex) unless `--no-token`.
- Binds `127.0.0.1:PORT` (default port `7878`; if taken, fail with a clear message naming the port — do not auto-scan).
- Prints `UI ready: http://127.0.0.1:PORT/?t=<token>` (token omitted when `--no-token`).
- `--open` launches the default browser at that URL (macOS `open`).
- Runs until Ctrl-C; clean shutdown.

### 2. `internal/webui/` — the UI server package

A `Server` struct holding: the brokerd HTTP client (unix-socket transport, reused from the CLI's `client.go` pattern), the audit root path, and the token. Exposes `Handler() http.Handler` (an `http.ServeMux`) so it is unit-testable without binding a port.

Routes:

| Method + path | Behavior |
|---|---|
| `GET /` and static assets | Serve the embedded SPA (`go:embed`). |
| `GET /api/tasks` | Proxy `GET /admin/tasks`. |
| `GET /api/pending` | Proxy `GET /admin/pending`. |
| `POST /api/approve/{id}` | Proxy `POST /admin/approve/{id}`. |
| `POST /api/deny/{id}` | Proxy `POST /admin/deny/{id}`. |
| `POST /api/kill/{id}` | Proxy `POST /admin/kill/{id}`. |
| `POST /api/submit` | Start a task on a **background context** (see "Submit decoupling"); return the task id immediately. |
| `GET /api/diff/{id}` | Read `<audit_root>/<id>.diff`; 404 if absent. `text/plain`. |
| `GET /api/logs/{id}` | Read `<audit_root>/<id>.jsonl`; supports a re-fetch for live tailing. 404 if absent. |
| `GET /api/history` | List `<audit_root>/*.jsonl`, parse each terminal record → `{id, repo, instruction, outcome, cost_usd, duration_ms, started/mtime}`. |

Cross-cutting middleware on every `/api/*` route:
- **Token gate:** require the token (via `Authorization: Bearer <token>` header OR a `t` cookie the SPA sets from the launch URL). Missing/wrong → `403`. Skipped entirely when `--no-token`.
- **Host check:** reject requests whose `Host` is not `127.0.0.1[:port]` or `localhost[:port]` → `403` (DNS-rebinding defense).
- **`{id}` path-safety:** task ids are validated against a strict charset (the broker's id format — hex/alphanumeric + `-`); reject anything else BEFORE building a filesystem path, so `/api/diff/../../etc/passwd` cannot traverse.

### 3. Shared audit parsing — `internal/audit/`

The terminal-record parsing currently lives in `cmd/drydock/tasks.go` (`auditResult`, `auditMeta`, the per-file summarizer). Extract the pure parsing (one `<id>.jsonl` → a summary struct) into a small `internal/audit` package so both `cmd/drydock/tasks.go` and `internal/webui` use one implementation. This is a targeted DRY improvement, not a rewrite: `cmd/drydock/tasks.go`'s display/formatting stays; only the parse-one-file logic moves and is imported back.

### 4. SPA — `internal/webui/assets/`

Vanilla HTML/CSS/JS, embedded. On first load it reads `?t=<token>` from the URL, stores it (cookie + in-memory), and sends it on every `/api/*` call; it then strips the token from the visible URL. Four views:

- **Board** (default): polls `/api/tasks` + `/api/pending` every ~1.5s. One row/card per task: id, repo, truncated instruction, stage badge, elapsed. Tasks at a gate float to the top:
  - stage `awaiting_egress` → **egress card** listing `egress_extra` domains, with Approve / Deny.
  - stage `awaiting_approval` → **diff card** with a "Review" action opening the Review view, plus inline Approve / Deny.
  - any live task → **Kill** action.
- **Review**: fetches `/api/diff/{id}`, renders it with unified-diff coloring; tails `/api/logs/{id}` for the live trace; Approve / Deny buttons (POST to the proxy).
- **Submit**: a form mirroring `drydock submit` — repo as an **https/git/ssh URL** (brokerd rejects local paths: `gitURLRef` at `broker.go`), instruction, agent + model picker, optional budget/flags — POSTs `/api/submit`, then routes to the Board to watch it.
- **History**: `/api/history` table — past tasks with outcome, cost, duration; clicking one opens its diff + logs (read-only).

## Submit decoupling (critical)

brokerd derives each task's context from the inbound `POST /tasks` request (`broker.go:379`: `context.WithCancel(r.Context())`) — **a client disconnect cancels the task.** Therefore the UI server, NOT the browser, must own that long-lived connection:

1. On `POST /api/submit`, the UI server dials brokerd `POST /tasks` using a request built with **`context.Background()`** (never the inbound browser request's context) and **no client timeout** (the admin client's 5s timeout would abort mid-task).
2. brokerd streams a newline-delimited event log; the **first line is `{"event":"accepted","task_id":...}`**. The UI server reads that one line, then returns `{"id": "<task_id>"}` to the SPA immediately.
3. The UI server keeps draining the stream to completion **in a background goroutine** (keeping the connection — and thus the task — alive regardless of what the browser does), discarding the body (state is observed via polling + the audit log). The goroutine ends when brokerd closes the stream.
4. The SPA never holds the task open; it gets the id and switches to Board polling. Closing the tab does not kill the task.

This is the single trickiest part of the implementation; the tests must assert that aborting the inbound `/api/submit` request does not cancel the brokerd task.

## Data flow

1. Operator runs `drydock ui --open`. Browser opens the token URL; SPA captures + stores the token.
2. **Submit:** SPA `POST /api/submit` → UI server starts the task on a background context, returns the task id (see "Submit decoupling"); the task appears in `/admin/tasks` immediately and the SPA watches it via Board polling.
3. **Monitor:** Board polling reflects stage transitions.
4. **Gate (egress):** task enters `awaiting_egress`; egress card shows domains; Approve → `POST /api/approve/{id}` → brokerd `signal`.
5. **Gate (push):** task enters `awaiting_approval`; diff card / Review view shows `<id>.diff`; Approve/Deny likewise.
6. **Completion:** task leaves the live set; its outcome + cost are read from the audit terminal record in History.

## Error handling

- brokerd socket unreachable (brokerd not running): `/api/*` proxy routes return a clear `502` with a body the SPA renders as "brokerd not running — run `drydock start`". The Board shows this state rather than erroring blankly.
- Missing diff/log file: `404`; the SPA shows "no diff yet / no logs yet".
- Bad token / bad Host / bad id: `403` (token/host) / `400` (id), with terse bodies. The SPA, on a `403`, shows "open the UI from the `drydock ui` link" rather than silently failing.
- Submit validation errors from brokerd (e.g. unknown agent, 400) are surfaced verbatim in the Submit view.
- Port already in use: `drydock ui` exits non-zero with a clear message naming the port.

## Security model

- **Loopback bind only** (`127.0.0.1`) — never `0.0.0.0`. No network exposure.
- **Token-gated** `/api/*` (default on) — guards against other local users / processes and CSRF-style cross-site calls (a random bearer the attacker can't read).
- **Host-header check** — blocks DNS-rebinding (a malicious page resolving a hostname to 127.0.0.1 still fails the Host allowlist).
- **No brokerd change** — the socket-only default and its file permissions remain the real boundary; the UI server is an explicit, ephemeral, same-user front door to it.
- **Path-safety** on all `{id}` → filesystem reads.
- The UI inherits brokerd's existing gates (push/egress still require explicit approval); the UI never auto-approves.

## Testing

- **UI server (`internal/webui`)** via `httptest`:
  - token enforcement: `/api/tasks` → 403 without token, 200 with; `--no-token` mode skips the gate.
  - Host-header rebinding: a non-loopback `Host` → 403.
  - `{id}` path-safety: `/api/diff/..%2f..` and similar → 400, no file read outside the audit root.
  - proxy correctness: stand up a fake brokerd on a temp unix socket; assert `/api/tasks`, `/api/approve/{id}`, `/api/submit` round-trip method+path+body+status.
  - **submit decoupling:** with a fake brokerd that emits `accepted` then blocks, assert `/api/submit` returns the task id promptly AND that aborting the inbound `/api/submit` request does not close the brokerd-side connection (the background goroutine keeps it open); assert the submit proxy uses a no-timeout, background-context request.
  - audit reads: temp audit dir with crafted `.diff` / `.jsonl`; assert `/api/diff`, `/api/logs`, `/api/history` return expected content and the history parse matches `drydock tasks`.
  - brokerd-down: socket path absent → `/api/tasks` returns 502 with the documented body.
- **`internal/audit`** parsing: table-driven over success/error/interrupted/missing-result fixtures (mirrors existing `tasks.go` cases; move them with the code).
- **`cmd/drydock/ui.go`**: token minting (non-empty, hex), URL formatting, `--no-token` omits the token; binding/serving is covered by the `internal/webui` handler tests (the command is a thin wrapper).
- **SPA**: kept logic-light; no JS test harness (consistent with the no-build ethos). Any non-trivial pure helper (e.g. diff-line classification) is small enough to keep correct by inspection; if it grows, it moves server-side where it can be tested in Go.

## Out of scope (fast-follows / explicitly deferred)

- Live per-task cost on the board (needs a brokerd change to expose lease spend).
- SSE/WebSocket streaming (polling is sufficient for single-user).
- Syntax-highlighted diffs (would require vendoring a JS highlighter).
- Auth beyond the loopback token (multi-user / remote access).
- Serving the UI from brokerd's TCP listener.
