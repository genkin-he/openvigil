// Console entry: overview, live updates, routing.
import { h, api, events, toast, statusCls, triggerText, hm, mdhm, relative, debounce, richText, plainText } from "./util.js";
import { renderToday, dayOf, waitsOnOwner } from "./home.js";
import { groupOf } from "./today.js";
import { openDetail, closeDetail, refreshDetail, currentDetail } from "./detail.js";
import { openForge } from "./forge.js";
import { t, applyStatic } from "./i18n.js";
import { todoCards, followList } from "./todos.js";
import { notifyDesktop, askNotifyPermission } from "./notify.js";
import { maybeWelcome, maybeTabsTour, maybeConsoleTour, replayTour, startConsoleTour } from "./tour.js";
import { initSettings } from "./settings.js";
import { exportStarted } from "./migrate.js";
import { keepFocus, hasDraftIn } from "./drafts.js";

const CLOCK_TICK = 20_000;
const PULSE_BARS = 16;
// The tally's "you stepped in" comes from the evolution signals; refreshed at most this often.
const AUTONOMY_EVERY = 5 * 60 * 1000;
const GROUPS = ["look", "ran", "tonight", "off"];

const state = { views: [], todos: [], follows: [], problems: [], hot: null, autonomy: null, autonomyAt: 0 };

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
    loadAutonomy();
  } catch (err) {
    // After a move was started the daemon stops on purpose: say so instead of showing an error.
    $("headline").textContent = exportStarted() ? t("mig.offlineHead") : t("conn.fail", { msg: err.message });
  }
}

function render() {
  renderHeadline();
  renderNeeds();
  renderFollows();
  renderTasks();
  drawToday();
}

function drawToday() {
  const now = new Date();
  renderToday(state, dayOf(state.views, now), { now, onTip: showTip, onPick: go });
}

/** Today's count of times the owner stepped in, across tasks; a failure just leaves the tile blank. */
async function loadAutonomy() {
  if (Date.now() - state.autonomyAt < AUTONOMY_EVERY) return;
  state.autonomyAt = Date.now();
  try {
    const sig = await api("GET", "/api/evolve/signals");
    const today = mdhm(new Date()).slice(0, 5);
    const rows = (sig.autonomy?.days || []).filter((d) => d.date === today);
    state.autonomy = { effort: rows.reduce((n, d) => n + d.effort, 0), auto: rows.reduce((n, d) => n + d.auto, 0) };
    drawToday();
  } catch {
    state.autonomyAt = 0;
  }
}

