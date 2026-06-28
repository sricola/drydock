"use strict";

// --- token: captured from the URL fragment (#t=...), then stripped so it
// never lingers in the visible URL / history entry. Held only in memory.
let TOKEN = "";
(function captureToken() {
  const m = location.hash.match(/(?:^|[#&])t=([0-9a-f]+)/);
  if (m) {
    TOKEN = m[1];
    history.replaceState(null, "", location.pathname); // drop the fragment
  }
})();

// api wraps fetch with the bearer header and JSON/text handling. Every call
// sends the token; brokerd-down surfaces as a thrown Error the views render.
async function api(method, path, body) {
  const headers = {};
  if (TOKEN) headers["Authorization"] = "Bearer " + TOKEN;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  return res;
}
async function apiJSON(path) {
  const res = await api("GET", path);
  if (!res.ok) throw new Error(`${res.status}`);
  return res.json();
}

// === dom + design system ===
function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "onclick") e.onclick = v;
    else if (k === "text") e.textContent = v;
    else e.setAttribute(k, v);
  }
  for (const kid of kids) e.append(kid);
  return e;
}
function fmtAgeFromUnix(sec) { return fmtAge(Date.now() / 1000 - sec); }
function fmtAge(s) {
  s = Math.max(0, Math.floor(s));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}
function elapsed(startedAt) { return fmtAge((Date.now() - new Date(startedAt).getTime()) / 1000); }
function fmtDurMs(ms) { return ms >= 1000 ? Math.round(ms / 1000) + "s" : ms + "ms"; }
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

// --- view router
const views = {};
let currentView = "board";
function show(view) {
  currentView = view;
  for (const b of document.querySelectorAll("nav button")) b.classList.toggle("active", b.dataset.view === view);
  views[view]();
}
document.querySelectorAll("nav button").forEach(b => (b.onclick = () => show(b.dataset.view)));

const app = () => document.getElementById("app");
function setConn(text, ok) {
  const c = document.getElementById("conn");
  c.textContent = text;
  c.className = "conn " + (ok ? "ok" : "bad");
}

// =================== BOARD ===================
let pollTimer = null;

// --- live running progress: parse the agent's stream-json to count turns,
// track cost, and label the current action. claude emits assistant/tool_use
// events; other agents only yield total_cost_usd (so turns/action stay 0/null).
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

// --- reconcile-by-id: build each card once, then mutate only changed nodes in
// place across polls (no replaceChildren on the board, so no flicker, no scroll
// jump, no lost focus). cardMap holds the persistent DOM + a content signature.
const cardMap = new Map(); // id -> { el, sig, ageEl, liveEl, _prog }
let boardTasks = new Map(); // id -> task (kept fresh each poll; a later keyboard layer needs it)
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
    rec._prog = t._prog; // remember last known progress for the non-fetch poll
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
  rec.el.classList.toggle("gate-host", t.stage.startsWith("awaiting"));
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

// boardEl is the persistent .board node; null until first mount / after an error
// unmounts it (so ensureBoardContainer rebuilds it on recovery).
let boardEl = null;

// boardError keeps the 401/403 (token rejected) vs brokerd-down split (commit 0ee639f).
function boardError(e){
  // A 401/403 means the access token was rejected (it changed since this tab
  // opened) — NOT that brokerd is down. Don't conflate the two.
  if (e.message === "401" || e.message === "403") {
    setConn("unauthorized — reopen the `drydock ui` link", false);
    app().replaceChildren(el("p", { class: "empty", text: "Access token rejected. Reopen the URL printed by `drydock ui` — the token may have changed since this tab was opened." }));
  } else {
    setConn("brokerd not running — run `drydock start`", false);
    app().replaceChildren(el("p", { class: "empty", text: "brokerd is not running. Start it with `drydock start`, then this board will populate." }));
  }
  boardEl = null; // the board node was just unmounted; rebuild it on recovery
  cardMap.clear();
  scheduleBoardPoll();
}

