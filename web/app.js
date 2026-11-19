// Console entry: overview, live updates, routing.
import { h, api, events, toast, statusCls, triggerText, hm, relative, debounce, richText, plainText } from "./util.js";
import { renderDial } from "./dial.js";
import { openDetail, closeDetail, refreshDetail, currentDetail } from "./detail.js";
import { openForge } from "./forge.js";
import { t, applyStatic } from "./i18n.js";
import { todoCards, followList } from "./todos.js";
import { notifyDesktop, askNotifyPermission } from "./notify.js";
import { maybeWelcome, maybeTabsTour, replayTour } from "./tour.js";
import { initSettings, getPrefs, setPrefs } from "./settings.js";
import { exportStarted } from "./migrate.js";
import { keepFocus, hasDraftIn } from "./drafts.js";

const PULSE_BARS = 24;
const CLOCK_TICK = 20_000;

const state = { views: [], todos: [], follows: [], problems: [], hot: null, filter: null, first: true };

const $ = (id) => document.getElementById(id);

async function load() {
  try {
    const data = await api("GET", "/api/overview");
    state.views = (data.tasks || []).map((v) => ({ ...v, recent: v.recent || [] }));
    state.todos = data.todos || [];
    state.follows = data.follows || [];
    state.draining = !!data.draining;
    state.problems = data.problems || [];
    render();
  } catch (err) {
    // After a move was started the daemon stops on purpose: say so instead of showing an error.
    $("headline").textContent = exportStarted() ? t("mig.offlineHead") : t("conn.fail", { msg: err.message });
  }
}

function render() {
  renderLegend();
  renderHeadline();
  renderNeeds();
  renderTasks();
  drawDial();
}

function drawDial() {
  renderDial($("dial"), state.views, {
    now: new Date(), first: state.first, hot: state.hot, filter: state.filter,
    onTip: showTip, onPick: go, onHover: setHot,
  });
  state.first = false;
}

function renderHeadline() {
  const el = $("headline");
  const vs = state.views;
  const live = vs.filter((v) => v.live).length;
  const jobs = vs.reduce((n, v) => n + (v.jobs?.length || 0), 0);
  const needs = vs.filter((v) => v.needs_you).length + state.todos.filter((it) => it.status === "open").length;
  const failing = vs.filter((v) => v.state.fail_streak > 0).length;
  const next = vs.filter((v) => v.next_run).map((v) => ({ v, t: new Date(v.next_run) })).sort((a, b) => a.t - b.t)[0];
  const parts = [];
  if (exportStarted()) parts.push(h("span", { class: "warn", text: t("mig.offlineHead") }));
  if (state.draining) parts.push(h("span", { class: "warn", text: t("head.draining") }));
  if (live) parts.push(h("b", { text: t("head.live", { n: live }) }));
  if (jobs) parts.push(h("span", { class: "live", text: t("head.jobs", { n: jobs }) }));
  if (needs) parts.push(h("span", { class: "warn", text: t("head.needs", { n: needs }) }));
  if (failing) parts.push(h("span", { class: "warn", text: t("head.failing", { n: failing }) }));
  if (next) parts.push(h("span", { text: t("head.next", { name: next.v.task.name, when: relative(next.t) }) }));
  if (!parts.length) parts.push(h("span", { text: t(vs.length ? "head.quiet" : "head.empty") }));
  el.replaceChildren(...parts.flatMap((p, i) => (i ? [" · ", p] : [p])));
}

const LEGEND = [
  ["ok", t("status.ok"), (st) => st === "ok"],
  ["warn", t("status.needs_you"), (st) => st === "needs_you"],
  ["bad", t("status.fail"), (st) => st === "fail" || st === "timeout"],
  ["skip", t("status.skipped"), (st) => st === "skipped"],
];

function startOfToday() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

/** Runs of a view that started today and have finished. */
function todayRuns(v, since = startOfToday()) {
  return v.recent.filter((r) => r.status !== "running" && new Date(r.started_at) >= since);
}

