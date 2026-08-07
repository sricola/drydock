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
    else if (k === "onchange") e.onchange = v;
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
// fmtDurMs mirrors the CLI's shortDur (cmd/drydock/util.go): "39m12s", not "2352s".
function fmtDurMs(ms) {
  if (ms < 1000) return ms + "ms";
  const s = ms / 1000;
  if (s < 10) return s.toFixed(1) + "s";
  if (s < 60) return Math.floor(s) + "s";
  const m = Math.floor(s / 60), rem = Math.floor(s) % 60;
  return m + "m" + String(rem).padStart(2, "0") + "s";
}
function shortId(id){ return (id||"").slice(0,12); }
// safeCell mirrors cmd/drydock/inspect.go's safeCell: strip control and
// format characters, cap at 200 code points. Values shown in the brief panel
// (file paths especially) originate in the untrusted work tree; even though
// everything lands via textContent (no HTML injection), Unicode format chars
// still render: a bidi override (U+202E RLO, U+202A–U+202E, U+2066–U+2069)
// lets a hostile filename visually spoof its extension ("wat‮gpj.exe"
// displays reversed), and zero-width chars/BOM hide content.
//   \p{Cc} = the Unicode Control category: exactly C0 (< 0x20), DEL (0x7f),
//            and C1 (0x80–0x9f) — the same set inspect.go strips by range.
//   \p{Cf} = the Format category: bidi overrides, zero-width chars, BOM —
//            matching inspect.go's unicode.Is(unicode.Cf, r).
// Ordinary non-ASCII letters (é, ©, …) are untouched. The cap counts code
// points (spread), not UTF-16 units, to match Go's rune-based count. The
// boundary matches Go exactly: inspect.go appends "…" once n reaches 200,
// so a string of exactly 200 code points also gets the ellipsis (>=, not >).
function safeCell(s){
  const cleaned = String(s ?? "").replace(/[\p{Cc}\p{Cf}]/gu, "");
  const cps = [...cleaned];
  return cps.length >= 200 ? cps.slice(0, 200).join("") + "…" : cleaned;
}
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
//
// EVERYTHING THIS PARSES IS AGENT-AUTHORED TEXT, including the running cost —
// the broker has no reason to write a per-turn figure into the stream, so this
// number is whatever the CLI printed. It is fine as a liveness/progress hint
// and it is rendered with a "~" so it never reads as measured spend. The
// broker-metered figure appears at the push gate (spentLabel), which is the
// only place a person is making a decision about it (plan G4).
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
function progressSig(t){ const p = t._prog || {}; return [t.stage, t.agent, p.turns, p.cost, p.action].join("|"); }

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
    const parts = [t.agent || "", p.turns ? p.turns + " turns" : "", p.cost != null ? "~$" + p.cost.toFixed(2) : "", p.action || ""].filter(Boolean);
    rec.liveEl.textContent = parts.join(" · ");
    rec.el.append(el("div", { class: "bar" }, el("i", {})));
  } else { rec.liveEl.textContent = ""; }
  if (t.stage === "awaiting_egress") rec.el.append(egressGate(t));
  else if (t.stage === "awaiting_approval") rec.el.append(pushGate(t));
  if (["queued","setting_up","running","verifying","pushing","awaiting_egress","awaiting_approval"].includes(t.stage))
    rec.el.append(el("div", { class: "actions" }, dangerButton("Kill", () => act("kill", t.id), t.id + ":kill:card")));
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
    // outcome_key is the stable machine classification (audit.OutcomeKeyWithMetrics):
    // "ok" covers both a real push and a no-diff run (unchanged operator
    // muscle memory); "completed" is the queue's clean-finish terminal —
    // those two are the ONLY keys that earn a checkmark. "dead_letter" (a
    // queued task parked as undeliverable) joins error/push_failed as red.
    // Everything else (denied, cancelled, interrupted, running, or any
    // unexpected passthrough key) gets the neutral glyph by default: a green
    // checkmark must never be the fallback for a key this code doesn't
    // recognize.
    //
    // THERE IS DELIBERATELY NO "ci_failed" HERE. outcome_key is derived from a
    // task's LAST {"type":"result"} row, and the broker never writes one for a
    // CI observation — by design, so a CI failure cannot relabel a push that
    // landed exactly as asked (see audit.CIObservation). A CI verdict lives on
    // the durable queue item instead: `drydock queue list` and GET /queue. A
    // key this map cannot receive is a lie waiting to be believed, so it stays
    // out; internal/webui/ci_assets_test.go pins that both ways.
    const isErr = it.outcome_key === "error" || it.outcome_key === "push_failed" ||
                  it.outcome_key === "dead_letter";
    const isOk = it.outcome_key === "ok" || it.outcome_key === "completed";
    // Color via class, not a style attribute: the strict CSP (default-src
    // 'self', no style-src 'unsafe-inline') blocks inline style attributes.
    const icon = isErr
      ? el("span", { class: "glyph-err", text: "✕" })
      : isOk
      ? el("span", { class: "glyph-ok", text: "✓" })
      : el("span", { class: "glyph-muted", text: "∅" });
    const row = el("div", { class: "recent-row", onclick: () => openReview(it.id, true) },
      icon,
      el("code", { class: "tid", text: shortId(it.id) }),
      el("span", { class: "age", text: fmtAgeFromUnix(it.mtime_unix) }));
    if (isErr){
      const lbl = { dead_letter: "dead-letter" }[it.outcome_key] || "error";
      const r = el("span", { class: "reason", text: lbl });
      row.append(r);
      failureReason(it.id).then(reason => { if (reason) r.textContent = lbl + " · " + reason; });
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
    // a 404 here is expected right after submit (the audit .jsonl doesn't
    // exist yet): skip quietly rather than parsing an error body as progress.
    try {
      const res = await api("GET", "/api/logs/" + t.id);
      if (res.ok) t._prog = parseProgress(await res.text());
    } catch {}
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

// stageBadge renders one lifecycle state as a badge. `stage` comes from
// /api/tasks, which is the LIVE-task stream, so the map covers the TaskStage
// vocabulary and nothing else. In particular it has no `awaiting_ci` entry:
// that is a durable QUEUE state whose task has already finished and been
// unregistered, so it can never appear here — the web UI has no queue view at
// all, and the CI observation is surfaced by `drydock queue list` and
// GET /queue. An unknown state falls through to its raw name rather than being
// dropped, so a state this build doesn't know about is still visible.
function stageBadge(stage) {
  const label = { queued: "queued", awaiting_egress: "egress?", setting_up: "setting up", running: "running", verifying: "verifying", awaiting_approval: "review?", pushing: "pushing" }[stage] || stage;
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
    dangerButton("Deny", () => act("deny", t.id), t.id + ":deny-egress:card")));
  return box;
}

