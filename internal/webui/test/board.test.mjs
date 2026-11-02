import { test } from "node:test";
import assert from "node:assert/strict";

import {
  positionBetween,
  placeCard,
  columnOf,
  dueISO,
  dueDay,
  STATUSES,
} from "../assets/views/board.js";

test("a dropped card goes between its neighbours", () => {
  assert.equal(positionBetween(1, 2), 1.5);
  assert.equal(positionBetween(undefined, 4), 3);
  assert.equal(positionBetween(4, undefined), 5);
  assert.equal(positionBetween(undefined, undefined), 1);
  assert.equal(positionBetween(-1, 0), -0.5);
});

const cards = [
  { id: "a", status: "todo", position: 1 },
  { id: "b", status: "todo", position: 2 },
  { id: "c", status: "todo", position: 3 },
  { id: "d", status: "done", position: 10 },
];

test("placeCard orders a column without the card being moved", () => {
  assert.deepEqual(
    columnOf(cards, "todo", "b").map((c) => c.id),
    ["a", "c"],
  );
  // To the top, the middle and the end of another column.
  assert.deepEqual(placeCard(cards, "d", "todo", 0), {
    status: "todo",
    position: 0,
  });
  assert.deepEqual(placeCard(cards, "d", "todo", 1), {
    status: "todo",
    position: 1.5,
  });
  assert.deepEqual(placeCard(cards, "d", "todo", Infinity), {
    status: "todo",
    position: 4,
  });
  // Down one in its own column: after c, the last.
  assert.deepEqual(placeCard(cards, "b", "todo", 2), {
    status: "todo",
    position: 4,
  });
  // Up one: before a.
  assert.deepEqual(placeCard(cards, "b", "todo", 0), {
    status: "todo",
    position: 0,
  });
  // Into an empty column.
  assert.deepEqual(placeCard(cards, "a", "review", 0), {
    status: "review",
    position: 1,
  });
});

test("the five columns are in the board's order", () => {
  assert.deepEqual(
    STATUSES.map(([s]) => s),
    ["backlog", "todo", "in_progress", "review", "done"],
  );
});

test("a due day survives the trip through a timestamp", () => {
  for (const day of ["2026-01-01", "2026-03-29", "2026-10-25", "2026-12-31"]) {
    assert.equal(dueDay(dueISO(day)), day);
  }
  assert.equal(dueISO(""), "");
  assert.equal(dueDay(""), "");
});
