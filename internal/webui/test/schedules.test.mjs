import { test } from "node:test";
import assert from "node:assert/strict";

import {
  cronProblem,
  timeZones,
  inZone,
  daysField,
  ruleFromCron,
  ruleToCron,
  ruleProblem,
  EVERY,
} from "../assets/views/schedules.js";

test("cronProblem catches what is plainly not cron", () => {
  assert.equal(cronProblem("0 8 * * 1-5"), "");
  assert.equal(cronProblem("  */15 * * * *  "), "");
  assert.equal(cronProblem("@daily"), "");
  assert.match(cronProblem(""), /when/);
  assert.match(cronProblem("0 8 * *"), /has 4/);
  assert.match(cronProblem("0 0 8 * * 1"), /has 6/);
});

test("days of the week read as short cron", () => {
  assert.equal(daysField([1, 2, 3, 4, 5]), "1-5");
  assert.equal(daysField([5, 1, 3]), "1,3,5");
  assert.equal(daysField([0, 6]), "0,6");
  assert.equal(daysField([1, 2]), "1,2");
  assert.equal(daysField([0, 1, 2, 3, 4, 5, 6]), "*");
});

test("what the picker builds reads back the same", () => {
  for (const [cron, repeat] of [
    ["0 8 * * *", "day"],
    ["30 7 * * 1-5", "week"],
    ["0 10 * * 0,6", "week"],
    ["0 6 15 * *", "month"],
    ["0 6 1 1 *", "year"],
    ["0 * * * *", "often"],
    ["*/30 * * * *", "often"],
    ["*/15 8-17 * * 1-5", "often"],
    ["0 */2 * * *", "often"],
    ["0 8-17/3 * * 1-5", "often"],
    ["0 9 * * *", "day"],
  ]) {
    const rule = ruleFromCron(cron);
    assert.equal(rule.repeat, repeat, cron);
    assert.equal(ruleToCron(rule), cron, cron);
  }
});

test("other forms of the same schedule fill in the picker", () => {
  assert.deepEqual(
    ruleFromCron("0 9 * * mon-fri").days.sort(),
    [1, 2, 3, 4, 5],
  );
  assert.deepEqual(ruleFromCron("0 9 * * 5-7").days.sort(), [0, 5, 6]);
  assert.equal(ruleToCron(ruleFromCron("@daily")), "0 0 * * *");
  assert.equal(ruleToCron(ruleFromCron("@hourly")), "0 * * * *");
  assert.equal(ruleFromCron("0 8 * * 1-5").time, "08:00");
});

test("what the picker cannot show stays as cron", () => {
  for (const cron of [
    "* * * * *",
    "0 8,17 * * *",
    "*/7 * * * *",
    "0 8/2 * * *",
    "15 */2 * * *",
    "0 8 1 * 1",
    "0 8 */2 * *",
    "0 8 * * 1/2",
    "nonsense",
  ]) {
    const rule = ruleFromCron(cron);
    assert.equal(rule.repeat, "custom", cron);
    assert.equal(ruleToCron(rule), cron, cron);
  }
});

test("one hour of several times a day is that hour alone", () => {
  const rule = {
    ...ruleFromCron("0 8 * * *"),
    repeat: "often",
    from: 8,
    to: 9,
  };
  assert.equal(ruleToCron({ ...rule, every: 15 }), "*/15 8 * * *");
  // 8/2 would run from 08:00 to the end of the day.
  assert.equal(ruleToCron({ ...rule, every: 120 }), "0 8 * * *");
});

test("every gap divides the hour or the day", () => {
  for (const m of EVERY)
    assert.ok(m < 60 ? 60 % m === 0 : 1440 % m === 0, String(m));
});

test("a rule with something missing says what", () => {
  const rule = ruleFromCron("0 8 * * 1-5");
  assert.equal(ruleProblem(rule), "");
  assert.match(ruleProblem({ ...rule, days: [] }), /day/);
  assert.match(
    ruleProblem({ ...rule, repeat: "often", from: 10, to: 9 }),
    /before/,
  );
  assert.match(ruleProblem({ ...rule, time: "" }), /time/);
  assert.match(
    ruleProblem({ ...rule, repeat: "custom", cron: "0 8" }),
    /five fields/,
  );
});

test("timeZones puts UTC first and keeps the zones asked for", () => {
  const zones = timeZones("Europe/Zurich", "Mars/Olympus_Mons");
  assert.equal(zones[0], "UTC");
  assert.ok(zones.includes("Europe/Zurich"));
  assert.ok(zones.includes("Mars/Olympus_Mons"));
  assert.equal(new Set(zones).size, zones.length);
});

test("a run time reads in the schedule's own zone", () => {
  const at = "2026-11-02T07:00:00Z";
  assert.match(inZone(at, "Europe/Zurich"), /08:00/);
  assert.match(inZone(at, "UTC"), /07:00/);
  // An unknown zone falls back to the browser's, rather than failing.
  assert.ok(inZone(at, "Not/A_Zone"));
});
