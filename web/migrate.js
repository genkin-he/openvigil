// "Move & health check" in the settings panel: run `vigil doctor` from the console, see what a move
// would pack, and start `vigil export` (which stops this daemon for good, so the page goes offline).
import { h, api, toast } from "./util.js";
import { t } from "./i18n.js";
import { confirmDialog } from "./confirm.js";

const MB = 1 << 20;

/** Set once an export started: the daemon is about to stop, so a lost connection is expected. */
let exported = null;
export const exportStarted = () => exported;

/**
 * The settings section.
 * @param {{autoDoctor?: boolean, onExported?: Function}} opts autoDoctor runs the check at once
 */
export function migrateSection({ autoDoctor = false, onExported } = {}) {
  const doctorBox = h("div", { class: "mig-out", "aria-live": "polite" });
  const planBox = h("div", { class: "mig-out", "aria-live": "polite" });
  const doctorBtn = h("button", { class: "btn", type: "button", text: t("mig.doctorRun") });
  const planBtn = h("button", { class: "btn", type: "button", text: t("mig.planShow") });
  doctorBtn.addEventListener("click", () => runDoctor(doctorBtn, doctorBox));
  planBtn.addEventListener("click", () => showPlan(planBtn, planBox, onExported));
  const sec = h("section", { class: "settings-sec mig", id: "settings-migrate" },
    h("h3", { text: t("mig.title") }),
    h("div", { class: "mig-part" },
      h("div", { class: "settings-row" }, h("p", { class: "settings-hint", text: t("mig.doctorHint") }), doctorBtn),
      doctorBox),
    h("div", { class: "mig-part" },
      h("div", { class: "settings-row" }, h("p", { class: "settings-hint", text: t("mig.planHint") }), planBtn),
      planBox));
  if (exported) planBox.replaceChildren(startedView(exported));
  if (autoDoctor) queueMicrotask(() => runDoctor(doctorBtn, doctorBox));
  return sec;
}

async function runDoctor(btn, box) {
  btn.disabled = true;
  btn.textContent = t("mig.doctorRunning");
  box.replaceChildren(h("p", { class: "mig-note", text: t("mig.doctorWait") }));
  try {
    const res = await api("GET", "/api/migrate/doctor");
    const findings = res.findings || [];
    const failed = findings.filter((f) => !f.ok);
    const passed = findings.filter((f) => f.ok);
    // What needs doing stays open on top; what passed folds away.
    box.replaceChildren(...[
      h("p", { class: `mig-sum ${failed.length ? "bad" : "ok"}`,
        text: failed.length ? t("mig.doctorBad", { n: failed.length, total: findings.length }) : t("mig.doctorOk", { n: findings.length }) }),
      failed.length ? h("ul", { class: "mig-list" }, ...failed.map(findingRow)) : null,
      passed.length ? h("details", { class: "mig-passed" },
        h("summary", { text: t("mig.passedFold", { n: passed.length }) }),
        h("ul", { class: "mig-list" }, ...passed.map(findingRow))) : null,
    ].filter(Boolean));
  } catch (err) {
    box.replaceChildren(h("p", { class: "mig-sum bad", text: err.message }));
  } finally {
    btn.disabled = false;
    btn.textContent = t("mig.doctorAgain");
  }
}

function findingName(f) {
  const key = `mig.kind.${f.kind}`;
  const name = t(key, { s: f.subject });
  return name === key ? f.name : name;
}

function findingHint(f) {
  if (f.kind === "source" || f.kind === "path") return t(`mig.fix.${f.kind}`);
  return f.hint;
}

function findingDetail(f) {
  return f.kind === "moved_out" ? t("mig.movedOutDetail") : f.detail;
}

function findingRow(f) {
  const detail = !f.ok && findingDetail(f);
  const hint = !f.ok && findingHint(f);
  return h("li", { class: `mig-item ${f.ok ? "ok" : "bad"}` },
    h("span", { class: "mig-mark", "aria-label": t(f.ok ? "mig.pass" : "mig.fail"), text: f.ok ? "✓" : "✗" }),
    h("div", { class: "mig-body" },
      h("b", { text: findingName(f) }),
      detail ? h("pre", { class: "mig-detail", text: detail }) : null,
      hint ? h("p", { class: "mig-hint", text: hint }) : null));
}

