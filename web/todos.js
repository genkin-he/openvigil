// To-do cards: things an agent needs the owner to decide or confirm, answered right here.
import { h, api, toast, hm, mdhm, richText } from "./util.js";
import { t } from "./i18n.js";
import { draftField, clearDrafts } from "./drafts.js";
import { replayFold } from "./trust.js";
import { getPrefs, setPrefs, openMigrate } from "./settings.js";

// Only these schemes become a link; anything else from an agent stays text.
const SAFE_LINK = /^(https?:\/\/|slack:\/\/)/i;

/**
 * @param {Array} items open to-dos from /api/overview
 * @param {Map<string,string>} taskNames task id → display name
 * @param {() => void} onChange reload after an answer
 */
export function todoCards(items, taskNames, onChange) {
  return items.map((it) => {
    if (it.status === "answered") return answeredCard(it, taskNames);
    return it.task_id === SYSTEM ? systemCard(it, taskNames, onChange) : openCard(it, taskNames, onChange);
  });
}

// To-dos Vigil raises about itself (expired logins): they close on their own once the check passes.
const SYSTEM = "vigil";

// A recheck runs in the background for up to a couple of minutes, and cards are rebuilt on every server
// event, so when the owner asked is kept here: id → time of the click.
const rechecks = new Map();
// Past this, a recheck that never reported back is forgotten and the card offers the button again.
const RECHECK_GIVE_UP_MS = 4 * 60 * 1000;

/**
 * Where a system to-do's recheck stands: "idle" (never asked, or forgotten), "running" (asked, no
 * report yet) or "failed" (the check ran again after the click and still fails; a pass closes the card).
 * @param {number|undefined} askedAt ms of the click
 * @param {string} updatedAt the to-do's updated_at, refreshed each time the check fails again
 * @param {number} now ms
 */
export function recheckState(askedAt, updatedAt, now) {
  if (askedAt === undefined) return "idle";
  if (Date.parse(updatedAt) > askedAt) return "failed";
  return now - askedAt > RECHECK_GIVE_UP_MS ? "idle" : "running";
}

function systemCard(it, taskNames, onChange) {
  let card;
  const state = recheckState(rechecks.get(it.id), it.updated_at, Date.now());
  if (state !== "running") rechecks.delete(it.id);
  const act = async (mode, note) => {
    if (mode === "instruct") rechecks.set(it.id, Date.now());
    try {
      await api("POST", `/api/todos/${it.id}/answer`, { mode, text: "" });
      if (mode === "dismiss") {
        toast(note);
        card.replaceWith(h("p", { class: "answered", text: note }));
      }
    } catch (err) {
      rechecks.delete(it.id);
      toast(err.message, true);
    }
    onChange();
  };
  const running = state === "running";
  card = h("article", { class: "need todo" },
    head(it, taskNames),
    h("p", { class: "need-q" }, ...richText(it.title)),
    it.detail ? h("p", { class: "need-s" }, ...richText(it.detail)) : null,
    running ? h("p", { class: "answered", text: t("todo.rechecking") }) : null,
    state === "failed" ? h("p", { class: "todo-still", text: t("todo.stillFailing") }) : null,
    it.analysis ? h("details", { class: "todo-analysis", open: state === "failed" },
      h("summary", { text: t("todo.checkOutput") }),
      h("p", {}, ...richText(it.analysis))) : null,
    h("div", { class: "options" },
      h("button", { class: "option rec", type: "button", disabled: running,
        text: t(running ? "todo.recheckRunning" : "todo.recheck"), onclick: () => act("instruct") }),
      h("button", { class: "option", type: "button", text: t("todo.mute"), onclick: () => act("dismiss", t("todo.muted")) }),
      h("button", { class: "option", type: "button", text: t("mig.fullCheck"), title: t("mig.fullCheckTitle"), onclick: openMigrate })));
  return card;
}

function head(it, taskNames) {
  return h("div", { class: "need-head" },
    h("span", { text: taskNames.get(it.task_id) || (it.task_id === SYSTEM ? t("todo.system") : it.task_id) }),
    h("time", { text: hm(new Date(it.updated_at || it.created_at)) }));
}

function sourceLink(it) {
  if (!it.link || !SAFE_LINK.test(it.link)) return null;
  return h("a", { class: "todo-link", href: it.link, target: "_blank", rel: "noopener noreferrer", text: t("todo.open") });
}