/** The legend doubles as today's tally across all tasks. */
function renderLegend() {
  const since = startOfToday();
  const runs = state.views.flatMap((v) => todayRuns(v, since));
  const end = new Date(since);
  end.setDate(end.getDate() + 1);
  const upcoming = state.views.filter((v) => v.next_run && new Date(v.next_run) < end).length;
  const item = (cls, label, n, filterable = true) => h("li", {},
    h("button", {
      type: "button", class: `legend-item${state.filter === cls ? " on" : ""}${state.filter && state.filter !== cls ? " off" : ""}`,
      title: filterable ? t("legend.filterTitle", { label, n }) : t("legend.itemTitle", { label, n }),
      "aria-pressed": String(state.filter === cls), disabled: !filterable,
      onclick: () => toggleFilter(cls),
    }, h("i", { class: `dot ${cls}` }), label, h("b", { class: "count", text: String(n) })));
  $("legend").replaceChildren(
    h("li", { class: "legend-title", text: t("legend.today") }),
    ...LEGEND.map(([cls, label, match]) => item(cls, label, runs.filter((r) => match(r.status)).length)),
    item("next", t("legend.upcoming"), upcoming, false));
}

/** Show only one outcome's dots on the dial; clicking the same status again shows all. */
function toggleFilter(cls) {
  state.filter = state.filter === cls ? null : cls;
  renderLegend();
  drawDial();
  if (state.filter === "warn") $("needs").scrollIntoView({ behavior: "smooth", block: "start" });
}

function todayLine(v) {
  const runs = todayRuns(v);
  if (!runs.length) return t("today.none");
  const parts = LEGEND.slice(1).map(([, label, match]) => [label, runs.filter((r) => match(r.status)).length])
    .filter(([, n]) => n > 0).map(([label, n]) => t("today.item", { label, n }));
  const detail = parts.length ? t("list.open") + parts.join(t("list.sep")) + t("list.close") : "";
  return t("today.runs", { n: runs.length }) + detail;
}

// True while the owner is typing in a to-do card: rebuilding would steal focus and the caret.
function typingIn(box) {
  return box.contains(document.activeElement) && document.activeElement.matches("textarea, input");
}

/** A quiet note that updates are waiting, added without touching the cards. */
function markHeld(box) {
  if (box.querySelector(".needs-held")) return;
  box.querySelector("h2")?.after(h("p", { class: "needs-held", text: t("needs.held") }));
}

/** The owner's rule: while anything they typed for a to-do is unsent, that section stays as it is. */
function holdNeeds(box) {
  return typingIn(box) || hasDraftIn(box);
}

function renderNeeds() {
  const box = $("needs");
  if (holdNeeds(box)) {
    state.needsStale = true;
    markHeld(box);
    return;
  }
  state.needsStale = false;
  const items = state.views.filter((v) => v.needs_you);
  const names = new Map(state.views.map((v) => [v.task.id, v.task.name]));
  const waiting = followList(state.follows, names);
  if (!items.length && !state.todos.length) {
    box.replaceChildren(...(waiting ? [waiting] : []));
    return;
  }
  keepFocus(() => box.replaceChildren(h("h2", { text: t("needs.title") }), ...todoCards(state.todos, names, load),
    ...items.map(needCard), ...(waiting ? [waiting] : [])));
}

function needCard(v) {
  const r = v.needs_you;
  const options = (r.options || []).map((opt) => {
    const isRec = r.recommendation && opt.trim().startsWith(r.recommendation.trim());
    return h("button", { class: `option${isRec ? " rec" : ""}`, type: "button", text: opt, onclick: (e) => answerOnce(e, v.task.id, opt) });
  });
  return h("article", { class: "need" },
    h("div", { class: "need-head" }, h("span", { text: v.task.name }), h("time", { text: hm(new Date(r.started_at)) })),
    h("p", { class: "need-q" }, ...richText(r.question || r.summary)),
    r.question && r.question !== r.summary ? h("p", { class: "need-s" }, ...richText(r.summary)) : null,
    h("div", { class: "options" }, ...options,
      h("button", { class: "option", type: "button", text: t("needs.detail"), onclick: () => go(v.task.id) })),
  );
}

