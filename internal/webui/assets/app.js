// The keera-agents panel: boot, shell and routing. It is laid out like the
// Keera Gateway control panel, so someone who knows one finds their way in
// the other.

import { api, setCsrf, ApiError } from "./api.js";
import {
  h,
  icon,
  icons,
  iconButton,
  brandMark,
  replace,
  toast,
  remembered,
  remember,
  banner,
  capitalise,
  personName,
  initials,
  menu,
  go,
  isAdmin,
  loading,
} from "./ui.js";
import { signIn } from "./views/signin.js";
import { dashboardView } from "./views/dashboard.js";
import { agentsView } from "./views/agents.js";
import { agentView } from "./views/agent.js";
import { squadsView, squadView } from "./views/squads.js";
import { tasksView, taskView } from "./views/tasks.js";
import { schedulesView } from "./views/schedules.js";
import { workspacesView } from "./views/workspaces.js";
import { workspaceView } from "./views/workspace.js";
import { connectorsView, connectorView, catalogueView } from "./views/connectors.js";
import { auditView } from "./views/audit.js";
import { billingView, gatewayBilling } from "./views/billing.js";
import { firstRunDue } from "./views/firstrun.js";

const root = document.getElementById("root");

const state = {
  me: null,
  /** setup is what the getting started guide ticks off, or null. */
  setup: null,
};

// The part of a screen's ctx that the shell's own links and checks need.
const shellCtx = { state, navigate };

/** routes is the whole panel in sidebar order; `group` splits it for the
 *  sidebar. A `detail` is one row opened up, such as /agents/agent_1. With
 *  `sub`, it may take one more segment, such as /agents/agent_1/logs for a
 *  tab. A route `under` another is not in the sidebar and lights that one
 *  up. */
const routes = [
  { path: "/", label: "Dashboard", icon: "dashboard", view: dashboardView },
  {
    path: "/agents",
    group: "Work",
    label: "Agents",
    icon: "agents",
    view: agentsView,
    detail: { label: "Agent", view: agentView, sub: true },
  },
  {
    path: "/workspaces",
    group: "Work",
    label: "Workspaces",
    icon: "workspace",
    view: workspacesView,
    detail: { label: "Workspace", view: workspaceView, sub: true },
  },
  {
    path: "/squads",
    group: "Work",
    label: "Squads",
    icon: "squads",
    view: squadsView,
    detail: { label: "Squad", view: squadView },
  },
  {
    path: "/tasks",
    group: "Work",
    label: "Tasks",
    icon: "tasks",
    view: tasksView,
    detail: { label: "Task", view: taskView },
  },
  { path: "/schedules", group: "Work", label: "Schedules", icon: "schedules", view: schedulesView },
  {
    path: "/connectors",
    group: "Organisation",
    label: "Connectors",
    icon: "connections",
    view: connectorsView,
    detail: { label: "Connector", view: connectorView },
  },
  // Every kind of connector. It opens from Connectors.
  {
    path: "/connectors/catalogue",
    group: "Organisation",
    label: "Connector catalogue",
    icon: "connections",
    view: catalogueView,
    under: "/connectors",
  },
  {
    path: "/audit",
    group: "Organisation",
    label: "Audit log",
    icon: "audit",
    view: auditView,
    admin: true,
  },
  // Only on a deployment that bills machine time.
  {
    path: "/billing",
    group: "Organisation",
    label: "Billing",
    icon: "billing",
    view: billingView,
    admin: true,
    needs: "billing",
  },
];

/** navGroups leaves out a group with nothing visible, so it has no empty
 *  heading. */
function navGroups(items) {
  const groups = [];
  for (const r of items) {
    const last = groups[groups.length - 1];
    if (last && last.group === (r.group || "")) last.items.push(r);
    else groups.push({ group: r.group || "", items: [r] });
  }
  return groups;
}

function visibleRoutes() {
  return routes.filter((r) => (!r.admin || isAdmin(shellCtx)) && (!r.needs || state.me[r.needs]));
}

/** matchRoute finds the screen for a path, or the first one the reader may
 *  see. A detail path is the list path plus one segment, decoded here so it
 *  cannot be read as more of the path. */
function matchRoute(available, pathname) {
  // Squads were one board before v0.3.0. Old links go to them.
  if (pathname === "/board") pathname = "/squads";
  const exact = available.find((r) => r.path === pathname);
  if (exact) return { route: exact, param: "", sub: "" };

  for (const r of available) {
    if (!r.detail || !pathname.startsWith(r.path + "/")) continue;
    const parts = pathname.slice(r.path.length + 1).split("/");
    if (!parts[0] || parts.length > (r.detail.sub ? 2 : 1)) continue;
    return {
      route: { ...r, label: r.detail.label, view: r.detail.view, parent: r },
      param: decode(parts[0]),
      sub: parts[1] ? decode(parts[1]) : "",
    };
  }
  return { route: available[0], param: "", sub: "" };
}