const size = (bytes) => t("mig.size", { mb: (bytes / MB).toFixed(1) });

async function showPlan(btn, box, onExported) {
  btn.disabled = true;
  box.replaceChildren(h("p", { class: "mig-note", text: t("mig.planLoading") }));
  try {
    const plan = await api("GET", "/api/migrate/plan");
    box.replaceChildren(plan.exporting ? startedView(plan.exporting) : planView(plan, box, onExported));
  } catch (err) {
    box.replaceChildren(h("p", { class: "mig-sum bad", text: err.message }));
  } finally {
    btn.disabled = false;
    btn.textContent = t("mig.planAgain");
  }
}

function planView(plan, box, onExported) {
  const packed = [plan.home, ...(plan.extras || [])].map((d) =>
    h("li", {}, h("code", { text: d.path }), h("span", { text: t("mig.files", { n: d.files }) + " · " + size(d.bytes) })));
  const needed = (plan.needed || []).map((p) =>
    h("li", {}, h("code", { text: p.path }), p.exists ? null : h("span", { class: "bad", text: t("mig.notHereEither") })));
  const busy = plan.busy || {};
  const busyText = busy.rounds || busy.jobs
    ? t("mig.busy", { rounds: busy.rounds, jobs: busy.jobs }) : t("mig.idle");
  const go = h("button", { class: "btn danger", type: "button", text: t("mig.exportBtn") });
  go.addEventListener("click", () => startExport(plan, go, box, onExported));
  return h("div", { class: "mig-plan" },
    h("h4", { text: t("mig.packed", { n: plan.total.files, size: size(plan.total.bytes) }) }),
    h("ul", { class: "mig-paths" }, ...packed),
    (plan.extras_missing || []).length
      ? h("p", { class: "mig-hint", text: t("mig.extrasMissing", { list: plan.extras_missing.join(t("list.sep")) }) }) : null,
    h("h4", { text: t("mig.notPacked") }),
    h("p", { class: "mig-hint", text: t("mig.notPackedText") }),
    h("h4", { text: t("mig.needed") }),
    needed.length ? h("ul", { class: "mig-paths" }, ...needed) : h("p", { class: "mig-hint", text: t("mig.neededNone") }),
    h("p", { class: "mig-hint", text: t("mig.neededText") }),
    h("p", { class: "mig-note", text: busyText }),
    h("div", { class: "mig-go" }, go));
}

async function startExport(plan, btn, box, onExported) {
  const ok = await confirmDialog({
    title: t("mig.confirmTitle"),
    body: t("mig.confirmBody", { path: plan.archive }),
    confirmText: t("mig.confirmGo"),
    danger: true,
  });
  if (!ok) return;
  btn.disabled = true;
  try {
    exported = await api("POST", "/api/migrate/export", { confirm: true });
    box.replaceChildren(startedView(exported));
    onExported?.(exported);
  } catch (err) {
    btn.disabled = false;
    toast(err.message, true);
  }
}

function startedView(res) {
  const cmd = h("code", { class: "mig-cmd", text: res.import_command });
  const copy = h("button", { class: "btn", type: "button", text: t("mig.copy") });
  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(res.import_command);
      toast(t("mig.copied"));
    } catch {
      toast(t("mig.copyFail"), true);
    }
  });
  return h("div", { class: "mig-started" },
    h("p", { class: "mig-sum ok", text: t("mig.started") }),
    h("dl", {},
      h("dt", { text: t("mig.pack") }), h("dd", {}, h("code", { text: res.path })),
      h("dt", { text: t("mig.log") }), h("dd", {}, h("code", { text: res.log })),
      h("dt", { text: t("mig.onNew") }), h("dd", { class: "mig-cmd-row" }, cmd, copy)),
    h("p", { class: "mig-hint", text: t("mig.onNewText") }),
    h("p", { class: "mig-note", text: t("mig.offlineSoon") }));
}
