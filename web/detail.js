// Task drawer: actions, runs timeline, docs and config.
import { h, api, toast, statusLabel, statusCls, triggerText, hm, mdhm, relative, richText, icon } from "./util.js";
import { t } from "./i18n.js";
import { confirmDialog } from "./confirm.js";
import { startTabsTour } from "./tour.js";
import { draftField, clearDrafts, keepFocus, hasDraftIn } from "./drafts.js";
import { trustBlock } from "./trust.js";

const drawer = document.getElementById("drawer");
const body = document.getElementById("detail");
let current = null; // { id, tab, data, hooks }

drawer.addEventListener("click", (e) => {
  if (e.target.hasAttribute("data-close")) location.hash = "";
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && current && !document.querySelector("dialog[open]")) location.hash = "";
});

export const currentDetail = () => current?.id ?? null;

/** Open a task's drawer; focusRun (a dot clicked on the dial) scrolls to and highlights that run. */
export async function openDetail(id, hooks, focusRun) {
  const sameTask = current?.id === id;
  const tab = focusRun ? "runs" : sameTask ? current.tab : "runs";
  current = { id, tab, data: null, hooks, focus: focusRun || null };
  drawer.classList.add("open");
  drawer.setAttribute("aria-hidden", "false");
  if (!sameTask) body.replaceChildren(h("p", { class: "d-meta", text: t("detail.loading") }));
  await refreshDetail();
}

export function closeDetail() {
  current = null;
  drawer.classList.remove("open");
  drawer.setAttribute("aria-hidden", "true");
}

export async function refreshDetail() {
  if (!current) return;
  const id = current.id;
  try {
    const focus = current.focus;
    const data = await api("GET", `/api/tasks/${id}${focus ? `?run=${encodeURIComponent(focus)}` : ""}`);
    if (current?.id !== id) return;
    current.data = { ...data, runs: data.runs || [], proposals: data.proposals || [], docs: data.docs || {} };
    render();
    if (focus) {
      current.focus = null;
      showRun(focus, data.view);
    }
  } catch (err) {
    body.replaceChildren(h("p", { class: "d-meta", text: t("detail.loadFail", { msg: err.message }) }));
  }
}

/** Run an action; when given the clicked button, it shows `busy` and stays disabled until the action ends. */
async function act(label, fn, btn, busy) {
  const idle = btn?.textContent;
  if (btn) {
    btn.disabled = true;
    btn.setAttribute("aria-busy", "true");
    if (busy) btn.textContent = busy;
  }
  try {
    const out = await fn();
    toast(out?.hint || label);
    current?.hooks.onChange();
    refreshDetail();
  } catch (err) {
    toast(err.message, true);
  } finally {
    if (btn?.isConnected) {
      btn.disabled = false;
      btn.removeAttribute("aria-busy");
      btn.textContent = idle;
    }
  }
}

/** Bring the clicked run into view: the question card when it is the one waiting, else its timeline entry. */
function showRun(runID, view) {
  const target = view.needs_you?.id === runID
    ? body.querySelector(".need")
    : body.querySelector(`[data-runs~="${CSS.escape(runID)}"]`);
  if (!target) return;
  target.classList.add("focus");
  requestAnimationFrame(() => target.scrollIntoView({ behavior: "smooth", block: "center" }));
  setTimeout(() => target.classList.remove("focus"), 2600);
}

function render() {
  if (document.activeElement?.matches?.("textarea.doc, .answer input")) return; // never clobber typing
  if (hasDraftIn(body.querySelector(".need"))) return; // an unsent answer holds the drawer as it is
  keepFocus(draw);
}

/** What the task cost over the last week and what it produced for that; nothing when it cost nothing. */
function weekLine(week) {
  if (!week || !week.cost_usd) return null;
  const o = week.output || {};
  const cost = week.cost_usd.toFixed(2);
  const text = o.total
    ? t("detail.week", { cost, total: o.total, made: o.made, replies: o.replies, taken: o.todos_taken, per: (o.cost_per_output || 0).toFixed(2) })
    : t("detail.weekNone", { cost });
  return h("p", { class: "d-meta d-week", text });
}

const AUTONOMY_DAYS = 7;
const AUTONOMY_BAR_PX = 28;
const AUTONOMY_TOP_KINDS = 3;

/**
 * The north star: how often the owner still had to step in on this task's to-dos, per day for a week.
 * Grey is all to-dos, amber the ones that took the owner's own words (typed, edited, answered elsewhere).
 */
