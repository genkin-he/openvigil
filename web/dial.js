// The 24-hour dial: midnight at the top, the hand is now, one orbit per task, one light per run.
import { s, statusCls, statusLabel, hm, mdhm } from "./util.js";
import { t } from "./i18n.js";

const C = 320;
const R_FACE = 286;
const R_LABEL = 303;
const R_MIN = 118;
const R_MAX = 262;
const MAX_STEP = 28;
const DOT_R = 4.2; // a run, at full ring spacing
const SKIP_R = 2.4;
const NEXT_R = 4.5;
const LIVE_R = 6;
const DOT_MIN = 1.8;
const HIT_W = 14; // hover band along an orbit
const DAY = 24 * 3600 * 1000;
const CARRY_OVER = 12 * 3600 * 1000; // early in the day, the dial keeps the last 12 hours of yesterday

/** Runs shown on the dial: today's, plus — until noon — yesterday's within the last 12 hours. */
function dialSince(now) {
  const midnight = new Date(now);
  midnight.setHours(0, 0, 0, 0);
  return new Date(Math.min(midnight, now - CARRY_OVER));
}
const NIGHT_END_HOUR = 7;
const SWEEP_DEG = 42;
const SWEEP_SLICES = 14;

const COLOR = { ok: "var(--ok)", warn: "var(--warn)", bad: "var(--bad)", skip: "var(--skip)", live: "var(--live)" };

const angleOf = (d) => ((d.getHours() * 60 + d.getMinutes() + d.getSeconds() / 60) / 1440) * Math.PI * 2 - Math.PI / 2;
const at = (r, a) => [C + r * Math.cos(a), C + r * Math.sin(a)];

function arcPath(r1, r2, a1, a2) {
  const large = a2 - a1 > Math.PI ? 1 : 0;
  const [x1, y1] = at(r2, a1);
  const [x2, y2] = at(r2, a2);
  const [x3, y3] = at(r1, a2);
  const [x4, y4] = at(r1, a1);
  return `M${x1},${y1} A${r2},${r2} 0 ${large} 1 ${x2},${y2} L${x3},${y3} A${r1},${r1} 0 ${large} 0 ${x4},${y4} Z`;
}

function ringStep(n) {
  return n <= 1 ? MAX_STEP : Math.min(MAX_STEP, (R_MAX - R_MIN) / (n - 1));
}

function radii(n) {
  if (n <= 1) return [Math.round((R_MIN + R_MAX) / 2)];
  const step = ringStep(n);
  return Array.from({ length: n }, (_, i) => R_MAX - i * step);
}

/**
 * Mark sizes for n orbits: one orbit per task however many there are, so with many tasks the rings
 * draw closer and the dots shrink with them instead of overlapping their neighbours.
 */
function marks(n) {
  const k = Math.min(1, ringStep(n) / MAX_STEP);
  const size = (full, min) => Math.max(min, +(full * k).toFixed(2));
  return { dot: size(DOT_R, DOT_MIN), skip: size(SKIP_R, 1.2), next: size(NEXT_R, DOT_MIN), live: size(LIVE_R, 2.4), hit: Math.max(4, Math.min(HIT_W, ringStep(n))) };
}

function face(svg) {
  svg.append(s("circle", { class: "d-face", cx: C, cy: C, r: R_FACE }));
  const nightStart = -Math.PI / 2;
  const nightEnd = (NIGHT_END_HOUR / 24) * Math.PI * 2 - Math.PI / 2;
  svg.append(s("path", { class: "d-night", d: arcPath(R_MIN - 18, R_FACE - 4, nightStart, nightEnd) }));
  for (let hr = 0; hr < 24; hr++) {
    const a = (hr / 24) * Math.PI * 2 - Math.PI / 2;
    const major = hr % 6 === 0;
    const [x1, y1] = at(R_FACE - (major ? 12 : 6), a);
    const [x2, y2] = at(R_FACE, a);
    svg.append(s("line", { class: `d-hour${major ? " major" : ""}`, x1, y1, x2, y2 }));
    if (hr % 3 === 0) {
      const [lx, ly] = at(R_LABEL, a);
      svg.append(s("text", { class: "d-hour-label", x: lx, y: ly, text: String(hr).padStart(2, "0") }));
    }
  }
}

function sweep(svg, now) {
  const end = angleOf(now);
  const slice = (SWEEP_DEG / SWEEP_SLICES) * (Math.PI / 180);
  for (let i = 0; i < SWEEP_SLICES; i++) {
    const a2 = end - i * slice;
    const opacity = 0.16 * (1 - i / SWEEP_SLICES);
    svg.append(s("path", { d: arcPath(60, R_FACE - 2, a2 - slice, a2), style: `fill:var(--live);fill-opacity:${opacity.toFixed(3)}` }));
  }
}

