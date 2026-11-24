// Shared helpers. Everything that came from an agent is rendered as text, never as HTML.

import { t } from "./i18n.js";

const TOKEN = document.querySelector('meta[name="vigil-token"]').content;
const SVG_NS = "http://www.w3.org/2000/svg";

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  setAttrs(el, attrs);
  append(el, children);
  return el;
}

export function s(tag, attrs = {}, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  setAttrs(el, attrs);
  append(el, children);
  return el;
}

function setAttrs(el, attrs) {
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
    else if (k === "text") el.textContent = v;
    else el.setAttribute(k, v === true ? "" : String(v));
  }
}

function append(el, children) {
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

// Agent text may carry links as [label](https://…) or bare https:// URLs. Only http(s) targets become
// anchors; everything else stays a text node, so agent output still never reaches the DOM as HTML.
const LINK_RE = /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>()（），。、；]+)/g;

export function richText(text) {
  const src = String(text ?? "");
  const nodes = [];
  let last = 0;
  for (const m of src.matchAll(LINK_RE)) {
    if (m.index > last) nodes.push(src.slice(last, m.index));
    const [, label, labelled, bare] = m;
    const href = labelled || bare;
    nodes.push(h("a", { class: "ext", href, target: "_blank", rel: "noopener noreferrer", title: href,
      text: label || bare, onclick: (e) => e.stopPropagation() }));
    last = m.index + m[0].length;
  }
  if (last < src.length) nodes.push(src.slice(last));
  return nodes;
}

// plainText keeps only the labels, for places where the whole row is already a click target.
export const plainText = (text) => String(text ?? "").replace(LINK_RE, (_, label, _href, bare) => label || bare);

export async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let env;
  try {
    env = await res.json();
  } catch {
    throw new Error(t("api.badResponse", { status: res.status }));
  }
  if (!env.ok) throw new Error(env.error || res.statusText);
  return env.data;
}

export function events(onEvent, onState) {
  let es;
  let retry = 1000;
  const open = () => {
    es = new EventSource(`/api/events?token=${encodeURIComponent(TOKEN)}`);
    es.onopen = () => { retry = 1000; onState(true); };
    es.onmessage = (m) => {
      try { onEvent(JSON.parse(m.data)); } catch { /* ignore malformed frames */ }
    };
    es.onerror = () => {
      onState(false);
      es.close();
      setTimeout(open, retry);
      retry = Math.min(retry * 2, 30000);
    };
  };
  open();
}

let toastTimer;
export function toast(msg, isError = false) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.classList.toggle("err", isError);
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), isError ? 6000 : 2800);
}

const STATUS_CLS = { ok: "ok", needs_you: "warn", fail: "bad", timeout: "bad", skipped: "skip", running: "live", queued: "live" };

export const statusCls = (st) => STATUS_CLS[st] || "skip";
export const statusLabel = (st) => (st in STATUS_CLS ? t(`status.${st}`) : st);

const pad = (n) => String(n).padStart(2, "0");
export const hm = (d) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;
export const mdhm = (d) => `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hm(d)}`;

export function relative(target, now = new Date()) {
  const diff = Math.round((target - now) / 60000);
  const abs = Math.abs(diff);
  if (abs < 1) return t("time.now");
  const hours = Math.floor(abs / 60), mins = abs % 60;
  const unit = abs < 60 ? t("time.min", { n: abs })
    : abs < 1440 ? (mins ? t("time.hourMin", { h: hours, m: mins }) : t("time.hour", { h: hours }))
    : t("time.day", { n: Math.floor(abs / 1440) });
  return t(diff > 0 ? "time.after" : "time.ago", { unit });
}

export function triggerText(task) {
  const tr = task.trigger;
  switch (tr.kind) {
    case "cron": return `cron ${tr.cron}`;
    case "every": return t("trigger.every", { d: everyText(tr.every) });
    case "once": return t("trigger.once", { at: mdhm(new Date(tr.at)) });
    case "stream": return t("trigger.stream");
    default: return t("trigger.manual");
  }
}

/** "1h30m0s" → "1 小时 30 分" / "1 h 30 min". */
function everyText(d) {
  return d.replace(/h0m0s$/, t("unit.h")).replace(/h(\d+)m0s$/, (_, m) => t("unit.hm", { m })).replace(/m0s$/, t("unit.m"));
}

export function debounce(fn, ms) {
  let timer;
  return (...a) => { clearTimeout(timer); timer = setTimeout(() => fn(...a), ms); };
}

// Line icons drawn on one 24-unit grid with one stroke, so buttons that sit side by side match in size and
// weight (text glyphs such as ✓ and × come from different fonts and never line up).
const ICON_PATHS = {
  close: ["M6 6l12 12", "M18 6L6 18"],
  check: ["M5 12.5l4.5 4.5L19 7.5"],
  help: ["M8.2 8.6a3.9 3.9 0 0 1 7.6 1.3c0 2.6-3.8 3.1-3.8 5.6", "M12 19.4v.1"],
};

/** A line icon by name (see ICON_PATHS), sized in px and drawn in currentColor. */
export function icon(name, size = 16) {
  return s("svg", { viewBox: "0 0 24 24", width: size, height: size, fill: "none", stroke: "currentColor", "stroke-width": 1.9,
    "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true" },
    ...(ICON_PATHS[name] || []).map((d) => s("path", { d })));
}
