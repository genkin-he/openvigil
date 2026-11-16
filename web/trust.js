// Trust per kind of to-do (task drawer) and replay reports (rule suggestions). Both are read-only.
import { h, api } from "./util.js";
import { t } from "./i18n.js";

const TRUST_TTL = 60_000; // the drawer redraws on every event; the numbers change only when a to-do is answered
const cache = new Map(); // task id → { at, kinds }

const pct = (x) => `${Math.round(x * 100)}%`;

/** The trust block for a task's drawer; empty until loaded, absent when the task has no reasoned decisions. */
export function trustBlock(taskID) {
  const box = h("section", { class: "trust", hidden: true, "aria-label": t("trust.title") });
  const hit = cache.get(taskID);
  if (hit) fill(box, hit.kinds);
  if (!hit || Date.now() - hit.at > TRUST_TTL) {
    api("GET", `/api/tasks/${taskID}/trust`)
      .then((kinds) => {
        cache.set(taskID, { at: Date.now(), kinds: kinds || [] });
        fill(box, kinds || []);
      })
      .catch(() => {}); // the block is optional; the drawer works without it
  }
  return box;
}

function fill(box, kinds) {
  if (!kinds.length) {
    box.hidden = true;
    return;
  }
  const head = h("tr", {}, ...["trust.kind", "trust.samples", "trust.adoption", "trust.noise", "trust.state"]
    .map((k) => h("th", { scope: "col", text: t(k) })));
  const rows = kinds.map((k) => h("tr", {},
    h("th", { scope: "row", text: k.kind }),
    h("td", { text: String(k.samples) }),
    h("td", { text: k.drafted ? `${pct(k.adoption)} · ${k.drafted}` : "—" }),
    h("td", { class: k.noise >= 0.8 ? "hot" : "", text: k.samples ? pct(k.noise) : "—" }),
    h("td", { text: stateText(k) })));
  box.replaceChildren(
    h("details", { class: "trust-box" },
      h("summary", { text: t("trust.title") }),
      h("p", { class: "trust-hint", text: t("trust.hint") }),
      h("div", { class: "trust-scroll" }, h("table", {}, h("thead", {}, head), h("tbody", {}, ...rows)))));
  box.hidden = false;
}

function stateText(k) {
  const parts = [];
  if (k.authorized) parts.push(t("trust.authorized"));
  if (k.proposal) parts.push(t(`trust.proposal.${k.proposal}`));
  return parts.join(" · ") || "—";
}

const REPLAY_LINK = /^\/api\/evolve\/replay\/(rp-[0-9a-f]+)$/;

/** A "see the replay" fold for a rule suggestion whose link is a replay report; null otherwise. */
export function replayFold(link) {
  const m = REPLAY_LINK.exec(link || "");
  if (!m) return null;
  const body = h("div", { class: "replay-body" }, h("p", { class: "d-meta", text: t("replay.loading") }));
  const fold = h("details", { class: "todo-analysis replay" }, h("summary", { text: t("replay.title") }), body);
  let loaded = false;
  fold.addEventListener("toggle", () => {
    if (!fold.open || loaded) return;
    loaded = true;
    api("GET", `/api/evolve/replay/${m[1]}`)
      .then((rep) => body.replaceChildren(...reportView(rep)))
      .catch((err) => body.replaceChildren(h("p", { class: "d-meta", text: t("replay.fail", { msg: err.message }) })));
  });
  return fold;
}

const decisionText = (d) => (d?.valid ? t(`replay.act.${d.action}`) + (d.text ? `：${d.text}` : "") : t("replay.unknown"));

function reportView(rep) {
  const moved = (rep.rows || []).filter((r) => r.verdict === "closer" || r.verdict === "farther" || r.verdict === "changed");
  return [
    h("p", { class: "replay-sum", text: rep.summary || t("replay.running") }),
    ...moved.map((r) => h("div", { class: `replay-row ${r.verdict}` },
      h("p", { class: "replay-input", text: (r.input || "").slice(0, 80) }),
      h("dl", {},
        h("dt", { text: t("replay.before") }), h("dd", { text: decisionText(r.before) }),
        h("dt", { text: t("replay.after") }), h("dd", { text: decisionText(r.after) }),
        h("dt", { text: t("replay.actual") }), h("dd", { text: decisionText(r.actual) + (r.actual_from_summary ? t("replay.inferred") : "") })),
      h("p", { class: "replay-tags", text: [t(`replay.v.${r.verdict}`), r.no_evidence ? t("replay.noEvidence") : ""].filter(Boolean).join(" · ") }))),
  ];
}
