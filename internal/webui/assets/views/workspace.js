// One workspace: its state and controls, how to reach it, its connectors,
// its settings and what it printed.

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
  tabs,
  replace,
  banner,
  confirm,
  toast,
  card,
  loading,
  every,
  ago,
} from "../ui.js";
import { agentStatePill, isUp, workspacePath, EXPERIENCES, TIERS } from "../common.js";
import { noRuntime } from "./agents.js";
import { channelsTab } from "./channels.js";
import { logPane } from "./logs.js";
import { workspaceForm, connectSteps } from "./workspaces.js";

const TABS = [
  { value: "overview", label: "Overview" },
  { value: "connectors", label: "Connectors" },
  { value: "settings", label: "Settings" },
  { value: "logs", label: "Logs" },
];

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
  ctx.setTitle(w.name, w.description || "", icon(EXPERIENCES[w.experience]?.icon || icons.workspace));
  let current = TABS.some((t) => t.value === ctx.sub) ? ctx.sub : "overview";

  if (w.state === "starting") {
    every(ctx, 4000, async () => {
      const fresh = await api.workspace(w.id);
      if (fresh.state !== "starting") ctx.reload();
    });
  }

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
    const path = workspacePath(w.id, tab === "overview" ? "" : tab);
    if (location.pathname !== path) history.replaceState({}, "", path);
    replace(panel, loading("160px"));
    try {
      const body = await TAB_VIEWS[tab](tabCtx, w);
      if (current === tab && ctx.current()) replace(panel, body);
    } catch (err) {
      if (current === tab) replace(panel, banner("bad", err.message));
    }
  }
  const strip = tabs(
    TABS.map((t) => ({ ...t, count: t.value === "connectors" ? (w.bindings || []).length : 0 })),
    current,
    show,
  );
  show(current);

  return h("div", { class: "grid" }, head(ctx, w), noRuntime(ctx.state.me), strip, panel);
}

function head(ctx, w) {
  const act = (action, done) => async () => {
    await api.workspaceAction(w.id, action);
    toast(`${w.name} ${done}`, "good");
    ctx.reload();
  };
  const controls = [];
  if (w.can_edit) {
    controls.push(
      w.enabled !== false && isUp(w.state)
        ? busyButton(
            { title: "Stop its machine. Your files are kept." },
            [icon(icons.stop), "Stop"],
            act("stop", "is stopping"),
          )
        : busyButton({ title: "Start its machine" }, [icon(icons.play), "Start"], act("start", "is starting")),
    );
  }
  return detailHead(
    ctx,
    ["/workspaces", "Workspaces"],
    [
      w.enabled === false ? pill("Stopped") : agentStatePill(w.state),
      pill(EXPERIENCES[w.experience]?.name || w.experience),
      w.state_detail ? h("span", { class: "faint small", style: { alignSelf: "center" } }, w.state_detail) : null,
    ],
    h("div", { class: "row-tight", style: { flexWrap: "wrap" } }, controls),
  );
}

const TAB_VIEWS = {
  overview: overviewTab,
  connectors: (ctx, w) => channelsTab(ctx, w),
  settings: settingsTab,
  logs: (ctx, w) => logPane(ctx, w),
};

function overviewTab(_ctx, w) {
  const cls = w.class || {};
  return h(
    "div",
    { class: "grid grid-2" },
    card(
      "Connect",
      w.can_connect
        ? connectSteps(w)
        : h("div", { class: "muted" }, `Only ${w.owner_email} can get into this workspace.`),
    ),
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
  );
}

async function settingsTab(ctx, w) {
  if (!w.can_edit) return card(null, h("div", { class: "muted" }, "Only its owner and administrators change it."));
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