// push gate: cost + budget, open the diff to review, approve only after viewing.
// A diff with required second-look categories gets a warning chip; the
// acknowledgment checkboxes live in the review overlay (openReview), and an
// approve fired from here without them gets brokerd's 422 → act's toast.
function pushGate(t) {
  const title = el("div", { class: "gate-title", text: "Push awaiting review" });
  if ((t.second_look || []).length) title.append(" ", el("span", { class: "brief-chip warn", text: "second-look" }));
  const box = el("div", { class: "gate push dominant" }, title);
  box.append(el("span", { class: "cost", text: spentLabel(t) }));
  const approveBtn = el("button", { class: "ok", disabled: "", onclick: () => act("approve", t.id) }, "Approve push");
  box.append(el("div", { class: "actions" },
    el("button", { onclick: () => { openReview(t.id); approveBtn.removeAttribute("disabled"); } }, "Review diff"),
    approveBtn,
    dangerButton("Deny", () => act("deny", t.id), t.id + ":deny-push:card"),
    el("span", { class: "kbd" }, "R review · A approve · D deny")));
  return box;
}

// costCell renders a history row's cost. The broker-authored figure renders
// plain; one that exists only because the AGENT reported it arrives from
// /api/history already carrying audit.AgentReportedCostMark ("?") and sets
// cost_agent_reported, so it gets a hover title spelling out what the mark
// means. Shown, but never shown as measured (plan G4).
function costCell(it) {
  const td = el("td", { text: it.cost });
  if (it.cost_agent_reported) {
    td.className = "muted";
    td.title = "agent-reported cost; the broker did not meter this task";
  }
  return td;
}

