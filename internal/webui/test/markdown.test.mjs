import { test } from "node:test";
import assert from "node:assert/strict";

import { installFakeDOM } from "./fakedom.mjs";

installFakeDOM();
const { render, code } = await import("../assets/markdown.js");

const tags = (node) =>
  node
    .all()
    .map((n) => n.tagName)
    .filter(Boolean);

test("HTML in a result stays text", () => {
  const out = render(
    'Hello <script>alert(1)</script> <img src=x onerror="alert(2)">',
  );
  assert.ok(!tags(out).includes("SCRIPT"));
  assert.ok(!tags(out).includes("IMG"));
  assert.match(out.textContent, /<script>alert\(1\)<\/script>/);
});

test("only web and mail links become links", () => {
  const out = render(
    "[safe](https://example.ch) [bad](javascript:alert(1)) [data](data:text/html,x)",
  );
  const links = out.all().filter((n) => n.tagName === "A");
  assert.equal(links.length, 1);
  assert.equal(links[0].attributes.href, "https://example.ch");
  assert.equal(links[0].attributes.rel, "noopener noreferrer");
  assert.match(out.textContent, /bad/);
});

test("the usual marks render", () => {
  const out = render(
    "# Title\n\n**bold** and `code`\n\n- one\n- two\n\n```js\nlet x = 1;\n```",
  );
  const t = tags(out);
  for (const tag of ["H2", "STRONG", "CODE", "UL", "LI", "PRE"])
    assert.ok(t.includes(tag), tag);
});

test("comments render only code", () => {
  const out = code(
    "Run `make check` and **ship** it.\n\n```sh\nmake build\n```\n\nA stray ` stays.",
  );
  const t = tags(out);
  assert.ok(t.includes("CODE"));
  assert.ok(t.includes("PRE"));
  assert.ok(!t.includes("STRONG"));
  assert.match(out.textContent, /\*\*ship\*\*/);
  assert.match(out.textContent, /A stray ` stays\./);
  const spans = out.all().filter((n) => n.tagName === "CODE");
  assert.deepEqual(
    spans.map((n) => n.textContent),
    ["make check", "make build"],
  );
});
