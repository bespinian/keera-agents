// One agent: its state and controls, and a tab each for what it is, how it
// is set up, where it takes work, when it runs, what it did and what it
// printed.

import { api } from "../api.js";
import {
  h,
  pill,
  stat,
  num,
  ago,
  stamp,
  dash,
  date,
  dateTime,
  money,
  fact,
  icon,
  icons,
  busyButton,
  detailHead,
  gone,
  banner,
  confirm,
  toast,
  field,
  link,
  card,
  listCard,
  miniRow,
  every,
  isAdmin,
  plural,
  sectionHead,
  avatarOf,
  currentRange,
  rangeOf,
  rangePicker,
} from "../ui.js";
import {
  agentStatePill,
  isUp,
  BUILTINS,
  ACCESS,
  taskStatePill,
  firstLine,
  sourceCell,
  tasksCard,
  taskTotal,
  agentPath,
  taskPath,
  squadPath,
  detailTabs,
  settingsCard,
  machineText,
} from "../common.js";
import { agentForm, noRuntime } from "./agents.js";
import { bindingsTab } from "./bindings.js";
import { scheduleTable, openScheduleEditor } from "./schedules.js";
import { taskList, giveTask } from "./tasks.js";
import { logPane } from "./logs.js";
import { agentCostCard } from "./billing.js";

export async function agentView(ctx) {
  let agent;
  try {
    agent = await api.agent(ctx.param);
  } catch (err) {
    if (err.status === 404) {
      return gone(ctx, "/agents", "Agents", "No such agent", "It was deleted, or it belongs to another organisation.");
    }
    throw err;
  }
  ctx.setTitle(
    agent.name,
    agent.description || "",
    avatarOf(agent.name, { agent: true, icon: agent.icon, large: true }),
  );

  // A machine on its way up is watched, so the state does not go stale.
  if (agent.state === "starting") {
    every(ctx, 4000, async () => {
      const fresh = await api.agent(agent.id);
      if (fresh.state !== "starting") ctx.reload();
    });
  }

  const { strip, panel } = detailTabs(
    ctx,
    [
      { value: "overview", label: "Overview", view: overviewTab },
      { value: "settings", label: "Settings", view: settingsTab },
      { value: "connectors", label: "Connectors", count: (agent.bindings || []).length, view: bindingsTab },
      { value: "schedules", label: "Schedules", count: (agent.schedules || []).length, view: schedulesTab },
      { value: "tasks", label: "Tasks", view: (ctx) => h("div", {}, taskList(ctx, { agentID: ctx.param })) },
      { value: "logs", label: "Logs", view: logPane },
    ],
    (tab) => agentPath(agent.id, tab),
    agent,
  );

  return h("div", { class: "grid" }, head(ctx, agent), noRuntime(ctx.state.me), strip, panel);
}

function head(ctx, agent) {
  const act = (action, done) => async () => {
    await api.agentAction(agent.id, action);
    toast(`${agent.name} ${done}`, "good");
    ctx.reload();
  };
  const controls = [];
  if (agent.can_edit) {
    if (isUp(agent.state)) {
      controls.push(
        busyButton(
          { title: "Stop its machine. Its files and memory are kept." },
          [icon(icons.stop), "Stop"],
          act("stop", "is stopping"),
        ),
        busyButton(
          { title: "Stop its machine and start it again" },
          [icon(icons.restart), "Restart"],
          act("restart", "is restarting"),
        ),
      );
    } else {
      controls.push(
        busyButton({ title: "Start its machine" }, [icon(icons.play), "Start"], act("start", "is starting")),
      );
    }
  }
  // Keera Chat lists only agents that take work from the person.
  const chatURL = ctx.state.me.chat_url;
  if (chatURL && agent.enabled !== false && agent.can_give) {
    controls.push(
      h(
        "a",
        {
          class: "btn",
          href: `${chatURL}/?agent=${encodeURIComponent(agent.id)}`,
          target: "_blank",
          rel: "noopener",
          title: `Start a chat with ${agent.name} in Keera Chat`,
        },
        icon(icons.chat),
        "Chat",
      ),
    );
  }
  if (agent.enabled !== false && agent.can_give) {
    controls.push(
      h(
        "button",
        { class: "btn", title: "Ask it to do something now", onClick: () => giveTask(ctx, { agent }) },
        icon(icons.send),
        "Give a task",
      ),
    );
  }

  return detailHead(
    ctx,
    ["/agents", "Agents"],
    [
      agentStatePill(agent.state),
      agent.running_task_id
        ? link(ctx, taskPath(agent.running_task_id), [h("span", { class: "blip" }), "Working on a task"], {
            class: "pill pill-accent pill-button",
          })
        : null,
      agent.queued ? pill(`${num(agent.queued)} queued`) : null,
      agent.state_detail
        ? h("span", { class: "faint small", style: { alignSelf: "center" } }, agent.state_detail)
        : null,
    ],
    h("div", { class: "row-tight", style: { flexWrap: "wrap" } }, controls),
  );
}

