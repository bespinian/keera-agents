// Dashboard: how the organisation's agents are doing, what they did lately
// and what comes next.

import { api } from "../api.js";
import {
  h,
  stat,
  num,
  link,
  listCard,
  miniRow,
  stamp,
  plural,
  currentRange,
  rangeOf,
  rangePicker,
  listHead,
} from "../ui.js";
import { tasksCard, taskTotal, agentPath } from "../common.js";
import { taskRow } from "./agent.js";
import { noAgents, noRuntime, openNewAgent } from "./agents.js";
import { firstRun, firstRunDue } from "./firstrun.js";

export async function dashboardView(ctx) {
  // A dashboard of zeroes helps nobody, so the guide replaces it at first.
  const setup = await api.setup().catch(() => null);
  // The sidebar names this screen from it.
  if (setup) ctx.setSetup(setup);
  if (firstRunDue(setup)) {
    ctx.setTitle("Getting started", ctx.state.me.org_name || "");
    const agents = setup.agents
      ? ((await api.agents()).data || []).filter((a) => a.enabled !== false && a.can_give)
      : [];
    return h("div", { class: "grid" }, noRuntime(ctx.state.me), firstRun(ctx, setup, agents));
  }

  const since = currentRange();
  const range = rangeOf(since);
  const o = await api.overview(since);
  const agents = o.agents || {};
  const tasks = o.tasks || {};
  ctx.setSubtitle(ctx.state.me.org_name || "");

  if (!agents.total) {
    return h(
      "div",
      { class: "grid" },
      listHead(null, null, { label: "New agent", onClick: () => openNewAgent(ctx) }),
      noRuntime(ctx.state.me),
      noAgents(),
    );
  }

  const recent = o.recent_tasks || [];
  const upcoming = o.upcoming || [];
  const total = taskTotal(tasks);
  const period = range.long.toLowerCase();

  return h(
    "div",
    { class: "grid" },
    noRuntime(ctx.state.me),
    h("div", { class: "row" }, h("div", { class: "spacer" }), rangePicker(ctx, since)),
    h(
      "div",
      { class: "grid grid-4" },
      stat("Agents running", num(agents.running), agentNote(agents)),
      stat("Tasks", num(total), `${num(tasks.running)} running, ${num(tasks.queued)} queued`),
      stat("Finished", num(tasks.succeeded), period),
      stat("Failed", num(tasks.failed), period),
    ),
    tasksCard(o),
    h(
      "div",
      { class: "grid grid-2" },
      listCard({
        title: "Recent tasks",
        more: link(ctx, "/tasks", "All tasks"),
        rows: recent.map((t) => taskRow(ctx, t)),
        emptyTitle: "No tasks yet",
        emptyBody: "Give an agent a task, or put a card in To do on a squad's board.",
      }),
      listCard({
        title: "Coming up",
        more: link(ctx, "/schedules", "All schedules"),
        rows: upcoming.map((u) =>
          miniRow(
            ctx,
            agentPath(u.agent_id, "schedules"),
            u.schedule_name,
            u.agent_name,
            stamp(u.next_run_at, { ahead: true, cls: "muted nowrap" }),
          ),
        ),
        emptyTitle: "Nothing scheduled",
        emptyBody: "A schedule gives an agent the same task at set times.",
      }),
    ),
  );
}

// agentNote says how many agents there are, and those suspended or broken.
function agentNote(agents) {
  const parts = [`of ${num(agents.total)} ${plural(agents.total, "agent")}`];
  if (agents.suspended) parts.push(`${num(agents.suspended)} suspended`);
  if (agents.failed) parts.push(`${num(agents.failed)} failed`);
  return parts.join(", ");
}
