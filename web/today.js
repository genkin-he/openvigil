// How today has gone, worked out from the overview: no DOM and no imports, so it is tested with
// `node --test web/`. Failures that land close together are one incident; the homepage tells those,
// not each red run.

const FAILED = new Set(["fail", "timeout"]);
export const INCIDENT_GAP_MS = 10 * 60 * 1000;

const CAUSES = [
  ["account", /can't receive replies|Verification Portal|organization has been disabled/i],
  ["blocked", /Request blocked|network_edge_blocked|Attention Required/i],
  ["unreachable", /Can't reach the API server|ENOTFOUND|ECONNRESET|ECONNREFUSED|\bEOF\b/i],
  ["overloaded", /overloaded|rate.?limit|\b529\b/i],
];

export function startOfDay(now) {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  return d;
}

function endOfDay(now) {
  const d = startOfDay(now);
  d.setDate(d.getDate() + 1);
  return d;
}

/** Every task's runs that finished today, oldest first, each carrying its task. */
export function todayRuns(views, now) {
  const since = startOfDay(now);
  return views.flatMap((v) => v.recent
    .filter((r) => r.status !== "running")
    .map((r) => ({ ...r, at: new Date(r.started_at), taskId: v.task.id, name: v.task.name, resident: v.task.mode === "resident" }))
    .filter((r) => r.at >= since && r.at <= now))
    .sort((a, b) => a.at - b.at);
}

/** Which usual failure a run's text is: account, blocked, unreachable, overloaded or other. */
export function causeOf(text) {
  const hit = CAUSES.find(([, re]) => re.test(String(text ?? "")));
  return hit ? hit[0] : "other";
}

/**
 * Failures no more than gap apart, across tasks, as incidents. Each knows its runs, how many per
 * task (most first), its cause and the first success after it in any of its tasks (null: still failing).
 */
export function incidents(runs, gap = INCIDENT_GAP_MS) {
  const groups = [];
  for (const r of runs.filter((x) => FAILED.has(x.status))) {
    const last = groups.at(-1);
    if (last && r.at - last.at(-1).at <= gap) last.push(r);
    else groups.push([r]);
  }
  return groups.map((group) => {
    const end = group.at(-1).at;
    const tasks = new Set(group.map((r) => r.taskId));
    const counts = new Map();
    for (const r of group) counts.set(r.name, (counts.get(r.name) || 0) + 1);
    return {
      start: group[0].at, end, runs: group,
      byTask: [...counts].map(([name, n]) => ({ name, n })).sort((a, b) => b.n - a.n),
      cause: causeOf(group[0].summary), text: group[0].summary || "",
      recovered: runs.find((r) => r.status === "ok" && r.at > end && tasks.has(r.taskId)) || null,
    };
  });
}

/**
 * The day's story, newest first: each incident and its recovery, what waited on the owner, and the
 * runs of scheduled (non-resident) tasks that finished. A resident's routine rounds are left out.
 */
export function feed(runs, incs) {
  const recoveries = new Set(incs.map((i) => i.recovered?.id).filter(Boolean));
  const items = [];
  for (const inc of incs) {
    items.push({ at: inc.start, kind: "bad", type: inc.runs.length > 1 ? "incident" : "failed", inc });
    if (inc.recovered) items.push({ at: inc.recovered.at, kind: "ok", type: "recovered", inc });
  }
  for (const r of runs) {
    if (r.status === "needs_you") items.push({ at: r.at, kind: "want", type: "needs", run: r });
    else if (r.status === "ok" && !r.resident && !recoveries.has(r.id)) items.push({ at: r.at, kind: "ok", type: "done", run: r });
  }
  return items.sort((a, b) => b.at - a.at);
}

/** Enabled tasks still due before midnight, soonest first. */
export function upcoming(views, now) {
  const end = endOfDay(now);
  return views
    .filter((v) => v.task.enabled && v.next_run && new Date(v.next_run) > now && new Date(v.next_run) < end)
    .sort((a, b) => new Date(a.next_run) - new Date(b.next_run));
}

/** Where a task sits on the homepage: look (waits on the owner or failing), ran, tonight or off. */
export function groupOf(v, runs, now) {
  if (!v.task.enabled) return "off";
  if (v.needs_you || v.state.fail_streak > 0) return "look";
  if (runs.length || v.live) return "ran";
  return upcoming([v], now).length ? "tonight" : "off";
}