// spentLabel renders the BROKER-METERED spend at the push gate, from the
// broker's own task state (`spent_usd`/`spent_src` on /api/tasks).
//
// This replaced spentSoFar, which fetched /api/logs/<id> and took the last
// total_cost_usd it could find on ANY line. That file is the task's audit
// stream, and the AGENT's stdout is copied into it verbatim — so a compromised
// CLI could print {"type":"result","total_cost_usd":0.0001} and that number was
// what got rendered, in the "spent" slot, immediately beside the Approve
// button. An untrusted figure next to a human security decision is exactly the
// display plan G4 calls out; the broker's gateway lease is the only spend
// figure that means anything, and it is now published on the task state
// (broker.taskRun.publishGateSpend) so this function has nothing to parse.
//
// It never renders a bare "$0.00": a lane with no USD metering and a lane the
// broker could not measure are different facts, and both are different from
// "this task spent nothing".
function spentLabel(t) {
  if (t.spent_src === "broker") return "spent: $" + Number(t.spent_usd || 0).toFixed(4) + " (broker-metered)";
  // "unmetered" is TWO lanes, not one: a subscription lane, and an
  // openai_compat lane configured with no prices. Naming only the first was
  // wrong on the second — an operator on an unpriced lane was told they were on
  // a subscription. Both are $0 by construction rather than by measurement,
  // which is the fact that matters at an approval gate.
  if (t.spent_src === "unmetered") return "spent: n/a — no USD metering on this lane (subscription, or no prices configured)";
  return "spent: unknown — not broker-metered";
}

// act performs approve/deny/kill with optimistic feedback + immediate re-poll.
// deny/kill are destructive, but confirmation now lives in dangerButton's
// two-step (not a native confirm()); failures surface as a toast, not alert().
// acks (approve only): second-look categories to acknowledge — sent as the
// {"acknowledge":[...]} JSON body brokerd's gate validation reads. A 422
// means required categories were NOT acknowledged: brokerd refused, the task
// stays pending, and the toast names what's missing (the review overlay's
// checkboxes are where they get acknowledged).
async function act(verb, id, acks) {
  try {
    const body = verb === "approve" && Array.isArray(acks) && acks.length ? { acknowledge: acks } : undefined;
    const res = await api("POST", "/api/" + verb + "/" + id, body);
    if (res.status === 409 || res.status === 404) { /* already resolved — just refresh */ }
    else if (res.status === 422) {
      let missing = [];
      try { missing = ((await res.json()).missing || []).map(safeCell); } catch {}
      toast("approve refused — unacknowledged second-look: " + (missing.join(", ") || "(categories unknown)") + " · open Review diff to acknowledge", "bad");
    }
    else if (!res.ok) toast(`${verb} failed: HTTP ${res.status}`, "bad");
  } catch (e) { toast(`${verb} failed: ${e.message}`, "bad"); }
  renderBoard(); // immediate re-poll (don't wait the interval)
}

// armedGates: key -> { expiry (ms epoch), timer } for dangerButton's two-press
// confirm. Module-level so the arm survives updateCard/paintBody rebuilding the
// button on every poll (1.5s, 500ms while a gate is open): without this, a
// deliberate second click can land on a freshly-rebuilt button that forgot it
// was armed. Callers pass a task-id+action+surface key so the same logical
// button (across rebuilds) shares one entry; a bare local key (no caller key
// given) still works, it just isn't shared with anything else.
//
// scheduleDisarm always cancels any timer already stored for the key before
// installing its own, so at most one disarm timer is ever pending per key:
// a rebuilt button re-arming (or a fresh arm) never leaves a prior instance's
// timer orphaned in the event loop. An orphaned timer would otherwise fire at
// its original expiry and delete a later, still-legitimate arm out from under
// a newer button (the button would keep reading "Confirm?" but the next click
// would silently re-arm instead of firing).
const armedGates = new Map();
function scheduleDisarm(key, ms, onDisarm){
  const prev = armedGates.get(key);
  if (prev) clearTimeout(prev.timer);
  const timer = setTimeout(() => { armedGates.delete(key); onDisarm(); }, ms);
  armedGates.set(key, { expiry: Date.now() + ms, timer });
}

