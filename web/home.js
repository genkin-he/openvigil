// The homepage's left column: the day as a 24-hour strip, the tally, and
// what happened told as events (an outage is one line, not forty red dots). The numbers come from today.js.
import { h, s, hm, mdhm, statusCls, statusLabel, plainText } from "./util.js";
import { t } from "./i18n.js";
import { todayRuns, incidents, feed, upcoming, startOfDay } from "./today.js";

const FEED_MAX = 9;
const STRIP = { w: 884, x0: 8, x1: 876 };
const FAILED = new Set(["fail", "timeout"]);
const DAY_MS = 24 * 3600 * 1000;
const BLINK_MS = 1600; // --blink in style.css

/** Everything the left column shows, worked out once per render. */
export function dayOf(views, now) {
  const runs = todayRuns(views, now);
  const incs = incidents(runs);
  return { runs, incs, items: feed(runs, incs), next: upcoming(views, now) };
}

/**
 * @param {{views: Array, todos: Array, autonomy: {effort:number, auto:number}|null}} state
 * @param {{now: Date, onTip: Function, onPick: Function}} opts
 */
export function renderToday(state, day, { now, onTip, onPick }) {
  renderStrip(document.getElementById("strip"), day, now, onTip, onPick);
  renderStats(day, state.autonomy);
  renderFeed(state.views, day, now);
}

/** An open to-do, or an answered one whose handoff failed: either way it is the owner's move. */
export const waitsOnOwner = (it) => it.status === "open" || (it.status === "answered" && !!it.handoff_error);

const X = (d, now) => {
  const m = (d - startOfDay(now)) / 60000;
  return +(STRIP.x0 + (Math.min(Math.max(m, 0), 1440) / 1440) * (STRIP.x1 - STRIP.x0)).toFixed(1);
};

function renderStrip(svg, day, now, onTip, onPick) {
  const nowX = X(now, now);
  const kids = [
    s("rect", { class: "st-past", x: STRIP.x0, y: 10, width: Math.max(0, nowX - STRIP.x0), height: 16, rx: 8 }),
    s("rect", { class: "st-future", x: nowX, y: 10, width: Math.max(0, STRIP.x1 - nowX), height: 16, rx: 8 }),
    ...day.incs.filter((i) => i.runs.length > 1).map((i) => {
      const x = X(i.start, now) - 3;
      return s("rect", { class: "st-inc", x, y: 7, width: Math.max(6, X(i.end, now) + 3 - x), height: 22, rx: 4 });
    }),
  ];
  for (const r of day.runs) {
    const cls = statusCls(r.status);
    const dot = s("circle", { class: `st-dot ${cls}`, cx: X(r.at, now), cy: 18, r: cls === "ok" || cls === "skip" ? 2.4 : 3.6 });
    dot.addEventListener("mouseenter", (e) => onTip(e, `${r.name} · ${statusLabel(r.status)}`, mdhm(r.at), plainText(r.summary)));
    dot.addEventListener("mouseleave", () => onTip(null));
    dot.addEventListener("click", () => onPick(r.taskId, r.id));
    kids.push(dot);
  }
  for (const v of day.next) {
    const at = new Date(v.next_run);
    const ring = s("circle", { class: "st-next", cx: X(at, now), cy: 18, r: 3.5 });
    ring.addEventListener("mouseenter", (e) => onTip(e, t("dial.next", { name: v.task.name }), hm(at), ""));
    ring.addEventListener("mouseleave", () => onTip(null));
    ring.addEventListener("click", () => onPick(v.task.id));
    kids.push(ring);
  }
  // The strip is redrawn every 20 s and on every event; keep the blink in phase so it never jumps.
  kids.push(s("line", { class: "st-now", x1: nowX, y1: 4, x2: nowX, y2: 32, style: `animation-delay:-${Date.now() % BLINK_MS}ms` }));
  for (const hr of [0, 6, 12, 18, 24]) {
    const at = new Date(startOfDay(now).getTime() + (hr / 24) * DAY_MS);
    kids.push(s("text", { class: "st-hour", x: X(at, now), y: 44, text: String(hr).padStart(2, "0") }));
  }
  svg.replaceChildren(...kids);
}

function stat(label, value, cls, unit) {
  return h("div", { class: "stat" },
    h("span", { class: "stat-label", text: label }),
    h("span", { class: `stat-value${cls ? ` ${cls}` : ""}` }, value, unit ? h("small", { text: unit }) : null));
}

