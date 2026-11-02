// The audit log: who changed what, for administrators.

import { api } from "../api.js";
import { h, table, dateTime, ago, loadMore } from "../ui.js";

const PAGE = 100;

// detailText reads an entry's detail, which the server keeps as an object
// such as {"name": "Scout"}.
function detailText(detail) {
  if (!detail) return "";
  return Object.entries(detail)
    .map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : v}`)
    .join(", ");
}

export async function auditView(ctx) {
  const rows = (await api.audit()).data || [];
  const list = table(
    [
      {
        label: "When",
        shrink: true,
        cell: (e) => h("span", { class: "nowrap", title: ago(e.at) }, dateTime(e.at)),
      },
      { label: "Who", shrink: true, cell: (e) => h("span", { class: "muted" }, e.actor || "-") },
      { label: "Action", shrink: true, cell: (e) => h("span", { class: "mono" }, e.action) },
      { label: "Target", cell: (e) => e.target || "-" },
      {
        label: "Detail",
        cell: (e) => (e.detail ? h("div", { class: "faint clamp-2" }, detailText(e.detail)) : null),
      },
    ],
    rows,
    {
      search: (e) => `${e.actor} ${e.action} ${e.target} ${detailText(e.detail)}`,
      searchLabel: "the loaded entries",
      emptyTitle: "Nothing logged yet",
      emptyBody: "Changes to agents, connectors, schedules and boards show here.",
    },
  );
  return h(
    "div",
    {},
    h(
      "div",
      { class: "muted", style: { marginBottom: "16px" } },
      "Every change to agents, channels, schedules, boards and connectors, newest first.",
    ),
    list,
    rows.length === PAGE
      ? loadMore(
          PAGE,
          async () => {
            const more = (await api.audit(rows[rows.length - 1].id)).data || [];
            rows.push(...more);
            list.redraw();
            return more.length;
          },
          "That is the whole log.",
        )
      : null,
  );
}
