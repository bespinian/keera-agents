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
  icon,
  icons,
  until,
  dateTime,
  empty,
  plural,
  currentRange,
  rangeOf,
  rangePicker,
} from "../ui.js";
import { tasksCard, taskTotal, agentPath } from "../common.js";
import { taskRow } from "./agent.js";
import { openNewAgent, noRuntime } from "./agents.js";

export async function dashboardView(ctx) {
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
      noRuntime(ctx.state.me),
      h(
        "div",
        { class: "card" },
        empty("No agents yet", [
          h(
            "div",
            {},
            "An agent is an assistant for your organisation. It takes tasks from chats, folders, " +
              "schedules and the board, and does them with the tools you allow.",
          ),
          h(
            "button",
            { class: "btn btn-primary", style: { marginTop: "14px" }, onClick: () => openNewAgent(ctx) },
            icon(icons.plus),
            "New agent",
          ),
        ]),
      ),
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
      stat("Done", num(tasks.succeeded), period),
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
        emptyBody: "Give an agent a task, or put a card in To do.",
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
            h("span", { class: "muted nowrap", title: dateTime(u.next_run_at) }, until(u.next_run_at)),
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
