// Forge: describe a task in one sentence, then talk it through with an agent running in herdr.
import { h, api, toast } from "./util.js";
import { t } from "./i18n.js";

const root = document.getElementById("forge");
const body = document.getElementById("forge-body");
const SCREEN_POLL = 2000;
let poll = null;
let hooks = { onCreated: () => {} };

root.addEventListener("click", (e) => {
  if (e.target.hasAttribute("data-close")) closeForge();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !root.hidden) closeForge();
});

export async function openForge(h2) {
  hooks = h2;
  root.hidden = false;
  let sessions = [];
  try {
    sessions = await api("GET", "/api/forge");
  } catch (err) {
    toast(t("forge.readFail", { msg: err.message }), true);
  }
  showIdea(sessions || []);
}

function closeForge() {
  root.hidden = true;
  clearInterval(poll);
  poll = null;
  hooks.onCreated();
}

function showIdea(sessions) {
  const idea = h("textarea", { class: "idea", placeholder: t("forge.ideaPh"), "aria-label": t("forge.ideaAria") });
  const start = h("button", { class: "btn primary", type: "button", text: t("forge.start") });
  start.addEventListener("click", async () => {
    if (!idea.value.trim()) return idea.focus();
    start.disabled = true;
    start.textContent = t("forge.starting");
    try {
      const out = await api("POST", "/api/forge", { idea: idea.value.trim() });
      showChat(out.name);
    } catch (err) {
      toast(err.message, true);
      start.disabled = false;
      start.textContent = t("forge.start");
    }
  });
  body.replaceChildren(
    h("h2", { id: "forge-title", text: t("forge.title") }),
    h("p", { class: "lede", text: t("forge.lede") }),
    idea,
    h("div", { class: "forge-row" },
      sessions.length
        ? h("div", { class: "sessions" }, h("span", { class: "d-meta", text: t("forge.inProgress") }),
          ...sessions.map((s) => h("button", { class: "ghost", type: "button", text: s.title || s.name, onclick: () => showChat(s.name) })))
        : h("span"),
      start));
  idea.focus();
}

const STAGES = ["asking", "drafted", "trialed", "live"];
const TITLED = new Set([...STAGES, "gone"]);

function showChat(name) {
  const screen = h("pre", { class: "screen", "aria-live": "polite" });
  const input = h("textarea", { placeholder: t("forge.replyPh"), "aria-label": t("forge.replyAria") });
  const send = async () => {
    const text = input.value.trim();
    if (!text) return;
    input.value = "";
    try {
      await api("POST", `/api/forge/${name}/say`, { text });
      setTimeout(refresh, 400);
    } catch (err) {
      toast(err.message, true);
      input.value = text;
    }
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
  });
  const title = h("h2", { id: "forge-title", text: t("forge.forging") });
  const steps = h("ol", { class: "forge-steps", "aria-label": t("forge.stepsAria") },
    ...STAGES.map((key) => h("li", { "data-stage": key, text: t(`forge.stage.${key}`) })));
  const finish = h("button", { class: "btn primary", type: "button", text: t("forge.finish"), hidden: true });
  let taskID = "";
  finish.addEventListener("click", async () => {
    finish.disabled = true;
    try {
      await api("POST", `/api/forge/${name}/close`);
      closeForge();
      if (taskID) location.hash = `#/task/${taskID}`;
    } catch (err) {
      toast(err.message, true);
      finish.disabled = false;
    }
  });
  const showStage = (st) => {
    const idx = STAGES.indexOf(st.stage);
    steps.querySelectorAll("li").forEach((li, i) => {
      li.classList.toggle("done", i < idx || st.stage === "live");
      li.classList.toggle("now", i === idx && st.stage !== "live");
    });
    title.textContent = TITLED.has(st.stage) ? t(`forge.title.${st.stage}`) : t("forge.forging");
    taskID = st.task_id || "";
    finish.hidden = st.stage !== "live";
  };
  const refresh = async () => {
    api("GET", `/api/forge/${name}/status`).then(showStage).catch(() => {});
    try {
      const out = await api("GET", `/api/forge/${name}/screen`);
      const atBottom = screen.scrollHeight - screen.scrollTop - screen.clientHeight < 40;
      screen.textContent = tidy(out.screen);
      if (atBottom) screen.scrollTop = screen.scrollHeight;
    } catch (err) {
      screen.textContent = t("forge.ended", { msg: err.message });
      clearInterval(poll);
    }
  };
  body.replaceChildren(
    h("div", { class: "forge-row" }, title, h("span", { class: "d-meta", text: name })),
    h("div", { class: "forge-row" }, steps, finish),
    screen,
    h("div", { class: "compose" }, input, h("button", { class: "btn primary", type: "button", text: t("forge.send"), onclick: send })));
  clearInterval(poll);
  refresh();
  poll = setInterval(refresh, SCREEN_POLL);
  input.focus();
}

/**
 * Drop the agent's input box and status chrome: everything from the second-to-last ──── rule down,
 * but only when that rule opens the ❯ prompt line. A question the agent asks (a ☐ menu) is drawn between
 * the same rules and must stay visible, or the owner never sees there is something to answer.
 */
export function tidy(text) {
  const lines = text.replace(/\s+$/, "").split("\n");
  const rules = lines.flatMap((l, i) => (/^─{10,}/.test(l.trim()) ? [i] : []));
  const at = rules.length >= 2 ? rules[rules.length - 2] : -1;
  const isPromptBox = at >= 0 && /^[❯>]/.test((lines[at + 1] || "").trim());
  const cut = isPromptBox ? at : lines.length;
  return lines.slice(0, cut).join("\n").replace(/\n{3,}/g, "\n\n");
}
