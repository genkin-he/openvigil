// Desktop notifications while the console is open in a background tab or window.
// Browsers only allow asking on a user gesture, so permission is requested on the first click.

export function askNotifyPermission() {
  if (!("Notification" in window) || Notification.permission !== "default") return;
  Notification.requestPermission().catch(() => {});
}

/**
 * Notify when the console is not in front; a click brings it forward and runs onClick.
 * A tag replaces the earlier notification with the same tag (repeated failures of one task stay one).
 */
export function notifyDesktop(title, body, onClick, tag) {
  if (!("Notification" in window) || Notification.permission !== "granted" || !document.hidden) return;
  try {
    const n = new Notification(title, { body: String(body).slice(0, 180), tag: tag || `vigil-${title}-${body}`.slice(0, 64) });
    n.onclick = () => {
      window.focus();
      onClick?.();
      n.close();
    };
  } catch {
    /* some browsers refuse notifications from non-secure origins; the toast still shows */
  }
}