function autonomyBlock(a) {
  if (!a || !a.days || !a.days.length) return null;
  const byDate = new Map(a.days.map((d) => [d.date, d]));
  const dates = Array.from({ length: AUTONOMY_DAYS }, (_, i) => {
    const d = new Date();
    d.setDate(d.getDate() - (AUTONOMY_DAYS - 1 - i));
    return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  });
  const sum = (key) => a.days.reduce((n, d) => n + (d[key] || 0), 0);
  const peak = Math.max(1, ...a.days.map((d) => d.total));
  const bars = dates.map((date) => {
    const d = byDate.get(date) || { total: 0, effort: 0 };
    const px = (n) => `${Math.round((n / peak) * AUTONOMY_BAR_PX)}px`;
    const tip = d.total ? t("auto.dayTip", { date, total: d.total, effort: d.effort, typed: d.typed, edited: d.edited,
      elsewhere: d.elsewhere, picked: d.picked, asis: d.as_is }) : t("auto.dayNone", { date });
    return h("div", { class: "auto-day", title: tip, "aria-label": tip },
      h("div", { class: "auto-bar", style: `height:${px(d.total)}` }, h("i", { style: `height:${px(d.effort)}` })),
      h("span", { text: date.slice(3) }));
  });
  const kinds = a.kinds.filter((k) => k.effort > 0).slice(0, AUTONOMY_TOP_KINDS);
  return h("section", { class: "autonomy", title: t("auto.help") },
    h("p", { class: "d-meta" }, h("b", { text: t("auto.title") }),
      t("auto.sum", { effort: sum("effort"), total: sum("total"), picked: sum("picked"), asis: sum("as_is") })),
    h("div", { class: "auto-bars", role: "img", "aria-label": t("auto.aria") }, ...bars),
    kinds.length ? h("p", { class: "d-meta", text: t("auto.kinds", { list: kinds.map((k) => `${k.kind || "—"} ${k.effort}`).join(" · ") }) }) : null);
}

function draw() {
  const { view, held } = current.data;
  const task = view.task;
  const next = view.next_run ? t("detail.nextIn", { when: relative(new Date(view.next_run)) }) : t(task.enabled ? "task.waiting" : "detail.paused");
  const mode = t(task.mode === "resident" ? "mode.resident" : "mode.headless");
  body.replaceChildren(...[
    h("div", { class: "d-head" },
      h("div", {},
        h("h2", { id: "d-title", text: task.name }),
        h("p", { class: "d-meta", text: [task.id, mode, triggerText(task), next, t("detail.runCount", { n: view.state.run_count })].join(" · ") })),
      h("div", { class: "d-head-tools" },
        h("button", { class: "head-btn", id: "d-tour", type: "button", title: t("tour.tabsTitle"), "aria-label": t("tour.tabsTitle"),
          onclick: () => startTabsTour() }, icon("help", 18)),
        h("button", { class: "head-btn", type: "button", title: t("detail.close"), "aria-label": t("detail.close"),
          onclick: () => { location.hash = ""; } }, icon("close", 18)))),
    weekLine(current.data.week),
    autonomyBlock(current.data.autonomy),
    held ? h("p", { class: "note", text: t("detail.held") }) : null,
    view.needs_you ? needsBlock(task.id, view.needs_you) : null,
    view.answered ? h("p", { class: "answered", text: t("detail.answeredQ", { q: view.answered.question, a: view.answered.answer }) }) : null,
    actions(task, view),
    trustBlock(task.id),
    tabs(view),
    panel(),
  ].filter(Boolean));
}

function needsBlock(id, r) {
  const key = `need:${id}:${r.id}`;
  const input = draftField(h("input", { type: "text", placeholder: t("detail.answerPh"), "aria-label": t("detail.answerAria") }), key);
  let block;
  const done = (text) => {
    block.replaceWith(h("p", { class: "answered", text: t("needs.answered", { text }) }));
    clearDrafts(key);
    return current.hooks.onAnswer(id, text).then(refreshDetail);
  };
  const send = () => input.value.trim() && done(input.value.trim());
  input.addEventListener("keydown", (e) => { if (e.key === "Enter") send(); });
  block = h("article", { class: "need" },
    h("div", { class: "need-head" }, h("span", { text: t("detail.waiting") }), h("time", { text: mdhm(new Date(r.started_at)) })),
    h("p", { class: "need-q" }, ...richText(r.question || r.summary)),
    h("p", { class: "need-s" }, ...richText(r.summary)),
    h("div", { class: "options" }, ...(r.options || []).map((o) =>
      h("button", { class: `option${r.recommendation && o.trim().startsWith(r.recommendation.trim()) ? " rec" : ""}`, type: "button", text: o,
        onclick: () => done(o) }))),
    h("div", { class: "answer" }, input, h("button", { class: "btn", type: "button", text: t("detail.answer"), onclick: send })));
  return block;
}

