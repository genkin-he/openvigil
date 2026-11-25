// Per-viewer console settings: language, guided tour; plus the machine's health check and moving Vigil
// to another computer. The panel floats beside the page instead of covering it.
import { h } from "./util.js";
import { t, lang, setLang } from "./i18n.js";
import { migrateSection } from "./migrate.js";

const STORE_KEY = "vigil.prefs";
const REOPEN_KEY = "vigil.settings.reopen"; // switching language reloads; reopen the panel afterwards

const DEFAULTS = {};

let prefs = load();

function load() {
  try {
    const saved = JSON.parse(localStorage.getItem(STORE_KEY) || "{}") || {};
    return { ...DEFAULTS, ...saved };
  } catch {
    return { ...DEFAULTS };
  }
}

/** The current settings (a copy). */
export function getPrefs() {
  return { ...prefs };
}

/** Change settings; the stored copy is best effort (private windows may refuse). */
export function setPrefs(patch) {
  prefs = { ...prefs, ...patch };
  try {
    localStorage.setItem(STORE_KEY, JSON.stringify(prefs));
  } catch {
    // Storage unavailable: the change lasts until the page reloads.
  }
  return getPrefs();
}

let panel = null;
let wired = null; // the settings button and hooks, so other views can open the panel (openMigrate)

/**
 * Wire the settings button.
 * @param {HTMLElement} button the header button that toggles the panel
 * @param {{onTour: Function, onConsoleTour: Function, onExported: Function}} hooks
 */
export function initSettings(button, hooks) {
  wired = { button, hooks };
  button.addEventListener("click", () => (panel ? close() : open(button, hooks)));
  let reopen = false;
  try {
    reopen = sessionStorage.getItem(REOPEN_KEY) === "1";
    sessionStorage.removeItem(REOPEN_KEY);
  } catch {
    // ignore: the panel just stays closed after a language switch
  }
  if (reopen) open(button, hooks);
}

function close() {
  if (!panel) return;
  panel.el.remove();
  document.removeEventListener("pointerdown", panel.away, true);
  document.removeEventListener("keydown", panel.esc, true);
  panel.button.setAttribute("aria-expanded", "false");
  panel = null;
}

/** Open the panel at "Move & health check" and run the health check (from a failed-check to-do). */
export function openMigrate() {
  if (!wired) return;
  close();
  open(wired.button, wired.hooks, { autoDoctor: true });
  panel?.el.querySelector("#settings-migrate")?.scrollIntoView({ block: "start" });
}

function open(button, hooks, { autoDoctor = false } = {}) {
  const el = h("div", { class: "settings", role: "dialog", "aria-label": t("settings.title") },
    h("h2", { text: t("settings.title") }),
    section(t("settings.lang"), langChoices()),
    section(t("settings.tour"), h("div", { class: "settings-row" },
      h("p", { class: "settings-hint", text: t("settings.tourHint") }),
      h("button", { class: "btn", type: "button", text: t("settings.tourReplay"), onclick: () => { close(); hooks.onTour(); } }),
      h("button", { class: "btn", type: "button", text: t("settings.tourConsole"), onclick: () => { close(); hooks.onConsoleTour(); } }))),
    migrateSection({ autoDoctor, onExported: hooks.onExported }));
  document.body.append(el);
  // A confirm dialog opened from the panel sits outside it: clicks and Esc there are the dialog's.
  const inDialog = (target) => target instanceof Element && !!target.closest("dialog");
  const away = (e) => { if (!el.contains(e.target) && !button.contains(e.target) && !inDialog(e.target)) close(); };
  const esc = (e) => {
    if (e.key !== "Escape" || document.querySelector("dialog[open]")) return;
    e.stopPropagation(); close(); button.focus();
  };
  document.addEventListener("pointerdown", away, true);
  document.addEventListener("keydown", esc, true);
  button.setAttribute("aria-expanded", "true");
  panel = { el, button, away, esc };
  el.querySelector("[aria-checked='true']")?.focus();
}

function section(title, body) {
  return h("section", { class: "settings-sec" }, h("h3", { text: title }), body);
}

/** A radio group of buttons; picking one calls pick(value) and marks it. */
function choices(name, items, current, pick) {
  const group = h("div", { class: "settings-choices", role: "radiogroup", "aria-label": name });
  const buttons = items.map(({ value, label, hint, sketch }) => h("button", {
    type: "button", role: "radio", class: "settings-choice", "aria-checked": String(value === current),
    onclick: () => {
      buttons.forEach((b) => b.setAttribute("aria-checked", String(b === buttonOf(value))));
      pick(value);
    },
  }, sketch || null, h("b", { text: label }), hint ? h("span", { text: hint }) : null));
  const buttonOf = (value) => buttons[items.findIndex((it) => it.value === value)];
  group.append(...buttons);
  return group;
}

function langChoices() {
  const items = [{ value: "zh", label: "中文" }, { value: "en", label: "English" }];
  return choices(t("settings.lang"), items, lang, (next) => {
    if (next === lang) return;
    try {
      sessionStorage.setItem(REOPEN_KEY, "1");
    } catch {
      // ignore: the panel just stays closed after the reload
    }
    setLang(next);
  });
}
