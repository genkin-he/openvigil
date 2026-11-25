// Run with: node --test web/today.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { todayRuns, causeOf, incidents, feed, upcoming, groupOf } from "./today.js";

const NOW = new Date(2026, 10, 25, 14, 30);
const at = (hh, mm) => new Date(2026, 10, 25, hh, mm).toISOString();
const run = (id, hh, mm, status, summary = "") => ({ id, status, summary, started_at: at(hh, mm) });
const view = (id, recent, extra = {}) => ({
  task: { id, name: id, enabled: true, mode: "headless", ...extra.task },
  state: { fail_streak: 0 }, recent, ...extra,
});
const BLOCKED = "API Error: This organization can't receive replies from Claude right now.";

test("todayRuns keeps finished runs since midnight, oldest first, with the task's name", () => {
  const v = view("a", [run("r3", 14, 0, "running"), run("r2", 9, 0, "ok"), run("r1", 8, 0, "fail")]);
  const old = view("b", [{ id: "r0", status: "ok", summary: "", started_at: new Date(2026, 10, 24, 23, 0).toISOString() }]);
  const got = todayRuns([v, old], NOW);
  assert.deepEqual(got.map((r) => r.id), ["r1", "r2"]);
  assert.equal(got[0].name, "a");
});

test("causeOf names the usual API failures", () => {
  assert.equal(causeOf(BLOCKED), "account");
  assert.equal(causeOf("claude exited: exit status 1: API Error: Can't reach the API server — check your internet"), "unreachable");
  assert.equal(causeOf("API Error: Request blocked before it reached Anthropic"), "blocked");
  assert.equal(causeOf("runbook step 3 failed"), "other");
});

test("failures close together across tasks are one incident, and the next success marks its recovery", () => {
  const runs = todayRuns([
    view("slack", [run("s1", 12, 34, "fail", BLOCKED), run("s2", 12, 40, "fail", BLOCKED), run("s3", 13, 50, "fail", BLOCKED),
      run("s4", 13, 52, "ok"), run("s5", 14, 20, "ok")]),
    view("evolve", [run("e1", 12, 49, "fail", BLOCKED)]),
    view("digest", [run("d1", 8, 54, "fail", "Can't reach the API server"), run("d2", 9, 44, "ok")]),
  ], NOW);
  // With an 80-minute gap the 12:34–13:50 failures, evolve's among them, are one incident; 08:54 stands alone.
  const got = incidents(runs, 80 * 60 * 1000);
  assert.equal(got.length, 2);
  const [digest, outage] = got;
  assert.equal(digest.cause, "unreachable");
  assert.equal(digest.runs.length, 1);
  assert.equal(digest.recovered.id, "d2");
  assert.equal(outage.cause, "account");
  assert.equal(outage.runs.length, 4);
  assert.deepEqual(outage.byTask, [{ name: "slack", n: 3 }, { name: "evolve", n: 1 }]);
  assert.equal(outage.recovered.id, "s4");
});

test("an incident still failing has no recovery", () => {
  const runs = todayRuns([view("a", [run("a1", 14, 0, "fail", BLOCKED), run("a2", 14, 5, "timeout", BLOCKED)])], NOW);
  const [inc] = incidents(runs);
  assert.equal(inc.runs.length, 2);
  assert.equal(inc.recovered, null);
});

test("feed tells the day newest first: recoveries, incidents, what waits on the owner, headless runs that finished", () => {
  const views = [
    view("slack", [run("s1", 12, 34, "fail", BLOCKED), run("s2", 13, 52, "ok"), run("s3", 14, 28, "ok")], { task: { mode: "resident" } }),
    view("restart", [run("c1", 12, 0, "needs_you")]),
    view("brief", [run("b1", 8, 0, "ok", "早报已发")]),
  ];
  const runs = todayRuns(views, NOW);
  const items = feed(runs, incidents(runs));
  assert.deepEqual(items.map((i) => i.type), ["recovered", "failed", "needs", "done"]);
  assert.equal(items[0].inc.recovered.id, "s2");
  assert.equal(items[3].run.id, "b1");
});

test("a headless run that is an incident's recovery is told once, as the recovery", () => {
  const runs = todayRuns([view("digest", [run("d1", 8, 54, "fail", "Can't reach the API server"), run("d2", 9, 44, "ok")])], NOW);
  assert.deepEqual(feed(runs, incidents(runs)).map((i) => i.type), ["recovered", "failed"]);
});

test("upcoming lists enabled tasks still due before midnight, soonest first", () => {
  const views = [
    view("late", [], { next_run: at(21, 30) }),
    view("soon", [], { next_run: at(18, 30) }),
    view("tomorrow", [], { next_run: new Date(2026, 10, 26, 8, 0).toISOString() }),
    view("off", [], { next_run: at(19, 0), task: { enabled: false } }),
  ];
  assert.deepEqual(upcoming(views, NOW).map((v) => v.task.id), ["soon", "late"]);
});

test("groupOf sorts a task into what needs a look, ran today, still due tonight, or not today", () => {
  const runs = (v) => todayRuns([v], NOW);
  const needs = view("n", [run("n1", 12, 0, "needs_you")], { needs_you: { started_at: at(12, 0) } });
  const failing = view("f", [run("f1", 12, 0, "fail")], { state: { fail_streak: 2 } });
  const ran = view("r", [run("r1", 8, 0, "ok")], { next_run: at(20, 0) });
  const tonight = view("t", [], { next_run: at(18, 30) });
  const later = view("l", [], { next_run: new Date(2026, 10, 27, 17, 0).toISOString() });
  const paused = view("p", [run("p1", 9, 0, "ok")], { task: { enabled: false } });
  assert.equal(groupOf(needs, runs(needs), NOW), "look");
  assert.equal(groupOf(failing, runs(failing), NOW), "look");
  assert.equal(groupOf(ran, runs(ran), NOW), "ran");
  assert.equal(groupOf(tonight, [], NOW), "tonight");
  assert.equal(groupOf(later, [], NOW), "off");
  assert.equal(groupOf(paused, runs(paused), NOW), "off");
});
