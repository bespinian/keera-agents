// Agents: the organisation's assistants. Each runs in its own machine, calls
// models through Keera Gateway, and takes work from its connectors, schedules,
// the board and this panel.

import { api } from "../api.js";
import {
  h,
  table,
  rowLink,
  field,
  formModal,
  listHead,
  stamp,
  empty,
  fail,
  dash,
  num,
  banner,
  replace,
  fold,
  checkRow,
  francs,
  microsOf,
  splitList,
  plural,
  toast,
  agentIconPicker,
  agentIconPath,
  icon,
} from "../ui.js";
import { agentStatePill, agentPath, machineFold, AGENT_STATES, BUILTINS, ACCESS } from "../common.js";

export async function agentsView(ctx) {
  const agents = (await api.agents()).data || [];
  const me = ctx.state.me;
  ctx.setSubtitle(`${agents.length} ${plural(agents.length, "agent")}`);

  return h(
    "div",
    {},
    listHead(null, [], { label: "New agent", onClick: () => openNewAgent(ctx) }),
    noRuntime(me),
    agents.length ? agentTable(ctx, agents) : noAgents(),
  );
}

function agentTable(ctx, agents) {
  return table(
    [
      {
        label: "Name",
        cell: (a) =>
          h(
            "div",
            { class: "row" },
            h("span", { class: "tile-mark tile-mark-sm" }, icon(agentIconPath(a.icon))),
            h(
              "div",
              { class: "stack" },
              rowLink(ctx, agentPath(a.id), a.name),
              h("span", { class: "faint small" }, a.model),
              a.description ? h("div", { class: "faint small clamp-2" }, a.description) : null,
            ),
          ),
        sortKey: (a) => a.name,
      },
      {
        label: "State",
        shrink: true,
        cell: (a) =>
          h(
            "div",
            { class: "stack" },
            h("div", { class: "row-tight" }, agentStatePill(a.state)),
            a.state === "failed" && a.state_detail ? h("div", { class: "faint small" }, a.state_detail) : null,
          ),
        sortKey: (a) => a.state,
      },
      {
        label: "Owner",
        shrink: true,
        cell: (a) => h("span", { class: "muted" }, a.owner_email || "-"),
        sortKey: (a) => a.owner_email,
      },
      {
        label: "Connectors",
        shrink: true,
        num: true,
        cell: (a) => (a.binding_count ? num(a.binding_count) : dash()),
        sortKey: (a) => a.binding_count || 0,
        sortDir: "desc",
      },
      {
        label: "Last active",
        shrink: true,
        num: true,
        cell: (a) => (a.last_active_at ? stamp(a.last_active_at) : dash()),
        sortKey: (a) => a.last_active_at || "",
        sortDir: "desc",
      },
    ],
    agents,
    {
      ctx,
      open: (a) => agentPath(a.id),
      search: agents.length > 6 ? (a) => `${a.name} ${a.description} ${a.model} ${a.owner_email}` : null,
      searchLabel: "agents",
      choices:
        agents.length > 6
          ? [
              {
                label: "States",
                options: Object.entries(AGENT_STATES).map(([value, [label]]) => ({ value, label })),
                test: (a, v) => a.state === v,
              },
            ]
          : [],
      sortBy: "Last active",
      sortDir: "desc",
      rowClass: (a) => (a.enabled === false ? "row-faint" : ""),
    },
  );
}

/** noAgents is what the agents list and the dashboard show before the first
 *  agent: what one is. */
export function noAgents() {
  return h(
    "div",
    { class: "card" },
    empty(
      "No agents yet",
      "An agent is an assistant for your organisation. It takes tasks from chats, folders, " +
        "schedules and the board, and does them with the tools you allow.",
    ),
  );
}

/** noRuntime warns that agents cannot start on this deployment. */
export function noRuntime(me) {
  if (me.runtime && me.runtime.driver) return null;
  return banner(
    "warn",
    "This deployment has no machine runtime set up, so agents cannot start yet. Its operator sets one up.",
  );
}

/** openNewAgent asks for a new agent and opens it once created. */
export async function openNewAgent(ctx) {
  let form;
  try {
    form = await agentForm(ctx, null);
  } catch (err) {
    toast(err.message, "bad");
    return;
  }
  formModal({
    title: "New agent",
    subtitle: "It runs in its own machine and calls models through Keera Gateway.",
    wide: true,
    body: form.el,
    submitLabel: "Create agent",
    submit: () => api.createAgent(form.values()),
    done: (agent) => {
      ctx.navigate(agentPath(agent.id));
      return `${agent.name} created`;
    },
  });
}

/** agentForm is the agent's settings, for a new agent or an existing one.
 *  values() returns the body to send, or throws { message } after focusing
 *  the field at fault. */