async function overviewTab(ctx, agent) {
  const stats = agent.stats || {};
  const since = currentRange();
  const period = rangeOf(since).long.toLowerCase();
  // What its machine costs, for those who can change its size.
  const showCost = ctx.state.me.billing && agent.can_edit;
  const [recentRes, act, cost, squadsRes] = await Promise.all([
    api.tasks({ agent_id: agent.id, limit: 8 }).catch(() => ({ data: [] })),
    api.agentActivity(agent.id, since),
    showCost ? api.agentCost(agent.id).catch(() => null) : null,
    api.squads().catch(() => ({ data: [] })),
  ]);
  const squads = (squadsRes.data || []).filter((q) => q.members.some((m) => m.id === agent.id));
  const recent = recentRes.data || [];
  const tasks = act.tasks || {};

  return h(
    "div",
    { class: "grid" },
    h("div", { class: "row" }, h("div", { class: "spacer" }), rangePicker(ctx, since)),
    h(
      "div",
      { class: "grid grid-4" },
      stat("Tasks", num(taskTotal(tasks)), period),
      stat("Failed", num(tasks.failed), period),
      stat("Queued", num(agent.queued), agent.running_task_id ? "and one running" : "none running"),
      stat(
        "Last task",
        stats.last_task_at ? ago(stats.last_task_at) : dash(),
        stats.last_task_at ? dateTime(stats.last_task_at) : "none yet",
      ),
    ),
    tasksCard(act),
    h(
      "div",
      { class: "grid grid-2" },
      h(
        "div",
        { class: "grid", style: { alignSelf: "start" } },
        card(
          "Setup",
          [
            fact("Model", h("span", { class: "mono" }, agent.model)),
            fact(
              "Tools",
              (agent.builtins || []).length
                ? h(
                    "div",
                    { class: "wrap-chips" },
                    agent.builtins.map((b) => pill((BUILTINS[b] || [b])[0])),
                  )
                : h("span", { class: "faint" }, "None"),
            ),
            agent.transcription_model || (agent.ask_models || []).length
              ? fact(
                  "Other models",
                  h(
                    "div",
                    { class: "wrap-chips" },
                    agent.transcription_model ? pill(`${agent.transcription_model}, transcribes`) : null,
                    (agent.ask_models || []).map((m) => pill(`${m}, to ask`)),
                  ),
                )
              : null,
            agent.mcp_servers
              ? fact(
                  "MCP servers",
                  agent.mcp_servers.length
                    ? h(
                        "div",
                        { class: "wrap-chips" },
                        agent.mcp_servers.map((m) => pill(m)),
                      )
                    : h("span", { class: "faint" }, "None"),
                )
              : null,
            fact(
              "Takes work from",
              agent.access === "people"
                ? `${ACCESS.people}: ${(agent.allowed_emails || []).join(", ")}`
                : ACCESS[agent.access] || agent.access,
            ),
            fact(
              "Squads",
              squads.length
                ? h(
                    "div",
                    { class: "wrap-chips" },
                    squads.map((q) =>
                      link(
                        ctx,
                        squadPath(q.id),
                        [h("span", {}, q.name), q.lead_id === agent.id ? pill("Lead", "accent") : null],
                        { class: "who" },
                      ),
                    ),
                  )
                : h("span", { class: "faint" }, "None"),
            ),
            fact("Budget", agent.budget_micros ? `${money(agent.budget_micros)} a month` : "None of its own"),
            fact("Machine", machineText(agent.class)),
            fact("Owner", agent.owner_email),
            fact("Created", date(agent.created_at)),
          ],
          { bodyClass: "card-body facts" },
        ),
        cost ? agentCostCard(ctx, cost) : null,
      ),
      listCard({
        title: "Recent tasks",
        more: h(
          "a",
          {
            href: agentPath(agent.id, "tasks"),
            onClick: (e) => {
              e.preventDefault();
              ctx.showTab("tasks");
            },
          },
          "All tasks",
        ),
        rows: recent.map((t) => taskRow(ctx, t)),
        emptyTitle: "No tasks yet",
        emptyBody: "Give it one with Give a task, above.",
      }),
    ),
    agent.instructions ? card("Instructions", h("div", { class: "prompt-text" }, agent.instructions)) : null,
  );
}