/** Replace the card with a short note right away, so it is obvious the answer was taken. */
function answerOnce(e, id, text) {
  const card = e.currentTarget.closest(".need");
  card?.replaceWith(h("p", { class: "answered", text: t("needs.answered", { text }) }));
  return answer(id, text);
}

export async function answer(id, text) {
  try {
    await api("POST", `/api/tasks/${id}/answer`, { text });
    toast(t("needs.sent"));
    load();
  } catch (err) {
    toast(err.message, true);
  }
}

function renderTasks() {
  const list = $("tasks");
  const { layout, normalOpen } = getPrefs();
  $("empty").hidden = state.views.length > 0;
  list.className = `tasks layout-${layout}`;
  const urgent = state.views.filter(needsLook);
  const rest = state.views.filter((v) => !needsLook(v));
  let items;
  if (layout === "groups") {
    // What needs a look keeps the full card; everything else folds into one list of single lines.
    const fold = h("li", { class: "tasks-sect" }, h("button", {
      type: "button", class: "tasks-fold", "aria-expanded": String(normalOpen),
      text: t("roster.normal", { n: rest.length }),
      onclick: () => { setPrefs({ normalOpen: !normalOpen }); renderTasks(); },
    }));
    items = [...urgent.map((v, i) => taskRow(v, i)), ...(rest.length ? [fold] : []),
      ...(normalOpen ? rest.map((v, i) => taskRow(v, urgent.length + i, true)) : [])];
  } else {
    const rows = [...urgent, ...rest].map((v, i) => taskRow(v, i));
    items = layout === "rows" && urgent.length
      ? [sect(t("roster.urgent", { n: urgent.length })), ...rows.slice(0, urgent.length),
        sect(t("roster.watching", { n: rest.length })), ...rows.slice(urgent.length)]
      : rows;
  }
  list.replaceChildren(...items);
  $("problems").replaceChildren(...state.problems.map((p) => h("p", { text: `⚠ ${p}` })));
}

/** Tasks waiting on the owner or failing come first in every layout. */
function needsLook(v) {
  return v.task.enabled && (!!v.needs_you || v.state.fail_streak > 0);
}

function sect(text) {
  return h("li", { class: "tasks-sect", text });
}

function stateClass(v) {
  if (v.live) return "live";
  if (!v.task.enabled) return "paused";
  if (v.needs_you) return "warn";
  if (v.state.fail_streak > 0) return "bad";
  return "";
}

function taskRow(v, i, compact = false) {
  const last = v.last_run;
  const when = v.live ? t("task.running") : v.next_run ? relative(new Date(v.next_run)) : t(v.task.enabled ? "task.waiting" : "task.paused");
  const tone = stateClass(v);
  const li = h("li", {
    class: `task${tone ? ` is-${tone}` : ""}${compact ? " compact" : ""}${state.hot === v.task.id ? " hot" : ""}`,
    "data-id": v.task.id, tabindex: 0, style: `animation-delay:${Math.min(i, 12) * 40}ms`,
    onclick: () => go(v.task.id),
    onkeydown: (e) => { if (e.key === "Enter") go(v.task.id); },
    onmouseenter: () => setHot(v.task.id),
    onmouseleave: () => setHot(null),
  },
    h("span", { class: `task-state ${tone}`, "aria-hidden": true }),
    h("div", { class: "task-main" },
      h("div", { class: "task-name" }, h("span", { text: v.task.name }),
        h("i", { class: `chip${v.task.mode === "resident" ? " resident" : ""}`, text: t(v.task.mode === "resident" ? "mode.resident" : "mode.headless") }),
        v.jobs?.length ? h("i", { class: "chip job", title: v.jobs.map((j) => j.title).join("\n"), text: t("task.jobs", { n: v.jobs.length }) }) : null,
        v.proposals ? h("i", { class: "chip proposal", title: t("task.proposals.title"), text: t("task.proposals", { n: v.proposals }) }) : null),
      h("p", { class: "task-last" },
        last ? h("time", { text: hm(new Date(last.started_at)) }) : null,
        last ? plainText(last.summary) : t("task.never"))),
    h("div", { class: "task-meta" },
      h("b", { class: `task-when${v.live ? " live" : ""}`, text: when }),
      h("span", { class: "task-trigger", text: triggerText(v.task) }),
      h("span", { class: "task-today", text: todayLine(v) }),
      pulse(v)),
  );
  return li;
}

