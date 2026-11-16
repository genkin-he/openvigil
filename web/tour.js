// Guided tour for first-time users: create a first task, then meet the task drawer's tabs.
// One step at a time a ring lights up the thing to look at and a card explains it. Steps that wait
// for the user (open the forge, start it) move on by themselves once that has happened.
import { h } from "./util.js";
import { t } from "./i18n.js";

const STORE_KEY = "vigil.tour"; // per viewer: { welcome, tabs: "done", pending: true }
const STARTER = "self-evolve";
const PAD = 6; // px between the target and the ring
const GAP = 14; // px between the ring and the card
const GUTTER = 16; // px kept clear at the viewport edges

const TAB_ORDER = ["runs", "brief", "runbook", "memory", "config"];

function load() {
  try {
    return JSON.parse(localStorage.getItem(STORE_KEY) || "{}") || {};
  } catch {
    return {};
  }
}

function save(patch) {
  const next = { ...load(), ...patch };
  try {
    localStorage.setItem(STORE_KEY, JSON.stringify(next));
  } catch {
    // Private window or blocked storage: the tour may show again next time, which is harmless.
  }
  return next;
}

const q = (sel) => () => document.querySelector(sel);
const visible = (el) => !!el && el.getClientRects().length > 0;
const tabButton = (key) => () => document.querySelectorAll("#detail .tab")[TAB_ORDER.indexOf(key)] || null;

function welcomeSteps(hasStarter) {
  return [
    { title: "tour.welcome.title", body: "tour.welcome.body", next: "tour.welcome.go",
      alt: hasStarter ? { label: "tour.welcome.look", run: () => { save({ welcome: "done" }); startTabsTour(STARTER); } } : null },
    { target: q("#forge-open"), title: "tour.new.title", body: "tour.new.body",
      until: () => !document.getElementById("forge").hidden },
    { target: q("#forge textarea.idea"), title: "tour.idea.title", body: "tour.idea.body",
      until: () => visible(document.querySelector("#forge .forge-steps")) },
    { target: q("#forge .forge-steps"), title: "tour.forge.title", body: "tour.forge.body", next: "tour.forge.ok",
      done: () => save({ welcome: "done", pending: true }) },
  ];
}

function tabsSteps() {
  const tab = (key) => ({ target: tabButton(key), title: `tour.tab.${key}.title`, body: `tour.tab.${key}.body`,
    enter: (el) => el.click() });
  return [
    { target: q("#detail .d-actions"), title: "tour.actions.title", body: "tour.actions.body" },
    ...TAB_ORDER.map(tab),
    { target: q("#d-tour"), title: "tour.end.title", body: "tour.end.body", next: "tour.end.ok" },
  ];
}

let active = null; // { name, steps, i, root, ring, card, frame, entered }

function stop(markDone = true) {
  if (!active) return;
  cancelAnimationFrame(active.frame);
  active.root.remove();
  document.removeEventListener("keydown", onKey, true);
  if (markDone) save({ [active.name]: "done", pending: false });
  active = null;
}

function onKey(e) {
  if (e.key === "Escape" && active) {
    e.stopPropagation();
    stop();
  }
}

function run(name, steps) {
  stop(false);
  const ring = h("div", { class: "coach-ring", "aria-hidden": true });
  const card = h("div", { class: "coach-card", role: "dialog", "aria-live": "polite", "aria-label": t("tour.aria") });
  const root = h("div", { class: "coach" }, ring, card);
  document.body.append(root);
  document.addEventListener("keydown", onKey, true);
  active = { name, steps, i: -1, root, ring, card, frame: 0, entered: false };
  show(0);
  loop();
}

function show(i) {
  const a = active;
  if (i >= a.steps.length) {
    stop();
    return;
  }
  a.i = i;
  a.entered = false;
  const step = a.steps[i];
  const next = step.until ? null : h("button", { class: "btn primary", type: "button", text: t(step.next || (i === a.steps.length - 1 ? "tour.done" : "tour.next")),
    onclick: () => { step.done?.(); if (step.done) stop(false); else show(i + 1); } });
  const alt = step.alt ? h("button", { class: "btn", type: "button", text: t(step.alt.label), onclick: () => { stop(false); step.alt.run(); } }) : null;
  a.card.replaceChildren(...[
    h("div", { class: "coach-count", text: `${i + 1} / ${a.steps.length}` }),
    h("h3", { text: t(step.title) }),
    h("p", { text: t(step.body) }),
    step.until ? h("p", { class: "coach-wait", text: t("tour.wait") }) : null,
    h("div", { class: "coach-actions" },
      h("button", { class: "coach-skip", type: "button", text: t("tour.skip"), onclick: () => stop() }),
      h("span", { class: "coach-grow" }), alt, next),
  ].filter(Boolean));
  a.card.classList.remove("in");
  void a.card.offsetWidth; // restart the entrance animation
  a.card.classList.add("in");
  next?.focus({ preventScroll: true });
}

/** Each frame: follow the target (the console re-renders often), and move on when a waiting step is met. */
function loop() {
  const a = active;
  if (!a) return;
  const step = a.steps[a.i];
  const el = step.target?.();
  if (el && visible(el) && !a.entered) {
    a.entered = true;
    el.scrollIntoView({ block: "nearest", behavior: "smooth" });
    step.enter?.(el);
  }
  place(el && visible(el) ? el : null);
  if (step.until?.()) {
    show(a.i + 1);
  }
  if (active) active.frame = requestAnimationFrame(loop);
}

function place(el) {
  const { ring, card, root } = active;
  root.classList.toggle("center", !el);
  if (!el) {
    ring.style.opacity = "0";
    card.style.transform = "";
    return;
  }
  const r = el.getBoundingClientRect();
  // Only transform moves (cheap to animate); the size snaps to the target.
  Object.assign(ring.style, { opacity: "1", transform: `translate(${r.left - PAD}px, ${r.top - PAD}px)`,
    width: `${r.width + PAD * 2}px`, height: `${r.height + PAD * 2}px` });
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const cw = card.offsetWidth;
  const ch = card.offsetHeight;
  const below = r.bottom + PAD + GAP;
  const top = below + ch <= vh - GUTTER ? below : Math.max(GUTTER, r.top - PAD - GAP - ch);
  const left = Math.min(Math.max(GUTTER, r.left), vw - cw - GUTTER);
  card.style.transform = `translate(${left}px, ${top}px)`;
}

/** First visit with no task of the user's own: walk them through creating one. */
export function maybeWelcome(views) {
  const st = load();
  if (st.welcome === "done" || active) return;
  if (views.some((v) => v.task.id !== STARTER)) return;
  run("welcome", welcomeSteps(views.some((v) => v.task.id === STARTER)));
}

/** A task drawer just opened: continue the tour if the user came here from the welcome tour. */
export function maybeTabsTour() {
  const st = load();
  if (!st.pending || st.tabs === "done" || active) return;
  run("tabs", tabsSteps());
}

/** Open a task (when given) and walk through its drawer. */
export function startTabsTour(id) {
  if (id) location.hash = `#/task/${id}`;
  run("tabs", tabsSteps());
}

/** The ? at the top: start over from creating a task. (The task drawer has its own ? for its tabs.) */
export function replayTour(views) {
  run("welcome", welcomeSteps(views.some((v) => v.task.id === STARTER)));
}
