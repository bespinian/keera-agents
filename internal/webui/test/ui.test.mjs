// Unit tests for the panel's pure helpers. node:test keeps the panel free of
// a build step and dependencies.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  amount,
  money,
  francs,
  microsOf,
  until,
  splitList,
  plural,
  initials,
  every,
} from "../assets/ui.js";
import {
  firstLine,
  took,
  isLive,
  kindLabel,
  agentPath,
  taskPath,
} from "../assets/common.js";
import { modesFor } from "../assets/views/channels.js";
import { setupText } from "../assets/views/connectors.js";
import { trimLog } from "../assets/views/logs.js";

test("money reads in francs with the currency first", () => {
  assert.equal(money(12_500_000), "CHF 12.50");
  assert.equal(amount(0), "0.00");
  assert.equal(amount(1_000_000), "1.00");
  // Small amounts keep their digits rather than rounding to zero.
  assert.equal(amount(1234), "0.0012");
});

test("a budget field round-trips through micro-CHF", () => {
  assert.equal(francs(0), "");
  assert.equal(francs(50_000_000), "50");
  assert.equal(francs(12_500_000), "12.5");
  assert.equal(microsOf(""), 0);
  assert.equal(microsOf("50"), 50_000_000);
  assert.equal(microsOf("12.50"), 12_500_000);
  assert.equal(microsOf("12,5"), 12_500_000);
  assert.equal(microsOf("CHF 1'200"), 1_200_000_000);
  assert.equal(microsOf(francs(33_330_000)), 33_330_000);
  assert.ok(Number.isNaN(microsOf("-5")));
  assert.ok(Number.isNaN(microsOf("ten")));
});

test("until says how far ahead, and falls back to ago behind", () => {
  const now = Date.parse("2026-11-02T08:00:00Z");
  assert.equal(until("2026-11-02T08:00:20Z", now), "in a moment");
  assert.equal(until("2026-11-02T08:30:00Z", now), "in 30m");
  assert.equal(until("2026-11-02T11:00:00Z", now), "in 3h");
  assert.equal(until("2026-11-05T08:00:00Z", now), "in 3d");
  assert.equal(until("", now), "-");
  assert.match(until("2020-01-01T00:00:00Z", now), /ago|20/);
});

test("splitList reads a list typed by hand", () => {
  assert.deepEqual(splitList("a@x.ch, b@x.ch\n\nA@x.ch; c@x.ch"), [
    "a@x.ch",
    "b@x.ch",
    "c@x.ch",
  ]);
  assert.deepEqual(splitList("Anna@X.ch", { lower: true }), ["anna@x.ch"]);
  assert.deepEqual(splitList("finance, , urgent "), ["finance", "urgent"]);
  assert.deepEqual(splitList(""), []);
});

test("words and initials", () => {
  assert.equal(plural(1, "agent"), "agent");
  assert.equal(plural(3, "agent"), "agents");
  assert.equal(initials("Anna Muster", "anna@x.ch"), "AM");
  assert.equal(initials("", "anna@x.ch"), "AN");
});

test("firstLine turns a prompt into a title", () => {
  assert.equal(
    firstLine("\n  Summarise the tickets\nand more"),
    "Summarise the tickets",
  );
  assert.equal(firstLine("x".repeat(100), 10), "xxxxxxxxx…");
  assert.equal(firstLine(""), "");
});

test("took measures a task, or how long it has run", () => {
  const t = {
    started_at: "2026-11-02T08:00:00Z",
    finished_at: "2026-11-02T08:01:30Z",
  };
  assert.equal(took(t), "1 min 30 s");
  assert.equal(
    took(
      { started_at: "2026-11-02T08:00:00Z" },
      Date.parse("2026-11-02T08:00:45Z"),
    ),
    "45 s",
  );
  assert.equal(took({}), "");
  assert.ok(isLive("queued") && isLive("running") && !isLive("failed"));
});

test("a channel offers the modes its kind supports", () => {
  assert.deepEqual(modesFor({ chat: true, files: false }), ["chat"]);
  assert.deepEqual(modesFor({ chat: false, files: true }), ["watch", "files"]);
  assert.deepEqual(modesFor({ code: true }), ["repo"]);
  assert.deepEqual(modesFor({ api: true }), ["read", "write"]);
  assert.deepEqual(modesFor(null), []);
  assert.equal(kindLabel("slack", []), "Slack");
  assert.equal(
    kindLabel("teams", [{ kind: "teams", label: "Teams" }]),
    "Teams",
  );
});

test("set-up text names the webhook before it exists", () => {
  assert.equal(
    setupText("Paste {webhook} in the app.", "https://x/hooks/1"),
    "Paste https://x/hooks/1 in the app.",
  );
  assert.match(
    setupText("Paste {webhook} in the app.", ""),
    /shown once saved/,
  );
});

test("the log pane keeps the newest lines", () => {
  assert.equal(trimLog("short", 100), "short");
  assert.equal(trimLog("one\ntwo\nthree\n", 8), "three\n");
});

test("paths encode the id, so it stays one segment", () => {
  assert.equal(agentPath("agent_1"), "/agents/agent_1");
  assert.equal(agentPath("a/b?c"), "/agents/a%2Fb%3Fc");
  assert.equal(agentPath("agent_1", "tasks"), "/agents/agent_1/tasks");
  assert.equal(taskPath("t 1"), "/tasks/t%201");
});

test("every skips hidden tabs and busy runs, and stops with the screen", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  globalThis.document = { hidden: false };
  const teardowns = [];
  const ctx = { onTeardown: (fn) => teardowns.push(fn) };
  const settle = () => new Promise((r) => setImmediate(r));

  let runs = 0;
  let finish;
  every(ctx, 1000, () => {
    runs++;
    return new Promise((r) => (finish = r));
  });

  t.mock.timers.tick(1000);
  assert.equal(runs, 1);
  // Still running: the next tick waits.
  t.mock.timers.tick(1000);
  assert.equal(runs, 1);
  finish();
  await settle();

  document.hidden = true;
  t.mock.timers.tick(1000);
  assert.equal(runs, 1);
  document.hidden = false;
  t.mock.timers.tick(1000);
  assert.equal(runs, 2);
  finish();
  await settle();

  for (const fn of teardowns) fn();
  t.mock.timers.tick(5000);
  assert.equal(runs, 2);
});

test("every stops when told to", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  globalThis.document = { hidden: false };
  let runs = 0;
  const stop = every({ onTeardown() {} }, 1000, () => {
    runs++;
    stop();
  });
  t.mock.timers.tick(3000);
  assert.equal(runs, 1);
});
