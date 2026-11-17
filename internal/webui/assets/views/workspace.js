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
  ago,
  avatarOf,
  modal,
} from "../ui.js";
import { agentStatePill, isUp, workspacePath, detailTabs, EXPERIENCES, TIERS } from "../common.js";
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
      w.enabled === false ? pill("Stopped") : agentStatePill(w.state),
      pill(EXPERIENCES[w.experience]?.name || w.experience),
      w.state_detail ? h("span", { class: "faint small", style: { alignSelf: "center" } }, w.state_detail) : null,
    ],
    h("div", { class: "row-tight", style: { flexWrap: "wrap" } }, control, connect),
  );
}

async function overviewTab(ctx, w) {
  const cls = w.class || {};
  // Whoever sees a workspace may see what its machine costs.
  const cost = ctx.state.me.billing ? await api.workspaceCost(w.id).catch(() => null) : null;
  return h(
    "div",
    { class: "grid grid-2" },
    card(
      "About",
      h(
        "div",
        { class: "facts" },
        fact("Owner", w.owner_email),
        fact("Experience", EXPERIENCES[w.experience]?.what),
        fact("Starts with the model", h("span", { class: "mono" }, w.model)),
        fact("Machine", `${TIERS[cls.isolation] || cls.isolation || "-"}, ${cls.cpu || "-"} CPU, ${cls.memory || "-"}`),
        fact("Disk", cls.disk),
        fact("Last used", w.last_active_at ? ago(w.last_active_at) : ""),
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
  const save = busyButton({ class: "btn btn-primary" }, "Save", async () => {
    const body = form.values();
    await api.updateWorkspace(w.id, body);
    toast("Saved", "good");
    ctx.reload();
  });
  const remove = h(
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
    "Delete workspace",
  );
  return h(
    "div",
    { class: "stack", style: { gap: "16px" } },
    card("Settings", h("div", {}, form.el, h("div", { class: "row-tight" }, save))),
    card(
      "Delete",
      h(
        "div",
        { class: "stack" },
        h("div", { class: "muted" }, "Delete it and everything in it."),
        h("div", {}, remove),
      ),
    ),
  );
}