function answeredCard(it, taskNames) {
  return h("article", { class: "need todo is-answered" },
    head(it, taskNames),
    h("p", { class: "need-q" }, ...richText(it.title)),
    h("p", { class: "answered" }, t("todo.inProgress", { text: it.answer })));
}

function openCard(it, taskNames, onChange) {
  const ref = it.ref || "";
  const revoke = ref.startsWith("revoke:");
  // A patch card carries a diff to Vigil's own code: it is read, not edited, and only applied or declined.
  const patch = ref.startsWith("patch:");
  const rule = revoke || patch || ref.startsWith("rule:");
  // An owner step is something the agent is not allowed to do itself (loosening its own permissions):
  // "done" closes it instead of handing it back to an agent that would only be refused again.
  const owner = Boolean(it.owner);
  const labels = patch ? ["todo.patchDraft", "todo.patchApply", "todo.patchDecline"]
    : revoke ? ["todo.revokeDraft", "todo.revokeAccept", "todo.revokeDecline"]
    : rule ? ["todo.ruleDraft", "todo.ruleAccept", "todo.ruleDecline"]
      : owner ? ["todo.ownerDraft", "todo.ownerDone", "todo.ownerSkip"] : ["todo.draft", "todo.send", null];
  let card;
  // Cards are rebuilt on every server event; half-typed text lives in drafts until the to-do is answered.
  const replyKey = `todo:${it.id}:reply`;
  const instructKey = `todo:${it.id}:instruct`;
  const submit = async (mode, text, reason, choice) => {
    const note = mode === "dismiss" ? t("todo.dismissed") : owner && mode === "send" ? t("todo.ownerClosed") : t("todo.handedOver");
    const placeholder = h("p", { class: "answered", text: note });
    card.replaceWith(placeholder);
    try {
      const body = { mode, text };
      if (reason) body.reason = reason;
      if (choice) body.choice = choice;
      await api("POST", `/api/todos/${it.id}/answer`, body);
      clearDrafts(replyKey, instructKey);
      toast(note);
    } catch (err) {
      placeholder.replaceWith(card);
      toast(err.message, true);
    }
    onChange();
  };

  const draft = it.reply ? h("textarea", { class: "todo-reply", rows: 3, "aria-label": t("todo.replyAria") }) : null;
  if (draft) {
    if (revoke || owner || patch) draft.value = it.reply;
    else draftField(draft, replyKey, it.reply);
    // A revoked line is removed exactly as it stands in memory; owner steps are instructions to follow, not
    // to edit; a patch lands exactly as proposed (the server applies its stored diff, not this text).
    draft.readOnly = revoke || owner || patch;
    if (patch) {
      draft.classList.add("todo-diff");
      draft.rows = Math.min(24, Math.max(6, it.reply.split("\n").length));
    }
  }
  const instruct = draftField(h("input", { type: "text", placeholder: t("todo.instructPh"), "aria-label": t("todo.instructAria") }),
    instructKey);
  const sendInstruct = () => instruct.value.trim() && submit("instruct", instruct.value.trim());
  instruct.addEventListener("keydown", (e) => { if (e.key === "Enter") sendInstruct(); });

  card = h("article", { class: "need todo" },
    head(it, taskNames),
    h("p", { class: "need-q" }, ...richText(it.title)),
    it.detail ? h("p", { class: "need-s" }, ...richText(it.detail)) : null,
    it.analysis ? h("details", { class: "todo-analysis", open: !it.reply },
      h("summary", { text: t("todo.analysis") }),
      h("p", {}, ...richText(it.analysis))) : null,
    replayFold(it.link),
    draft ? h("div", { class: "todo-draft" },
      h("label", { class: "todo-label", text: t(labels[0]) }), draft) : null,
    choiceButtons(it, rule || owner, () => instruct.value.trim(), submit),
    h("div", { class: "options" },
      draft ? h("button", { class: "option rec", type: "button", text: t(labels[1]),
        onclick: () => draft.value.trim() && submit("send", draft.value.trim()) }) : null,
      ...dismissButtons(labels[2], submit),
      sourceLink(it)),
    rule ? null : h("div", { class: "answer" }, instruct, h("button", { class: "btn", type: "button", text: t("todo.instruct"), onclick: sendInstruct })));
  return card;
}

/**
 * The fixed answers an agent offered, one button each, with what picking it leads to as a tooltip and a
 * line under the buttons. Whatever is typed in the instruction box goes along as a remark.
 */