function renderHeadline() {
  const el = $("headline");
  const vs = state.views;
  const live = vs.filter((v) => v.live).length;
  const jobs = vs.reduce((n, v) => n + (v.jobs?.length || 0), 0);
  const needs = vs.filter((v) => v.needs_you).length + state.todos.filter(waitsOnOwner).length;
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

// True while the owner is typing in a to-do card: rebuilding would steal focus and the caret.
function typingIn(box) {
  return box.contains(document.activeElement) && document.activeElement.matches("textarea, input");
}

/** A quiet note that updates are waiting, added without touching the cards. */
function markHeld(box) {
  if (box.querySelector(".needs-held")) return;
  box.prepend(h("p", { class: "needs-held", text: t("needs.held") }));
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
  keepFocus(() => box.replaceChildren(...todoCards(state.todos, names, load), ...items.map(needCard)));
}

/** Waiting on others sits under today's tally: it rarely needs a click, the to-dos above do. */
function renderFollows() {
  const names = new Map(state.views.map((v) => [v.task.id, v.task.name]));
  const waiting = followList(state.follows, names);
  keepFocus(() => $("follows").replaceChildren(...(waiting ? [waiting] : [])));
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
  const now = new Date();
  const on = state.views.filter((v) => v.task.enabled).length;
  $("empty").hidden = state.views.length > 0;
  $("roster-sub").textContent = state.views.length ? t("home.tasksSub", { on, off: state.views.length - on }) : "";
  const placed = state.views.map((v) => {
    const runs = v.recent.filter((r) => r.status !== "running" && new Date(r.started_at) >= startOfToday());
    return { v, runs, group: groupOf(v, runs, now) };
  });
  const items = [];
  for (const g of GROUPS) {
    const rows = placed.filter((p) => p.group === g);
    if (g === "tonight") rows.sort((a, b) => new Date(a.v.next_run) - new Date(b.v.next_run));
    if (!rows.length) continue;
    items.push(sect(t(`home.group.${g}`)), ...rows.map((p, i) => taskRow(p, items.length + i)));
  }
  list.replaceChildren(...items);
  $("problems").replaceChildren(...state.problems.map((p) => h("p", { text: `⚠ ${p}` })));
}

function startOfToday() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
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

/** The grey line under a task's name: its trigger, when it last ran and what comes next, in its group's terms. */
function rowSub({ v, runs, group }) {
  const last = v.last_run ? hm(new Date(v.last_run.started_at)) : null;
  const next = v.next_run ? new Date(v.next_run) : null;
  const failed = runs.filter((r) => r.status === "fail" || r.status === "timeout").length;
  const bits = [triggerText(v.task)];
  if (group === "look" && v.needs_you) bits.push(t("home.row.waiting", { time: hm(new Date(v.needs_you.started_at)) }));
  else if (group === "look") bits.push(t("home.row.failing", { n: v.state.fail_streak }));
  if (group === "tonight") bits.push(t("home.row.notYet"));
  if (group === "off") bits.push(v.task.enabled ? (next ? t("home.row.nextOn", { when: mdhm(next) }) : null) : t("task.paused"));
  if ((group === "ran" || group === "look") && last) bits.push(t("home.row.last", { time: last }));
  if (group === "ran" && next && next.toDateString() === new Date().toDateString()) bits.push(t("home.row.next", { time: hm(next) }));
  if (group === "ran" && failed) bits.push(t("home.row.failedToday", { n: failed }));
  return bits.filter(Boolean).join(" · ");
}

function rowRight({ v, runs, group }) {
  if (v.live) return [t("task.running"), "live"];
  if (group === "look") return v.needs_you ? [t("home.row.needsYou"), "warn"] : [t("status.fail"), "bad"];
  if (group === "ran") return [t("home.row.runs", { n: runs.length }), ""];
  if (group === "tonight") return [hm(new Date(v.next_run)), "live"];
  return [v.task.enabled ? "—" : t("home.row.paused"), "muted"];
}

function taskRow(p, i) {
  const { v } = p;
  const tone = stateClass(v);
  const [right, rightCls] = rowRight(p);
  return h("li", {
    class: `task row${tone ? ` is-${tone}` : ""}${p.group === "look" ? " look" : ""}${state.hot === v.task.id ? " hot" : ""}`,
    "data-id": v.task.id, tabindex: 0, style: `animation-delay:${Math.min(i, 12) * 30}ms`,
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
        v.proposals ? h("i", { class: "proposal-dot", role: "img", title: t("task.proposals", { n: v.proposals }), "aria-label": t("task.proposals", { n: v.proposals }) }) : null),
      h("p", { class: "task-sub", text: rowSub(p) }),
      v.last_run ? h("p", { class: "task-last" }, h("time", { text: hm(new Date(v.last_run.started_at)) }), plainText(v.last_run.summary)) : null),
    h("div", { class: "task-meta" }, h("b", { class: `task-right ${rightCls}`, text: right }), pulse(v)));
}

/** The task's latest runs as bars, oldest first: taller for runs that need a look. */
function pulse(v) {
  const bars = v.recent.filter((r) => r.status !== "running").slice(0, PULSE_BARS).reverse().map((r) => {
    const cls = statusCls(r.status);
    return h("i", { class: cls, style: `height:${cls === "skip" ? 4 : cls === "ok" ? 8 : 12}px` });
  });
  return h("div", { class: "pulse", "aria-hidden": true }, ...bars);
}

function setHot(id) {
  if (state.hot === id) return;
  state.hot = id;
  document.querySelectorAll(".task").forEach((el) => el.classList.toggle("hot", el.dataset.id === id));
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

/** Open a task; with runID the drawer scrolls to that run (a dot clicked on the strip or a line of the feed). */
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
    onTour: () => replayTour(state.views), onConsoleTour: startConsoleTour,
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
  setInterval(() => { tickClock(); drawToday(); renderHeadline(); }, CLOCK_TICK);
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
    maybeConsoleTour(state.views);
  });
}

init();
