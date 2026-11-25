// Organisations, for an operator only, on their own screen as on Keera
// Gateway. They are created and deleted there; here an operator sees how each
// one's agents are doing and opens it. The dashboard sums them up while the
// operator views every organisation.

import { api } from "../api.js";
import { h, table, stat, num, plural, pill, date, stamp, dash, empty, listHead, link } from "../ui.js";

/** orgsDashboard is the dashboard while an operator views every
 *  organisation: the totals, with the list one click away. */
export async function orgsDashboard(ctx) {
  const orgs = (await api.orgs()).data || [];
  ctx.setSubtitle("All organisations");

  const sum = (key) => orgs.reduce((n, o) => n + (o[key] || 0), 0);
  const failed = sum("agents_failed");

  return h(
    "div",
    { class: "grid" },
    listHead(
      "Every organisation's agents, workspaces and tasks of the last 7 days.",
      link(ctx, "/organisations", "See each organisation", { class: "btn" }),
    ),
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Organisations",
        num(orgs.length),
        `${num(orgs.filter((o) => o.agents || o.workspaces).length)} with agents or workspaces`,
      ),
      stat(
        "Agents running",
        num(sum("agents_running")),
        `of ${num(sum("agents"))}` + (failed ? `, ${num(failed)} failed` : ""),
      ),
      stat("Workspaces running", num(sum("workspaces_running")), `of ${num(sum("workspaces"))}`),
      stat("Tasks", num(sum("tasks")), `last 7 days, ${num(sum("tasks_failed"))} failed`),
    ),
  );
}

export async function orgsView(ctx) {
  const orgs = (await api.orgs()).data || [];
  ctx.setSubtitle(`${orgs.length} ${plural(orgs.length, "organisation")}`);

  const gateway = ctx.state.me.gateway_url;

  const open = async (o) => {
    await ctx.setOrg(o.id);
    ctx.navigate("/");
  };

  return h(
    "div",
    { class: "grid" },
    listHead(
      "Each organisation's agents, workspaces and tasks of the last 7 days.",
      gateway
        ? h(
            "a",
            { class: "btn", href: `${gateway}/organisations`, target: "_blank", rel: "noopener" },
            "Manage in Keera Gateway",
          )
        : null,
    ),
    table(
      [
        {
          label: "Organisation",
          sortKey: (o) => o.name.toLowerCase(),
          cell: (o) =>
            h(
              "div",
              { class: "stack" },
              h("strong", {}, o.name),
              h("span", { class: "faint mono", style: { fontSize: "11px" } }, o.id),
            ),
        },
        {
          label: "Agents",
          sortKey: (o) => o.agents,
          sortDir: "desc",
          cell: (o) =>
            o.agents
              ? h(
                  "div",
                  { class: "row-tight" },
                  h("span", { class: "nowrap" }, `${num(o.agents_running)} of ${num(o.agents)} running`),
                  o.agents_failed ? pill(`${num(o.agents_failed)} failed`, "bad") : null,
                )
              : dash(),
        },
        {
          label: "Workspaces",
          sortKey: (o) => o.workspaces,
          sortDir: "desc",
          cell: (o) =>
            o.workspaces
              ? h("span", { class: "nowrap" }, `${num(o.workspaces_running)} of ${num(o.workspaces)} running`)
              : dash(),
        },
        {
          label: "Tasks, 7 days",
          sortKey: (o) => o.tasks,
          sortDir: "desc",
          cell: (o) =>
            o.tasks
              ? h(
                  "span",
                  { class: "nowrap" },
                  num(o.tasks),
                  o.tasks_failed ? h("span", { class: "muted" }, `, ${num(o.tasks_failed)} failed`) : null,
                )
              : dash(),
        },
        {
          label: "Last task",
          sortKey: (o) => (o.last_task_at ? new Date(o.last_task_at).getTime() : null),
          sortDir: "desc",
          cell: (o) => (o.last_task_at ? stamp(o.last_task_at, { cls: "muted nowrap" }) : dash()),
        },
        {
          label: "Created",
          shrink: true,
          sortKey: (o) => (o.created_at ? new Date(o.created_at).getTime() : null),
          sortDir: "desc",
          cell: (o) => h("span", { class: "muted nowrap" }, date(o.created_at)),
        },
        {
          label: "",
          shrink: true,
          // Not an edit: it switches the organisation every screen shows.
          cell: (o) =>
            h(
              "button",
              { class: "btn btn-sm", title: `Show ${o.name} on the other screens`, onClick: () => open(o) },
              "Switch to",
            ),
        },
      ],
      orgs,
      {
        emptyTitle: "No organisations yet",
        emptyBody: "Create them in Keera Gateway.",
        sortBy: "Agents",
        sortDir: "desc",
        search: (o) => [o.name, o.id].join(" "),
        searchLabel: "organisations",
      },
    ),
  );
}

/** chooseOrg is shown on a screen that needs one organisation while the
 *  operator views all of them, as on Keera Gateway. what is the plural of
 *  what the screen holds. */
export function chooseOrg(ctx, what) {
  const orgs = ctx.state.orgs || [];
  if (!orgs.length) {
    return h("div", { class: "card" }, empty("No organisations yet", "Create one in Keera Gateway first."));
  }

  const open = async (id) => {
    await ctx.setOrg(id);
    ctx.reload();
  };

  // A few organisations get a row of buttons, more get a list.
  let picker;
  if (orgs.length <= 8) {
    picker = h(
      "div",
      { class: "wrap-chips", style: { justifyContent: "center", marginTop: "14px" } },
      orgs.map((o) => h("button", { class: "btn", onClick: () => open(o.id) }, o.name)),
    );
  } else {
    const select = h(
      "select",
      { class: "select", style: { width: "auto" }, "aria-label": "Organisation" },
      h("option", { value: "" }, "Choose an organisation"),
      orgs.map((o) => h("option", { value: o.id }, o.name)),
    );
    picker = h(
      "div",
      { class: "row", style: { justifyContent: "center", marginTop: "14px" } },
      select,
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: () => {
            if (select.value) open(select.value);
          },
        },
        "Open",
      ),
    );
  }

  return h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "empty" },
      h("strong", {}, "Pick an organisation"),
      `${what} belong to one organisation, but the switcher above shows all of them.`,
      picker,
    ),
  );
}
