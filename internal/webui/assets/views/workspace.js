// One workspace: its state and controls, how to reach it, its settings, its
// connectors and what it printed.

import { api } from "../api.js";
import {
  h,
  pill,
  fact,
  icon,
  icons,
  busyButton,
  detailHead,
  gone,
  confirm,
  toast,
  card,
  every,
  stamp,
  avatarOf,
  modal,
} from "../ui.js";
import { agentStatePill, isUp, workspacePath, detailTabs, settingsCard, machineText, EXPERIENCES } from "../common.js";
import { noRuntime } from "./agents.js";
import { bindingsTab } from "./bindings.js";
import { logPane } from "./logs.js";
import { workspaceForm, connectSteps } from "./workspaces.js";
import { agentCostCard } from "./billing.js";

export async function workspaceView(ctx) {
  let w;
  try {
    w = await api.workspace(ctx.param);
  } catch (err) {
    if (err.status === 404) {
      return gone(ctx, "/workspaces", "Workspaces", "No such workspace", "It was deleted, or it is not yours.");
    }
    throw err;
  }
  ctx.setTitle(
    w.name,
    w.description || "",
    avatarOf(w.name, { agent: true, glyph: EXPERIENCES[w.experience]?.icon || icons.workspace, large: true }),
  );

  if (w.state === "starting") {
    every(ctx, 4000, async () => {
      const fresh = await api.workspace(w.id);
      if (fresh.state !== "starting") ctx.reload();
    });
  }

  const { strip, panel } = detailTabs(
    ctx,
    [
      { value: "overview", label: "Overview", view: overviewTab },
      { value: "settings", label: "Settings", view: settingsTab },
      { value: "connectors", label: "Connectors", count: (w.bindings || []).length, view: bindingsTab },
      { value: "logs", label: "Logs", view: logPane },
    ],
    (tab) => workspacePath(w.id, tab),
    w,
  );

  return h("div", { class: "grid" }, head(ctx, w), noRuntime(ctx.state.me), strip, panel);
}

function head(ctx, w) {
  const act = (action, done) => async () => {
    await api.workspaceAction(w.id, action);
    toast(`${w.name} ${done}`, "good");
    ctx.reload();
  };
  // Only its owner and administrators can load a workspace, so whoever sees
  // it may change it.
  const control =
    w.enabled !== false && isUp(w.state)
      ? busyButton(
          { title: "Stop its machine. Your files are kept." },
          [icon(icons.stop), "Stop"],
          act("stop", "is stopping"),
        )
      : busyButton({ title: "Start its machine" }, [icon(icons.play), "Start"], act("start", "is starting"));
  // Connecting wakes a suspended workspace, but not a stopped one. Only its
  // owner gets in.
  const awake = w.enabled !== false && (isUp(w.state) || w.state === "suspended");
  const connect =
    awake && w.can_connect
      ? h(
          "button",
          {
            class: "btn",
            title: "How to get into it",
            onClick: () =>
              modal({
                title: `Connect to ${w.name}`,
                subtitle: "With the keera CLI",
                body: connectSteps(w),
                wide: true,
              }),
          },
          icon(icons.terminal),
          "Connect",
        )
      : null;
  return detailHead(
    ctx,
    ["/workspaces", "Workspaces"],
    [
      agentStatePill(w.enabled === false ? "stopped" : w.state),
      pill(EXPERIENCES[w.experience]?.name || w.experience),
      w.state_detail ? h("span", { class: "faint small", style: { alignSelf: "center" } }, w.state_detail) : null,
    ],
    h("div", { class: "row-tight", style: { flexWrap: "wrap" } }, control, connect),
  );
}

async function overviewTab(ctx, w) {
  // Whoever sees a workspace may see what its machine costs.
  const cost = ctx.state.me.billing ? await api.workspaceCost(w.id).catch(() => null) : null;
  return h(
    "div",
    { class: "grid grid-2" },
    card(
      "Setup",
      h(
        "div",
        { class: "facts" },
        fact("Owner", w.owner_email),
        fact("Experience", EXPERIENCES[w.experience]?.what),
        fact("Starts with the model", h("span", { class: "mono" }, w.model)),
        fact("Machine", machineText(w.class)),
        fact("Last active", w.last_active_at ? stamp(w.last_active_at) : ""),
        h(
          "div",
          { class: "hint" },
          "A workspace nobody is connected to goes to sleep after a while. It wakes when you connect.",
        ),
      ),
    ),
    cost ? agentCostCard(ctx, cost) : null,
  );
}

async function settingsTab(ctx, w) {
  const form = await workspaceForm(ctx, w);
  const settings = settingsCard(ctx, form.el, async () => {
    await api.updateWorkspace(w.id, form.values());
    return "Changes saved";
  });
  // A form as wide as the screen is hard to read down.
  return h(
    "div",
    { class: "grid", style: { maxWidth: "820px" } },
    settings,
    h(
      "div",
      { class: "card" },
      h(
        "div",
        { class: "card-body row", style: { flexWrap: "wrap" } },
        h(
          "div",
          { class: "stack", style: { flex: "1 1 280px" } },
          h("strong", {}, "Delete this workspace"),
          h("span", { class: "muted small" }, "Stops its machine, deletes its files, and revokes its key."),
        ),
        h(
          "button",
          {
            class: "btn btn-danger",
            onClick: () =>
              confirm({
                title: `Delete ${w.name}?`,
                body: "Its machine and every file in it are deleted, and its key is revoked. This cannot be undone.",
                confirmLabel: "Delete workspace",
                danger: true,
                onConfirm: () => api.deleteWorkspace(w.id),
                done: () => {
                  ctx.navigate("/workspaces");
                  return `${w.name} deleted`;
                },
              }),
          },
          icon(icons.trash),
          "Delete workspace",
        ),
      ),
    ),
  );
}