/** taskRow is a task in a short list: its prompt, source and state. */
export function taskRow(ctx, t) {
  return miniRow(
    ctx,
    taskPath(t.id),
    firstLine(t.prompt, 90) || "Untitled task",
    [sourceCell(t), h("span", {}, "·"), stamp(t.created_at, { cls: "nowrap" })],
    taskStatePill(t.state),
  );
}

async function settingsTab(ctx, agent) {
  if (!agent.can_edit) {
    return banner("info", "Only its owner or an administrator can change this agent.");
  }
  const form = await agentForm(ctx, agent);
  const settings = settingsCard(ctx, form.el, async () => {
    await api.updateAgent(agent.id, form.values());
    return "Changes saved. A running agent picks them up with its next task.";
  });

  // A form as wide as the screen is hard to read down.
  return h(
    "div",
    { class: "grid", style: { maxWidth: "820px" } },
    settings,
    takeOverCard(ctx, agent),
    h(
      "div",
      { class: "card" },
      h(
        "div",
        { class: "card-body row", style: { flexWrap: "wrap" } },
        h(
          "div",
          { class: "stack", style: { flex: "1 1 280px" } },
          h("strong", {}, "Delete this agent"),
          h(
            "span",
            { class: "muted small" },
            "Stops its machine, deletes its files and memory, and revokes its key. Its tasks stay in Tasks.",
          ),
        ),
        h(
          "button",
          {
            class: "btn btn-danger",
            onClick: () =>
              confirm({
                title: `Delete ${agent.name}?`,
                body: "Its machine, files, memory, connectors and schedules go with it. This cannot be undone.",
                confirmLabel: "Delete agent",
                danger: true,
                onConfirm: () => api.deleteAgent(agent.id),
                done: () => {
                  ctx.navigate("/agents");
                  return `${agent.name} deleted`;
                },
              }),
          },
          icon(icons.trash),
          "Delete agent",
        ),
      ),
    ),
  );
}

/** takeOverCard lets an administrator become the owner, as when the owner
 *  left and the agent was stopped. Null for anyone else. */
function takeOverCard(ctx, agent) {
  if (!isAdmin(ctx) || agent.owner_id === ctx.state.me.user_id) return null;
  return h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "card-body row", style: { flexWrap: "wrap" } },
      h(
        "div",
        { class: "stack", style: { flex: "1 1 280px" } },
        h("strong", {}, "Take over this agent"),
        h(
          "span",
          { class: "muted small" },
          `Its owner is ${agent.owner_email}. Take it over when they left or can no longer look after it.`,
        ),
      ),
      h(
        "button",
        {
          class: "btn",
          onClick: () =>
            confirm({
              title: `Take over ${agent.name}?`,
              body:
                "You become its owner, and it gets a new gateway key in your name. " +
                "What it spends and does from now on counts as yours.",
              confirmLabel: "Take over",
              onConfirm: () => api.takeOverAgent(agent.id),
              done: `${agent.name} is yours now`,
              ctx,
            }),
        },
        icon(icons.person),
        "Take over",
      ),
    ),
  );
}

function schedulesTab(ctx, agent) {
  const schedules = agent.schedules || [];
  return h(
    "div",
    { class: "stack", style: { gap: "12px" } },
    sectionHead(
      `${schedules.length} ${plural(schedules.length, "schedule")}`,
      agent.can_edit
        ? h(
            "button",
            {
              class: "btn btn-primary",
              onClick: () => openScheduleEditor(ctx, { agent, onSaved: ctx.reload }),
            },
            icon(icons.plus),
            "New schedule",
          )
        : null,
      { margin: "0" },
    ),
    scheduleTable(ctx, schedules, { agent, onChange: ctx.reload }),
  );
}