function decode(segment) {
  try {
    return decodeURIComponent(segment);
  } catch {
    // A path the browser will not decode is not an id anything here issued.
    return segment;
  }
}

function navigate(path) {
  closeDrawer();
  if (location.pathname + location.search !== path) history.pushState({}, "", path);
  renderRoute({ moved: true });
}

window.addEventListener("popstate", () => renderRoute({ moved: true }));

function currentTheme() {
  return remembered("keera.theme", { lasting: true }) || "system";
}

function applyTheme(theme) {
  if (theme === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", theme);
}

function cycleTheme() {
  const order = ["system", "light", "dark"];
  const next = order[(order.indexOf(currentTheme()) + 1) % order.length];
  remember("keera.theme", next, { lasting: true });
  applyTheme(next);
  toast(`Theme: ${capitalise(next)}`);
}

let outlet;
let topbarHost;
let shell;
let collapseButton;

function navCollapsed() {
  return remembered("keera.nav", { lasting: true }) === "collapsed";
}

function applyNavCollapsed(collapsed) {
  shell.classList.toggle("nav-collapsed", collapsed);
  const label = collapsed ? "Expand sidebar" : "Collapse sidebar";
  collapseButton.setAttribute("aria-expanded", String(!collapsed));
  collapseButton.setAttribute("aria-label", label);
  collapseButton.title = label;
}

function toggleNav() {
  const collapsed = !navCollapsed();
  remember("keera.nav", collapsed ? "collapsed" : "expanded", { lasting: true });
  applyNavCollapsed(collapsed);
}

/* On a narrow screen the sidebar is a drawer. Navigating, the scrim, Escape
 * and a wide enough window all close it. */
let drawerOpen = false;

function setDrawer(open) {
  drawerOpen = open;
  if (shell) shell.classList.toggle("nav-open", open);
  // The button is rebuilt with the topbar on every route, so look it up.
  const toggle = document.querySelector(".nav-toggle");
  if (toggle) applyDrawerButton(toggle);
}

function applyDrawerButton(toggle) {
  const label = drawerOpen ? "Close menu" : "Menu";
  toggle.setAttribute("aria-expanded", String(drawerOpen));
  toggle.setAttribute("aria-label", label);
  toggle.title = label;
}

function closeDrawer() {
  if (drawerOpen) setDrawer(false);
}

document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeDrawer();
});

// Same breakpoint as the stylesheet, so the drawer is not left open behind a
// sidebar that shows anyway.
const wide = matchMedia("(min-width: 991px)");
wide.addEventListener("change", (e) => {
  if (e.matches) closeDrawer();
  placeListActions();
});

/* On a wide screen a list's buttons sit in the topbar, beside the title. A
 * comment keeps their place in the screen for when the window narrows. */
let listActions = null;

function adoptListActions() {
  const node = outlet.querySelector(".list-actions");
  if (!node) return;
  const home = document.createComment("list actions");
  node.before(home);
  listActions = { node, home };
  placeListActions();
}

function placeListActions() {
  const slot = document.getElementById("page-actions");
  if (!listActions || !slot) return;
  if (wide.matches) slot.append(listActions.node);
  else listActions.home.after(listActions.node);
}

function navItem(r) {
  return h(
    "a",
    {
      class: "nav-item" + (r.highlight ? " nav-highlight" : ""),
      href: r.path,
      // The only label once the sidebar is collapsed.
      title: r.label,
      dataset: { path: r.path },
      onClick: go(shellCtx, r.path),
    },
    icon(icons[r.icon]),
    h("span", { class: "nav-text" }, r.label),
  );
}

/** markHome names the first screen: the getting started guide until the
 *  organisation's first task, then the dashboard. */
function markHome() {
  const home = routes[0];
  const due = firstRunDue(state.setup);
  home.label = due ? "Getting started" : "Dashboard";
  home.icon = due ? "start" : "dashboard";
  home.highlight = due;
  const brand = document.querySelector(".brand");
  if (brand) brand.title = home.label;
  const el = document.querySelector('.nav-item[data-path="/"]');
  if (!el) return;
  const next = navItem(home);
  if (el.hasAttribute("aria-current")) next.setAttribute("aria-current", "page");
  el.replaceWith(next);
}