// dangerButton replaces a native confirm() with an inline two-step: the first
// click arms the button (label -> "Confirm?", red .confirming style) and starts
// a 3s disarm timer; a second click within that window runs fn(). Class is
// "btn danger" so both the red .btn.danger and the .btn.confirming styles apply.
// key (optional): task-id+action+surface string identifying this logical
// button across rebuilds; when a rebuild passes the same key while still
// armed, the new button re-renders as "Confirm?" with the remaining window
// instead of resetting to the plain label. Keys must be unique per (task,
// action, surface): two different actions on the same task+surface (e.g.
// the egress gate's Deny and the push gate's Deny, both on the "card"
// surface) MUST use distinct action segments, and two different surfaces
// for the same task+action (e.g. the board card and the review overlay)
// MUST use distinct surface segments: sharing a key across either axis
// would let arming one button silently pre-arm a single click to fire the
// other. An omitted key falls back to an unshared Symbol, which still works
// but loses cross-rebuild persistence: a rebuild always gets a fresh
// unarmed button.
function dangerButton(label, fn, key){
  const gateKey = key != null ? key : Symbol("dangerButton"); // unshared fallback
  const b = el("button", { class: "btn danger" }, label);
  const show = () => { b.textContent = "Confirm?"; b.classList.add("confirming"); };
  const reset = () => { b.textContent = label; b.classList.remove("confirming"); };
  const existing = armedGates.get(gateKey);
  if (existing){
    const msLeft = existing.expiry - Date.now();
    if (msLeft > 0){ show(); scheduleDisarm(gateKey, msLeft, reset); }
    else { clearTimeout(existing.timer); armedGates.delete(gateKey); }
  }
  b.onclick = () => {
    const rec = armedGates.get(gateKey);
    if (rec){ clearTimeout(rec.timer); armedGates.delete(gateKey); reset(); fn(); return; }
    show();
    scheduleDisarm(gateKey, 3000, reset);
  };
  return b;
}
views.board = renderBoard;

// =================== REVIEW (diff overlay) ===================
let overlayState = null; // { close, approve?, deny? } — read by the keys section
function closeOverlay(){ if (overlayState){ overlayState.close(); overlayState = null; } }

// ackCheckbox builds the per-category acknowledgment control for a required
// second-look category. ack = { required, acked:Set, update() }: checking a
// box records the category and calls update(), which enables the overlay's
// Approve button once every required category is acked. This is UX, not the
// security boundary — brokerd independently refuses (422) any approve whose
// {"acknowledge":[...]} body doesn't superset the requirement.
function ackCheckbox(cat, ack){
  const cb = el("input", { type: "checkbox", class: "ack-cb", onchange: () => {
    if (cb.checked) ack.acked.add(cat); else ack.acked.delete(cat);
    ack.update();
  }});
  if (ack.acked.has(cat)) cb.checked = true;
  return el("label", { class: "ack" }, cb, el("span", { class: "ack-text", text: "acknowledge " + safeCell(cat) }));
}

// appendAckRows renders bare acknowledgment rows for required categories that
// have no FLAG row to attach to — including the fallback when the brief
// itself failed to load: the approve must stay reachable (and still gated on
// every checkbox) even without brief evidence on screen.
function appendAckRows(box, cats, ack){
  for (const cat of cats)
    box.append(el("div", { class: "brief-row brief-flag" },
      el("span", { class: "brief-k", text: "ACK" }), ackCheckbox(cat, ack)));
}

