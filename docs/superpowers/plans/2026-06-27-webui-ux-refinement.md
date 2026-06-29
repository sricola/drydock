# drydock web UI — UX refinement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refine the embedded drydock web UI across interaction/keyboard, live task insight, visual polish, and submit flow — staying dependency-free and FE-only.

**Architecture:** A single, well-sectioned `internal/webui/assets/app.js` (vanilla, served as a static asset) plus a token-driven `style.css`. The board switches from a full `replaceChildren` every poll to **reconcile-by-id** (update cards in place), which kills the flicker and unlocks smooth live updates. New capability (live running progress, a Logs transcript, failure reasons) is read from the existing `/api/logs`.

**Tech Stack:** Vanilla ES (no modules — see constraints), HTML, CSS. Go `go:embed` serves the assets; verification is browser-driven (Playwright) since there is no JS test runner.

## Global Constraints

- **No dependencies, no build step.** Vanilla JS + CSS only. No npm, framework, or bundler.
- **No ES modules.** Go's `http.FileServer` resolves `.js` MIME from the host mime table, which can break module `import` silently. Use one classic-script `app.js`.
- **FE-only.** No Go / broker / server / API changes. Everything uses existing endpoints: `/api/tasks|pending|history|diff|logs|widen|submit|approve|deny|kill`.
- **All-monospace** (terminal identity). Two font weights only: 400 / 500. No font below 11px. Flat — no gradients/shadows/glow.
- **Reference spec:** `docs/superpowers/specs/2026-06-27-webui-ux-refinement-design.md`.

## The dev / verify loop (every task uses this)

The assets are embedded, so editing them does not affect a running server until you rebuild. Per task:

```bash
# 1. rebuild so the new assets are embedded
go build -o bin/drydock ./cmd/drydock
# 2. restart the UI (brokerd must already be running from `drydock start`)
lsof -ti tcp:7878 | xargs -r kill; sleep 1
PATH="$PWD/bin:$PATH" nohup ./bin/drydock ui --port 7878 >/tmp/dd-ui.out 2>&1 &
sleep 2; grep -o '#t=[a-f0-9]*' /tmp/dd-ui.out   # the tokenized URL to load
# 3. drive it: load http://127.0.0.1:7878/#t=<token> in Playwright; screenshot; assert
# 4. Go server tests stay green:
go test ./internal/webui/
```

