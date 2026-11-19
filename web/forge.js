// Forge: describe a task in one sentence; the talk with the forger happens in herdr (pictures, menus and
// all), and this dialog follows its progress.
import { h, api, toast } from "./util.js";
import { t } from "./i18n.js";

const root = document.getElementById("forge");
const body = document.getElementById("forge-body");
const STATUS_POLL = 2000;
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
      showProgress(out.name, out.hint);
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
          ...sessions.map((s) => h("button", { class: "ghost", type: "button", text: s.title || s.name, onclick: () => reopen(s.name) })))
        : h("span"),
      start));
  idea.focus();
}

const STAGES = ["asking", "drafted", "trialed", "live"];
const TITLED = new Set([...STAGES, "gone"]);

async function reopen(name) {
  try {
    const out = await api("POST", `/api/forge/${name}/show`);
    showProgress(name, out.hint);
  } catch (err) {
    toast(err.message, true);
  }
}

function showProgress(name, hint) {
  const title = h("h2", { id: "forge-title", text: t("forge.forging") });
  const steps = h("ol", { class: "forge-steps", "aria-label": t("forge.stepsAria") },
    ...STAGES.map((key) => h("li", { "data-stage": key, text: t(`forge.stage.${key}`) })));
  const finish = h("button", { class: "btn primary", type: "button", text: t("forge.finish"), hidden: true });
  const note = h("p", { class: "lede", "aria-live": "polite", text: hint || t("forge.inHerdr") });
  const back = h("button", { class: "btn", type: "button", text: t("forge.show") });
  let taskID = "";
  back.addEventListener("click", async () => {
    try {
      const out = await api("POST", `/api/forge/${name}/show`);
      note.textContent = out.hint;
    } catch (err) {
      toast(err.message, true);
    }
  });
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
  const refresh = () => api("GET", `/api/forge/${name}/status`).then(showStage).catch(() => {});
  body.replaceChildren(
    h("div", { class: "forge-row" }, title, h("span", { class: "d-meta", text: name })),
    h("div", { class: "forge-row" }, steps, finish),
    note,
    h("div", { class: "forge-row" }, h("span"), back));
  clearInterval(poll);
  refresh();
  poll = setInterval(refresh, STATUS_POLL);
}