function renderStats(day, autonomy) {
  const ok = day.runs.filter((r) => r.status === "ok").length;
  const failed = day.runs.filter((r) => FAILED.has(r.status)).length;
  document.getElementById("stats").replaceChildren(
    stat(t("home.stat.runs"), String(day.runs.length), "", t("home.unit.runs")),
    stat(t("home.stat.ok"), String(ok), "ok"),
    failed
      ? stat(t("home.stat.failed", { n: failed }), String(day.incs.length), "bad", t("home.unit.events"))
      : stat(t("home.stat.failedNone"), "0", ""),
    stat(t("home.stat.you"), autonomy ? String(autonomy.effort) : "—", "",
      autonomy ? t("home.unit.you", { auto: autonomy.auto }) : ""));
}

const causeLabel = (inc) => t(inc.cause === "other" && inc.runs.length > 1 ? "cause.otherMany" : `cause.${inc.cause}`);
const minutes = (inc) => Math.max(1, Math.round((inc.end - inc.start) / 60000));
const byTaskList = (inc) => inc.byTask.map((x) => `${x.name} ${x.n}`).join(" · ");

function feedText(it) {
  const { inc, run } = it;
  switch (it.type) {
    case "incident":
      return [t("home.feed.incident"), t("home.feed.incidentTitle", { cause: causeLabel(inc), min: minutes(inc) }),
        t("home.feed.incidentDetail", { n: inc.runs.length, list: byTaskList(inc) })];
    case "failed":
      return [t("home.feed.failed"), t("home.feed.failedTitle", { name: inc.runs[0].name, cause: causeLabel(inc) }), ""];
    case "recovered":
      return inc.byTask.length > 1
        ? [t("home.feed.recovered"), t("home.feed.recoveredMany", { cause: causeLabel(inc) }), inc.byTask.map((x) => x.name).join(" · ")]
        : [t("home.feed.recovered"), t("home.feed.recoveredOne", { name: inc.recovered.name }), ""];
    case "needs":
      return [t("home.feed.needs"), t("home.feed.needsTitle", { name: run.name }), plainText(run.summary)];
    default:
      return [t("home.feed.done"), t("home.feed.doneTitle", { name: run.name }), plainText(run.summary)];
  }
}

/** The first line: all fine, or what is still failing, plus how the busiest resident task is doing. */
function nowItem(views, day, now) {
  const failing = views.filter((v) => v.task.enabled && v.state.fail_streak > 0);
  const counts = new Map();
  for (const r of day.runs) if (r.resident) counts.set(r.taskId, (counts.get(r.taskId) || []).concat(r));
  const busiest = [...counts.values()].sort((a, b) => b.length - a.length)[0];
  const detail = busiest ? t("home.feed.nowDetail", { name: busiest[0].name, n: busiest.length, time: hm(busiest.at(-1).at) }) : "";
  return {
    at: now, kind: failing.length ? "bad" : "now", strong: true,
    text: [t("home.feed.now"), failing.length ? t("home.feed.nowBad", { n: failing.length }) : t("home.feed.nowOk"), detail],
  };
}

function feedRow({ at, kind, strong, text, onclick }) {
  const [tag, title, detail] = text;
  return h("li", { class: `feed-item k-${kind}${strong ? " strong" : ""}${onclick ? " link" : ""}`, onclick },
    h("time", { text: hm(at) }),
    h("span", { class: "feed-dot", "aria-hidden": true }),
    h("div", { class: "feed-body" },
      h("span", { class: "feed-title" }, h("b", { text: tag }), title),
      detail ? h("span", { class: "feed-detail", text: detail.length > 140 ? `${detail.slice(0, 139)}…` : detail }) : null));
}

function renderFeed(views, day, now) {
  const recent = now - 3 * 3600 * 1000;
  const rows = day.items.slice(0, FEED_MAX).map((it) => {
    const target = it.run || it.inc?.recovered || it.inc?.runs[0];
    return feedRow({ at: it.at, kind: it.kind, strong: it.at >= recent || it.kind !== "ok", text: feedText(it),
      onclick: target ? () => { location.hash = `#/task/${target.taskId}/run/${target.id}`; } : undefined });
  });
  const more = day.items.length - FEED_MAX;
  document.getElementById("feed").replaceChildren(feedRow(nowItem(views, day, now)), ...rows,
    ...(more > 0 ? [h("li", { class: "feed-more", text: t("home.feed.more", { n: more }) })] : []));
}