// ensureBoardContainer returns the persistent .board node, mounting it only when
// entering the board view (not every poll) so reconcile can mutate it in place.
function ensureBoardContainer(){
  if (!boardEl) boardEl = el("div", { class: "board" });
  if (boardEl.parentNode !== app()) app().replaceChildren(boardEl);
  return boardEl;
}

// failureReason fetches the task's logs and extracts the first line of the
// terminal result's error text (the agent's own error message). Returns null
// when unavailable or when the task did not error.
async function failureReason(id){
  try {
    const text = await (await api("GET", "/api/logs/" + id)).text();
    let reason = null;
    for (const line of text.split("\n")){
      if (!line.includes('"result"')) continue;
      let o; try { o = JSON.parse(line); } catch { continue; }
      if (o.type === "result" && (o.is_error || o.subtype === "error") && typeof o.result === "string")
        reason = o.result.split("\n")[0]; // first line of the agent's error text
    }
    return reason;
  } catch { return null; }
}

// renderFinishedStrip ports the "Just finished" strip: the most recent completed
// tasks, so a task leaving the live set doesn't silently vanish. Best-effort.
async function renderFinishedStrip(container, liveIDs){
  // clear any previously-appended strip (reconcile owns the cards, not this)
  for (const old of container.querySelectorAll(".recent")) old.remove();
  let hist; try { hist = await apiJSON("/api/history"); } catch { return; }
  const recent = hist.filter(h => !liveIDs.has(h.id)).slice(0, 5);
  if (!recent.length) return;
  const strip = el("div", { class: "recent" }, el("div", { class: "recent-title", text: "Just finished" }));
  for (const it of recent){
    const isErr = it.outcome.startsWith("error");
    const icon = isErr
      ? el("span", { style: "color:var(--red)", text: "✕" })
      : el("span", { style: "color:var(--green)", text: "✓" });
    const row = el("div", { class: "recent-row", onclick: () => openReview(it.id, true) },
      icon,
      el("code", { class: "tid", text: shortId(it.id) }),
      el("span", { class: "age", text: fmtAgeFromUnix(it.mtime_unix) }));
    if (isErr){
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

let progressTick = 0;
async function renderBoard(){
  let tasks;
  try { tasks = await apiJSON("/api/tasks"); setConn("brokerd connected", true); }
  catch (e){ return boardError(e); }   // boardError keeps the 401/403 vs down split (commit 0ee639f)
  // keep boardTasks fresh for a later keyboard layer (harmless now)
  boardTasks = new Map(tasks.map(t => [t.id, t]));
  // throttle live-progress fetches to every other poll; reuse last known on the off-poll
  const doProg = (progressTick++ % 2) === 0;
  if (doProg) await Promise.all(tasks.filter(t => t.stage === "running").map(async t => {
    try { t._prog = parseProgress(await (await api("GET","/api/logs/"+t.id)).text()); } catch {}
  }));
  else for (const t of tasks){ const r = cardMap.get(t.id); if (r && r._prog) t._prog = r._prog; }
  const container = ensureBoardContainer();   // creates the .board div once; preserves it across polls
  reconcile(container, tasks);
  // empty prompt: shown only when no live cards remain (managed after reconcile
  // so a some->zero transition clears the stale cards first).
  for (const old of container.querySelectorAll(":scope > .empty")) old.remove();
  if (tasks.length === 0) {
    container.prepend(el("p", { class: "empty" }, "No tasks running. ", el("a", { href: "#", onclick: () => show("submit") }, "Submit one"), " or see ", el("a", { href: "#", onclick: () => show("history") }, "History"), "."));
  }
  await renderFinishedStrip(container, new Set(tasks.map(t => t.id)));
  if (newTaskID) {
    const c = container.querySelector(`[data-tid="${newTaskID}"]`);
    if (c) { c.classList.add("justnew"); c.scrollIntoView({ block: "center" }); }
    newTaskID = null;
  }
  scheduleBoardPoll(tasks);
}

function scheduleBoardPoll(tasks) {
  if (pollTimer) clearTimeout(pollTimer);
  if (currentView !== "board") return;
  const atGate = Array.isArray(tasks) && tasks.some(t => t.stage === "awaiting_approval" || t.stage === "awaiting_egress");
  pollTimer = setTimeout(renderBoard, atGate ? 500 : 1500); // faster while a gate is open
}

// 1s elapsed ticker: bump each card's age without a poll, so the board feels live.
setInterval(() => {
  if (currentView !== "board") return;
  for (const rec of cardMap.values()){
    const s = rec.el.getAttribute("data-started");
    if (s) rec.ageEl.textContent = elapsed(s);
  }
}, 1000);

function stageBadge(stage) {
  const label = { awaiting_egress: "egress?", running: "running", awaiting_approval: "review?", pushing: "pushing" }[stage] || stage;
  return el("span", { class: "badge stage-" + stage, text: label });
}

function shortRepo(r) { const i = r.lastIndexOf(":"); return i >= 0 ? r.slice(i + 1) : r; }

// egress gate: show the requested hosts (from the persisted widen file) PLUS the
// instruction/repo so the operator can judge WHY the host was requested.
function egressGate(t) {
  const box = el("div", { class: "gate egress dominant" }, el("div", { class: "gate-title", text: "Egress widening requested" }));
  apiJSON("/api/widen/" + t.id).then(domains => {
    box.append(el("ul", {}, ...domains.map(d => el("li", { text: d.host + ":" + (d.ports || []).join(",") }))));
  }).catch(() => box.append(el("p", { class: "muted", text: "(host list unavailable)" })));
  box.append(el("div", { class: "actions" },
    el("button", { class: "ok", onclick: () => act("approve", t.id) }, "Approve egress"),
    dangerButton("Deny", () => act("deny", t.id))));
  return box;
}

// push gate: cost + budget, open the diff to review, approve only after viewing.
function pushGate(t) {
  const box = el("div", { class: "gate push dominant" }, el("div", { class: "gate-title", text: "Push awaiting review" }));
  const cost = el("span", { class: "cost", text: "spent: …" });
  box.append(cost);
  spentSoFar(t.id).then(s => (cost.textContent = s));
  const approveBtn = el("button", { class: "ok", disabled: "", onclick: () => act("approve", t.id) }, "Approve push");
  box.append(el("div", { class: "actions" },
    el("button", { onclick: () => { openReview(t.id); approveBtn.removeAttribute("disabled"); } }, "Review diff"),
    approveBtn,
    dangerButton("Deny", () => act("deny", t.id)),
    el("span", { class: "kbd" }, "R review · A approve · D deny")));
  return box;
}

// spentSoFar parses the latest total_cost_usd from the live jsonl (best-effort).
async function spentSoFar(id) {
  try {
    const res = await api("GET", "/api/logs/" + id);
    const text = await res.text();
    let cost = null;
    for (const line of text.split("\n")) {
      const i = line.indexOf('"total_cost_usd"');
      if (i >= 0) { try { const o = JSON.parse(line); if (typeof o.total_cost_usd === "number") cost = o.total_cost_usd; } catch (_) {} }
    }
    return cost === null ? "spent: (not reported)" : "spent: $" + cost.toFixed(4);
  } catch (_) { return "spent: —"; }
}

// act performs approve/deny/kill with optimistic feedback + immediate re-poll.
// deny/kill are destructive, but confirmation now lives in dangerButton's
// two-step (not a native confirm()); failures surface as a toast, not alert().
async function act(verb, id) {
  try {
    const res = await api("POST", "/api/" + verb + "/" + id);
    if (res.status === 409 || res.status === 404) { /* already resolved — just refresh */ }
    else if (!res.ok) toast(`${verb} failed: HTTP ${res.status}`, "bad");
  } catch (e) { toast(`${verb} failed: ${e.message}`, "bad"); }
  renderBoard(); // immediate re-poll (don't wait the interval)
}

// dangerButton replaces a native confirm() with an inline two-step: the first
// click arms the button (label -> "Confirm?", red .confirming style) and starts
// a 3s disarm timer; a second click within that window runs fn(). Class is
// "btn danger" so both the red .btn.danger and the .btn.confirming styles apply.
function dangerButton(label, fn){
  const b = el("button", { class: "btn danger" }, label);
  let armed = false, timer = null;
  b.onclick = () => {
    if (armed){ clearTimeout(timer); fn(); return; }
    armed = true; b.dataset.orig = label; b.textContent = "Confirm?"; b.classList.add("confirming");
    timer = setTimeout(() => { armed = false; b.textContent = label; b.classList.remove("confirming"); }, 3000);
  };
  return b;
}
views.board = renderBoard;

// =================== REVIEW (diff overlay) ===================
let overlayState = null; // { close, approve?, deny? } — read by the keys section
function closeOverlay(){ if (overlayState){ overlayState.close(); overlayState = null; } }

async function openReview(id, readonly = false){
  if (overlayState) closeOverlay();   // close any existing overlay first
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

  // Esc is owned by the global keydown registry (via overlayState), not a
  // per-open listener here — exactly one keydown handler in the app.
  const overlay = el("div", { class: "overlay", onclick: (e) => { if (e.target === overlay) closeOverlay(); } }, panel);
  document.body.append(overlay);
  overlayState = { close: () => { overlay.remove(); },
                   approve: readonly ? null : () => { act("approve", id); closeOverlay(); },
                   deny: readonly ? null : () => { act("deny", id); closeOverlay(); } };
  try {
    const res = await api("GET", "/api/diff/" + id);
    if (res.status === 404) { diffBox.textContent = "no diff"; return; }
    renderDiff(diffBox, await res.text());
  } catch { diffBox.textContent = "could not load diff"; }
}

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

// renderDiff colors a unified diff line-by-line (+ add, - del, @@ hunk, file
// headers) and shows a +X/-Y summary. No syntax highlighting (per spec).
function renderDiff(box, text) {
  box.replaceChildren();
  let add = 0, del = 0;
  const pre = el("pre", { class: "diff-pre" });
  for (const line of text.split("\n")) {
    let cls = "ctx";
    if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("diff ") || line.startsWith("index ")) cls = "fhead";
    else if (line.startsWith("@@")) cls = "hunk";
    else if (line.startsWith("+")) { cls = "add"; add++; }
    else if (line.startsWith("-")) { cls = "del"; del++; }
    pre.append(el("span", { class: "dl " + cls, text: line + "\n" }));
  }
  box.append(el("div", { class: "diffstat", text: `+${add} −${del}` }), pre);
}

// =================== KEYS (global keydown registry) ===================
// One keydown handler for the whole app. Single-key shortcuts are suppressed
// while typing (the field gets the char); the Cmd/Ctrl+Enter submit chord is the
// only exception. When an overlay is open it owns A/D/Esc via overlayState; on
// the board, R/A/D act only when exactly one task sits at a gate (singleGate).
function isTyping(e){ const t = e.target; return t && (/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable); }
function singleGate(){
  const gs = [...cardMap.keys()].map(id => boardTasks.get(id)).filter(t => t && (t.stage === "awaiting_approval" || t.stage === "awaiting_egress"));
  return gs.length === 1 ? gs[0] : null;
}
addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "Enter"){ if (currentView === "submit") document.querySelector(".submit-form")?.requestSubmit(); return; }
  if (isTyping(e)) return;                       // never hijack typing
  if (overlayState){
    if (e.key === "Escape") closeOverlay();
    else if (e.key === "a" && overlayState.approve) overlayState.approve();
    else if (e.key === "d" && overlayState.deny) overlayState.deny();
    return;
  }
  if (e.key === "?"){ toast("R review · A approve · D deny · Esc close · ⌘↵ submit"); return; }
  if (currentView === "board"){
    const g = singleGate();
    if (g){
      if (e.key === "r") openReview(g.id);
      else if (e.key === "a") act("approve", g.id);
      else if (e.key === "d") act("deny", g.id);
    }
  }
});

