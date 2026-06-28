# drydock web UI — UX refinement design

- **Date:** 2026-06-27
- **Status:** Design approved (look-and-feel); implementation plan pending
- **Scope:** `internal/webui/assets/` only (the embedded SPA + CSS). **No Go / broker / server changes.**
- **Branch:** `feat/web-ui`

## Problem

The web UI (PR #85) has solid bones — dependency-free vanilla SPA, clean terminal
aesthetic, and genuinely good safety UX (approve-disabled-until-reviewed,
`confirm()` on deny/kill, token in fragment + cleared). But it's thin in four
areas, confirmed by driving it end-to-end:

1. **Interaction friction.** The board does a full `app().replaceChildren()`
   **every 1.5s** — it flickers, drops scroll position, and can interrupt a
   click. No `Esc`/backdrop to close the diff overlay; no keyboard shortcuts;
   native `alert()`/`confirm()`; copying a task id gives no feedback.
2. **Live insight is shallow.** "running" is just a badge + elapsed, even though
   `/api/logs` has turns/cost/last-action. A failed task vanishes into History
   with **no visible reason**. There's no way to view the agent's transcript.
3. **Visual polish is spartan.** Flat cards, the all-important approval gate
   isn't visually dominant, plain-text loading/empty states, text-only status.
4. **Submit is minimal.** No client-side repo validation, free-text model field,
   no recent-repos memory.

## Goals & constraints

Refine all four areas while honoring the project's ethos:

- **Dependency-free, no build step.** Vanilla JS + CSS, served as static assets
  via the existing `go:embed` file server. No npm, no framework, no bundler.
- **Evolution, not reskin.** Keep the dark, **all-monospace** terminal identity.
- **FE-only.** Every feature is satisfiable from the existing API
  (`/api/tasks|pending|history|diff|logs|widen|submit|approve|deny|kill`). No
  server, broker, or endpoint changes.

## Approach (decided)

Targeted vanilla refactor + a small design system. The load-bearing change is
reworking the board's render loop to **reconcile by task id** (update cards in
place) instead of nuking the DOM each poll — that's what kills the flicker and
unlocks smooth live updates. (Rejected: a reactive lib like Preact — it adds a
JS dependency + build step to a deliberately vanilla, supply-chain-conscious
project.)

## Design system

A refined evolution of the current palette (dark, mono, fully flat — no
gradients/glow). Centralize as CSS custom properties at the top of `style.css`:

```
--bg:#0d0f13  --surface:#161922  --surface-2:#1b1f27  --line:#262c38  --line-2:#2e3543
--fg:#e6e8eb  --muted:#8b919c  --faint:#5a606b
--green:#3fb950 (ok / connected / added)   --amber:#e3a008 (gate / awaiting)
--red:#f85149 (danger / deleted / error)   --blue:#58a6ff (running / info / file-head)
--radius:8px  --radius-card:10px   type scale: 11 / 12 / 12.5 / 13px   weights: 400 / 500
```

Component vocabulary (CSS classes + dom helpers): `card`, `pill`/`badge`,
`btn` (`.btn`, `.btn.ok`, `.btn.danger`), `gate` block, `chip`, `tab`, `toast`.
All-monospace throughout (identity). Two weights only.

## Architecture

### File structure

The current single `app.js` (~315 lines) grows to ~600+ with these features.
**Resolved (was open):** keep it a **single, clearly-sectioned `app.js`** rather
than splitting into files.

> **Why not split into ES modules?** Modules require the server to send a
> JavaScript MIME type for each file, and Go's `http.FileServer` resolves
> `.js`'s `Content-Type` from the *host* mime table — on some operator machines
> that isn't module-compatible, which *silently* breaks `import`. Fixing it
> needs a Go change (setting the header), violating the FE-only boundary.
> Classic `<script>` files dodge the MIME issue but reintroduce global-scope
> coupling + load-order fragility. For a cohesive ~600-line SPA, one
> well-organized file is simpler and robust; split into classic ordered
> `<script>` files only if a later feature makes it unwieldy.