> Load the UI via a fresh page load to the tokenized URL each time (the SPA reads the token from the fragment then clears it; an in-tab hash change won't re-read it).

## File structure

| File | Change | Responsibility |
|---|---|---|
| `internal/webui/assets/style.css` | rewrite | `:root` tokens block + component styles (cards, pill, btn, gate, tab, chip, toast, states) |
| `internal/webui/assets/app.js` | rewrite, sectioned | sections: `api` · `dom+ds` · `keys` · `board` · `review` · `submit` · `history` · `boot` |
| `internal/webui/assets/index.html` | minor | unchanged structure; it already loads `app.js` and `style.css` |

Section banners in `app.js`, in dependency order (classic script = one global scope; define before use):
`// === api ===`, `// === dom + design system ===`, `// === keys ===`, `// === board ===`, `// === review ===`, `// === submit ===`, `// === history ===`, `// === boot ===`.

---

## Task 1: Design system — tokens, dom helpers, toast, copy feedback

Foundation everything else consumes. No behavior change yet beyond a visual refresh + a working `toast()` and copy-with-feedback.

**Files:**
- Modify: `internal/webui/assets/style.css` (tokens + component classes)
- Modify: `internal/webui/assets/app.js` (`dom + design system` section: keep `el`, formatters; add `toast`, `copyId`)

**Interfaces produced (used by later tasks):**
- `el(tag, attrs, ...kids) -> HTMLElement` (unchanged)
- `toast(msg: string, kind?: "ok"|"bad") -> void`
- `copyId(id: string) -> void`  // copies + toasts "copied <short>"
- `shortId(id) -> string` (first 12 chars), `fmtAge`, `fmtAgeFromUnix`, `elapsed`, `fmtDurMs` (existing)
- CSS classes: `.card`, `.pill`, `.badge`, `.btn`/`.btn.ok`/`.btn.danger`, `.gate`, `.gate.dominant`, `.tab`/`.tab.on`, `.chip`, `.toast`, `.empty`, `.kbd`

- [ ] **Step 1: Rewrite `style.css` tokens + components**

Replace the top of `style.css` with the token block and refreshed components (keep existing diff/history/form classes, retune to tokens):

```css
:root{
  --bg:#0d0f13; --surface:#161922; --surface-2:#1b1f27; --line:#262c38; --line-2:#2e3543;
  --fg:#e6e8eb; --muted:#8b919c; --faint:#5a606b;
  --green:#3fb950; --amber:#e3a008; --red:#f85149; --blue:#58a6ff;
  --radius:8px; --radius-card:10px;
}
*{box-sizing:border-box}
body{margin:0;font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;background:var(--bg);color:var(--fg)}
header{display:flex;align-items:center;gap:18px;padding:11px 16px;border-bottom:1px solid var(--line)}
.brand{font-weight:500;letter-spacing:.3px}
nav button{background:none;border:none;color:var(--muted);cursor:pointer;font:inherit;padding:4px 2px}
nav button.active{color:var(--fg);border-bottom:2px solid var(--green)}
.conn{margin-left:auto;display:flex;align-items:center;gap:6px;font-size:12px}
.conn::before{content:"";width:7px;height:7px;border-radius:50%;background:var(--muted)}
.conn.ok{color:var(--green)} .conn.ok::before{background:var(--green)}
.conn.bad{color:var(--red)} .conn.bad::before{background:var(--red)}
main{padding:16px;max-width:900px;margin:0 auto}
.board{display:flex;flex-direction:column;gap:11px}
.card{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius-card);padding:12px 14px}
.card.gate-host{border-color:#3a3320}
.card-head{display:flex;align-items:center;gap:9px}
.tid{cursor:pointer;color:var(--muted);font-size:12px} .age{margin-left:auto;color:var(--muted);font-size:12px}
.repo{margin-top:6px} .instr{color:var(--muted);font-size:12.5px;margin-top:2px;white-space:pre-wrap}
.live{margin-top:8px;color:var(--muted);font-size:12px}
.bar{height:3px;background:var(--surface-2);border-radius:2px;overflow:hidden;margin-top:8px}
.bar>i{display:block;height:100%;width:38%;background:var(--blue);border-radius:2px;animation:ddb 1.9s ease-in-out infinite}
@keyframes ddb{0%{margin-left:-40%}100%{margin-left:100%}}
.badge,.pill{font-size:11px;border-radius:10px;padding:1px 8px;border:1px solid var(--line)}
.stage-running{color:var(--blue);border-color:#274b6b}
.stage-awaiting_approval,.stage-awaiting_egress{color:var(--amber);border-color:#7a5a12}
.stage-pushing{color:var(--muted)}
.gate{margin-top:10px;padding:10px 12px;border:1px solid var(--line);border-radius:8px}
.gate.dominant{background:#1a1710;border-color:#3a3320}
.gate-title{font-weight:500;color:var(--amber);margin-bottom:6px}
.actions{display:flex;gap:8px;margin-top:8px;align-items:center;flex-wrap:wrap}
button,.btn{background:var(--surface-2);color:var(--fg);border:1px solid var(--line-2);border-radius:6px;padding:5px 11px;cursor:pointer;font:inherit;font-size:12px}
.btn.ok{border-color:#2c5a33;color:var(--green)} .btn.danger{border-color:#5a2a2a;color:var(--red)}
.btn.confirming{border-color:var(--red);color:var(--red)}
button[disabled]{opacity:.45;cursor:not-allowed}
.kbd{margin-left:auto;color:var(--faint);font-size:11px}
.empty{color:var(--muted)} .muted{color:var(--muted)} a{color:var(--green)}
.chip{font-size:11px;color:var(--muted);background:var(--surface);border:1px solid var(--line-2);border-radius:12px;padding:2px 9px;cursor:pointer}
.toast{position:fixed;left:50%;bottom:18px;transform:translateX(-50%);background:var(--surface-2);border:1px solid var(--line-2);border-radius:8px;padding:7px 13px;font-size:12px;z-index:50}
.toast.bad{border-color:#5a2a2a;color:var(--red)}
.recent{margin-top:16px;border-top:1px solid var(--line);padding-top:10px}
.recent-title{color:var(--faint);font-size:11px;margin-bottom:4px}
.recent-row{display:flex;gap:10px;align-items:center;padding:4px 0;color:var(--muted);font-size:12.5px;cursor:pointer}
.recent-row .reason{color:#c98b8b} .recent-row .outcome{margin-left:auto;color:var(--fg)}
.overlay{position:fixed;inset:0;background:rgba(0,0,0,.55);display:flex;justify-content:center;align-items:flex-start;padding:6vh 16px;z-index:40}
.panel{background:var(--surface);border:1px solid var(--line-2);border-radius:10px;width:min(720px,100%);max-height:86vh;overflow:auto;padding:14px}
.panel-head{display:flex;justify-content:space-between;align-items:center;margin-bottom:8px}
.tabs{display:flex;gap:4px;border-bottom:1px solid var(--line);margin:6px 0 8px}
.tab{font-size:12.5px;color:var(--muted);padding:5px 10px;cursor:pointer;background:none;border:none}
.tab.on{color:var(--fg);border-bottom:2px solid var(--green)}
.diffstat{color:var(--muted);font-size:12px;margin-bottom:6px}
.diff-pre,.logs-pre{margin:0;white-space:pre;overflow-x:auto;font-size:12px;line-height:1.55}
.dl{display:block;white-space:pre} .dl.add{color:var(--green)} .dl.del{color:var(--red)} .dl.hunk{color:var(--amber)} .dl.fhead{color:var(--blue)} .dl.ctx{color:#c4c8cf}
.lg{display:block} .lg.tool{color:var(--blue)} .lg.res{color:var(--muted)} .lg.txt{color:var(--fg)} .lg.err{color:var(--red)}
.submit-form{display:flex;flex-direction:column;gap:12px;max-width:480px}
.submit-form label{display:flex;flex-direction:column;gap:4px}
.submit-form .lab{font-size:12px;color:var(--muted)}
input,textarea,select{background:var(--bg);color:var(--fg);border:1px solid var(--line-2);border-radius:6px;padding:7px 9px;font:inherit;font-size:12.5px}
.field{position:relative} .field .ok-mark{position:absolute;right:9px;top:8px;color:var(--green)}
.msg{color:var(--amber);white-space:pre-wrap;font-size:12px}
table.history{width:100%;border-collapse:collapse} .history th,.history td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line)}
.hrow{cursor:pointer} .hrow:hover{background:var(--surface)}
.card.justnew{outline:2px solid var(--green)}
```

- [ ] **Step 2: Add `toast` + `copyId` + `shortId` to the `dom + design system` section of `app.js`**

```js
function shortId(id){ return (id||"").slice(0,12); }
function toast(msg, kind){
  const t = el("div", { class: "toast" + (kind ? " " + kind : "") }, msg);
  document.body.append(t);
  setTimeout(() => t.remove(), 2200);
}
function copyId(id){
  if (navigator.clipboard) navigator.clipboard.writeText(id);
  toast("copied " + shortId(id));
}
```

- [ ] **Step 3: Rebuild, restart, verify**

Run the dev/verify loop. Load the tokenized URL in a browser. Expected: the board renders with the refreshed palette; the connection status shows a colored dot + label. Click a task id (if any present) → a "copied …" toast appears bottom-center and disappears. Run `go test ./internal/webui/` → green.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/assets/style.css internal/webui/assets/app.js
git commit -m "webui: design-system tokens, toast, copy feedback"
```

---

## Task 2: Reconcile render loop + live running progress

The core interaction fix. Replace the board's full re-render with reconcile-by-id, drive elapsed via a 1s ticker, and add live progress (turns/cost/action) for running tasks.

**Files:**
- Modify: `internal/webui/assets/app.js` (`board` section: `renderBoard`, `taskCard`, add `reconcile`, `parseProgress`, `actionLabel`, `liveLine`, 1s ticker)

**Interfaces consumed:** `apiJSON`, `el`, `copyId`, `shortId`, `elapsed`, stage classes.
**Interfaces produced:** `parseProgress(jsonl) -> {turns:int, cost:number|null, action:string|null}`; reconcile keyed `Map<id,{el,sig}>` internal to the board section.

- [ ] **Step 1: Add the progress parser**

```js
function base(p){ return p ? p.split("/").pop() : ""; }
function actionLabel(tu){
  const f = (tu.input && (tu.input.file_path || tu.input.path)) || "";
  switch (tu.name) {
    case "Edit": case "Write": case "MultiEdit": return "editing " + base(f);
    case "Read": return "reading " + base(f);
    case "Bash": return "running a command";
    case "Grep": case "Glob": return "searching";
    default: return (tu.name || "working").toLowerCase();
  }
}
// parseProgress reads claude stream-json; other agents yield turns/cost only.
function parseProgress(jsonl){
  let turns = 0, cost = null, action = null;
  for (const line of jsonl.split("\n")){
    if (!line) continue;
    let o; try { o = JSON.parse(line); } catch { continue; }
    if (o.type === "assistant"){
      turns++;
      const tu = (o.message && o.message.content || []).find(c => c.type === "tool_use");
      if (tu) action = actionLabel(tu);
    }
    if (typeof o.total_cost_usd === "number") cost = o.total_cost_usd;
  }
  return { turns, cost, action };
}
```

- [ ] **Step 2: Replace `renderBoard`/`taskCard` with a reconciling version**

The board section keeps a module-level `const cardMap = new Map();`. Build a card once, then update only changed nodes. Key code (full):

```js
const cardMap = new Map(); // id -> { el, sig, ageEl, liveEl }
function gateRank(s){ return (s === "awaiting_approval" || s === "awaiting_egress") ? 0 : 1; }
function progressSig(t){ const p = t._prog || {}; return [t.stage, p.turns, p.cost, p.action].join("|"); }

function reconcile(container, tasks){
  tasks.sort((a, b) => gateRank(a.stage) - gateRank(b.stage));
  const seen = new Set();
  let prev = null;
  for (const t of tasks){
    seen.add(t.id);
    let rec = cardMap.get(t.id);
    if (!rec){ rec = buildCard(t); cardMap.set(t.id, rec); }
    else if (rec.sig !== progressSig(t)){ updateCard(rec, t); rec.sig = progressSig(t); }
    const want = prev ? prev.nextSibling : container.firstChild;
    if (rec.el !== want) container.insertBefore(rec.el, want);
    prev = rec.el;
  }
  for (const [id, rec] of cardMap){ if (!seen.has(id)){ rec.el.remove(); cardMap.delete(id); } }
}

function buildCard(t){
  const ageEl = el("span", { class: "age", text: elapsed(t.started_at) });
  const head = el("div", { class: "card-head" },
    el("code", { class: "tid", title: "click to copy", onclick: () => copyId(t.id), text: shortId(t.id) }),
    stageBadge(t.stage), ageEl);
  const liveEl = el("div", { class: "live" });
  const el_ = el("div", { class: "card", "data-tid": t.id, "data-started": t.started_at }, head,
    el("div", { class: "repo", text: shortRepo(t.repo) }),
    el("div", { class: "instr", text: t.instruction || "" }), liveEl);
  const rec = { el: el_, sig: progressSig(t), ageEl, liveEl };
  paintBody(rec, t);
  return rec;
}
function updateCard(rec, t){
  // swap the badge (stage may have changed) and repaint the body/gate/live
  const head = rec.el.querySelector(".card-head");
  head.replaceChild(stageBadge(t.stage), head.children[1]);
  paintBody(rec, t);
}
// paintBody fills the live line, the bar, gate, and Kill action for the current stage.
function paintBody(rec, t){
  rec.liveEl.replaceChildren();
  // remove any existing gate/actions appended after liveEl
  let n = rec.liveEl.nextSibling; while (n){ const x = n; n = n.nextSibling; x.remove(); }
  if (t.stage === "running"){
    const p = t._prog || {};
    const parts = ["claude", p.turns ? p.turns + " turns" : "", p.cost != null ? "$" + p.cost.toFixed(2) : "", p.action || ""].filter(Boolean);
    rec.liveEl.textContent = parts.join(" · ");
    rec.el.append(el("div", { class: "bar" }, el("i", {})));
  } else { rec.liveEl.textContent = ""; }
  if (t.stage === "awaiting_egress") rec.el.append(egressGate(t));
  else if (t.stage === "awaiting_approval") rec.el.append(pushGate(t));
  if (["running","pushing","awaiting_egress","awaiting_approval"].includes(t.stage))
    rec.el.append(el("div", { class: "actions" }, dangerButton("Kill", () => act("kill", t.id))));
}
```

Make the gate card visually dominant: in `pushGate`/`egressGate`, give the host card class `gate-host` and the gate block class `gate dominant` (set `rec.el.classList.toggle("gate-host", t.stage.startsWith("awaiting"))` in `paintBody`).

`renderBoard` now fetches tasks, fetches per-running-task progress (throttled), reconciles, then schedules the next poll:

```js
let progressTick = 0;
async function renderBoard(){
  let tasks;
  try { tasks = await apiJSON("/api/tasks"); setConn("brokerd connected", true); }
  catch (e){ return boardError(e); }   // boardError keeps the 401/403 vs down split from commit 0ee639f
  // throttle live-progress fetches to every other poll
  const doProg = (progressTick++ % 2) === 0;
  if (doProg) await Promise.all(tasks.filter(t => t.stage === "running").map(async t => {
    try { t._prog = parseProgress(await (await api("GET","/api/logs/"+t.id)).text()); } catch {}
  }));
  else for (const t of tasks){ const r = cardMap.get(t.id); if (r && r._prog) t._prog = r._prog; }
  const container = ensureBoardContainer();   // creates the .board div once; preserves it across polls
  reconcile(container, tasks);
  await renderFinishedStrip(container, new Set(tasks.map(t => t.id)));  // Task on failure-reasons (next task) fills this
  scheduleBoardPoll(tasks);
}
```

(Keep `_prog` on the task objects between polls so the non-fetch poll still shows the last known progress; store it back onto the card rec if you prefer — either is fine as long as it persists.)

- [ ] **Step 3: Add the 1s elapsed ticker**

```js
setInterval(() => {
  if (currentView !== "board") return;
  for (const rec of cardMap.values()){
    const s = rec.el.getAttribute("data-started");
    if (s) rec.ageEl.textContent = elapsed(s);
  }
}, 1000);
```

- [ ] **Step 4: Rebuild, restart, verify (no flicker + live progress)**

Submit a task (or have one running). Drive in Playwright:
- Grab the running card's DOM node handle; wait through ≥2 polls; assert the **same node** is still present (reconcile, not replaced) and the page scrollY is unchanged. Concretely: `const n1 = await page.$('[data-tid]'); ...wait 3.5s...; expect(await page.evaluate(el=>el.isConnected, n1)).toBe(true)`.
- Assert the running card shows `claude · N turns · $X` and the indeterminate bar animates.
- `go test ./internal/webui/` → green.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/assets/app.js
git commit -m "webui: reconcile-by-id board render + live running progress"
```

---

## Task 3: Failure reasons on the finished strip

Surface why a failed task failed, derived client-side from its logs.

**Files:**
- Modify: `internal/webui/assets/app.js` (`board` section: `renderFinishedStrip`, `failureReason`)

**Interfaces produced:** `failureReason(id) -> Promise<string|null>`; `renderFinishedStrip(container, liveIDs:Set) -> Promise<void>`.

- [ ] **Step 1: Add the reason extractor + strip renderer**

```js
async function failureReason(id){
  try {
    const text = await (await api("GET", "/api/logs/" + id)).text();
    let reason = null;
    for (const line of text.split("\n")){
      if (!line.includes('"result"')) continue;
      let o; try { o = JSON.parse(line); } catch { continue; }
      if (o.type === "result" && (o.is_error || o.subtype === "error") && typeof o.result === "string")
        reason = o.result.split("\n")[0];   // first line of the agent's error text
    }
    return reason;
  } catch { return null; }
}

async function renderFinishedStrip(container, liveIDs){
  let hist; try { hist = await apiJSON("/api/history"); } catch { return; }
  const recent = hist.filter(h => !liveIDs.has(h.id)).slice(0, 5);
  if (!recent.length) return;
  const strip = el("div", { class: "recent" }, el("div", { class: "recent-title", text: "Just finished" }));
  for (const it of recent){
    const row = el("div", { class: "recent-row", onclick: () => openReview(it.id, true) },
      el("i", { class: it.outcome.startsWith("error") ? "ti ti-x" : "ti ti-check" }),  // or a ✓/✕ glyph; see note
      el("code", { class: "tid", text: shortId(it.id) }),
      el("span", { class: "age", text: fmtAgeFromUnix(it.mtime_unix) }));
    if (it.outcome.startsWith("error")){
      const r = el("span", { class: "reason", text: "error" });
      row.append(r);
      failureReason(it.id).then(reason => { if (reason) r.textContent = "error · " + reason; });
    } else {
      row.append(el("span", { class: "outcome", text: it.outcome }));
    }
    strip.append(row);
  }
  container.append(strip);
}
```

> Note: drydock's UI has no icon font. Use a plain glyph instead of Tabler: `el("span",{style:"color:var(--green)",text:"✓"})` for ok and `el("span",{style:"color:var(--red)",text:"✕"})` for error. (The mockups used Tabler only because the mockup canvas has it.)

- [ ] **Step 2: Rebuild, restart, verify**

Cause a failure (e.g. submit while brokerd's gateway can't auth, or kill a task) so History has an `error` row. Load the board; assert the *Just finished* strip shows the failed id with `error · <reason>` (e.g. `error · API Error: 502 …`). `go test ./internal/webui/` → green.

- [ ] **Step 3: Commit**

```bash
git add internal/webui/assets/app.js
git commit -m "webui: surface failure reasons on the finished strip"
```

---

## Task 4: Review overlay — Diff / Logs tabs + dismiss

**Files:**
- Modify: `internal/webui/assets/app.js` (`review` section: `openReview`, add `renderLogs`, tab switching, Esc/backdrop close)

**Interfaces consumed:** `renderDiff` (existing), `api`, `el`, `act`.
**Interfaces produced:** `openReview(id, readonly=false)`; `closeOverlay()` (closes the topmost overlay); a module-level `overlayState` the keys section reads.

- [ ] **Step 1: Rewrite `openReview` with tabs + dismissal**

```js
let overlayState = null; // { close, approve?, deny? } — read by the keys section
function closeOverlay(){ if (overlayState){ overlayState.close(); overlayState = null; } }

async function openReview(id, readonly = false){
  const diffBox = el("div", { class: "tabbody" }, "loading diff…");
  const logsBox = el("div", { class: "tabbody", style: "display:none" }, "");
  const diffTab = el("button", { class: "tab on" }, "Diff");
  const logsTab = el("button", { class: "tab" }, "Logs");
  let logsLoaded = false;
  diffTab.onclick = () => { diffTab.classList.add("on"); logsTab.classList.remove("on"); diffBox.style.display = ""; logsBox.style.display = "none"; };
  logsTab.onclick = async () => { logsTab.classList.add("on"); diffTab.classList.remove("on"); logsBox.style.display = ""; diffBox.style.display = "none";
    if (!logsLoaded){ logsLoaded = true; try { renderLogs(logsBox, await (await api("GET","/api/logs/"+id)).text()); } catch { logsBox.textContent = "could not load logs"; } } };

  const panel = el("div", { class: "panel" },
    el("div", { class: "panel-head" },
      el("strong", { text: "Review " + shortId(id) }),
      el("span", { class: "kbd", style:"display:flex;gap:8px;align-items:center" }, "Esc to close",
        el("button", { class:"tab", onclick: closeOverlay, text: "✕" }))),
    el("div", { class: "tabs" }, diffTab, logsTab), diffBox, logsBox);
  if (!readonly) panel.append(el("div", { class: "actions" },
    el("button", { class: "btn ok", onclick: () => { act("approve", id); closeOverlay(); } }, "Approve push"),
    dangerButton("Deny", () => { act("deny", id); closeOverlay(); }),
    el("span", { class: "kbd" }, "A approve · D deny · Esc close")));

  const overlay = el("div", { class: "overlay", onclick: (e) => { if (e.target === overlay) closeOverlay(); } }, panel);
  document.body.append(overlay);
  overlayState = { close: () => overlay.remove(),
                   approve: readonly ? null : () => { act("approve", id); closeOverlay(); },
                   deny: readonly ? null : () => { act("deny", id); closeOverlay(); } };
  try {
    const res = await api("GET", "/api/diff/" + id);
    if (res.status === 404) { diffBox.textContent = "no diff"; return; }
    renderDiff(diffBox, await res.text());
  } catch { diffBox.textContent = "could not load diff"; }
}
```

- [ ] **Step 2: Add `renderLogs` (compact transcript; raw-jsonl floor)**

```js
function renderLogs(box, text){
  box.replaceChildren();
  const pre = el("pre", { class: "logs-pre" });
  for (const line of text.split("\n")){
    if (!line.trim()) continue;
    let o; try { o = JSON.parse(line); } catch { pre.append(el("span", { class: "lg", text: line + "\n" })); continue; }
    let cls = "lg", txt = line;
    if (o.type === "assistant"){
      const c = (o.message && o.message.content) || [];
      const tu = c.find(x => x.type === "tool_use");
      const tx = c.find(x => x.type === "text");
      if (tu){ cls = "lg tool"; txt = "→ " + actionLabel(tu); }
      else if (tx){ cls = "lg txt"; txt = tx.text; }
    } else if (o.type === "user"){ cls = "lg res"; txt = "  (tool result)"; }
    else if (o.type === "result"){ cls = "lg " + (o.is_error ? "err" : "res"); txt = (o.is_error ? "error: " : "done: ") + (o.result || o.subtype || ""); }
    else if (o.type === "system"){ cls = "lg res"; txt = "· " + (o.subtype || "system"); }
    pre.append(el("span", { class: cls, text: txt + "\n" }));
  }
  box.append(pre);
}
```

- [ ] **Step 3: Rebuild, restart, verify**

Drive a gated task. Open Review. Assert: the Diff tab shows the colored diff + `+N −M`; clicking **Logs** swaps to the transcript (lines like `→ editing README.md`, `done: …`); `Esc` and a backdrop click both close the overlay. `go test ./internal/webui/` → green.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/assets/app.js
git commit -m "webui: review overlay Diff/Logs tabs + Esc/backdrop close"
```

---

## Task 5: Submit refinements — validation, recent repos, model picker

**Files:**
- Modify: `internal/webui/assets/app.js` (`submit` section: `renderSubmit`; add `validRepo`, `recentRepos`, `pushRecentRepo`, `MODEL_SUGGESTIONS`)

**Interfaces produced:** `validRepo(v) -> bool`; `recentRepos() -> string[]`; `pushRecentRepo(r)`.

- [ ] **Step 1: Add the helpers**

```js
const REPO_RE = /^(https?:\/\/|git@|ssh:\/\/|git:\/\/)\S+$/;
function validRepo(v){ v = (v || "").trim(); return REPO_RE.test(v) && !v.startsWith("/") && !v.startsWith("."); }
function recentRepos(){ try { return JSON.parse(localStorage.getItem("dd.repos") || "[]"); } catch { return []; } }
function pushRecentRepo(r){ const a = [r, ...recentRepos().filter(x => x !== r)].slice(0, 5); localStorage.setItem("dd.repos", JSON.stringify(a)); }
const MODEL_SUGGESTIONS = ["", "claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5-20251001"]; // "" = broker default; update when models change
```

- [ ] **Step 2: Rewrite `renderSubmit` with validation + chips + datalist**

Build the form so: the repo input shows a green `✓` (`.ok-mark`) when `validRepo`, recent-repo chips under it fill the input on click, the model field is an `<input list="models">` with a `<datalist id="models">` of `MODEL_SUGGESTIONS`, and submit blocks (inline `.msg`) when the repo is invalid. On success call `pushRecentRepo(repo.value.trim())` before `show("board")`. Wire `oninput` on the repo field to toggle the `.ok-mark` visibility via `validRepo`. Keep the existing POST `/api/submit` flow and `newTaskID` handoff.

Key fragments:

```js
const repo = el("input", { type: "text", placeholder: "git@github.com:owner/repo", required: "" });
const okMark = el("span", { class: "ok-mark", text: "✓", style: "display:none" });
repo.oninput = () => { okMark.style.display = validRepo(repo.value) ? "" : "none"; };
const repoField = el("div", { class: "field" }, repo, okMark);
const chips = el("div", { class: "actions" }, ...recentRepos().map(r =>
  el("span", { class: "chip", onclick: () => { repo.value = r; repo.oninput(); } }, r)));
const model = el("input", { type: "text", placeholder: "broker default", list: "models" });
const datalist = el("datalist", { id: "models" }, ...MODEL_SUGGESTIONS.map(m => el("option", { value: m })));
// ...on submit:
if (!validRepo(repo.value)) { msg.textContent = "enter a valid https/git/ssh repo URL (no local paths)"; return; }
// ...after a successful response:
pushRecentRepo(repo.value.trim());
```

- [ ] **Step 3: Rebuild, restart, verify**

Open Submit. Assert: typing an invalid repo (`/tmp/x`) shows no `✓` and blocks submit with the inline message; a valid `git@github.com:o/r` shows `✓`; the model field offers the datalist suggestions; after a real submit, returning to Submit shows the repo as a chip. `go test ./internal/webui/` → green.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/assets/app.js
git commit -m "webui: submit validation, recent-repo chips, model picker"
```

---

## Task 6: Interaction layer — keyboard, inline confirm, states

**Files:**
- Modify: `internal/webui/assets/app.js` (`keys` section: global keydown; `act` inline-confirm; replace remaining `alert`)

**Interfaces consumed:** `currentView`, `overlayState`, `cardMap`, `openReview`, `act`, `submitForm` (the submit `onsubmit` extracted to a callable), `toast`.

- [ ] **Step 1: Replace `confirm()` in `act` with inline confirm; `alert()` with `toast`**

```js
function dangerButton(label, fn){
  const b = el("button", { class: "danger" }, label);
  let armed = false, timer = null;
  b.onclick = () => {
    if (armed){ clearTimeout(timer); fn(); return; }
    armed = true; b.dataset.orig = label; b.textContent = "Confirm?"; b.classList.add("confirming");
    timer = setTimeout(() => { armed = false; b.textContent = label; b.classList.remove("confirming"); }, 3000);
  };
  return b;
}
// in act(): drop the confirm() line; on failure use toast(`${verb} failed: HTTP ${res.status}`, "bad") instead of alert().
```

- [ ] **Step 2: Add the keyboard registry (`keys` section)**

```js
function isTyping(e){ const t = e.target; return t && (/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable); }
function singleGate(){ const gs = [...cardMap.keys()].map(id => boardTasks.get(id)).filter(t => t && (t.stage === "awaiting_approval" || t.stage === "awaiting_egress")); return gs.length === 1 ? gs[0] : null; }
addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "Enter"){ if (currentView === "submit") submitForm(); return; }
  if (isTyping(e)) return;                       // never hijack typing
  if (overlayState){
    if (e.key === "Escape") closeOverlay();
    else if (e.key === "a" && overlayState.approve) overlayState.approve();
    else if (e.key === "d" && overlayState.deny) overlayState.deny();
    return;
  }
  if (currentView === "board"){
    const g = singleGate();
    if (g){
      if (e.key === "r") openReview(g.id);
      else if (e.key === "a") act("approve", g.id);
      else if (e.key === "d") act("deny", g.id);
    }
  }
});
```

> Maintain a `boardTasks = new Map()` (id → task) updated each poll in `renderBoard`, so `singleGate()` can resolve stages. Extract the submit form's `onsubmit` body into a `submitForm()` you can call from the chord. Add the visible `R review · A approve · D deny` hint to the push-gate actions (Task 2/`pushGate`).

- [ ] **Step 3: Rebuild, restart, verify**

Drive in Playwright with a single gated task: press `R` → overlay opens; `Esc` → closes; `A` → approves (assert the task leaves the gate). In Submit, focus the instruction field and press `a`/`d` → asserts they type, not act. Click Deny on a card → it becomes "Confirm?"; a second click denies; waiting 3s reverts it. `go test ./internal/webui/` → green.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/assets/app.js
git commit -m "webui: keyboard shortcuts, inline confirm, toast errors"
```

---

## Task 7: States polish + full verification pass

**Files:**
- Modify: `internal/webui/assets/app.js` (empty/loading/error states across views)

- [ ] **Step 1: Refine the empty/loading/error states**

Board empty: keep the "No tasks running…" line but styled `.empty`. Board error: the auth-vs-down split from `0ee639f` (preserve it in `boardError`). History empty / load error and overlay loading/error use the `.empty`/`.muted` classes and one-line copy ("could not load history", "no diff", "could not load logs").

- [ ] **Step 2: Full verification**

Run the dev/verify loop and walk every screen in the browser, screenshotting each: board (empty, running, gate, finished-with-failure), review overlay (Diff + Logs + Esc), submit (valid/invalid/chips), history. Confirm no console errors beyond the benign `favicon.ico` 404. Then:

```bash
gofmt -l . ; go vet ./... ; go build ./... ; go test ./...
```
All clean / green (the JS/CSS have no in-repo formatter; Go must stay clean).

- [ ] **Step 3: Commit**

```bash
git add internal/webui/assets/app.js
git commit -m "webui: refine empty/loading/error states"
```

---

## Self-review (completed during planning)

**Spec coverage:**
- Design system / tokens → Task 1. ✓
- Reconcile-by-id (no flicker) + 1s ticker → Task 2. ✓
- Live running progress (turns/cost/action, claude-only) → Task 2. ✓
- Failure reason on finished strip (FE-only, from logs) → Task 3. ✓
- Gate visual dominance → Task 2 (`gate-host`/`dominant`). ✓
- Review overlay Diff/Logs tabs + Esc/backdrop → Task 4. ✓
- Submit validation / recent repos / model picker → Task 5. ✓
- Keyboard layer (suppressed while typing), inline confirm, toast → Task 6. ✓
- States polish → Task 7. ✓
- Constraints: single `app.js` (no ES modules), FE-only (every endpoint used exists), all-mono, no deps — honored throughout; the icon-font note in Task 3 keeps it dependency-free. ✓

**Placeholder scan:** none — tricky algorithms (reconcile, parsers, keyboard, inline confirm) are shown in full; mechanical DOM-building references the exact new fragments + reused existing functions (`renderDiff`, `stageBadge`, `shortRepo`, `egressGate`).

**Type consistency:** `parseProgress -> {turns,cost,action}` consumed by `paintBody`; `_prog` carried on task objects; `overlayState {close,approve,deny}` produced by Task 4, consumed by Task 6; `validRepo`/`recentRepos` produced by Task 5; `cardMap`/`boardTasks` shared within the board/keys sections. Consistent.

**Testing reality:** no JS unit harness (by design); each task is gated by browser-driven (Playwright) verification + screenshots + the Go `internal/webui` tests staying green. Pure helpers (`parseProgress`, `validRepo`, `actionLabel`, `failureReason`) are factored to be unit-testable if a JS runner is ever added.