// renderBrief renders the trust brief (broker-observed evidence) above the
// diff tabs. Information architecture mirrors cmd/drydock/inspect.go's
// printBrief/printSetup/printVerification exactly: repo+labels, runtime,
// policy, egress, spend, diff summary, FLAG rows, setup block, verification
// block, gaps. Every string that
// could carry work-tree data passes through safeCell, and all text lands via
// el(...,{text:...}) (textContent) — never innerHTML.
// ack (optional): the review overlay's second-look state — each FLAG row
// whose kind is a required category gains an acknowledgment checkbox, and
// required categories without a FLAG row get standalone ACK rows.
function renderBrief(box, b, ack = null){
  box.replaceChildren();
  const t = b.task || {}, rt = b.runtime || {}, pol = b.policy || {}, sp = b.spend || {}, d = b.diff || {};
  const dash = s => s || "(none)";
  const val = (text, cls) => el("span", { class: "brief-v" + (cls ? " " + cls : ""), text });
  const row = (k, cls, ...kids) => el("div", { class: "brief-row" + (cls ? " " + cls : "") },
    el("span", { class: "brief-k", text: k }), ...kids);

  box.append(el("div", { class: "brief-observed", text: "trust brief · broker-observed" }));

  // repo @ base[:12] + sensitive/auto-approve chips
  let repoLine = safeCell(t.repo_ref || "");
  const base = safeCell(String(t.base_commit || "")).slice(0, 12);
  if (base) repoLine += " @ " + base;
  const repoKids = [val(repoLine)];
  if (t.sensitive) repoKids.push(el("span", { class: "brief-chip warn", text: "sensitive" }));
  if (t.auto_approve) repoKids.push(el("span", { class: "brief-chip", text: "auto-approve" }));
  box.append(row("repo", "", ...repoKids));

  box.append(row("runtime", "", val(
    "agent=" + safeCell(rt.agent || "") + " vendor=" + safeCell(rt.vendor || "") +
    " model=" + dash(safeCell(rt.model || "")) + " image=" + safeCell(rt.image_ref || ""))));

  let budget = "$" + Number(pol.budget_usd || 0).toFixed(2) + (pol.budget_hard ? " (hard)" : " (soft)");
  if (pol.budget_unbounded) budget = "uncapped (no USD metering on this lane)";
  box.append(row("policy", "", val(
    "budget " + budget + " · timeout " + Number(pol.timeout_seconds || 0) + "s" +
    " · policy sha " + safeCell(String(pol.snapshot_sha256 || "")).slice(0, 12))));

  box.append(row("egress", "", val(
    "default: " + dash((pol.egress_default || []).map(safeCell).join(" ")) +
    " · widened: " + dash((pol.egress_widened || []).map(safeCell).join(" ")))));

  box.append(row("spend", "", val(
    "$" + Number(sp.usd_broker_metered || 0).toFixed(4) + " · " + fmtDurMs(Number(sp.duration_ms || 0)))));

  // diff summary: sha[:12] · bytes · files (+adds −dels) [TRUNCATED]
  const files = d.files || [];
  let adds = 0, dels = 0;
  for (const f of files){ adds += Number(f.adds || 0); dels += Number(f.dels || 0); }
  const diffKids = [val(
    "sha " + safeCell(String(d.sha256 || "")).slice(0, 12) + " · " + Number(d.bytes || 0) + " bytes · " +
    (files.length + Number(d.files_omitted || 0)) + " files (+" + adds + " −" + dels + ")")];
  if (d.truncated) diffKids.push(el("span", { class: "brief-chip warn", text: "TRUNCATED" }));
  box.append(row("diff", "", ...diffKids));

  const ackRendered = new Set();
  for (const fl of d.flags || []){
    const flagRow = row("FLAG", "brief-flag", val(
      safeCell(fl.kind || "") + ": " + (fl.paths || []).map(safeCell).join(", ")));
    if (ack && ack.required.includes(fl.kind)){
      flagRow.append(ackCheckbox(fl.kind, ack));
      ackRendered.add(fl.kind);
    }
    box.append(flagRow);
  }
  if (ack) appendAckRows(box, ack.required.filter(c => !ackRendered.has(c)), ack);

  // Explicit switch, not an object-index lookup: indexing a plain object
  // with an untrusted status string resolves prototype keys too (e.g.
  // "constructor"), which would yield a truthy non-class value.
  const statusCls = (status) => {
    switch (status) {
      case "passed": return "v-passed";
      case "failed": return "v-failed";
      default: return "v-inconclusive";
    }
  };
  // One evidence-command line (shared by the setup and verification blocks;
  // the vocabulary — passed/failed/timed_out/error/skipped — is identical).
  const appendCmdRows = (cmds) => {
    for (const c of cmds){
      const argv = safeCell((c.argv || []).join(" "));
      let txt, cls;
      switch (c.status) {
      case "passed":
      case "failed":
        txt = argv + " → exit " + Number(c.exit_code || 0) + " (" + fmtDurMs(Number(c.duration_ms || 0)) + ")";
        cls = c.status === "passed" ? "v-passed" : "v-failed";
        break;
      case "skipped":
        txt = argv + " → skipped"; cls = "muted";
        break;
      default: // timed_out, error: no meaningful exit code
        txt = argv + " → " + safeCell(c.status || "") + " (" + fmtDurMs(Number(c.duration_ms || 0)) + ")";
        cls = "v-inconclusive";
      }
      box.append(row("", "", val(txt, cls)));
    }
  };

  // setup block (mirrors printSetup) — before verification: setup runs
  // first. A missing status (a brief from before the setup stage existed)
  // renders as not_configured, same as the CLI.
  const su = b.setup || {};
  if (!su.status || su.status === "not_configured"){
    box.append(row("setup", "", val("not_configured", "muted")));
  } else {
    let sline = safeCell(su.status);
    if (su.network) sline += " · network " + safeCell(su.network);
    box.append(row("setup", "", val(sline, statusCls(su.status))));
    const scmds = su.commands || [];
    appendCmdRows(scmds);
    // display-only pointer to the audit log; no fetch (the log is VM output)
    if (scmds.length) box.append(row("", "", val("log " + safeCell(b.task_id || "") + ".setup.log", "muted")));
  }

  // verification block (mirrors printVerification)
  const v = b.verification || {};
  if (!v.status || v.status === "not_configured"){
    box.append(row("verify", "", val(safeCell(v.status || "not_configured"), "muted")));
  } else {
    let line = safeCell(v.status);
    if (v.network) line += " · network " + safeCell(v.network);
    if (v.credentials === "none") line += " · no credentials";
    else if (v.credentials) line += " · credentials " + safeCell(v.credentials);
    if (v.tree_sha) line += " · tree " + safeCell(String(v.tree_sha)).slice(0, 12);
    box.append(row("verify", "", val(line, statusCls(v.status))));
    const cmds = v.commands || [];
    appendCmdRows(cmds);
    // display-only pointer to the audit log; no fetch (the log is agent output)
    if (cmds.length) box.append(row("", "", val("log " + safeCell(b.task_id || "") + ".verify.log", "muted")));
  }

  for (const m of b.missing_evidence || [])
    box.append(row("gap", "", val(safeCell(m), "muted")));
}