Organize `app.js` into clearly-banner'd sections, each a self-contained unit with
a documented interface, and put a `:root` tokens block at the top of `style.css`:

| Section in `app.js` | Responsibility |
|---|---|
| api | `api()`/`apiJSON()`, token header, error classification (auth vs down) |
| dom + design system | `el()`, formatters, `toast()`, copy-with-feedback, component helpers |
| keys | keyboard registry (see interaction layer) |
| board | render + **reconcile-by-id**, task card, gates, live progress |
| review | diff/logs overlay (tabs, shortcuts) |
| submit | form (validation, recent repos, model picker) |
| history | history table |
| boot | token capture, view router, poll orchestration |

### Render loop — reconcile by id (kills the flicker)

The board section keeps `Map<taskId, {el, sig}>`. On each poll:

- **New task** → build a card, insert at its sorted position (gate tasks float
  to top, as today).
- **Existing task** → compute a small signature from **stage + the live-progress
  fields** (turns/cost/action) — deliberately *not* age. If it changed, update
  only those nodes in place (badge text, live-progress line) — never rebuild the
  card. Age is owned by the 1s ticker below, so it never enters the signature.
- **Gone task** → remove its card (it surfaces in the "Just finished" strip).
- **Reorder** only when gate membership changes (minimal `insertBefore` moves).

Reconcile preserves transient UI state: keyboard focus and an open inline
"Confirm?" affordance sit on nodes the signature-driven update doesn't rebuild,
so a poll never clobbers an in-progress interaction.

A separate **1s ticker** updates just the `age`/elapsed spans, decoupled from
the 1.5s data poll, so elapsed time ticks smoothly. Result: no flicker, no
scroll-jump, no interrupted clicks.

## Feature design

### Board

- **Running card — live progress.** For each running task, fetch
  `/api/logs/<id>` (throttled: running tasks only, ~every other poll) and parse:
  turn count, latest `total_cost_usd`, and the **last tool-use → a short action
  label** (`Edit`/`Write` → "editing <file>", `Bash` → "running a command",
  `Read` → "reading <file>", else the tool name). Render
  `claude · 3 turns · $0.04 · editing client.go` + a CSS-only indeterminate
  progress bar. Turns/cost/elapsed are cheap; the action label is best-effort,
  parsed from claude's stream-json — codex/opencode emit a thinner trace and fall
  back to turns/cost/elapsed.
  *(Efficiency note: `/api/logs` returns the whole jsonl; acceptable for
  ≤`max_concurrent` running tasks, ~2. A future `Range`/tail optimization is
  out of scope.)*
- **Approval gate — visually dominant.** The card gets an amber-tinted full
  border (not a single-side bar) + an amber gate block: "Push awaiting review",
  `+N −M · K files · spent $X`, and `Review diff` / `Approve push` (enabled only
  after review, as today) / `Deny`, plus an inline shortcut hint
  `R review · A approve · D deny`.
- **"Just finished" strip — visible failure reason.** For recently-finished
  tasks, show outcome inline; for **failed** ones, derive the reason
  client-side from `/api/logs/<id>` (the terminal `result` event's error/result
  text), e.g. `error · 502 credential unavailable`. Best-effort, fetched only
  for the few shown failures.
- **Connection status** becomes a dot + label; the auth-vs-down distinction from
  the earlier `0ee639f` fix is preserved.

### Review overlay (Diff / Logs)

- **Tabs:** `Diff` (default) and **`Logs`** (new). Diff reuses the existing
  line-colored renderer + `+X/−Y` stat. Logs fetches `/api/logs/<id>` and renders
  a **compact transcript**: parse known stream-json event types (assistant text,
  tool calls, results, system) into a readable, colorized view; unknown lines
  shown raw. (Depth is best-effort and can iterate; raw-jsonl is the floor.)