// =================== SUBMIT ===================
const REPO_RE = /^(https?:\/\/|git@|ssh:\/\/|git:\/\/)\S+$/;
function validRepo(v){ v = (v || "").trim(); return REPO_RE.test(v) && !v.startsWith("/") && !v.startsWith("."); }
function recentRepos(){ try { return JSON.parse(localStorage.getItem("dd.repos") || "[]"); } catch { return []; } }
function pushRecentRepo(r){ const a = [r, ...recentRepos().filter(x => x !== r)].slice(0, 5); localStorage.setItem("dd.repos", JSON.stringify(a)); }
const MODEL_SUGGESTIONS = ["", "claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5-20251001"]; // "" = broker default; update when models change

const AGENTS = ["claude", "codex", "opencode"];
function renderSubmit() {
  if (pollTimer) clearTimeout(pollTimer);
  const form = el("form", { class: "submit-form" });
  const repo = el("input", { type: "text", placeholder: "git@github.com:owner/repo", required: "" });
  const okMark = el("span", { class: "ok-mark", text: "✓", style: "display:none" });
  repo.oninput = () => { okMark.style.display = validRepo(repo.value) ? "" : "none"; };
  const repoField = el("div", { class: "field" }, repo, okMark);
  const chips = el("div", { class: "actions" }, ...recentRepos().map(r =>
    el("span", { class: "chip", onclick: () => { repo.value = r; repo.oninput(); } }, r)));
  const instr = el("textarea", { placeholder: "What should the agent do?", rows: "4", required: "" });
  const agent = el("select", {});
  for (const a of AGENTS) agent.append(el("option", { value: a, text: a }));
  const model = el("input", { type: "text", placeholder: "broker default", list: "models" });
  const datalist = el("datalist", { id: "models" }, ...MODEL_SUGGESTIONS.map(m => el("option", { value: m })));
  const msg = el("div", { class: "msg" });
  form.append(
    label("Repo URL (https/git/ssh — no local paths)", repoField),
    chips,
    label("Instruction", instr),
    label("Agent", agent),
    label("Model", model),
    datalist,
    el("button", { type: "submit", class: "ok" }, "Submit task"),
    msg);
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (!validRepo(repo.value)) { msg.textContent = "enter a valid https/git/ssh repo URL (no local paths)"; return; }
    msg.textContent = "submitting…";
    const body = { repo_ref: repo.value.trim(), instruction: instr.value, agent: agent.value };
    if (model.value.trim()) body.model = model.value.trim();
    try {
      const res = await api("POST", "/api/submit", body);
      const txt = await res.text();
      if (!res.ok) { msg.textContent = "error: " + txt; return; }
      const { id } = JSON.parse(txt);
      newTaskID = id;
      pushRecentRepo(repo.value.trim());
      show("board");
    } catch (e) { msg.textContent = "error: " + e.message; }
  };
  app().replaceChildren(el("h2", { text: "Submit a task" }), form);
}
let newTaskID = null;
function label(text, input) { return el("label", {}, el("span", { text }), input); }
views.submit = renderSubmit;