function choiceButtons(it, skip, remark, submit) {
  const choices = Array.isArray(it.choices) ? it.choices : [];
  if (skip || choices.length === 0) return null;
  const notes = choices.filter((c) => c.note).map((c) => h("li", {}, h("b", { text: c.label }), ` — ${c.note}`));
  return h("div", { class: "todo-choices" },
    h("div", { class: "options" }, ...choices.map((c) =>
      h("button", { class: "option choice", type: "button", text: c.label, title: c.note || "",
        onclick: () => submit("choose", remark(), null, c.label) }))),
    notes.length ? h("ul", { class: "choice-notes" }, ...notes) : null);
}

// Why an ordinary to-do is dismissed decides what Vigil learns: only "shouldn't have asked me" is noise.
const DISMISS_REASONS = [["noise", "todo.noise"], ["handled_elsewhere", "todo.elsewhere"], ["stale", "todo.stale"]];

/** One dismiss button for decision cards (label given), three reasoned ones for ordinary to-dos. */
function dismissButtons(label, submit) {
  if (label) return [h("button", { class: "option", type: "button", text: t(label), onclick: () => submit("dismiss", "") })];
  return DISMISS_REASONS.map(([reason, key]) =>
    h("button", { class: "option", type: "button", text: t(key), onclick: () => submit("dismiss", "", reason) }));
}

/** Stops waiting on a follow-up: it is closed and no longer handed back to its task. */
/** An owner's "check now" shows on the row for this long, or until the list next refreshes after it settles. */
const FOLLOW_CHECK_SHOWN_MS = 10 * 60 * 1000;

const isChecking = (f) => Boolean(f.checked_at) && Date.now() - new Date(f.checked_at).getTime() < FOLLOW_CHECK_SHOWN_MS;

async function checkFollow(f, row, btn) {
  btn.disabled = true;
  try {
    await api("POST", `/api/follows/${encodeURIComponent(f.id)}/check`);
    row.classList.add("is-checking");
  } catch (err) {
    btn.disabled = false;
    toast(err.message, true);
  }
}

async function dropFollow(f, row, btn) {
  btn.disabled = true;
  try {
    await api("POST", `/api/follows/${encodeURIComponent(f.id)}/done`, { outcome: t("follow.dropped") });
    row.remove();
  } catch (err) {
    btn.disabled = false;
    toast(err.message, true);
  }
}

/**
 * What Vigil is waiting for from other people on the owner's behalf; due items go back to their task.
 * @param {Array} follows open follow-ups from /api/overview, soonest first
 * @param {Map<string,string>} taskNames task id → display name
 */
export function followList(follows, taskNames) {
  if (!follows || !follows.length) return null;
  const now = Date.now();
  const rows = follows.map((f) => {
    const overdue = new Date(f.due).getTime() <= now;
    const link = f.link && SAFE_LINK.test(f.link)
      ? h("a", { class: "ext", href: f.link, target: "_blank", rel: "noopener noreferrer", text: f.title })
      : h("span", { text: f.title });
    const checking = isChecking(f);
    const row = h("li", { class: ["follow", overdue && "is-due", checking && "is-checking"].filter(Boolean).join(" ") },
      link,
      h("span", { class: "follow-meta" },
        t("follow.meta", { who: f.who || "—", task: taskNames.get(f.task) || f.task, due: mdhm(new Date(f.due)), nudges: f.nudges || 0 }),
        h("span", { class: "follow-checking", text: " · " + t("follow.checking") })),
      h("span", { class: "follow-actions" },
        h("button", { class: "follow-btn follow-check", type: "button", text: "✓", title: t("follow.check"), "aria-label": t("follow.check"),
          disabled: checking, onclick: (e) => checkFollow(f, row, e.currentTarget) }),
        h("button", { class: "follow-btn follow-drop", type: "button", text: "×", title: t("follow.drop"), "aria-label": t("follow.drop"),
          onclick: (e) => dropFollow(f, row, e.currentTarget) })));
    return row;
  });
  // Remember whether the owner left the list open, so a refresh or reload does not fold it again.
  const fold = h("details", { class: "follows", open: getPrefs().followsOpen },
    h("summary", { text: t("follow.title", { n: follows.length }) }),
    h("ul", {}, ...rows));
  fold.addEventListener("toggle", () => setPrefs({ followsOpen: fold.open }));
  return fold;
}

