# drydock Web UI — Design Spec

**Status:** Approved (brainstorming), revised after PM + staff-engineer review — 2026-06-27
**Goal:** A local web UI that is a complete browser alternative to the `drydock` CLI: submit tasks, monitor them live, review the push diff and approve/deny/kill, approve egress-widening, and browse history — with the approval gate as the marquee, well-designed moment.

## Summary of decisions

| Decision | Choice |
|---|---|
| Primary purpose | Full control center (submit + monitor + approve/deny/kill + egress + history) |
| Access model | `drydock ui` loopback-only server, token-gated (header-bearer for actions) |
| Frontend stack | Vanilla HTML/CSS/JS, `go:embed`'d, no npm/bundler/framework |
| v1 scope | All four: Board, Diff review + approve/deny/kill, Submit, History |
| Submit lifetime | **Small brokerd change:** detach task context from the submit request; kill only via `/admin/kill`. Removes the "closing the UI kills my task" wart and hardens CLI submit too. |
| Real-time | Polling (~1.5s; ~0.5s while any task is at a gate) + optimistic update on actions; no streaming endpoint |
| Cost | Live *board* cost deferred; **cost + budget shown at the approval gate** (parsed from the live audit jsonl — no brokerd change) |
| Destructive actions | Deny + Kill require confirm/undo; Approve only after the diff is opened |
| Diff rendering | Client-side unified-diff coloring + per-file headers/collapse + `+X/−Y` stats; no vendored syntax highlighter |

## Background — what already exists

- **brokerd is already an HTTP/JSON server** over a per-user unix socket (optional TCP via `broker.addr`, explicitly no-auth — the UI does NOT use it). Routes (`cmd/brokerd/main.go:318-325`): `POST /tasks`; `POST /admin/{approve,deny,kill}/{id}`; `GET /admin/pending` (→ `[]string` ids at a gate); `GET /admin/tasks` (→ `[]TaskState`); `GET /healthz`.
- **`TaskState`** (`broker.go:181`): `id, repo, instruction, stage, started_at, egress_extra`. **No cost field.** `egress_extra` is populated only while at the egress gate and **cleared the instant it resolves** (`broker.go:409-411`), with `omitempty`.
- **Stages** (`broker.go:172-175`): `awaiting_egress`, `running`, `awaiting_approval` (push gate), `pushing`.
- **Two distinct result vocabularies — do not conflate (review finding B2):**
  - The **streamed HTTP** protocol emits `{"event":"result","outcome":"pushed|denied|cancelled|no_diff"}` (`broker.go:583,606,630`). This is **never written to disk.**
  - The **on-disk `<id>.jsonl`** terminal line is `{"type":"result","subtype":"success|error|interrupted","is_error":bool,"total_cost_usd","duration_ms","num_turns"}` (written by the agent, or synthesized at `broker.go:549,572`, or by the boot reconciler `reconcile.go`). The `.jsonl` has **no `outcome` field** — outcome is *derived* from `subtype`/`is_error` exactly as `cmd/drydock/tasks.go:summarize()` does, and cost is subscription-aware (`tasks.go:costCell` shows the literal `"subscription"`, not a dollar amount, when metering is off — keyed off the `drydock_meta` first line).
- **The push diff is persisted to disk** (`<audit_root>/<id>.diff`, mode 0600, `broker.go:741-744`); `drydock review` reads it directly (`diffPath()` at `cmd/drydock/util.go:58`). The egress-widen request is persisted to `<audit_root>/<id>.widen.json` (`broker.go:668`).
- **Submit error responses are synchronous and pre-stream:** brokerd returns plain non-200 (`http.Error`) for bad repo (`400`, `broker.go:362`; repos must be https/git/ssh URLs — `gitURLRef`, no local paths), concurrency cap (`503`, `broker.go:366`), invalid egress (`400`, `broker.go:388`) — all **before** any `accepted` event. The CLI checks `resp.StatusCode >= 400` before reading the stream (`submit.go:248`).
- Task ids are `newID()` = 16 random bytes hex = exactly `[0-9a-f]{32}` (`broker.go:239`). The audit dir is `0700`, files `0600`.
- The CLI talks to brokerd over the socket via `cmd/drydock/client.go` (note: admin client has a 5s timeout; `brokerdDown()` keys off the socket path).

The architectural fact: **the UI server runs on the same machine as brokerd and the audit dir**, so it reads diffs/logs/history directly and proxies the socket only for live state + actions.

## brokerd change — detach task lifetime from the submit request

