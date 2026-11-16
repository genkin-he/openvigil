// In-page confirmation in the console's own style, replacing the browser's native confirm().
import { h } from "./util.js";
import { t } from "./i18n.js";

/**
 * Ask before doing something. Resolves true only when the confirm button is pressed;
 * Esc, the cancel button or a click on the backdrop resolve false.
 * @param {{title: string, body?: string, confirmText: string, danger?: boolean}} opts
 * @returns {Promise<boolean>}
 */
export function confirmDialog({ title, body, confirmText, danger = false }) {
  return new Promise((resolve) => {
    const cancel = h("button", { class: "btn", type: "button", text: t("common.cancel") });
    const ok = h("button", { class: `btn ${danger ? "danger-solid" : "primary"}`, type: "button", text: confirmText });
    const dlg = h("dialog", { class: `confirm${danger ? " is-danger" : ""}`, "aria-labelledby": "confirm-title" },
      h("div", { class: "confirm-mark", "aria-hidden": true, text: danger ? "!" : "?" }),
      h("h2", { id: "confirm-title", text: title }),
      body ? h("p", { text: body }) : null,
      h("div", { class: "confirm-actions" }, cancel, ok));

    let settled = false;
    const finish = (answer) => {
      if (settled) return;
      settled = true;
      dlg.classList.add("leaving");
      setTimeout(() => { dlg.close(); dlg.remove(); }, 140);
      resolve(answer);
    };
    cancel.addEventListener("click", () => finish(false));
    ok.addEventListener("click", () => finish(true));
    dlg.addEventListener("cancel", (e) => { e.preventDefault(); finish(false); });
    dlg.addEventListener("click", (e) => { if (e.target === dlg) finish(false); });

    document.body.append(dlg);
    dlg.showModal();
    cancel.focus(); // destructive actions never default to "yes"
  });
}
