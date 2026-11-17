// Tasks: every piece of work an agent was given, from a chat, a schedule, a
// card, a folder or this panel, and what came of it.

import { api } from "../api.js";
import {
  h,
  table,
  rowLink,
  stamp,
  card,
  dateTime,
  dash,
  fact,
  banner,
  busyButton,
  detailHead,
  gone,
  empty,
  icon,
  icons,
  replace,
  toast,
  clock,
  loadMore,
  link,
  every,
  fold,
  avatarOf,
  plural,
  formModal,
  fail,
  field,
  listHead,
} from "../ui.js";
import { render } from "../markdown.js";
import {
  taskStatePill,
  sourceCell,
  took,
  isLive,
  firstLine,
  agentPath,
  taskPath,
  SOURCES,
  TASK_STATES,
  isUp,
} from "../common.js";

const PAGE = 50;

export async function tasksView(ctx) {
  const agents = ((await api.agents()).data || []).filter((a) => a.enabled !== false && a.can_give);
  return taskList(ctx, { agents });
}

/** taskList is the filtered, paged list. With agentID it shows one agent's
 *  tasks and no agent filter. It offers only the options some task has, and
 *  a filter only when it has more than one. With agents, those that take
 *  work from the reader, it has a New task button. */
export function taskList(ctx, { agentID = "", agents = [] }) {
  const filter = { agent_id: agentID, state: "", source: "" };
  const host = h("div");
  const toolbar = h("div", { class: "toolbar", hidden: true });
  // The same place as the other screens' New buttons.
  const head = agents.length
    ? listHead(null, [], { label: "New task", onClick: () => giveTask(ctx, { agents, pick: filter.agent_id }) })
    : null;
  // Stops the poll that follows running tasks; null while none is.
  let stopPoll = null;

  const select = (label, key, options) =>
    options.length > 1
      ? h(
          "select",
          {
            class: "select toolbar-select",
            "aria-label": label,
            onChange: (e) => {
              filter[key] = e.target.value;
              load();
            },
          },
          h("option", { value: "" }, `All ${label.toLowerCase()}`),
          options.map(([value, text]) => h("option", { value }, text)),
        )
      : null;

  async function filters() {
    const f = await api.taskFacets({ agent_id: agentID }).catch(() => null);
    if (!f || !ctx.current()) return;
    const known = (all, used) => Object.entries(all).filter(([k]) => used.includes(k));
    replace(
      toolbar,
      agentID
        ? null
        : select(
            "Agents",
            "agent_id",
            f.agents.map((a) => [a.id, a.name || a.id]).sort((a, b) => a[1].localeCompare(b[1])),
          ),
      select(
        "States",
        "state",
        known(TASK_STATES, f.states).map(([k, [label]]) => [k, label]),
      ),
      select(
        "Sources",
        "source",
        known(SOURCES, f.sources).map(([k, s]) => [k, s.label]),
      ),
    );
    toolbar.hidden = !toolbar.childElementCount;
  }

  async function load() {
    let rows;
    try {
      rows = (await api.tasks({ ...filter, limit: PAGE })).data || [];
    } catch (err) {
      replace(host, banner("bad", err.message));
      return;
    }
    if (!ctx.current()) return;
    const el = taskTable(ctx, rows, {
      showAgent: !agentID,
      emptyTitle: filter.state || filter.source || filter.agent_id ? "No tasks match" : "No tasks yet",
      emptyBody: agentID
        ? "Give it a task, add a connector, or put a card in To do with this agent."
        : "Tasks come from chats, schedules, the board and folders, or from New task.",
    });
    replace(
      host,
      el,
      rows.length === PAGE
        ? loadMore(
            PAGE,
            async () => {
              const more =
                (await api.tasks({ ...filter, limit: PAGE, before: rows[rows.length - 1].created_at })).data || [];
              rows.push(...more);
              el.redraw();
              return more.length;
            },
            "That is all of them.",
          )
        : null,
    );
    // While something runs, the list follows it.
    const live = rows.some((t) => isLive(t.state));
    if (live && !stopPoll) stopPoll = every(ctx, 10000, load);
    if (!live && stopPoll) {
      stopPoll();
      stopPoll = null;
    }
  }
  filters();
  load();
  return h("div", {}, head, toolbar, host);
}

/** giveTask asks an agent to do something now, then opens the task. It is
 *  given the agent, or the agents to choose from, with pick chosen first. */