Today brokerd derives each task's context from the inbound request: `taskCtx, cancel := context.WithCancel(r.Context())` (`broker.go:379`), so a client disconnect cancels the task. Change it to a **broker-owned context** independent of the request:

- `taskCtx, cancel := context.WithCancel(b.rootCtx)` (a broker-root context, so `CancelAll` on shutdown still reaches it) — NOT `r.Context()`.
- The stored `cancel` (via `registerTask`) continues to back `/admin/kill` unchanged. **Kill is now the only way to cancel a task**; client disconnect does not.
- Event streaming to the response writer becomes **best-effort**: if the client has gone, writes fail — the handler must swallow the write error and let the task run to completion (the `<id>.jsonl` audit log remains the source of truth). The run loop must not key off `r.Context()` for liveness.
- **Behavior change (document in release notes + `drydock submit` help):** Ctrl-C'ing `drydock submit`, or closing the browser/UI, no longer cancels the task — it keeps running; reattach via `drydock tasks`/`logs` or the UI Board, cancel via `drydock kill`/the UI. This is consistent with the existing crash-recovery reconciler (a task already survives a brokerd restart's reconciliation).
- **Tests:** disconnect the client mid-task → task still reaches a terminal `result` in the audit log; `/admin/kill` still cancels; `CancelAll` (shutdown) still cancels. Update any existing test that asserts disconnect-cancellation.

## Architecture

```
browser ──TCP 127.0.0.1:PORT──▶ drydock ui server ──┬─ unix socket ─▶ brokerd   (live state + actions)
        (token-gated, header)                        └─ reads <audit_root>/*.{jsonl,diff,widen.json}
```

The `drydock ui` server is loopback-only; it serves the embedded SPA, proxies an allow-listed set of brokerd endpoints over the socket, and reads the audit dir directly. With the detach change above, the UI never holds a task open.

## Components

### 1. `cmd/drydock/ui.go` — the `drydock ui` command

```
drydock ui [--port N] [--open] [--no-token]
```

- Resolves audit root + socket path from config (reuse the existing resolution, not a copy).
- Mints a random token (32 bytes hex) unless `--no-token`.
- Binds `127.0.0.1:PORT` (default `7878`; if taken, exit non-zero naming the port — no auto-scan).
- Prints `UI ready: http://127.0.0.1:PORT/#t=<token>` (token in the URL **fragment**, never the query — fragments are not sent in `Referer` or server logs). `--no-token` omits it and prints a loud warning that any local process/page can drive privileged actions.
- `--open` launches the browser (macOS `open`). Runs until Ctrl-C; clean shutdown (does not affect running tasks, given the detach change).

### 2. `internal/webui/` — the UI server package

A `Server` struct holding: the brokerd socket client (reuse the `client.go` transport + `brokerdDown` logic), the audit root, and the token. Exposes `Handler() http.Handler` (an `http.ServeMux`) — port-free, `httptest`-able.

| Method + path | Behavior |
|---|---|
| `GET /` + assets | Serve embedded SPA via `http.FS` over the `go:embed` FS (no `http.Dir`, no traversal). |
| `GET /api/tasks` | Proxy `GET /admin/tasks`. |
| `GET /api/pending` | Proxy `GET /admin/pending`. |
| `POST /api/approve/{id}` | Proxy `POST /admin/approve/{id}`. |
| `POST /api/deny/{id}` | Proxy `POST /admin/deny/{id}`. |
| `POST /api/kill/{id}` | Proxy `POST /admin/kill/{id}`. |
| `POST /api/submit` | Start a task (see "Submit flow"); reject `auto_approve:true`. |
| `GET /api/diff/{id}` | Read `<audit>/<id>.diff`; 404 if absent; `text/plain`. |
| `GET /api/logs/{id}` | Read `<audit>/<id>.jsonl` (re-fetch for tailing; tolerate an unterminated final line). |
| `GET /api/widen/{id}` | Read `<audit>/<id>.widen.json` for the egress card detail (the live `egress_extra` snapshot may already be cleared). |
| `GET /api/history` | List+parse `<audit>/*.jsonl` via `internal/audit`. |

