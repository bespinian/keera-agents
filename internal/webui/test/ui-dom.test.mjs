// Tests for ui.js helpers that build elements, on the fake DOM.

import { test } from "node:test";
import assert from "node:assert/strict";

import { installFakeDOM } from "./fakedom.mjs";

installFakeDOM();
const { pill, card, fail, banner } = await import("../assets/ui.js");

test("a pill may start with a dot", () => {
  const plain = pill("Off");
  assert.equal(plain.className, "pill");
  assert.equal(plain.childNodes.length, 1);

  const state = pill("Running", "good", { dot: true });
  assert.equal(state.className, "pill pill-good");
  assert.equal(state.childNodes[0].className, "dot");
  assert.equal(state.textContent, "Running");
});

test("a card has its title, what goes beside it, and its body", () => {
  const [head, body] = card("Tasks", "chart", { more: "3 failed" }).childNodes;
  assert.equal(head.className, "card-head");
  assert.equal(head.childNodes[0].textContent, "Tasks");
  assert.equal(head.textContent, "Tasks3 failed");
  assert.equal(body.className, "card-body");

  // With no bodyClass, the body goes in as it is.
  const bare = card("Steps", "timeline", { bodyClass: null });
  assert.equal(bare.childNodes[0].childNodes.length, 1);
  assert.equal(bare.childNodes[1].textContent, "timeline");
});

test("fail opens the fold, focuses the field and throws its message", () => {
  const details = { open: false };
  let focused = false;
  const input = { closest: () => details, focus: () => (focused = true) };
  assert.throws(() => fail(input, "Give it a name."), {
    message: "Give it a name.",
  });
  assert.ok(details.open && focused);

  // Something with only focus(), such as the schedule's picker, works too.
  assert.throws(() => fail({ focus() {} }, "Pick a day."), {
    message: "Pick a day.",
  });
});

test("a bad banner is an alert, and the others are not", () => {
  assert.equal(banner("bad", "It failed.").getAttribute("role"), "alert");
  assert.equal(banner("info", "Note this.").getAttribute("role"), null);
  // Attributes given still apply.
  const styled = banner("bad", { id: "why" }, "It failed.");
  assert.equal(styled.getAttribute("id"), "why");
  assert.equal(styled.className, "banner banner-bad");
});