function renderShell() {
  outlet = h("div", { class: "content" });
  topbarHost = h("div", { class: "topbar-host" });

  collapseButton = h(
    "button",
    { class: "nav-item nav-collapse", onClick: toggleNav },
    icon(icons.collapse),
    h("span", { class: "nav-text" }, "Collapse"),
  );

  const me = state.me;
  const nav = h(
    "nav",
    { class: "sidebar", id: "sidebar" },
    h(
      "a",
      {
        class: "brand",
        href: "/",
        title: routes[0].label,
        onClick: go(shellCtx, "/"),
      },
      brandMark(22),
      h("span", { class: "nav-text" }, "Keera Agents"),
    ),
    navGroups(visibleRoutes().filter((r) => !r.under)).map(({ group, items }) =>
      h("div", { class: "nav-group" }, group ? h("div", { class: "nav-label" }, group) : null, items.map(navItem)),
    ),
    h(
      "div",
      { class: "sidebar-foot" },
      collapseButton,
      h("hr", { class: "sidebar-rule" }),
      h(
        "button",
        {
          class: "account",
          type: "button",
          title: me.email,
          "aria-haspopup": "menu",
          onClick: (e) => accountMenu(e.currentTarget),
        },
        h("span", { class: "avatar", "aria-hidden": "true" }, initials(me.name, me.email)),
        h(
          "span",
          { class: "account-id nav-text" },
          h("span", { class: "account-name" }, personName(me)),
          h("span", { class: "account-org" }, me.org_name || ""),
        ),
        h("span", { class: "nav-text account-more" }, icon(icons.more)),
      ),
    ),
  );

  shell = h(
    "div",
    { class: "shell" },
    nav,
    h("main", { class: "main" }, topbarHost, outlet),
    // Shown only with the drawer, to catch a tap outside it.
    h("div", { class: "nav-scrim", "aria-hidden": "true", onClick: closeDrawer }),
  );
  replace(root, shell);
  drawerOpen = false;
  applyNavCollapsed(navCollapsed());
  root.classList.remove("boot");
}

function renderTopbar() {
  const toggle = iconButton(icons.menu, "Menu", () => setDrawer(!drawerOpen), {
    quiet: true,
    cls: "nav-toggle",
    "aria-controls": "sidebar",
  });
  applyDrawerButton(toggle);
  replace(
    topbarHost,
    h(
      "header",
      { class: "topbar" },
      toggle,
      h(
        "div",
        { class: "stack" },
        // Focused when the screen changes, so a screen reader starts there.
        h("h1", { id: "page-title", tabindex: "-1" }, ""),
        h("div", { class: "page-sub", id: "page-sub" }, ""),
      ),
      h("div", { class: "spacer" }),
      h("div", { class: "topbar-actions", id: "page-actions" }),
    ),
  );
}

/** setTitle heads the screen. mark, such as an agent's icon, goes before the
 *  title. */
function setTitle(title, sub, mark) {
  const t = document.getElementById("page-title");
  const s = document.getElementById("page-sub");
  if (t) t.replaceChildren(...(mark ? [mark] : []), title || "");
  if (s) s.textContent = sub || "";
  document.title = title && location.pathname !== "/" ? `${title} · Keera Agents` : "Keera Agents";
}

/** teardowns are what the current screen must undo before the next, such as
 *  closing a log stream or stopping a poll. */
let teardowns = [];

/** rendering counts renderRoute calls. A screen that finishes loading after
 *  the reader moved on is dropped and torn down at once. */
let rendering = 0;

function tearDown() {
  const pending = teardowns;
  teardowns = [];
  for (const fn of pending) {
    try {
      fn();
    } catch {
      // A failed teardown must not stop the next screen.
    }
  }
}

/** creditNotice says, on every screen, that no machine starts because the
 *  organisation has no credit, or a charge is due. An administrator gets a
 *  link to where they pay. */
function creditNotice() {
  const refusal = state.me.credit_refusal;
  if (!refusal) return null;
  return banner(
    "bad",
    { style: { marginBottom: "16px" } },
    `No agent or workspace starts: ${refusal.message}`,
    isAdmin(shellCtx) ? gatewayBilling(state.me, "Open Billing in Keera Gateway", { marginLeft: "8px" }) : null,
  );
}

/** renderRoute shows the screen for the current path. moved is a move to
 *  another screen, not a reload of this one: focus then goes to its title,
 *  as it would on a new page. */
