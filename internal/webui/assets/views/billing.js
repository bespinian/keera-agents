// What the organisation's machines cost: the time keera-agents metered, per
// agent and workspace. The bill and the credit are in Keera Gateway, which
// takes the payments. See docs/billing.md.

import { api } from "../api.js";
import { h, table, money, icons, iconButton, stat, remembered, remember, card, fact, link, isAdmin } from "../ui.js";

// YYYY-MM in UTC, like the bill.
function thisMonth() {
  return new Date().toISOString().slice(0, 7);
}

function shift(month, n) {
  const [y, m] = month.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1 + n, 1)).toISOString().slice(0, 7);
}

function monthName(month) {
  const [y, m] = month.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, 1)).toLocaleDateString(undefined, {
    month: "long",
    year: "numeric",
    timeZone: "UTC",
  });
}

/** amount shows a quantity of hours or months, with at most two decimals. */
export function amount(n) {
  return (n || 0).toLocaleString(undefined, { maximumFractionDigits: 2 });
}

/** gatewayBilling links to Billing in Keera Gateway, where the bill is and
 *  the credit is paid. */
export function gatewayBilling(me, label, style = {}) {
  return h(
    "a",
    { class: "btn btn-sm", style, href: `${me.gateway_url}/billing`, target: "_blank", rel: "noopener" },
    label,
  );
}

/** priceNote says what machine time costs, leaving out what is free. */
export function priceNote(p) {
  const parts = [];
  if (p.cpu_hour_micros) parts.push(`${money(p.cpu_hour_micros)} a CPU-hour`);
  if (p.memory_gib_hour_micros) parts.push(`${money(p.memory_gib_hour_micros)} a GiB-hour of memory`);
  if (p.disk_gib_month_micros) parts.push(`${money(p.disk_gib_month_micros)} a GiB-month of disk`);
  return parts.join(", ");
}

/** agentCostCard is what one agent's or workspace's machine costs, on its
 *  page. */
export function agentCostCard(ctx, cost) {
  const note = (text) => h("div", { class: "faint small" }, text);
  return card(
    "Machine cost",
    [
      fact("This month", [money(cost.month_micros), note(`So far in ${monthName(cost.month)}`)]),
      fact("Per hour running", money(cost.hour_micros)),
      note("Model calls are billed in Keera Gateway."),
    ],
    {
      style: { alignSelf: "start" },
      bodyClass: "card-body facts",
      more: isAdmin(ctx) ? link(ctx, "/billing", "All machines") : null,
    },
  );
}

export async function billingView(ctx) {
  const now = thisMonth();
  const stored = remembered("keera.billing.month");
  const month = stored && stored <= now ? stored : now;
  const res = await api.billing(month);
  const machines = res.machines || [];
  const credit = res.credit;
  ctx.setSubtitle(monthName(month));

  const go = (m) => {
    remember("keera.billing.month", m);
    ctx.reload();
  };
  const pay = gatewayBilling(
    ctx.state.me,
    credit && credit.payments ? "Add credit in Keera Gateway" : "Open the bill in Keera Gateway",
  );

  const stats = h(
    "div",
    { class: "grid grid-2", style: { marginBottom: "16px" } },
    stat("Machines this month", money(res.total_micros), priceNote(res.prices)),
    credit && credit.payments
      ? stat("Credit", money(credit.balance_micros), "Paid in advance, in Keera Gateway")
      : stat("Bill", "Keera Gateway", "Machines are on the same bill as model use"),
  );

  const controls = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    iconButton(icons.back, "Previous month", () => go(shift(month, -1))),
    h("strong", { style: { minWidth: "130px", textAlign: "center" } }, monthName(month)),
    iconButton(icons.forward, "Next month", () => go(shift(month, 1)), { disabled: month >= now }),
    h("div", { class: "spacer" }),
    pay,
  );

  const list = table(
    [
      {
        label: "Machine",
        cell: (m) =>
          h(
            "div",
            { class: "stack" },
            m.deleted ? h("span", { class: "faint" }, `${m.name} (deleted)`) : h("span", {}, m.name),
            h(
              "span",
              { class: "faint", style: { fontSize: "11px" } },
              `${m.kind === "workspace" ? "Workspace" : "Agent"} · ${m.owner_email}`,
            ),
          ),
        sortKey: (m) => m.name.toLowerCase(),
      },
      { label: "CPU-hours", num: true, cell: (m) => amount(m.cpu_hours), sortKey: (m) => m.cpu_hours, sortDir: "desc" },
      {
        label: "Memory, GiB-hours",
        num: true,
        cell: (m) => amount(m.memory_gib_hours),
        sortKey: (m) => m.memory_gib_hours,
        sortDir: "desc",
      },
      {
        label: "Disk, GiB-months",
        num: true,
        cell: (m) => amount(m.disk_gib_months),
        sortKey: (m) => m.disk_gib_months,
        sortDir: "desc",
      },
      {
        label: "Cost",
        num: true,
        cell: (m) => h("strong", {}, money(m.cost_micros)),
        sortKey: (m) => m.cost_micros,
        sortDir: "desc",
      },
    ],
    machines,
    {
      search: (m) => `${m.name} ${m.owner_email}`,
      searchLabel: "machines",
      emptyTitle: month === now ? "No machine time yet this month" : "No machine time that month",
      emptyBody: "Agents and workspaces count from when they first start.",
    },
  );

  return h(
    "div",
    {},
    // A refusal shows above every screen, so not again here.
    stats,
    controls,
    list,
    h(
      "p",
      { class: "muted small", style: { marginTop: "12px" } },
      "CPU and memory count while a machine runs, disk while it exists. Each hour goes to the bill " +
        "in Keera Gateway once it is over, so the last hour shows here first.",
    ),
  );
}
