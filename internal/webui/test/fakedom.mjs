// Just enough of the DOM for the panel's h() and the Markdown renderer, so
// node:test can check what they build without a browser or a dependency.

class FakeNode {
  constructor(tag) {
    this.tagName = tag ? tag.toUpperCase() : "";
    this.childNodes = [];
    this.attributes = {};
    this.className = "";
    this.style = {};
    this.dataset = {};
  }
  append(...nodes) {
    for (const n of nodes)
      this.childNodes.push(typeof n === "string" ? new FakeText(n) : n);
  }
  replaceChildren(...nodes) {
    this.childNodes = [];
    this.append(...nodes);
  }
  setAttribute(k, v) {
    this.attributes[k] = String(v);
  }
  getAttribute(k) {
    return this.attributes[k] ?? null;
  }
  addEventListener() {}
  get textContent() {
    return this.childNodes.map((c) => c.textContent).join("");
  }
  /** all lists this node and every node below it. */
  all() {
    return [this, ...this.childNodes.flatMap((c) => c.all())];
  }
}

class FakeText extends FakeNode {
  constructor(data) {
    super("");
    this.data = data;
  }
  get textContent() {
    return this.data;
  }
}

export function installFakeDOM() {
  globalThis.Node = FakeNode;
  globalThis.document = {
    createElement: (tag) => new FakeNode(tag),
    createElementNS: (_, tag) => new FakeNode(tag),
    createTextNode: (data) => new FakeText(data),
  };
}