Cross-cutting middleware on `/api/*`:
- **Token gate (default on):** **all** `/api/*` routes require the token in the **`Authorization: Bearer` header** — no cookie is used for auth anywhere (a cookie-borne token is CSRF-forgeable, finding I3). The SPA holds the token in memory and sends it on every fetch. Missing/wrong → `403`. `--no-token` skips the gate but keeps every other check.
- **Origin/Host checks:** reject `/api/*` whose `Host` is not `127.0.0.1[:port]`/`localhost[:port]` (DNS-rebinding), and reject a present `Origin` that is not the loopback origin (CSRF defense-in-depth). → `403`.
- **`{id}` safety:** validate against the anchored regex `^[0-9a-f]{32}$` before building any path. After `filepath.Join`, `filepath.Clean` and verify the audit-root prefix; open audit files with `O_NOFOLLOW` (or `Lstat` reject symlinks) so a planted symlink can't escape. → `400` on bad id.

### 3. Shared audit parsing — `internal/audit/`

Extract the **on-disk** parse + derivation from `cmd/drydock/tasks.go` (`auditResult`/`auditMeta` decode, `summarize()` outcome derivation, subscription-aware `costCell`, the tail-read that tolerates a partial last line) into `internal/audit`. Both `cmd/drydock/tasks.go` and `internal/webui` import it. The move is behavior-preserving: `tasks.go` keeps its table formatting; the existing `tasks_test.go` fixtures are re-pointed at the new package and must still pass. The History API returns, per task: `id, repo, instruction, outcome (derived), cost (string — a dollar figure OR "subscription"), duration, started/mtime`.

### 4. SPA — `internal/webui/assets/`

Vanilla HTML/CSS/JS, embedded. On load it reads `#t=<token>` from the fragment, stores it in memory, and `history.replaceState`s to strip it **before** any `/api/*` or asset fetch; the served HTML carries `Referrer-Policy: no-referrer`. Views:

