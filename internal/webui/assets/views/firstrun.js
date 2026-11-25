// The getting started guide, ticked off from what the control plane has. It
// replaces the dashboard until the organisation's first task. Its steps and
// overview look like Keera Gateway's.

import { h, svg, icon, icons, pill, isAdmin, brandMark, kindMark } from "../ui.js";
import { openNewAgent } from "./agents.js";
import { giveTask } from "./tasks.js";

/** The sidebar uses this too, to name the screen. setup is /setup's answer. */
export function firstRunDue(setup) {
  return !!setup && !setup.started;
}

/** firstRun is the guide. agents are those the reader may give work to. */
export function firstRun(ctx, setup, agents) {
  const admin = isAdmin(ctx);
  const steps = [
    {
      title: "Create an agent",
      icon: icons.agents,
      done: setup.agents > 0,
      body:
        "An agent is an assistant with its own machine. Give it " + "instructions, a model and the tools it may use.",
      action: { label: "New agent", run: () => openNewAgent(ctx) },
    },
    {
      title: "Connect your services",
      icon: icons.connections,
      done: setup.connections > 0,
      body:
        "Connectors let agents take work from Teams, Google Chat or Slack, " +
        "work with files in Google Drive or OneDrive, and use GitHub, Jira " +
        "and more. Then add them to your agent on its Connectors tab.",
      action: admin ? { label: "Open connector catalogue", run: () => ctx.navigate("/connectors/catalogue") } : null,
      skipped: !admin,
      by: "An administrator",
    },
    {
      title: "Give it a first task",
      icon: icons.send,
      done: setup.started,
      body: "Give it a task here, mention it in a chat, or put a card for it in To do on a squad's board.",
      action: agents.length ? { label: "Give a task", run: () => giveTask(ctx, { agents }) } : null,
      also: setup.agents > 0 ? { label: "Open squads", run: () => ctx.navigate("/squads") } : null,
    },
  ];
  return h("div", {}, overview(), stepList(steps));
}

function stepList(steps) {
  const nextIndex = steps.findIndex((s) => !s.done && !s.skipped);
  const button = (a, primary) =>
    h("button", { class: "btn" + (primary ? " btn-primary" : ""), onClick: a.run }, a.label);

  return h(
    "div",
    { class: "steps" },
    steps.map((s, i) => {
      const current = i === nextIndex;
      return h(
        "div",
        { class: "step" + (s.done ? " step-done" : "") + (current ? " step-now" : "") },
        h(
          "div",
          { class: "step-icon" },
          icon(s.icon),
          h("span", { class: "step-n" }, s.done ? icon(icons.check) : String(i + 1)),
        ),
        h(
          "div",
          { class: "step-body" },
          h(
            "div",
            { class: "row" },
            h("h2", { style: { marginBottom: "6px" } }, s.title),
            h("div", { class: "spacer" }),
            s.done ? pill("Done", "good") : null,
          ),
          h("p", { class: "muted" }, s.body),
          !s.done && s.action
            ? h("div", { class: "row" }, button(s.action, current), s.also ? button(s.also, false) : null)
            : null,
          !s.done && !s.action && s.skipped ? h("p", { class: "hint" }, s.by + " does this one.") : null,
        ),
      );
    }),
  );
}

// Where work comes from, by connector kind, or an icon for the panel's own.
const flowFrom = [
  ["Microsoft Teams", () => kindMark("teams")],
  ["Google Chat", () => kindMark("googlechat")],
  ["Slack", () => kindMark("slack")],
  ["Squads and schedules", () => icon(icons.squads)],
];

// Where an agent works, and where its models come from.
const flowTo = [
  ["Google Drive", () => kindMark("gdrive")],
  ["OneDrive", () => kindMark("onedrive")],
  ["GitHub", () => kindMark("github")],
  // Keera Gateway carries the same mark as Keera Agents.
  ["Keera Gateway", () => brandMark(18)],
];

const flowAgent = [
  ["Instructions", icons.docs],
  ["Tools", icons.tool],
  ["Memory", icons.database],
  ["Own machine", icons.terminal],
];

/** overview shows what the steps below set up: work comes in from chats and
 *  the panel, and an agent does it with the services it is connected to. */
function overview() {
  // A long name is cut short on a narrow screen.
  const item = (mark, label) => h("div", { class: "flow-item" }, mark, h("span", { title: label }, label));
  return h(
    "figure",
    { class: "flow" },
    h("div", { class: "flow-head" }, "Work comes from"),
    h("div", { class: "flow-head flow-head-end" }, "It works in"),
    h(
      "div",
      { class: "flow-list flow-tools" },
      flowFrom.map(([label, mark]) => item(mark(), label)),
    ),
    flowLines(flowFrom.length, false),
    h(
      "div",
      { class: "flow-gateway" },
      brandMark(40),
      h("strong", {}, "Your agent"),
      h(
        "ul",
        {},
        flowAgent.map(([label, path]) => h("li", {}, icon(path), label)),
      ),
    ),
    flowLines(flowTo.length, true),
    h(
      "div",
      { class: "flow-list flow-models" },
      flowTo.map(([label, mark]) => item(mark(), label)),
    ),
    h(
      "figcaption",
      { class: "hint" },
      "People give an agent work where they already are. It works in its own machine, with the " +
        "services you connect, and calls its models through Keera Gateway, within its guardrails " +
        "and budget.",
    ),
  );
}

/** flowLines joins n equal rows to the agent's edge. It stretches to the
 *  rows' height, so the strokes do not scale, to keep their width. */
function flowLines(n, out) {
  const height = n * 10;
  const mid = height / 2;
  return svg(
    "svg",
    {
      class: "flow-lines",
      viewBox: `0 0 100 ${height}`,
      preserveAspectRatio: "none",
      "aria-hidden": "true",
    },
    Array.from({ length: n }, (_, i) => {
      const y = i * 10 + 5;
      // The lines meet the agent close together, not in one point.
      const g = mid + (y - mid) * 0.3;
      const [a, b] = out ? [g, y] : [y, g];
      return svg("path", {
        d: `M0 ${a} C50 ${a} 50 ${b} 100 ${b}`,
        "vector-effect": "non-scaling-stroke",
      });
    }),
  );
}