export async function agentForm(ctx, agent) {
  const me = ctx.state.me;
  const a = agent || {
    builtins: ["memory", "files"],
    access: "owner",
    allowed_emails: [],
  };
  let catalog = await api.catalog(a.project_id || "");

  const name = h("input", { class: "input", value: a.name || "", maxlength: "60", placeholder: "Invoice helper" });
  const iconPick = agentIconPicker(a.icon);
  const description = h("input", {
    class: "input",
    value: a.description || "",
    placeholder: "Checks incoming invoices and files them",
  });
  const instructions = h(
    "textarea",
    {
      class: "input",
      rows: 7,
      placeholder:
        "You check supplier invoices for the finance team.\n" +
        "Compare each invoice with its purchase order and flag differences.\n" +
        "Answer in the language you were asked in. Be brief.",
    },
    a.instructions || "",
  );

  const model = h("select", { class: "select" });
  function fillModels() {
    const keep = model.value || a.model;
    const models = catalog.models || [];
    const group = (kind, label) => {
      const items = models.filter((m) => m.kind === kind);
      return items.length
        ? h(
            "optgroup",
            { label },
            items.map((m) => h("option", { value: m.alias, selected: m.alias === keep }, m.alias)),
          )
        : null;
    };
    replace(
      model,
      models.length ? null : h("option", { value: "" }, "No model is allowed in this project"),
      // A model taken away since keeps showing, so saving does not change it
      // unasked.
      agent && keep === agent.model && !models.some((m) => m.alias === keep)
        ? h("option", { value: keep, selected: true }, `${keep} (no longer allowed)`)
        : null,
      group("model", "Models"),
      group("router", "Routers"),
    );
  }
  fillModels();

  const projects = catalog.projects || [];
  const project = projects.length
    ? h(
        "select",
        {
          class: "select",
          onChange: async (e) => {
            const select = e.target;
            select.disabled = true;
            try {
              catalog = await api.catalog(select.value);
              fillModels();
            } catch (err) {
              toast(err.message, "bad");
            } finally {
              select.disabled = false;
            }
          },
        },
        projects.map((p) =>
          h("option", { value: p.id, selected: p.id === (a.project_id || catalog.project_id) }, p.name),
        ),
      )
    : null;

  const tools = (me.builtins && me.builtins.length ? me.builtins : Object.keys(BUILTINS)).map((b) => {
    const [label, hint] = BUILTINS[b] || [b, ""];
    return { name: b, ...checkRow(label, hint, (a.builtins || []).includes(b)) };
  });

  const access = h(
    "select",
    { class: "select" },
    Object.entries(ACCESS).map(([value, label]) => h("option", { value, selected: value === a.access }, label)),
  );
  const emails = h(
    "textarea",
    { class: "input", rows: 3, placeholder: "anna@example.ch\nmarc@example.ch" },
    (a.allowed_emails || []).join("\n"),
  );
  const accessHint = h("div", { class: "hint" });
  const emailsField = field("People", emails, "Email addresses, one per line. Its owner may always give it work.");
  const syncAccess = () => {
    emailsField.hidden = access.value !== "people";
    accessHint.textContent = {
      owner: "Anyone else who asks it, in a chat or on the board, is told it only works for its owner.",
      org: "Anyone signed in to this organisation, in a chat it is in or on the board.",
      people: "Only the people listed, and its owner.",
    }[access.value];
  };
  access.addEventListener("change", syncAccess);
  syncAccess();

  const budget = h("input", {
    class: "input",
    inputmode: "decimal",
    value: francs(a.budget_micros),
    placeholder: "No budget of its own",
  });

  const machine = machineFold(me, a.class || {}, {
    memory: "Such as 512Mi or 2Gi.",
    disk: "Its workspace and memory. They survive restarts.",
    diskSize: "10Gi",
  });
  const maxSteps = h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "1",
    value: a.max_steps || "",
    placeholder: "No limit",
  });

  const el = h(
    "form",
    {},
    field(
      "Name",
      h("div", { class: "row-tight" }, iconPick.button, name),
      "Shown in chats and on the board. Unique in the organisation. Choose an icon to tell it apart.",
      iconPick.grid,
    ),
    field("Description", description, "Optional. One line on what it is for."),
    field(
      "Instructions",
      instructions,
      "What it does and how. It reads these before every task, like a job description.",
    ),
    h(
      "div",
      { class: project ? "field-row" : "" },
      project ? field("Project", project, "Decides which models it may use and whose budget it spends.") : null,
      field("Model", model, "What it thinks with. Pick a model that supports tool calling."),
    ),
    h(
      "div",
      { class: "field" },
      h("label", {}, "Tools"),
      h(
        "div",
        { class: "check-grid" },
        tools.map((t) => t.el),
      ),
    ),
    h(
      "div",
      { class: "field-row" },
      field("Who may give it work", access, accessHint),
      field(
        "Monthly budget in CHF",
        budget,
        "What its model calls may cost each month. Once spent, it takes no more tasks until the next month.",
      ),
    ),
    emailsField,
    machine.el,
    fold(
      "Advanced",
      field(
        "Max steps per task",
        maxSteps,
        "Each reply and tool call is a step. A task that reaches this stops. Empty means no limit; the task's time limit still applies.",
      ),
    ),
  );

  function values() {
    const n = name.value.trim();
    if (!n) fail(name, "Give the agent a name.");
    if (n.length > 60) fail(name, "A name is at most 60 characters.");
    if (!model.value) fail(model, "Choose a model. If there is none, the project allows none yet.");
    const list = splitList(emails.value, { lower: true });
    if (access.value === "people") {
      if (!list.length) fail(emails, "List at least one person, or choose another option.");
      const bad = list.find((e) => !/^[^@\s]+@[^@\s]+$/.test(e));
      if (bad) fail(emails, `${bad} is not an email address.`);
    }
    const micros = microsOf(budget.value);
    if (Number.isNaN(micros)) fail(budget, "Write the budget as an amount, such as 50 or 12.50.");
    const steps = maxSteps.value.trim() ? Number(maxSteps.value) : 0;
    if (!Number.isInteger(steps) || steps < 0) fail(maxSteps, "Steps are a whole number.");
    const body = {
      name: n,
      icon: iconPick.value(),
      description: description.value.trim(),
      instructions: instructions.value.trim(),
      model: model.value,
      builtins: tools.filter((t) => t.input.checked).map((t) => t.name),
      max_steps: steps,
      access: access.value,
      allowed_emails: access.value === "people" ? list : [],
      budget_micros: micros,
      class: machine.value(),
    };
    if (project) body.project_id = project.value;
    return body;
  }

  return { el, values };
}
