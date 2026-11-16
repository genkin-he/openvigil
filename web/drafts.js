// Unsent text survives re-renders. The console redraws whole sections whenever an event arrives
// (any task finishing a round), which would otherwise replace a half-written reply with an empty box.
const store = new Map(); // draft key → text the owner typed and has not sent
const initials = new Map(); // draft key → the text the field started with (a suggested reply, a saved doc)

/**
 * Bind a text field to a draft: it shows what the owner typed before the last redraw, else `initial`.
 * @param {HTMLInputElement|HTMLTextAreaElement} el
 * @param {string} key unique per field, e.g. "todo:td-1:reply"
 * @param {string} [initial] the server's text, used until the owner edits it
 */
export function draftField(el, key, initial = "") {
  el.value = store.has(key) ? store.get(key) : initial;
  el.dataset.draft = key;
  initials.set(key, initial);
  el.addEventListener("input", () => store.set(key, el.value));
  return el;
}

/** Forget drafts once their text was sent or saved. */
export function clearDrafts(...keys) {
  keys.forEach((k) => {
    store.delete(k);
    initials.delete(k);
  });
}

/**
 * Whether a field under root holds unsent text of the owner's own: something typed into an empty box,
 * or a suggested reply they changed. Text the field came with does not count.
 */
export function hasDraftIn(root) {
  if (!root) return false;
  return [...root.querySelectorAll("[data-draft]")].some((el) => {
    const text = store.get(el.dataset.draft);
    return text !== undefined && text.trim() !== "" && text !== initials.get(el.dataset.draft);
  });
}

/** Run a redraw and give focus, caret and scroll back to the draft field the owner was typing in. */
export function keepFocus(redraw) {
  const active = document.activeElement;
  const key = active?.dataset?.draft;
  const caret = key ? { start: active.selectionStart, end: active.selectionEnd, top: active.scrollTop } : null;
  redraw();
  if (!key) return;
  const el = document.querySelector(`[data-draft="${CSS.escape(key)}"]`);
  if (!el || el === active) return;
  el.focus({ preventScroll: true });
  try {
    el.setSelectionRange(caret.start, caret.end);
  } catch {
    // Not a text field that has a caret: focus alone is enough.
  }
  el.scrollTop = caret.top;
}