- **Board** (default): polls `/api/tasks` + `/api/pending` (interval ~1.5s normally, ~0.5s while any task is at a gate). One card per task: id (click-to-copy), repo, truncated instruction, stage badge, elapsed. Tasks at a gate sort to the top; **a card's position is pinned for ~2s after hover/focus** so a re-sort can't move a click target.
  - `awaiting_egress` → **egress card**: shows the requested hosts (from `/api/widen/{id}`) **plus the instruction + repo** so the operator can judge *why*; Approve / Deny.
  - `awaiting_approval` → **diff card**: "Review" opens the Review view; shows **spent-so-far + budget** (parsed from `/api/logs/{id}`'s latest cost line and the per-task budget). Approve is enabled only after the diff has been opened; Deny requires confirm.
  - any live task → **Kill** (with confirm/undo).
  - Empty board → a real empty state linking to Submit/History (not a blank page).
  - A task that reaches terminal state stays briefly in a **"just finished"** zone with its outcome+cost before aging into History — completed tasks never silently vanish.
- **Review**: `/api/diff/{id}` rendered with unified-diff coloring, per-file headers/collapse, and a `+X/−Y` summary; tails `/api/logs/{id}`; shows spent/budget; Approve / Deny (Deny confirmed).
- **Submit**: form mirroring `drydock submit` — repo as **https/git/ssh URL** (no local paths), instruction, agent + model picker (agents from the provider registry), optional budget/egress. POSTs `/api/submit`; on success highlights/scrolls to the new task on the Board; on error renders brokerd's message verbatim (bad repo, queue-full 503, bad egress). **Re-run:** a History row can prefill this form.
- **History**: `/api/history` table — outcome, cost (or "subscription"), duration; a row opens its diff + logs read-only and can prefill Submit.

## Submit flow (with the detach change)

1. SPA `POST /api/submit` with the token header. UI server **rejects `auto_approve:true`** (the UI's purpose is a human at the gate) and forwards the rest.
2. UI server dials brokerd `POST /tasks` with a background context and **no short timeout**.
3. **If `resp.StatusCode >= 400`** (bad repo / slot full / bad egress): copy brokerd's status+body straight back to the SPA as the submit error and return — **no stream read, no goroutine** (finding B1).
4. On `200`: read the **first NDJSON line**; expect `{"event":"accepted","task_id":...}` → return `{"id":...}` to the SPA. If the first post-accept event is instead `{"event":"error",...}` (e.g. mint failure), surface that. Because the task no longer depends on the connection, the UI server then **closes the brokerd connection** — the task runs independently and is observed via polling + audit.

## Real-time & actions

Polling (no SSE). On any action POST (approve/deny/kill), the SPA **optimistically updates** the card, disables the button until the response, and **immediately re-polls** rather than waiting the interval — so the gate feels instant without a streaming endpoint. macOS notifications (already built) remain the out-of-band "you're needed" nudge; the notification path should deep-link to the Review view (stable URL — see Security/token lifetime).

## Error handling

- brokerd socket unreachable: proxy routes return `502` with a body the SPA renders as "brokerd not running — run `drydock start`" (reuse `brokerdDown`).
- Missing diff/log/widen file: `404` → "no diff/logs yet".
- `403` (token/host/origin) → "open the UI from the `drydock ui` link"; `400` (bad id) terse.
- Approve/deny race: brokerd returns `409` "already signaled" (still pending) or `404` (gate resolved/gone) — the SPA treats both as "already resolved, refreshing," not a hard error.
- Submit `503` (queue full) rendered as "concurrency limit reached — wait or raise the cap," not a crash.
- Torn reads: log/history parsing ignores an unterminated trailing line (the diff is written atomically once at the gate).

## Security model

- **Loopback bind only** (`127.0.0.1`), never `0.0.0.0`.
- **Header-bearer token** for state-changing routes (cookie tokens are CSRF-forgeable, I3); token delivered via URL **fragment** + `Referrer-Policy: no-referrer` to avoid Referer/log leakage (I4); residual argv visibility of `--open` documented, not hidden.
- **`--no-token`** kept for trusted single-user machines but gated behind a loud startup warning; it is NOT "localhost is safe" — the Host check is only a rebinding mitigation, not an auth boundary (I5).
- **`/api/submit` refuses `auto_approve`** so the UI can't be used (via CSRF or a local process) to push without a human (I6).
- **Path-safety:** anchored `^[0-9a-f]{32}$`, prefix re-check, `O_NOFOLLOW`/symlink rejection (I7). Embedded assets served from the embed FS (no traversal).
- **Token lifetime / re-entry:** the token persists for the life of the `drydock ui` process; running `drydock ui` while one is already up re-prints the live URL (so the notification deep-link and bookmarks resolve). brokerd's socket-only default and file modes remain the real boundary; the UI server is an explicit same-user front door.

## Discoverability

- `drydock init` final hints mention `drydock ui` (`init.go` "next:" block).
- `drydock status` prints the UI URL if a `drydock ui` server is running, else a hint to start one.
- `drydock pending`, when a task is at a gate, suggests `drydock ui` for browser diff review.

## Testing

- **`internal/webui`** via `httptest` + a fake brokerd on a temp unix socket:
  - token: state-changing POST with no `Authorization` header → 403; **cookie-only token → 403** (CSRF shape); valid header → 200; `--no-token` skips.
  - Host/Origin: non-loopback `Host` → 403; cross-origin `Origin` → 403.
  - `{id}`: `..%2f..`, non-hex, wrong length → 400; planted symlink `<hex>.diff` not followed.
  - proxy round-trips: `/api/tasks`, `/api/approve/{id}` method+path+body+status.
  - **submit happy path:** fake brokerd emits `accepted` → `/api/submit` returns the id and closes; task not tied to the request.
  - **submit error path (the critical one, B1):** fake brokerd returns `400`/`503` pre-accept → the body+status reach the SPA, **no goroutine spawned, no slot leaked**; and a `200` whose first event is `error` is surfaced.
  - **`auto_approve` rejected (I6):** `/api/submit` with `auto_approve:true` → rejected before reaching brokerd.
  - audit reads: temp audit dir with crafted `.diff`/`.jsonl`/`.widen.json` → `/api/diff`, `/api/logs`, `/api/widen`, `/api/history` return expected content; history parse matches `drydock tasks` for success/error/interrupted/subscription/still-running.
  - brokerd-down → `/api/tasks` 502 with the documented body.
- **brokerd detach:** client disconnects mid-task → task still reaches a terminal `result`; `/admin/kill` still cancels; existing disconnect-cancellation tests updated.
- **`internal/audit`:** table-driven over success/error/interrupted/missing-result/subscription fixtures (moved from `tasks_test.go`); `drydock tasks` output unchanged.
- **`cmd/drydock/ui.go`:** token minting (hex, non-empty), fragment-URL formatting, `--no-token` omits token + warns; binding covered by the handler tests.
- **SPA:** kept logic-light; no JS test harness. Any non-trivial pure helper (diff-line classification) stays small enough to verify by inspection, or moves server-side for Go testing.

## Out of scope (deferred)

- Live per-task cost **on the board** (needs a brokerd lease-spend endpoint). Cost at the gate + in History is in scope.
- SSE/WebSocket streaming (polling + optimistic update is enough for single-user).
- Syntax-highlighted diffs (would require vendoring a JS highlighter; unified-diff coloring + per-file structure ships instead).
- Auth beyond the loopback token (multi-user / remote).
- Serving the UI from brokerd's TCP listener.