function actions(task, view) {
  const id = task.id;
  return h("div", { class: "d-actions" },
    h("button", { class: "btn primary", type: "button", disabled: !!view.live, text: t(view.live ? "act.running" : "act.run"),
      onclick: () => act(t("act.queued"), () => api("POST", `/api/tasks/${id}/run`, {})) }),
    task.enabled
      ? h("button", { class: "btn", type: "button", text: t("act.pause"), onclick: () => act(t("act.paused"), () => api("POST", `/api/tasks/${id}/pause`)) })
      : h("button", { class: "btn", type: "button", text: t("act.resume"), onclick: () => act(t("act.resumed"), () => api("POST", `/api/tasks/${id}/resume`)) }),
    h("button", { class: "btn", type: "button", text: t("act.open"), title: t("act.openTitle"),
      onclick: (e) => act(t("act.opened"), () => api("POST", `/api/tasks/${id}/open`), e.currentTarget, t("act.opening")) }),
    view.reserved ? h("span", { class: "d-reserved", text: t("act.reserved") }) : h("button", { class: "btn danger", type: "button", text: t("act.delete"),
      onclick: async () => {
        const yes = await confirmDialog({ title: t("act.deleteTitle", { name: task.name }), body: t("act.deleteBody"),
          confirmText: t("act.delete"), danger: true });
        if (yes) act(t("act.deleted"), async () => { await api("DELETE", `/api/tasks/${id}`); location.hash = ""; });
      } }));
}

const TABS = [
  "runs", "brief", "runbook", "memory", "evolve", "config",
];

function tabs(view) {
  return h("div", { class: "tabs", role: "tablist" }, ...TABS.map((key) =>
    h("button", { class: "tab", role: "tab", type: "button", "aria-selected": String(current.tab === key),
      onclick: () => { current.tab = key; render(); } },
      t(`tab.${key}`), key === "runbook" && view.proposals ? h("span", { class: "badge", text: `+${view.proposals}` }) : null)));
}

function panel() {
  switch (current.tab) {
    case "brief": return docEditor("brief", t("doc.brief"));
    case "runbook": return h("div", {}, ...current.data.proposals.map(proposal), docEditor("runbook", t("doc.runbook")));
    case "memory": return memoryPanel();
    case "evolve": return evolutionPanel();
    case "config": return configEditor();
    default: return timeline();
  }
}

function timeline() {
  const runs = current.data.runs;
  if (!runs.length) return h("p", { class: "d-meta", text: t("runs.none") });
  return h("ol", { class: "timeline" }, ...foldSkips(runs).map(runItem));
}

/**
 * Consecutive skips for the same reason (a takeover, an unchanged signal) become one entry spanning
 * the oldest start to the newest end. Runs arrive newest first.
 */
function foldSkips(runs) {
  const out = [];
  for (const r of runs) {
    const prev = out[out.length - 1];
    const count = r.repeats || 1;
    if (prev && r.status === "skipped" && prev.status === "skipped" && prev.summary === r.summary) {
      prev.started_at = r.started_at;
      prev.repeats += count;
      prev.ids.push(r.id);
      continue;
    }
    out.push({ ...r, repeats: count, ids: [r.id] });
  }
  return out;
}

function runItem(r) {
  const folded = r.repeats > 1;
  const when = folded ? `${mdhm(new Date(r.started_at))} – ${hm(new Date(r.ended_at))}` : mdhm(new Date(r.started_at));
  const meta = [when, r.job ? t("runs.job") : r.trigger, statusLabel(r.status)];
  if (folded) meta.push(t("runs.repeats", { n: r.repeats }));
  else if (r.ended_at) meta.push(`${Math.max(1, Math.round((new Date(r.ended_at) - new Date(r.started_at)) / 1000))}s`);
  if (r.cost_usd) meta.push(`$${r.cost_usd.toFixed(2)}`);
  if (!r.structured && r.status === "ok") meta.push(t("runs.unstructured"));
  const li = h("li", { class: `run ${r.status}${folded ? " folded" : ""}`, "data-runs": (r.ids || [r.id]).join(" ") },
    h("div", { class: "run-top" }, ...meta.map((m, i) => h("span", { class: i === 2 ? "st" : "", text: m }))),
    r.job_title ? h("p", { class: "run-job", text: r.job_title }) : null,
    r.model ? h("p", { class: "run-model", text: t("run.light", { model: r.model }) }) : null,
    h("p", { class: "run-sum" }, ...richText(r.summary || (r.status === "running" ? t("runs.executing") : ""))),
    r.question ? h("p", { class: "run-q" }, ...richText(`↳ ${r.question}  ${(r.options || []).join(" / ")}`)) : null,
    r.input ? h("details", {}, h("summary", { class: "linkish", text: t("runs.input") }), h("pre", { class: "transcript", text: r.input })) : null);
  return li;
}

