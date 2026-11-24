// The audit log: who changed what, for administrators. Its columns and
// action pills follow Keera Gateway's.

import { api } from "../api.js";
import { h, table, pill, dateTime, ago, dash, modal, loadMore } from "../ui.js";

const PAGE = 100;

// Who acts when no person does, such as when an owner leaves.
const SYSTEM = "keera-agents";

const TONES = {
  "agent.create": "accent",
  "agent.delete": "bad",
  "agent.start": "good",
  "agent.stop": "warn",
  "agent.restart": "warn",
  "agent.take_over": "warn",
  "agent.owner_left": "warn",
  "workspace.create": "accent",
  "workspace.delete": "bad",
  "workspace.start": "good",
  "workspace.stop": "warn",
  "workspace.restart": "warn",
  "workspace.connect": "warn",
  "connection.create": "accent",
  "connection.delete": "bad",
  "connection.sign_ins_forgotten": "warn",
  "account.connect": "good",
  "account.disconnect": "bad",
  "account.user_left": "warn",
  "ssh_key.add": "accent",
  "ssh_key.delete": "bad",
  "squad.delete": "bad",
  "schedule.delete": "bad",
  "binding.delete": "bad",
  "task.cancel": "warn",
};

// The wire keeps `noun.verb`. An action missing here shows its wire id.
const LABELS = {
  "agent.create": "Agent created",
  "agent.update": "Agent changed",
  "agent.delete": "Agent deleted",
  "agent.start": "Agent started",
  "agent.stop": "Agent stopped",
  "agent.restart": "Agent restarted",
  "agent.take_over": "Agent taken over",
  "agent.owner_left": "Agent's owner left",
  "workspace.create": "Workspace created",
  "workspace.update": "Workspace changed",
  "workspace.delete": "Workspace deleted",
  "workspace.start": "Workspace started",
  "workspace.stop": "Workspace stopped",
  "workspace.restart": "Workspace restarted",
  "workspace.connect": "Workspace port opened",
  "connection.create": "Connector added",
  "connection.update": "Connector changed",
  "connection.delete": "Connector deleted",
  "connection.sign_ins_forgotten": "Connector sign-ins forgotten",
  "account.connect": "Account connected",
  "account.disconnect": "Account disconnected",
  "account.user_left": "Account's user left",
  "ssh_key.add": "SSH key added",
  "ssh_key.delete": "SSH key removed",
  "squad.create": "Squad created",
  "squad.update": "Squad changed",
  "squad.delete": "Squad deleted",
  "schedule.create": "Schedule created",
  "schedule.update": "Schedule changed",
  "schedule.delete": "Schedule deleted",
  "binding.create": "Connector added to an agent",
  "binding.delete": "Connector removed from an agent",
  "task.cancel": "Task cancelled",
};

function label(action) {
  return LABELS[action] || action;
}

export async function auditView() {
  const rows = (await api.audit()).data || [];
  const list = table(
    [
      {
        label: "When",
        shrink: true,
        cell: (e) => h("span", { class: "muted nowrap", title: dateTime(e.at) }, ago(e.at)),
      },
      {
        label: "Who",
        cell: (e) =>
          !e.actor || e.actor === SYSTEM
            ? h("span", { class: "muted" }, e.actor || "-", h("span", { class: "faint" }, " (automatic)"))
            : h("strong", {}, e.actor),
      },
      {
        label: "Action",
        shrink: true,
        cell: (e) => pill(label(e.action), TONES[e.action] || ""),
      },
      {
        label: "Target",
        cell: (e) => (e.target ? h("span", { class: "mono muted", style: { fontSize: "12px" } }, e.target) : dash()),
      },
      {
        label: "",
        shrink: true,
        cell: (e) =>
          e.detail
            ? h(
                "button",
                {
                  class: "btn btn-sm",
                  title: "Show details",
                  onClick: () =>
                    modal({
                      title: label(e.action),
                      subtitle: `${e.actor || "-"} · ${dateTime(e.at)}`,
                      body: h(
                        "pre",
                        { class: "key-value", style: { whiteSpace: "pre-wrap", userSelect: "text" } },
                        JSON.stringify(e.detail, null, 2),
                      ),
                    }),
                },
                "View",
              )
            : null,
      },
    ],
    rows,
    {
      search: (e) =>
        `${e.actor} ${label(e.action)} ${e.action} ${e.target} ${e.detail ? JSON.stringify(e.detail) : ""}`,
      searchLabel: "the loaded entries",
      emptyTitle: "Nothing logged yet",
      emptyBody: "Changes to agents, connectors, schedules and squads show here.",
    },
  );
  return h(
    "div",
    {},
    h(
      "div",
      { class: "muted", style: { marginBottom: "16px" } },
      "Every change to agents, their connectors, schedules and squads, newest first.",
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
