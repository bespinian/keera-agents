// Agents: the organisation's assistants. Each runs in its own machine, calls
// models through Keera Gateway, and takes work from its connectors, schedules,
// squads' boards and this panel.

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
import { agentStatePill, agentPath, machineFold, fillModels, AGENT_STATES, BUILTINS, ACCESS } from "../common.js";

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

// The list shows the agents one may give work to. Everyone may see the rest,
// so the toggle only filters; the URL keeps it for the way back.
function agentTable(ctx, agents) {
  const others = agents.some((a) => !a.can_give);
  const all = others && new URLSearchParams(location.search).get("all") === "1";
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
              a.description ? h("div", { class: "faint small clamp-2" }, a.description) : null,
            ),
          ),
        sortKey: (a) => a.name,
      },
      {
        label: "Model",
        shrink: true,
        cell: (a) => h("span", { class: "mono muted" }, a.model),
        sortKey: (a) => a.model,
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
        cell: (a) =>
          a.owner_name || a.owner_email
            ? h("span", { class: "muted", title: a.owner_email }, a.owner_name || a.owner_email)
            : dash(),
        sortKey: (a) => a.owner_name || a.owner_email,
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
      search:
        agents.length > 6 ? (a) => `${a.name} ${a.description} ${a.model} ${a.owner_name} ${a.owner_email}` : null,
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
      toggles: others
        ? [
            {
              label: "Show everyone's",
              on: all,
              hidden: (a) => !a.can_give,
              onChange: (v) => history.replaceState({}, "", "/agents" + (v ? "?all=1" : "")),
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
        "schedules and squads, and does them with the tools you allow.",
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
    lead: form.cost && (() => form.cost),
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
  // An administrator may create an agent for someone else. Without the list,
  // the form still works: the agent is theirs.
  const [cat, members] = await Promise.all([
    api.catalog(a.project_id || ""),
    !agent && me.can_admin ? api.members().catch(() => null) : null,
  ]);
  let catalog = cat;

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
  const listModels = () => fillModels(model, catalog.models || [], agent && agent.model);
  listModels();

  // Other models it may use as tools.
  const transcription = h("select", { class: "select" });
  const transcriptionHint = h("div", { class: "hint" });
  const askGrid = h("div", { class: "check-grid" });
  let asks = [];
  function fillToolModels(keepT, keepAsk) {
    const speech = (catalog.transcription_models || []).map((m) => m.alias);
    replace(
      transcription,
      h("option", { value: "" }, "None"),
      speech.map((alias) => h("option", { value: alias, selected: alias === keepT }, alias)),
      keepT && !speech.includes(keepT)
        ? h("option", { value: keepT, selected: true }, `${keepT} (no longer allowed)`)
        : null,
    );
    transcriptionHint.textContent = speech.length
      ? "Turns recordings in its folders into text, such as for meeting minutes."
      : "This project allows no transcription model.";
    asks = aliasChecks(
      askGrid,
      (catalog.models || []).map((m) => m.alias),
      keepAsk,
    );
  }
  fillToolModels(a.transcription_model || "", a.ask_models || []);
  const otherModels = fold(
    "Other models",
    field("Transcription", transcription, transcriptionHint),
    field(
      "Models it may ask",
      askGrid,
      "It sends them a question and reads the answer, such as for a second opinion or work another model does better.",
    ),
  );
  otherModels.open = Boolean(a.transcription_model || (a.ask_models || []).length);

  // The gateway's MCP servers it may call. null is all the project allows.
  const mcpAll = checkRow("All the project allows", "Also servers added to it later.", a.mcp_servers == null);
  const mcpGrid = h("div", { class: "check-grid" });
  const mcpHint = h("div", { class: "hint" });
  let servers = [];
  function fillServers(keep) {
    const allowed = (catalog.mcp_servers || []).map((m) => m.alias);
    servers = aliasChecks(mcpGrid, allowed, keep);
    mcpHint.textContent = allowed.length
      ? "It calls their tools through Keera Gateway, which enforces this list."
      : "This project allows no MCP server.";
  }
  fillServers(a.mcp_servers || []);
  const syncServers = () => {
    mcpGrid.hidden = mcpAll.input.checked;
    // Choosing starts from what it had, so unticking one is enough.
    if (!mcpAll.input.checked && !ticked(servers).length) servers.forEach((x) => (x.input.checked = true));
  };
  mcpAll.input.addEventListener("change", syncServers);
  mcpGrid.hidden = mcpAll.input.checked;
  const mcp = fold("MCP servers", field("MCP servers it may call", h("div", {}, mcpAll.el, mcpGrid), mcpHint));
  mcp.open = a.mcp_servers != null;

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
              listModels();
              fillToolModels(transcription.value, ticked(asks));
              fillServers(ticked(servers));
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

  const people = (members && members.data) || [];
  const owner =
    people.length > 1
      ? h(
          "select",
          { class: "select" },
          people.map((m) =>
            h("option", { value: m.id, selected: m.id === me.user_id }, m.name ? `${m.name} (${m.email})` : m.email),
          ),
        )
      : null;

  // Two to a row, in this order, whichever there are.
  const placed = [
    project && field("Project", project, "Decides which models it may use and whose budget it spends."),
    owner && field("Owner", owner, "It works for them and spends in their name."),
    field("Model", model, "What it thinks with. Pick a model that supports tool calling."),
  ].filter(Boolean);
  const rows = [];
  for (let i = 0; i < placed.length; i += 2) {
    const pair = placed.slice(i, i + 2);
    rows.push(pair.length > 1 ? h("div", { class: "field-row" }, pair) : pair[0]);
  }

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
      owner: "Anyone else who asks it, in a chat or on a squad's board, is told it only works for its owner.",
      org: "Anyone signed in to this organisation, in a chat it is in or on a squad's board.",
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
    disk: "Its workspace and memory. They survive restarts. Fixed once the agent exists.",
    diskFixed: Boolean(agent),
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
      "Shown in chats and on squads' boards. Unique in the organisation. Choose an icon to tell it apart.",
      iconPick.grid,
    ),
    field("Description", description, "Optional. One line on what it is for."),
    rows,
    field(
      "Instructions",
      instructions,
      "What it does and how. It reads these before every task, like a job description.",
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
    mcp,
    otherModels,
    machine.el,
    fold(
      "Advanced",
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
    if (!model.value) fail(model, "Choose a model. If there is none, the project allows none that calls tools yet.");
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
      transcription_model: transcription.value,
      ask_models: ticked(asks),
      mcp_servers: mcpAll.input.checked ? null : ticked(servers),
      max_steps: steps,
      access: access.value,
      allowed_emails: access.value === "people" ? list : [],
      budget_micros: micros,
      class: machine.value(),
    };
    if (project) body.project_id = project.value;
    if (owner) body.owner_id = owner.value;
    return body;
  }

  return { el, values, cost: machine.cost };
}

/** aliasChecks fills grid with a checkbox for each allowed alias, ticked if
 *  in keep. One in keep taken away since stays, marked, so saving does not
 *  drop it unasked. */
function aliasChecks(grid, allowed, keep) {
  const gone = keep.filter((alias) => !allowed.includes(alias));
  const rows = [...allowed, ...gone].map((alias) => ({
    name: alias,
    ...checkRow(gone.includes(alias) ? `${alias} (no longer allowed)` : alias, "", keep.includes(alias)),
  }));
  replace(
    grid,
    rows.map((x) => x.el),
  );
  return rows;
}

const ticked = (rows) => rows.filter((x) => x.input.checked).map((x) => x.name);