async function renderRoute({ moved = false } = {}) {
  tearDown();
  const gen = ++rendering;
  const available = visibleRoutes();
  const { route, param, sub } = matchRoute(available, location.pathname);

  // A detail screen lights up its list in the sidebar.
  const current = route.under || (route.parent || route).path;
  for (const el of document.querySelectorAll(".nav-item")) {
    if (el.dataset.path === current) el.setAttribute("aria-current", "page");
    else el.removeAttribute("aria-current");
  }
  if (!route.parent && location.pathname !== route.path) {
    history.replaceState({}, "", route.path);
  }
  listActions = null;
  renderTopbar();
  setTitle(route.label, "");
  replace(outlet, loading("240px"));

  let title = route.label;
  let titleMark = null;
  const ctx = {
    state,
    reload: () => renderRoute(),
    navigate,
    /** param is a detail screen's path segment, such as an agent id, and sub
     *  the one after it, such as a tab. */
    param,
    sub,
    setSubtitle: (s) => setTitle(title, s, titleMark),
    onTeardown: (fn) => (gen === rendering ? teardowns.push(fn) : fn()),
    /** setSetup takes fresh counts, so the sidebar follows steps done since
     *  the panel started. */
    setSetup: (setup) => {
      state.setup = setup;
      markHome();
    },
    /** setTitle heads a screen with what it shows, such as an agent's name. */
    setTitle: (t, s, mark) => {
      title = t;
      titleMark = mark || null;
      setTitle(t, s, titleMark);
    },
    /** current reports whether this screen is still the one shown, for work
     *  that finishes later. */
    current: () => gen === rendering,
  };
  try {
    const view = await route.view(ctx);
    if (gen !== rendering) return;
    replace(outlet, creditNotice(), view);
    adoptListActions();
  } catch (err) {
    if (gen !== rendering) return;
    if (err instanceof ApiError && err.status === 401) {
      return boot(); // the session expired underneath us
    }
    replace(outlet, banner("bad", err.message || "Something went wrong loading this page."));
  }
  if (moved) document.getElementById("page-title")?.focus({ preventScroll: true });
}

function accountMenu(anchor) {
  const me = state.me;
  const others = (me.orgs || []).filter((o) => o.id !== me.org_id);
  menu(
    anchor,
    [
      h(
        "div",
        { class: "menu-who" },
        h("strong", {}, personName(me)),
        me.name ? h("span", {}, me.email) : null,
        me.org_name ? h("span", {}, me.org_name) : null,
      ),
      { separator: true },
      ...others.map((o) => ({
        label: o.name,
        hint: "Switch to this organisation",
        icon: icons.orgs,
        onClick: () => switchOrg(o),
      })),
      others.length ? { separator: true } : null,
      {
        label: "Theme",
        icon: icons.theme,
        hint: { system: "Follows your system", light: "Light", dark: "Dark" }[currentTheme()],
        onClick: cycleTheme,
      },
      me.gateway_url ? { label: "Keera Gateway", icon: icons.shield, href: me.gateway_url } : null,
      { label: "Documentation", icon: icons.docs, href: "https://docs.keera.ch/use/agents" },
      { separator: true },
      { label: "Sign out", icon: icons.signout, onClick: signOut },
    ],
    { up: true },
  );
}

/** switchOrg moves the session to another organisation. The role changes
 *  with it, so the whole panel starts again. */
async function switchOrg(org) {
  try {
    await api.switchOrg(org.id);
  } catch (err) {
    toast(err.message || "Could not switch the organisation.", "bad");
    return;
  }
  // A detail screen belongs to the old organisation.
  const { route } = matchRoute(visibleRoutes(), location.pathname);
  location.href = route.parent ? route.parent.path : location.pathname;
}

async function signOut() {
  try {
    await api.signOut();
  } catch {
    // Signing out locally is what matters, even if the server refuses.
  }
  location.href = "/";
}

async function boot() {
  applyTheme(currentTheme());
  root.replaceChildren();
  root.classList.add("boot");
  root.append(h("div", { class: "boot-mark" }));

  let me;
  try {
    me = await api.me();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      return signIn(root);
    }
    root.classList.remove("boot");
    return replace(
      root,
      h(
        "div",
        { class: "signin" },
        h(
          "div",
          { class: "signin-card" },
          banner("bad", err.message || "Keera Agents is unreachable."),
          h("button", { class: "btn", onClick: boot }, "Try again"),
        ),
      ),
    );
  }

  state.me = me;
  setCsrf(me.csrf);
  // Without it the first screen is named Dashboard until it is opened.
  state.setup = await api.setup().catch(() => null);
  markHome();
  renderShell();
  await renderRoute();
}

boot();
