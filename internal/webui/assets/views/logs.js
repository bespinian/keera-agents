// An agent's container output, followed as it is written.

import { api } from "../api.js";
import { h } from "../ui.js";

/** MAX_CHARS bounds the pane, so a chatty agent does not slow the page. */
const MAX_CHARS = 400_000;

/** trimLog keeps the end of a log within max characters, cut at a line. */
export function trimLog(text, max = MAX_CHARS) {
  if (text.length <= max) return text;
  const cut = text.indexOf("\n", text.length - max);
  return cut < 0 ? text.slice(text.length - max) : text.slice(cut + 1);
}

export function logPane(ctx, agent) {
  const pane = h("pre", { class: "log-pane", tabindex: "0", "aria-label": "Logs", "aria-live": "off" });
  const blip = h("span", { class: "blip" });
  const status = h("span", {}, "Live");
  const follow = h(
    "button",
    { class: "btn btn-live", type: "button", "aria-pressed": "true", onClick: () => (abort ? stop() : start()) },
    blip,
    status,
  );
  const autoScroll = h("input", { type: "checkbox", checked: true });
  let abort = null;
  let text = "";

  const setState = (what, words, title) => {
    blip.dataset.state = what;
    status.textContent = words;
    follow.title = title;
    follow.setAttribute("aria-pressed", String(what === "live" || what === "connecting"));
  };

  function paint() {
    pane.textContent = text || "Nothing written yet.";
    pane.classList.toggle("faint", !text);
    if (autoScroll.checked) pane.scrollTop = pane.scrollHeight;
  }

  async function start() {
    stop();
    const ac = (abort = new AbortController());
    text = "";
    paint();
    setState("connecting", "Connecting", "Connecting to the agent's output");
    try {
      const res = await api.agentLogs(agent.id, ac.signal);
      setState("live", "Live", "Following its output. Click to stop.");
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        text = trimLog(text + decoder.decode(value, { stream: true }));
        paint();
      }
      if (abort === ac) {
        abort = null;
        setState("stopped", "Ended", "The output ended, as when the machine stops. Click to follow again.");
      }
    } catch (err) {
      if (err.name === "AbortError") return;
      if (abort === ac) abort = null;
      text += (text && !text.endsWith("\n") ? "\n" : "") + `[${err.message}]\n`;
      paint();
      setState("stopped", "Stopped", "The stream failed. Click to try again.");
    }
  }

  function stop() {
    if (!abort) return;
    const ac = abort;
    abort = null;
    ac.abort();
    setState("paused", "Paused", "Not following. Click to follow its output again.");
  }

  // Scrolling up to read stops the pane from jumping back down.
  pane.addEventListener("scroll", () => {
    const atEnd = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 8;
    if (autoScroll.checked !== atEnd && document.activeElement === pane) autoScroll.checked = atEnd;
  });

  ctx.onTeardown(stop);
  start();

  return h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "card-head", style: { flexWrap: "wrap" } },
      h("h2", {}, "Output"),
      h("span", { class: "faint small" }, "What its machine prints, newest at the bottom."),
      h("div", { class: "spacer" }),
      h("label", { class: "row-tight small muted" }, autoScroll, "Scroll to the end"),
      h(
        "button",
        {
          class: "btn btn-sm btn-quiet",
          type: "button",
          title: "Clear the pane. The log itself is kept.",
          onClick: () => {
            text = "";
            paint();
          },
        },
        "Clear",
      ),
      follow,
    ),
    pane,
  );
}
