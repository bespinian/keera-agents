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
  date,
  dateTime,
  money,
  fact,
  icon,
  icons,
  busyButton,
  detailHead,
  gone,
  tabs,
  replace,
  banner,
  confirm,
  toast,
  field,
  checkRow,
  link,
  card,
  loading,
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
  TIERS,
  taskStatePill,
  firstLine,
  sourceCell,
  tasksCard,
  taskTotal,
  agentPath,
  taskPath,
} from "../common.js";
import { agentForm, noRuntime } from "./agents.js";
import { channelsTab } from "./channels.js";
import { scheduleTable, openScheduleEditor } from "./schedules.js";
import { taskList, giveTask } from "./tasks.js";
import { logPane } from "./logs.js";

const TABS = [
  { value: "overview", label: "Overview" },
  { value: "settings", label: "Settings" },
  { value: "channels", label: "Channels" },
  { value: "schedules", label: "Schedules" },
  { value: "tasks", label: "Tasks" },
  { value: "logs", label: "Logs" },
];

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
  let current = TABS.some((t) => t.value === ctx.sub) ? ctx.sub : "overview";

  // A machine on its way up is watched, so the state does not go stale.
  if (agent.state === "starting") {
    every(ctx, 4000, async () => {
      const fresh = await api.agent(agent.id);
      if (fresh.state !== "starting") ctx.reload();
    });
  }

  // Each tab may leave something running, such as the log stream.
  let tabTeardown = [];
  const endTab = () => {
    for (const fn of tabTeardown.splice(0)) fn();
  };
  ctx.onTeardown(endTab);
  const tabCtx = { ...ctx, onTeardown: (fn) => tabTeardown.push(fn) };

  const panel = h("div", { class: "grid" });
  async function show(tab) {
    endTab();
    current = tab;
    // The URL names the tab, so a reload or a shared link opens it.
    const path = agentPath(agent.id, tab === "overview" ? "" : tab);
    if (location.pathname !== path) history.replaceState({}, "", path);
    replace(panel, loading("160px"));
    try {
      const body = await TAB_VIEWS[tab](tabCtx, agent);
      if (current === tab && ctx.current()) replace(panel, body);
    } catch (err) {
      if (current === tab) replace(panel, banner("bad", err.message));
    }
  }

  const strip = tabs(
    TABS.map((t) => ({
      ...t,
      count:
        t.value === "channels"
          ? (agent.bindings || []).length
          : t.value === "schedules"
            ? (agent.schedules || []).length
            : 0,
    })),
    current,
    show,
  );
  tabCtx.showTab = (tab) => {
    strip.select(tab);
    show(tab);
  };
  show(current);

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
        { class: "btn btn-primary", title: "Ask it to do something now", onClick: () => giveTask(ctx, { agent }) },
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
      agent.enabled === false ? pill("Off: takes no new tasks") : null,
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

const TAB_VIEWS = {
  overview: overviewTab,
  settings: settingsTab,
  channels: (ctx, agent) => channelsTab(ctx, agent),
  schedules: schedulesTab,
  tasks: (ctx) => h("div", {}, taskList(ctx, { agentID: ctx.param })),
  logs: (ctx, agent) => logPane(ctx, agent),
};

async function overviewTab(ctx, agent) {
  const stats = agent.stats || {};
  const since = currentRange();
  const period = rangeOf(since).long.toLowerCase();
  const [recentRes, act] = await Promise.all([
    api.tasks({ agent_id: agent.id, limit: 8 }).catch(() => ({ data: [] })),
    api.agentActivity(agent.id, since),
  ]);
  const recent = recentRes.data || [];
  const tasks = act.tasks || {};
  const cls = agent.class || {};
  const machine = [
    TIERS[cls.isolation] || cls.isolation,
    cls.cpu && `${cls.cpu} CPU`,
    cls.memory,
    cls.disk && `${cls.disk} disk`,
  ]
    .filter(Boolean)
    .join(" · ");

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
        stats.last_task_at ? ago(stats.last_task_at) : "-",
        stats.last_task_at ? dateTime(stats.last_task_at) : "none yet",
      ),
    ),
    tasksCard(act),
    h(
      "div",
      { class: "grid grid-2" },
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
          fact(
            "Takes work from",
            agent.access === "people"
              ? `${ACCESS.people}: ${(agent.allowed_emails || []).join(", ")}`
              : ACCESS[agent.access] || agent.access,
          ),
          fact("Budget", agent.budget_micros ? `${money(agent.budget_micros)} a month` : "None of its own"),
          fact("Machine", machine),
          fact("Owner", agent.owner_email),
          fact("Created", date(agent.created_at)),
        ],
        { style: { alignSelf: "start" }, bodyClass: "card-body facts" },
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
        emptyBody: "Give it one with Give a task.",
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
    [sourceCell(t), h("span", {}, "·"), h("span", { class: "nowrap" }, ago(t.created_at))],
    taskStatePill(t.state),
  );
}

async function settingsTab(ctx, agent) {
  if (!agent.can_edit) {
    return banner("info", "Only its owner or an administrator can change this agent.");
  }
  const form = await agentForm(ctx, agent);
  const enabled = checkRow(
    "Takes new tasks",
    "When off, it finishes what it is doing and takes nothing new. Its channels and schedules stay.",
    agent.enabled !== false,
  );
  const err = h("div");
  const save = busyButton({ class: "btn btn-primary" }, "Save changes", async () => {
    replace(err);
    try {
      const body = { ...form.values(), enabled: enabled.input.checked };
      await api.updateAgent(agent.id, body);
    } catch (ex) {
      replace(err, banner("bad", ex.message || "That did not work."));
      return;
    }
    toast("Changes saved. A running agent picks them up with its next task.", "good");
    ctx.reload();
  });
  // Enter in a field saves, as in the dialogs.
  form.el.addEventListener("submit", (e) => {
    e.preventDefault();
    save.click();
  });

  // A form as wide as the screen is hard to read down.
  return h(
    "div",
    { class: "grid", style: { maxWidth: "820px" } },
    h(
      "div",
      { class: "card" },
      h(
        "div",
        { class: "card-body" },
        err,
        h("div", { class: "field" }, enabled.el),
        form.el,
        h("div", { class: "row", style: { marginTop: "16px" } }, h("div", { class: "spacer" }), save),
      ),
    ),
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
                body: "Its machine, files, memory, channels and schedules go with it. This cannot be undone.",
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