async function openReview(id, readonly = false){
  if (overlayState) closeOverlay();   // close any existing overlay first

  // Second-look acknowledgment state. The required categories come from the
  // live task (/api/tasks → second_look, kept fresh in boardTasks by the
  // board poll); a read-only (history) overlay has no approve, so no acks.
  // The Approve button starts disabled whenever anything is required and is
  // enabled only by allAcked() — and even a bypassed click can't push:
  // brokerd revalidates the ack superset server-side (422 otherwise).
  const liveTask = boardTasks.get(id);
  const required = (!readonly && liveTask && Array.isArray(liveTask.second_look)) ? liveTask.second_look.slice() : [];
  const acked = new Set();
  const allAcked = () => required.every(c => acked.has(c));
  let approveBtn = null; // assigned below (non-readonly only)
  const ackState = required.length ? {
    required, acked,
    update: () => {
      if (!approveBtn) return;
      if (allAcked()) approveBtn.removeAttribute("disabled");
      else approveBtn.setAttribute("disabled", "");
    },
  } : null;
  const doApprove = () => {
    if (!allAcked()){
      toast("acknowledge each second-look category first: " + required.filter(c => !acked.has(c)).map(safeCell).join(", "), "bad");
      return;
    }
    act("approve", id, required.length ? required.slice() : undefined);
    closeOverlay();
  };

  const diffBox = el("div", { class: "tabbody" }, el("p", { class: "muted", text: "loading…" }));
  const logsBox = el("div", { class: "tabbody" }, "");
  // CSSOM property write, not a style attribute: CSP blocks the attribute
  // form, and the tab toggles below flip this same property ("" / "none").
  logsBox.style.display = "none";
  const diffTab = el("button", { class: "tab on" }, "Diff");
  const logsTab = el("button", { class: "tab" }, "Logs");
  let logsLoaded = false;
  diffTab.onclick = () => { diffTab.classList.add("on"); logsTab.classList.remove("on"); diffBox.style.display = ""; logsBox.style.display = "none"; };
  logsTab.onclick = async () => { logsTab.classList.add("on"); diffTab.classList.remove("on"); logsBox.style.display = ""; diffBox.style.display = "none";
    if (!logsLoaded){ logsLoaded = true; try { renderLogs(logsBox, await (await api("GET","/api/logs/"+id)).text()); } catch { logsBox.replaceChildren(el("p", { class: "empty", text: "could not load logs" })); } } };

  // Trust brief: broker-observed evidence, rendered above the tabs so the
  // reviewer triages facts before reading the (agent-authored) diff.
  const briefBox = el("div", { class: "briefbody" }, el("p", { class: "muted", text: "loading…" }));

  const panel = el("div", { class: "panel" },
    el("div", { class: "panel-head" },
      el("strong", { text: "Review " + shortId(id) }),
      el("span", { class: "kbd kbd-row" }, "Esc to close",
        el("button", { class:"tab", onclick: closeOverlay, text: "✕" }))),
    briefBox,
    el("div", { class: "tabs" }, diffTab, logsTab), diffBox, logsBox);
  if (!readonly) {
    approveBtn = el("button", { class: "btn ok", onclick: doApprove }, "Approve push");
    if (required.length) approveBtn.setAttribute("disabled", "");
    panel.append(el("div", { class: "actions" },
      approveBtn,
      dangerButton("Deny", () => { act("deny", id); closeOverlay(); }, id + ":deny-push:overlay"),
      el("span", { class: "kbd" }, "A approve · D deny · Esc close")));
  }

  // Esc is owned by the global keydown registry (via overlayState), not a
  // per-open listener here — exactly one keydown handler in the app.
  const overlay = el("div", { class: "overlay", onclick: (e) => { if (e.target === overlay) closeOverlay(); } }, panel);
  document.body.append(overlay);
  // Keyboard approve routes through the same doApprove gate as the button:
  // unchecked second-look boxes refuse (toast) instead of firing an approve.
  overlayState = { close: () => { overlay.remove(); },
                   approve: readonly ? null : doApprove,
                   deny: readonly ? null : () => { act("deny", id); closeOverlay(); } };
  // Fetch the brief concurrently — the diff fetch below must never wait on it
  // (older tasks have no brief; the diff is the primary review surface).
  // When the brief is missing/unreadable but acks are required, standalone
  // ACK rows still render: the approve stays reachable and stays gated.
  (async () => {
    try {
      const res = await api("GET", "/api/brief/" + id);
      if (res.status === 404) {
        briefBox.replaceChildren(el("p", { class: "muted", text: "no trust brief recorded" }));
        if (ackState) appendAckRows(briefBox, ackState.required, ackState);
        return;
      }
      if (!res.ok) throw new Error(`${res.status}`);
      renderBrief(briefBox, await res.json(), ackState);
    } catch {
      briefBox.replaceChildren(el("p", { class: "muted", text: "could not load trust brief" }));
      if (ackState) appendAckRows(briefBox, ackState.required, ackState);
    }
  })();
  try {
    const res = await api("GET", "/api/diff/" + id);
    if (res.status === 404) { diffBox.replaceChildren(el("p", { class: "muted", text: "no diff" })); return; }
    renderDiff(diffBox, await res.text());
  } catch { diffBox.replaceChildren(el("p", { class: "empty", text: "could not load diff" })); }
}