function pulse(v) {
  const runs = v.recent.filter((r) => r.status !== "running").slice(0, PULSE_BARS).reverse();
  const bars = runs.map((r) => {
    const cls = statusCls(r.status);
    const height = cls === "skip" ? 4 : cls === "ok" ? 10 : 16;
    return h("i", { class: cls, style: `height:${height}px` });
  });
  return h("div", { class: "pulse", "aria-hidden": true }, ...bars);
}

function setHot(id) {
  if (state.hot === id) return;
  state.hot = id;
  document.querySelectorAll(".task").forEach((el) => el.classList.toggle("hot", el.dataset.id === id));
  drawDial();
}

function showTip(e, title, time, text) {
  const tip = $("tip");
  if (!e) {
    tip.hidden = true;
    return;
  }
  tip.replaceChildren(...[h("b", { text: title }), h("time", { text: time }), text ? h("div", { text: text.slice(0, 160) }) : null].filter(Boolean));
  const box = tip.parentElement.getBoundingClientRect();
  tip.style.left = `${Math.min(e.clientX - box.left + 14, box.width - 300)}px`;
  tip.style.top = `${e.clientY - box.top + 14}px`;
  tip.hidden = false;
}

/** Open a task; with runID the drawer scrolls to that run (a dot clicked on the dial). */
function go(id, runID) {
  location.hash = runID ? `#/task/${id}/run/${runID}` : `#/task/${id}`;
}

function route() {
  const m = location.hash.match(/^#\/task\/([a-z][a-z0-9-]{0,31})(?:\/run\/(r[0-9a-f-]{1,40}))?$/);
  if (m) {
    openDetail(m[1], { onChange: load, onAnswer: answer }, m[2]);
    maybeTabsTour();
  } else closeDetail();
}

function tickClock() {
  $("clock").textContent = hm(new Date());
}

const refresh = debounce(() => {
  load();
  if (currentDetail()) refreshDetail();
}, 400);

function init() {
  applyStatic();
  initSettings($("settings"), {
    onChange: renderTasks, onTour: () => replayTour(state.views),
    onExported: renderHeadline,
  });
  document.addEventListener("click", askNotifyPermission, { once: true });
  $("forge-open").addEventListener("click", () => openForge({ onCreated: load }));
  window.addEventListener("hashchange", route);
  // Catch up on updates skipped while typing, but not when focus moves to a button in the same
  // list: rebuilding then would remove the button before its click lands.
  $("needs").addEventListener("focusout", (e) => {
    if (state.needsStale && !$("needs").contains(e.relatedTarget)) renderNeeds();
  });
  // Emptying the box ends the hold: catch up then, without waiting for the next event.
  $("needs").addEventListener("input", debounce(() => {
    if (state.needsStale && !hasDraftIn($("needs")) && !typingIn($("needs"))) renderNeeds();
  }, 600));
  tickClock();
  setInterval(() => { tickClock(); drawDial(); renderHeadline(); }, CLOCK_TICK);
  events((ev) => {
    if (ev.type === "stream.event") return;
    if (ev.type === "run.finished" && ev.status === "needs_you") {
      toast(t("needs.toast", { text: ev.text || ev.task_id }));
      notifyDesktop(t("needs.title"), ev.text || ev.task_id, () => go(ev.task_id));
    }
    if (ev.type === "run.finished" && (ev.status === "fail" || ev.status === "timeout")) {
      const name = state.views.find((v) => v.task.id === ev.task_id)?.task.name || ev.task_id;
      notifyDesktop(t("fail.title", { name }), ev.text || "", () => go(ev.task_id), `vigil-fail-${ev.task_id}`);
    }
    if (ev.type === "todo.added") notifyDesktop(t("todo.newTitle"), ev.text || "", () => $("needs").scrollIntoView({ block: "start" }));
    refresh();
  }, (on) => $("conn").classList.toggle("on", on));
  load().then(() => {
    route();
    maybeWelcome(state.views);
  });
}

init();