- **Dismiss:** `Esc` and backdrop-click close the overlay (today: neither).
- **Shortcuts:** when not read-only, `A` approve, `D` deny; `Esc` always closes.
  Footer shows the hint.

### Submit

- **Repo URL validation** (client-side): accept `https`/`git`/`ssh` URL shapes,
  reject local paths; show a green check when valid, a muted hint when not; block
  submit with an inline message if invalid (no native dialogs).
- **Recent repos:** persist the last ~5 `repo_ref`s in `localStorage`; render as
  clickable chips under the field; update on successful submit.
- **Model picker:** an `<input>` backed by a `<datalist>` of suggested model ids
  (empty = "broker default", plus a small hardcoded set of common ids) — a picker
  with free-text fallback, robust to model churn.
- **Feedback:** submit status as inline message / `toast()`, not `alert()`.

### Interaction layer (cross-cutting)

- **Keyboard registry** dispatched by current view + context:
  - Board: when **exactly one** task is at a gate, `R` review · `A` approve ·
    `D` deny. Overlay open: `A`/`D`/`Esc`. Submit: `Cmd/Ctrl+Enter` submits.
    `?` toggles a small shortcuts cheatsheet.
  - **Suppressed while typing:** the handler ignores single-key shortcuts when the
    focused element is an `input`/`textarea`/`select` or is `contenteditable`, so
    keystrokes in the submit form (or any field) are never hijacked. The
    `Cmd/Ctrl+Enter` submit chord is the deliberate exception.
- **Copy feedback:** clicking a task id copies it and shows a transient `toast()`
  ("copied 78633fdc…") — today it's silent.
- **Replace native dialogs:** deny/kill use a two-step **inline confirm** (the
  button becomes "Confirm?" → second click acts) instead of `confirm()`; errors
  use `toast()` instead of `alert()`.
- **States:** refined empty / loading / error states (icon + one-line copy)
  across board, overlay, submit, history.

## Testing

The repo has **no JS test harness** (no node/build, by design), so verification
is browser-driven (Playwright), covering: board reconcile (assert no full
re-render / scroll preserved across polls), gate shortcuts, overlay tabs + `Esc`,
submit validation + recent-repo chips + model datalist, copy-toast, inline
confirm, and the empty/error states. Pure helpers (formatters, repo-URL
validation, diff parsing, log-action parsing) are factored to be unit-testable
if a JS harness is ever added. The existing Go `internal/webui` server tests are
unaffected and must stay green.

## Scope

**In scope (this spec):** the design system; the reconcile render loop + live
progress; the Diff/Logs overlay; submit validation/recent-repos/model picker; the
keyboard + copy-feedback + inline-confirm interaction layer; refined states.

**Out of scope:** any reactive framework / build step; SSE/WebSocket streaming
(polling + reconcile is smooth enough); a full side-by-side or syntax-highlighted
diff viewer; server/broker/API changes; auth changes.

## Suggested phasing (for the plan)

1. Design system (tokens + component CSS + dom helpers + `toast`).
2. Reconcile render loop + live progress (board no-flicker).
3. Review overlay (Diff/Logs tabs + shortcuts + dismiss).
4. Submit refinements.
5. Interaction layer (keyboard, copy-feedback, inline confirm).
6. States polish + browser verification.

## Resolved decisions

- **Typography:** all-monospace (terminal identity) — kept.
- **File structure:** a single, well-sectioned `app.js` + `style.css`, served as
  static assets. No ES modules — Go's file-server `.js` MIME is host-dependent and
  can silently break module `import`; classic scripts would dodge that but couple
  the global scope. One file is simplest and robust at this size.
- **Live "current action":** included (best-effort from `/api/logs`; claude only,
  others fall back to turns/cost/elapsed).
- **Surface area:** FE-only — no Go/broker/server/API changes.
- **Keyboard:** single-key shortcuts suppressed while a text field is focused.