function hand(svg, now) {
  const a = angleOf(now);
  const [x1, y1] = at(66, a);
  const [x2, y2] = at(R_FACE + 6, a);
  svg.append(s("line", { class: "d-hand", x1, y1, x2, y2 }));
  svg.append(s("circle", { class: "d-hand-cap", cx: x2, cy: y2, r: 2.6 }));
}

/** One line on how today has gone so far, across every task. */
function todaySummary(views, now) {
  const start = new Date(now);
  start.setHours(0, 0, 0, 0);
  const runs = views.flatMap((v) => v.recent).filter((r) => r.status !== "running" && new Date(r.started_at) >= start);
  const failed = runs.filter((r) => r.status === "fail" || r.status === "timeout").length;
  if (!runs.length) return t("dial.todayNone");
  return t(failed ? "dial.todayMixed" : "dial.todayOk", { n: runs.length, bad: failed });
}

function center(svg, views, now) {
  const watching = views.filter((v) => v.task.enabled).length;
  svg.append(s("text", { class: "d-center-big", x: C, y: C + 6, text: String(watching) }));
  svg.append(s("text", { class: "d-center-sub", x: C, y: C + 30, text: t("dial.watching") }));
  svg.append(s("text", { class: "d-center-note", x: C, y: C + 52, text: todaySummary(views, now) }));
}

/**
 * Render the dial.
 * @param {SVGSVGElement} svg
 * @param {Array} views task views from /api/overview
 * @param {{now: Date, first: boolean, hot: string|null, onTip: Function, onPick: Function, onHover: Function}} opts
 */
export function renderDial(svg, views, opts) {
  const { now, first, hot, onTip, onPick, onHover, filter } = opts;
  svg.replaceChildren();
  face(svg);
  sweep(svg, now);
  const rs = radii(views.length);
  const mk = marks(views.length);
  views.forEach((v, i) => orbit(svg, v, rs[i], i, { now, first, hot, onTip, onPick, onHover, filter, mk }));
  // Names stay off the face; the hovered orbit (or hovered task card) shows its name.
  views.forEach((v, i) => {
    if (hot !== v.task.id) return;
    svg.append(s("text", { class: "d-label hot", x: C - 8, y: C - rs[i], "text-anchor": "end", text: short(v.task.name) }));
  });
  hand(svg, now);
  center(svg, views, now);
}

function orbit(svg, v, r, i, { now, first, hot, onTip, onPick, onHover, filter, mk }) {
  const id = v.task.id;
  const isHot = hot === id;
  const ring = s("circle", {
    class: `d-orbit${v.task.enabled ? "" : " paused"}${isHot ? " hot" : ""}${first ? " draw" : ""}`,
    cx: C, cy: C, r, style: first ? `animation-delay:${i * 90}ms` : undefined,
  });
  svg.append(ring);
  // A wide invisible band along the orbit, so hovering near the thin line is enough to name it.
  const hit = s("circle", { class: "d-orbit-hit", cx: C, cy: C, r, "stroke-width": mk.hit });
  hit.addEventListener("mouseenter", () => onHover(id));
  hit.addEventListener("mouseleave", () => onHover(null));
  hit.addEventListener("click", () => onPick(id));
  svg.append(hit);

  const since = dialSince(now);
  const span = Math.max(1, now - since);
  for (const run of [...v.recent].reverse()) {
    if (run.status === "running") continue;
    const runAt = new Date(run.started_at);
    const age = now - runAt;
    if (runAt < since || age < 0) continue;
    const [x, y] = at(r, angleOf(runAt));
    const cls = statusCls(run.status);
    const dot = s("circle", {
      class: `d-tick${filter && filter !== cls ? " dim" : ""}`, cx: x, cy: y, r: cls === "skip" ? mk.skip : mk.dot,
      style: `fill:${COLOR[cls]};fill-opacity:${(1 - 0.72 * (age / span)).toFixed(2)};${first ? `animation: rise 600ms ${300 + i * 90}ms both` : ""}`,
    });
    dot.addEventListener("mouseenter", (e) => onTip(e, `${v.task.name} · ${statusLabel(run.status)}`, mdhm(runAt), run.summary));
    dot.addEventListener("mouseleave", () => onTip(null));
    dot.addEventListener("click", () => onPick(id, run.id));
    svg.append(dot);
  }
  if (v.next_run) {
    const nextAt = new Date(v.next_run);
    if (nextAt - now < DAY) {
      const [x, y] = at(r, angleOf(nextAt));
      const ring2 = s("circle", { class: "d-next", cx: x, cy: y, r: mk.next });
      ring2.addEventListener("mouseenter", (e) => onTip(e, t("dial.next", { name: v.task.name }), hm(nextAt), ""));
      ring2.addEventListener("mouseleave", () => onTip(null));
      svg.append(ring2);
    }
  }
  if (v.live) {
    const [x, y] = at(r, angleOf(now));
    svg.append(s("circle", { class: "d-live", cx: x, cy: y, r: mk.live }));
  }
}

function short(name) {
  const chars = [...name];
  return chars.length > 12 ? chars.slice(0, 11).join("") + "…" : name;
}