function renderLogs(box, text){
  box.replaceChildren();
  const pre = el("pre", { class: "logs-pre" });
  for (const line of text.split("\n")){
    if (!line.trim()) continue;
    let o; try { o = JSON.parse(line); } catch { pre.append(el("span", { class: "lg", text: line + "\n" })); continue; }
    if (o.type === "stream_event" || o.type === "drydock_meta" || o.type === "metrics") continue; // streaming deltas + meta + broker metrics row: noise, the coarse events carry the content
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
// Two-press confirm for keyboard deny — first d arms, second d (within 3 s) fires.
let denyArmed = false, denyTimer = null;
function disarmDeny(){ denyArmed = false; if (denyTimer) clearTimeout(denyTimer); denyTimer = null; }
function armOrDeny(doDeny){
  if (denyArmed){ disarmDeny(); doDeny(); return; }
  denyArmed = true;
  toast("press d again to deny · Esc cancels");
  denyTimer = setTimeout(disarmDeny, 3000);
}
addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "Enter"){ if (currentView === "submit") document.querySelector(".submit-form")?.requestSubmit(); return; }
  if (isTyping(e)) return;                       // never hijack typing
  if (overlayState){
    if (e.key === "Escape"){ disarmDeny(); closeOverlay(); }
    else if (e.key === "a" && overlayState.approve){ disarmDeny(); overlayState.approve(); }
    else if (e.key === "d" && overlayState.deny) armOrDeny(() => overlayState.deny());
    return;
  }
  if (e.key === "?"){ toast("R review · A approve · D deny · Esc close · ⌘↵ submit"); return; }
  if (currentView === "board"){
    const g = singleGate();
    if (g){
      if (e.key === "r"){ disarmDeny(); openReview(g.id); }
      else if (e.key === "a"){ disarmDeny(); act("approve", g.id); }
      else if (e.key === "d") armOrDeny(() => act("deny", g.id));
    }
  }
});