// =================== HISTORY ===================
async function renderHistory() {
  if (pollTimer) clearTimeout(pollTimer);
  let items;
  try { items = await apiJSON("/api/history"); }
  catch (e) { app().replaceChildren(el("p", { class: "empty", text: "could not load history" })); return; }
  const table = el("table", { class: "history" });
  table.append(el("tr", {}, ...["ID", "AGE", "DUR", "COST", "OUTCOME"].map(h => el("th", { text: h }))));
  if (items.length === 0) table.append(el("tr", {}, el("td", { colspan: "5", text: "(no tasks yet)" })));
  for (const it of items) {
    const row = el("tr", { class: "hrow" },
      el("td", {}, el("code", { onclick: () => navigator.clipboard && navigator.clipboard.writeText(it.id), title: "copy", text: it.id.slice(0, 12) })),
      el("td", { text: fmtAgeFromUnix(it.mtime_unix) }),
      el("td", { text: it.has_duration ? fmtDurMs(it.duration_ms) : "-" }),
      el("td", { text: it.cost }),
      el("td", { text: it.outcome }));
    row.onclick = () => openReview(it.id, true); // read-only diff/logs for past tasks
    table.append(row);
  }
  app().replaceChildren(el("h2", { text: "History" }), table);
}
views.history = renderHistory;

// boot
show("board");