function docEditor(doc, hint) {
  const key = `doc:${current.id}:${doc}`;
  const area = draftField(h("textarea", { class: "doc", spellcheck: "false", "aria-label": hint }), key, current.data.docs[doc] || "");
  const save = h("button", { class: "btn", type: "button", text: t("doc.save"),
    onclick: () => act(t("doc.saved"), async () => {
      const out = await api("PUT", `/api/tasks/${current.id}/docs/${doc}`, { text: area.value });
      clearDrafts(key);
      return out;
    }) });
  return h("div", {}, area, h("div", { class: "doc-bar" }, h("span", { text: hint }), save));
}

/** Rules (memory.md, read every round, capped) above the knowledge base (facts by topic, read on demand). */
function memoryPanel() {
  const m = current.data.memory || { bytes: 0, cap: 0, knowledge: [] };
  const over = m.cap > 0 && m.bytes > m.cap;
  return h("div", {},
    h("p", { class: `mem-size${over ? " over" : ""}`, text: t(over ? "memory.over" : "memory.size", { size: kb(m.bytes), cap: kb(m.cap) }) }),
    docEditor("memory", t("doc.memory")),
    knowledgeSection(m.knowledge || []));
}

/** What self-evolution has changed in this task, newest first, with each change's verdict or review date. */
function evolutionPanel() {
  const ev = current.data.evolution || { policy: "", counts: {}, changes: [] };
  const n = (status) => ev.counts[status] || 0;
  const total = Object.values(ev.counts).reduce((a, b) => a + b, 0);
  return h("div", { class: "evolution" },
    h("p", { class: "d-meta" }, t(`evo.policy.${ev.policy}`), " · ",
      total ? t("evo.counts", { total, watching: n("watching"), kept: n("kept"), reverted: n("reverted") }) : t("evo.none")),
    (ev.rounds || []).length ? h("section", { class: "evo-rounds" },
      h("h3", { text: t("evo.rounds", { n: ev.rounds.length }) }),
      h("ol", { class: "evo-list" }, ...ev.rounds.map(roundItem))) : null,
    ev.changes.length ? h("h3", { class: "evo-sub", text: t("evo.changes") }) : null,
    ev.changes.length ? h("ol", { class: "evo-list" }, ...ev.changes.map(evolutionItem)) : null);
}

/** One self-evolution round about this task: when, how it ended, what it cost and what it did. */
function roundItem(r) {
  return h("li", { class: "evo-item" },
    h("div", { class: "evo-head" },
      h("time", { text: mdhm(new Date(r.started_at)) }),
      h("span", { class: `evo-status ${statusCls(r.status)}`, text: statusLabel(r.status) }),
      r.cost_usd ? h("span", { text: "$" + r.cost_usd.toFixed(2) }) : null),
    h("p", { class: "evo-change" }, ...richText(r.summary || "")));
}

function evolutionItem(c) {
  const status = c.status in EVO_TONE ? c.status : "watching";
  const since = c.since && c.since.total ? t("evo.measure", { kind: c.metric.split("/").pop(),
    before: c.baseline ? `${c.baseline.effort}/${c.baseline.total}` : "—", after: `${c.since.effort}/${c.since.total}` }) : null;
  const tail = status === "watching" ? t("evo.review", { date: mdhm(new Date(c.review_at)) }) : c.verdict ? t("evo.verdict", { text: c.verdict }) : null;
  return h("li", { class: "evo-item" },
    h("div", { class: "evo-head" },
      h("time", { text: mdhm(new Date(c.at)) }),
      h("span", { class: `evo-status ${EVO_TONE[status]}`, text: t(`evo.status.${status}`) }),
      h("span", { class: "evo-target", text: c.target })),
    h("p", { class: "evo-change" }, ...richText(c.change)),
    h("p", { class: "evo-line", text: t("evo.expect", { text: c.expect }) }),
    since ? h("p", { class: "evo-line evo-measure", text: since }) : null,
    tail ? h("p", { class: "evo-line" }, ...richText(tail)) : null,
    c.why ? h("details", { class: "evo-why" }, h("summary", { text: t("evo.why") }), h("p", {}, ...richText(c.why))) : null);
}