// =================== SUBMIT ===================
const REPO_RE = /^(https?:\/\/|git@|ssh:\/\/|git:\/\/)\S+$/;
function validRepo(v){ v = (v || "").trim(); return REPO_RE.test(v) && !v.startsWith("/") && !v.startsWith("."); }
function recentRepos(){ try { return JSON.parse(localStorage.getItem("dd.repos") || "[]"); } catch { return []; } }
function pushRecentRepo(r){ const a = [r, ...recentRepos().filter(x => x !== r)].slice(0, 5); localStorage.setItem("dd.repos", JSON.stringify(a)); }
const MODEL_SUGGESTIONS = ["", "claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5-20251001"]; // "" = broker default; update when models change

const AGENTS = ["claude", "codex", "gemini", "opencode"];
function renderSubmit() {
  if (pollTimer) clearTimeout(pollTimer);
  const form = el("form", { class: "submit-form" });
  const repo = el("input", { type: "text", placeholder: "git@github.com:owner/repo", required: "" });
  const okMark = el("span", { class: "ok-mark", text: "✓" });
  // CSSOM write (CSP-safe); repo.oninput below toggles this same property.
  okMark.style.display = "none";
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
function label(text, input) { return el("label", {}, el("span", { class: "lab", text }), input); }
views.submit = renderSubmit;

// =================== HISTORY ===================
async function renderHistory() {
  if (pollTimer) clearTimeout(pollTimer);
  let items;
  try { items = await apiJSON("/api/history"); }
  catch (e) { app().replaceChildren(el("p", { class: "empty", text: "could not load history" })); return; }
  const table = el("table", { class: "history" });
  table.append(el("tr", {}, ...["ID", "AGE", "DUR", "COST", "OUTCOME"].map(h => el("th", { text: h }))));
  if (items.length === 0) table.append(el("tr", {}, el("td", { colspan: "5", class: "muted", text: "no tasks yet" })));
  for (const it of items) {
    const row = el("tr", { class: "hrow" },
      el("td", {}, el("code", { onclick: () => navigator.clipboard && navigator.clipboard.writeText(it.id), title: "copy", text: it.id.slice(0, 12) })),
      el("td", { text: fmtAgeFromUnix(it.mtime_unix) }),
      el("td", { text: it.has_duration ? fmtDurMs(it.duration_ms) : "-" }),
      costCell(it),
      el("td", { text: it.outcome }));
    row.onclick = () => openReview(it.id, true); // read-only diff/logs for past tasks
    table.append(row);
  }
  app().replaceChildren(el("h2", { text: "History" }), table);
}
views.history = renderHistory;

// boot
show("board");
