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

async function renderBoard() {
  let tasks;
  try {
    tasks = await apiJSON("/api/tasks");
    setConn("brokerd connected", true);
  } catch (e) {
    // A 401/403 means the access token was rejected (it changed since this tab
    // opened) — NOT that brokerd is down. Don't conflate the two.
    if (e.message === "401" || e.message === "403") {
      setConn("unauthorized — reopen the `drydock ui` link", false);
      app().replaceChildren(el("p", { class: "empty", text: "Access token rejected. Reopen the URL printed by `drydock ui` — the token may have changed since this tab was opened." }));
    } else {
      setConn("brokerd not running — run `drydock start`", false);
      app().replaceChildren(el("p", { class: "empty", text: "brokerd is not running. Start it with `drydock start`, then this board will populate." }));
    }
    scheduleBoardPoll(tasks);
    return;
  }
  // gate tasks always float to top.
  const gateRank = s => (s === "awaiting_approval" || s === "awaiting_egress" ? 0 : 1);
  tasks.sort((a, b) => gateRank(a.stage) - gateRank(b.stage));

  const container = el("div", { class: "board" });
  if (tasks.length === 0) {
    container.append(el("p", { class: "empty" }, "No tasks running. ", el("a", { href: "#", onclick: () => show("submit") }, "Submit one"), " or see ", el("a", { href: "#", onclick: () => show("history") }, "History"), "."));
  }
  for (const t of tasks) container.append(taskCard(t));

  // "Just finished" strip: show the most recent completed tasks so a task that
  // leaves the live set (its most interesting moment) doesn't silently vanish.
  const liveIDs = new Set(tasks.map(t => t.id));
  try {
    const hist = await apiJSON("/api/history");
    const recent = hist.filter(h => !liveIDs.has(h.id)).slice(0, 5);
    if (recent.length) {
      const strip = el("div", { class: "recent" }, el("div", { class: "recent-title", text: "Just finished" }));
      for (const it of recent) {
        strip.append(el("div", { class: "recent-row hrow", onclick: () => openReview(it.id, true) },
          el("code", { text: it.id.slice(0, 12) }),
          el("span", { class: "age", text: fmtAgeFromUnix(it.mtime_unix) }),
          el("span", { text: it.cost }),
          el("span", { class: "outcome", text: it.outcome })));
      }
      container.append(strip);
    }
  } catch (_) { /* history is best-effort on the board */ }

  app().replaceChildren(container);
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

function stageBadge(stage) {
  const label = { awaiting_egress: "egress?", running: "running", awaiting_approval: "review?", pushing: "pushing" }[stage] || stage;
  return el("span", { class: "badge stage-" + stage, text: label });
}

function taskCard(t) {
  const card = el("div", { class: "card", "data-tid": t.id });
  const head = el("div", { class: "card-head" },
    el("code", { class: "tid", title: "click to copy", onclick: () => navigator.clipboard && navigator.clipboard.writeText(t.id), text: t.id.slice(0, 12) }),
    stageBadge(t.stage),
    el("span", { class: "age", text: elapsed(t.started_at) }));
  card.append(head);
  card.append(el("div", { class: "repo", text: shortRepo(t.repo) }));
  card.append(el("div", { class: "instr", text: t.instruction || "" }));

  if (t.stage === "awaiting_egress") card.append(egressGate(t));
  else if (t.stage === "awaiting_approval") card.append(pushGate(t));
  if (t.stage === "running" || t.stage === "pushing" || t.stage === "awaiting_egress" || t.stage === "awaiting_approval") {
    card.append(el("div", { class: "actions" }, dangerButton("Kill", () => act("kill", t.id))));
  }
  return card;
}

function shortRepo(r) { const i = r.lastIndexOf(":"); return i >= 0 ? r.slice(i + 1) : r; }

// egress gate: show the requested hosts (from the persisted widen file) PLUS the
// instruction/repo so the operator can judge WHY the host was requested.
function egressGate(t) {
  const box = el("div", { class: "gate egress" }, el("div", { class: "gate-title", text: "Egress widening requested" }));
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
  const box = el("div", { class: "gate push" }, el("div", { class: "gate-title", text: "Push awaiting review" }));
  const cost = el("span", { class: "cost", text: "spent: …" });
  box.append(cost);
  spentSoFar(t.id).then(s => (cost.textContent = s));
  const approveBtn = el("button", { class: "ok", disabled: "", onclick: () => act("approve", t.id) }, "Approve push");
  box.append(el("div", { class: "actions" },
    el("button", { onclick: () => { openReview(t.id); approveBtn.removeAttribute("disabled"); } }, "Review diff"),
    approveBtn,
    dangerButton("Deny", () => act("deny", t.id))));
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
// deny/kill are destructive, so they confirm first.
async function act(verb, id) {
  if ((verb === "deny" || verb === "kill") && !confirm(`${verb} task ${id.slice(0, 12)}? This cannot be undone.`)) return;
  try {
    const res = await api("POST", "/api/" + verb + "/" + id);
    if (res.status === 409 || res.status === 404) { /* already resolved — just refresh */ }
    else if (!res.ok) alert(`${verb} failed: HTTP ${res.status}`);
  } catch (e) { alert(`${verb} failed: ${e.message}`); }
  renderBoard(); // immediate re-poll (don't wait the interval)
}

function dangerButton(label, fn) { return el("button", { class: "danger", onclick: fn }, label); }
views.board = renderBoard;

// =================== REVIEW (diff overlay) ===================
async function openReview(id, readonly = false) {
  const overlay = el("div", { class: "overlay" });
  const panel = el("div", { class: "panel" });
  panel.append(el("div", { class: "panel-head" },
    el("strong", { text: "Review " + id.slice(0, 12) }),
    el("button", { class: "close", onclick: () => overlay.remove() }, "✕")));
  const diffBox = el("div", { class: "diff", text: "loading diff…" });
  panel.append(diffBox);
  if (!readonly) {
    panel.append(el("div", { class: "actions" },
      el("button", { class: "ok", onclick: () => { act("approve", id); overlay.remove(); } }, "Approve push"),
      dangerButton("Deny", () => { act("deny", id); overlay.remove(); })));
  }
  overlay.append(panel);
  document.body.append(overlay);
  try {
    const res = await api("GET", "/api/diff/" + id);
    if (res.status === 404) { diffBox.textContent = "no diff yet"; return; }
    renderDiff(diffBox, await res.text());
  } catch (_) { diffBox.textContent = "could not load diff"; }
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

// =================== SUBMIT ===================
const AGENTS = ["claude", "codex", "opencode"];
function renderSubmit() {
  if (pollTimer) clearTimeout(pollTimer);
  const form = el("form", { class: "submit-form" });
  const repo = el("input", { type: "text", placeholder: "https://github.com/owner/repo.git", required: "" });
  const instr = el("textarea", { placeholder: "What should the agent do?", rows: "4", required: "" });
  const agent = el("select", {});
  for (const a of AGENTS) agent.append(el("option", { value: a, text: a }));
  const model = el("input", { type: "text", placeholder: "model (optional)" });
  const msg = el("div", { class: "msg" });
  form.append(
    label("Repo URL (https/git/ssh — no local paths)", repo),
    label("Instruction", instr),
    label("Agent", agent),
    label("Model", model),
    el("button", { type: "submit", class: "ok" }, "Submit task"),
    msg);
  form.onsubmit = async (e) => {
    e.preventDefault();
    msg.textContent = "submitting…";
    const body = { repo_ref: repo.value.trim(), instruction: instr.value, agent: agent.value };
    if (model.value.trim()) body.model = model.value.trim();
    try {
      const res = await api("POST", "/api/submit", body);
      const txt = await res.text();
      if (!res.ok) { msg.textContent = "error: " + txt; return; }
      const { id } = JSON.parse(txt);
      newTaskID = id;
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