export function giveTask(ctx, { agent, agents = [], pick = "" }) {
  const choose = agent
    ? null
    : h(
        "select",
        { class: "select" },
        [...agents]
          .sort((a, b) => a.name.localeCompare(b.name))
          .map((a) => h("option", { value: a.id, selected: a.id === pick }, a.name)),
      );
  const chosen = () => agent || agents.find((a) => a.id === choose.value);
  const starts = "Its machine starts if it is stopped.";
  const hint = h("div", { class: "hint" });
  const showHint = () => {
    hint.textContent = isUp(chosen()?.state) ? "" : starts;
    hint.hidden = !hint.textContent;
  };
  const prompt = h("textarea", {
    class: "input",
    rows: 6,
    placeholder: "Draft a reply to the latest message from Müller AG and put it in /Drafts.",
  });
  if (choose) {
    choose.addEventListener("change", showHint);
    showHint();
  }
  formModal({
    title: agent ? `Give ${agent.name} a task` : "New task",
    subtitle: agent && !isUp(agent.state) ? starts : null,
    wide: true,
    body: h(
      "form",
      {},
      choose ? field("Agent", choose, hint) : null,
      field("Task", prompt, "Ctrl+Enter gives it. You can follow it on the task's page."),
    ),
    submitLabel: "Give task",
    submit: () => {
      if (choose && !chosen()) fail(choose, "Pick an agent.");
      if (!prompt.value.trim()) fail(prompt, "Say what it should do.");
      return api.giveTask(chosen().id, prompt.value.trim());
    },
    done: (task) => {
      ctx.navigate(taskPath(task.id));
      return "Task given";
    },
  });
  // A textarea takes Enter for new lines, so Ctrl+Enter sends.
  prompt.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      document.querySelector(".modal-foot .btn-primary")?.click();
    }
  });
}

/** taskTable lists tasks, each opening its own screen. */
function taskTable(ctx, rows, { showAgent = true, emptyTitle, emptyBody } = {}) {
  return table(
    [
      {
        label: "Task",
        cell: (t) =>
          h(
            "div",
            { class: "stack" },
            rowLink(ctx, taskPath(t.id), firstLine(t.prompt, 90) || "Untitled task"),
            t.error ? h("div", { class: "faint small clamp-2" }, t.error) : null,
          ),
      },
      { label: "Source", shrink: true, cell: (t) => sourceCell(t) },
      showAgent
        ? {
            label: "Agent",
            shrink: true,
            cell: (t) => (t.agent_id ? agentLink(ctx, t) : dash()),
          }
        : null,
      {
        label: "Requested by",
        shrink: true,
        cell: (t) => (t.requested_by ? h("span", { class: "muted" }, t.requested_by) : dash()),
      },
      { label: "State", shrink: true, cell: (t) => taskStatePill(t.state) },
      {
        label: "Created",
        shrink: true,
        num: true,
        cell: (t) => stamp(t.created_at),
      },
      { label: "Took", shrink: true, num: true, cell: (t) => took(t) || dash() },
    ].filter(Boolean),
    rows,
    {
      ctx,
      open: (t) => taskPath(t.id),
      emptyTitle,
      emptyBody,
    },
  );
}

/** agentLink names a task's agent, with its icon, as a link to it. */
function agentLink(ctx, task) {
  return link(
    ctx,
    agentPath(task.agent_id),
    [avatarOf(task.agent_name, { agent: true, icon: task.agent_icon }), h("span", {}, task.agent_name)],
    { class: "who" },
  );
}

/** taskView is one task: what was asked, what came of it, and each step on
 *  the way, followed while it runs. */