const EVO_TONE = { watching: "live", kept: "ok", reverted: "bad", rejected_by_replay: "skip" };

const kb = (bytes) => `${(bytes / 1024).toFixed(1)} KB`;

function knowledgeSection(topics) {
  const entries = topics.reduce((n, k) => n + k.entries, 0);
  return h("section", { class: "knowledge" },
    h("h3", { text: t("knowledge.title", { n: topics.length, entries }) }),
    h("p", { class: "d-meta", text: t(topics.length ? "knowledge.hint" : "knowledge.empty") }),
    ...topics.map(topicItem));
}

function topicItem(k) {
  const body = h("ol", { class: "knowledge-body" }, h("li", { class: "d-meta", text: t("knowledge.loading") }));
  const item = h("details", { class: "knowledge-topic" },
    h("summary", {}, h("b", { text: k.topic }), h("span", { class: "d-meta", text: t("knowledge.meta", { n: k.entries, size: kb(k.bytes) }) })),
    body);
  let loaded = false;
  item.addEventListener("toggle", async () => {
    if (!item.open || loaded) return;
    loaded = true;
    try {
      const out = await api("GET", `/api/tasks/${encodeURIComponent(current.id)}/knowledge/${encodeURIComponent(k.topic)}`);
      body.replaceChildren(...knowledgeLines(out.content));
    } catch (err) {
      loaded = false;
      body.replaceChildren(h("li", { class: "d-meta", text: err.message }));
    }
  });
  return item;
}

/** "- 2026-11-19 fact" lines as dated list items; links in a fact stay clickable. */
function knowledgeLines(content) {
  return content.split("\n").filter((line) => line.startsWith("- ")).map((line) => {
    const m = line.match(/^- (\d{4}-\d{2}-\d{2})\s+(.*)$/);
    return h("li", {}, m ? h("time", { text: m[1].slice(5) }) : null, h("span", {}, ...richText(m ? m[2] : line.slice(2))));
  });
}

function configEditor() {
  const key = `config:${current.id}`;
  const area = draftField(h("textarea", { class: "doc", spellcheck: "false", "aria-label": "task.yaml" }), key, current.data.yaml);
  const save = h("button", { class: "btn", type: "button", text: t("doc.saveConfig"),
    onclick: () => act(t("doc.configSaved"), async () => {
      const out = await api("POST", "/api/tasks", { yaml: area.value });
      clearDrafts(key);
      return out;
    }) });
  return h("div", {}, area, h("div", { class: "doc-bar" }, h("span", { text: t("doc.configHint") }), save));
}

function proposal(p) {
  const decide = (approve) => act(t(approve ? "proposal.approved" : "proposal.rejected"),
    () => api("POST", `/api/tasks/${current.id}/proposals/${p.name}`, { approve }));
  return h("section", { class: "proposal" },
    h("h3", { text: p.reason ? t("proposal.reason", { reason: p.reason }) : t("proposal.title") }),
    h("div", { class: "diff" }, ...diffLines(current.data.docs.runbook || "", p.content)),
    h("div", { class: "d-actions" },
      h("button", { class: "btn primary", type: "button", text: t("proposal.approve"), onclick: () => decide(true) }),
      h("button", { class: "btn", type: "button", text: t("proposal.reject"), onclick: () => decide(false) })));
}

/** Line diff via LCS; fine for runbook-sized documents. */
function diffLines(a, b) {
  const x = a.split("\n"), y = b.split("\n");
  const n = x.length, m = y.length;
  const dp = Array.from({ length: n + 1 }, () => new Uint16Array(m + 1));
  for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--)
    dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  const out = [];
  let i = 0, j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && x[i] === y[j]) { out.push(h("div", { text: `  ${x[i]}` })); i++; j++; }
    else if (j < m && (i >= n || dp[i][j + 1] >= dp[i + 1][j])) { out.push(h("div", { class: "add", text: `+ ${y[j]}` })); j++; }
    else { out.push(h("div", { class: "del", text: `- ${x[i]}` })); i++; }
  }
  return out;
}