export async function taskView(ctx) {
  let task;
  try {
    task = await api.task(ctx.param);
  } catch (err) {
    if (err.status === 404) {
      return gone(ctx, "/tasks", "Tasks", "No such task", "It was deleted, or it belongs to another organisation.");
    }
    throw err;
  }

  const page = h("div", { class: "grid" });
  const events = task.events || [];
  const timeline = h("div", { class: "timeline" });

  function paint() {
    ctx.setTitle(
      firstLine(task.prompt, 60) || "Task",
      [task.agent_name, task.origin_label].filter(Boolean).join(" · "),
    );
    replace(
      page,
      detailHead(
        ctx,
        ["/tasks", "Tasks"],
        [taskStatePill(task.state), sourceCell(task)],
        h(
          "div",
          { class: "row-tight" },
          isLive(task.state)
            ? busyButton({ class: "btn btn-danger", title: "Stop this task" }, "Cancel task", async () => {
                task = { ...task, ...(await api.cancelTask(task.id)) };
                toast("Task cancelled", "good");
                paint();
              })
            : null,
        ),
      ),
      h(
        "div",
        { class: "grid grid-2" },
        h(
          "div",
          { class: "grid", style: { alignContent: "start" } },
          card("Prompt", [
            h("div", { class: "prompt-text" }, task.prompt || ""),
            task.context
              ? fold("Context the agent was given", h("div", { class: "prompt-text mono" }, task.context))
              : null,
          ]),
          resultCard(task),
        ),
        factsCard(ctx, task),
      ),
      card(
        "Steps",
        events.length || isLive(task.state)
          ? timeline
          : empty("No steps recorded", "The agent left no messages or tool calls for this task."),
        {
          more: isLive(task.state)
            ? h("span", { class: "faint small row-tight" }, h("span", { class: "blip" }), "following")
            : h("span", { class: "faint small" }, `${events.length} ${plural(events.length, "step")}`),
          bodyClass: null,
        },
      ),
    );
  }

  replace(timeline, events.map(eventItem));
  if (isLive(task.state) && !events.length) {
    timeline.append(h("div", { class: "empty" }, "Waiting for the agent…"));
  }

  paint();
  if (isLive(task.state)) {
    const stop = every(ctx, 2000, async () => {
      const last = events[events.length - 1];
      const res = await api.taskEvents(task.id, last ? last.id : "");
      const fresh = res.events || [];
      if (fresh.length) {
        if (!events.length) timeline.replaceChildren();
        events.push(...fresh);
        timeline.append(...fresh.map(eventItem));
      }
      if (res.task && res.task.state !== task.state) {
        // The answer holds the whole task, with its result and times.
        task = { ...res.task, events };
        paint();
      }
      if (!isLive(task.state)) stop();
    });
  }
  return page;
}

function resultCard(task) {
  let body;
  if (task.result) body = render(task.result);
  else if (task.state === "queued") body = h("div", { class: "muted" }, "Waiting for the agent to start.");
  else if (task.state === "running")
    body = h("div", { class: "muted row" }, h("span", { class: "blip" }), "The agent is working on it.");
  else body = h("div", { class: "faint" }, "No result.");
  return card("Result", [task.error ? banner("bad", task.error) : null, body]);
}

function factsCard(ctx, task) {
  return card(
    "Details",
    [
      fact("Agent", task.agent_id ? agentLink(ctx, task) : null),
      fact("From", sourceCell(task)),
      fact("Requested by", task.requested_by),
      fact("Created", dateTime(task.created_at)),
      fact("Started", task.started_at ? dateTime(task.started_at) : null),
      fact("Finished", task.finished_at ? dateTime(task.finished_at) : null),
      fact("Took", took(task)),
      task.source === "chat"
        ? fact("Reply", task.delivered ? "Sent to the chat" : isLive(task.state) ? "Not yet" : "Not sent")
        : null,
    ],
    { style: { alignSelf: "start" }, bodyClass: "card-body facts" },
  );
}

const EVENT_ICONS = {
  message: icons.chat,
  tool_call: icons.tool,
  tool_result: icons.right,
  log: icons.terminal,
};

/** eventItem is one step. Tool calls and results start folded: they are
 *  long, and the messages tell the story. */
function eventItem(e) {
  let body;
  const text = String(e.text || "");
  if (e.kind === "message") body = render(text);
  else if (e.kind === "tool_call" || e.kind === "tool_result") {
    body = h(
      "details",
      { class: "tl-details" },
      h("summary", {}, h("strong", {}, e.kind === "tool_call" ? "Tool call" : "Result"), " ", firstLine(text, 140)),
      h("pre", {}, text),
    );
  } else body = h("div", { class: "tl-logline" }, text);
  return h(
    "div",
    { class: `tl-item tl-${e.kind}` },
    h("span", { class: "tl-icon", "aria-hidden": "true" }, icon(EVENT_ICONS[e.kind] || icons.more)),
    h("div", { class: "tl-body" }, body),
    h("span", { class: "tl-at", title: dateTime(e.at) }, clock(e.at)),
  );
}
