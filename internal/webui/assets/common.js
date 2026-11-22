// Words, tones and small cells that several screens share, so an agent's or
// a task's state reads the same everywhere.

import { h, pill, card, icon, icons, duration, num, tabs, replace, loading, banner, field, fold } from "./ui.js";
import { areaChart } from "./chart.js";

/** agentPath is an agent's screen, or one of its tabs. */
export function agentPath(id, tab) {
  return `/agents/${encodeURIComponent(id)}` + (tab ? `/${tab}` : "");
}

/** connectorPath is a connector's screen. */
export function connectorPath(id) {
  return `/connectors/${encodeURIComponent(id)}`;
}

/** workspacePath is a workspace's screen, or one of its tabs. */
export function workspacePath(id, tab) {
  return `/workspaces/${encodeURIComponent(id)}` + (tab ? `/${tab}` : "");
}

/** EXPERIENCES are what a workspace opens in the browser. */
export const EXPERIENCES = {
  terminal: { name: "Terminal", icon: icons.workspace, what: "Pi, the coding agent, in a terminal in your browser." },
  vscode: { name: "VS Code", icon: icons.code, what: "VS Code in your browser, with the Cline coding agent." },
};

/** detailTabs is a screen's tab strip and the panel under it. Each item is
 *  { value, label, count, view }, and view(ctx, subject) builds its panel.
 *  The first tab is the default. ctx.showTab, given to the views, opens
 *  another. */
export function detailTabs(ctx, items, pathOf, subject) {
  let current = items.some((t) => t.value === ctx.sub) ? ctx.sub : items[0].value;

  // Each tab may leave something running, such as the log stream.
  const teardown = [];
  const endTab = () => {
    for (const fn of teardown.splice(0)) fn();
  };
  ctx.onTeardown(endTab);
  const tabCtx = { ...ctx, onTeardown: (fn) => teardown.push(fn) };

  const panel = h("div", { class: "grid" });
  async function show(tab) {
    endTab();
    current = tab;
    // The URL names the tab, so a reload or a shared link opens it.
    const path = pathOf(tab === items[0].value ? "" : tab);
    if (location.pathname !== path) history.replaceState({}, "", path);
    replace(panel, loading("160px"));
    try {
      const body = await items.find((t) => t.value === tab).view(tabCtx, subject);
      if (current === tab && ctx.current()) replace(panel, body);
    } catch (err) {
      if (current === tab) replace(panel, banner("bad", err.message));
    }
  }
  const strip = tabs(items, current, show);
  tabCtx.showTab = (tab) => {
    strip.select(tab);
    show(tab);
  };
  show(current);
  return { strip, panel };
}

/** taskPath is a task's screen. */
export function taskPath(id) {
  return `/tasks/${encodeURIComponent(id)}`;
}

/** AGENT_STATES maps each state to its label and pill tone. */
export const AGENT_STATES = {
  running: ["Running", "good"],
  starting: ["Starting", "accent"],
  suspended: ["Suspended", "warn"],
  stopped: ["Stopped", ""],
  failed: ["Failed", "bad"],
};

export function agentStatePill(state) {
  const [label, tone] = AGENT_STATES[state] || [state || "Unknown", ""];
  return pill(label, tone, { dot: true });
}

/** isUp reports whether an agent's machine runs or is on its way. */
export function isUp(state) {
  return state === "running" || state === "starting";
}

export const TASK_STATES = {
  queued: ["Queued", ""],
  running: ["Running", "accent"],
  succeeded: ["Done", "good"],
  failed: ["Failed", "bad"],
  cancelled: ["Cancelled", ""],
};

export function taskStatePill(state) {
  const [label, tone] = TASK_STATES[state] || [state || "Unknown", ""];
  return pill(label, tone);
}

/** isLive reports whether a task may still change. */
export function isLive(state) {
  return state === "queued" || state === "running";
}

/** SOURCES are where a task comes from. A task given in the panel is
 *  "manual". */
export const SOURCES = {
  chat: { label: "Chat", icon: icons.chat },
  schedule: { label: "Schedule", icon: icons.schedules },
  board: { label: "Board", icon: icons.board },
  file: { label: "File", icon: icons.file },
  manual: { label: "Panel", icon: icons.person },
};

/** sourceCell names where a task came from, such as "Slack #sales". */
export function sourceCell(task) {
  const s = SOURCES[task.source] || { label: task.source || "-", icon: icons.tasks };
  return h(
    "span",
    { class: "source", title: s.label },
    icon(s.icon),
    h("span", { class: "nowrap" }, task.origin_label || s.label),
  );
}

/** took is how long a task ran, or how long it has been running. */
export function took(task, now = Date.now()) {
  if (!task.started_at) return "";
  const end = task.finished_at ? new Date(task.finished_at).getTime() : now;
  return duration(end - new Date(task.started_at).getTime());
}

/** firstLine shortens a prompt to a title. */
export function firstLine(text, max = 80) {
  const line =
    String(text || "")
      .split("\n")
      .find((l) => l.trim()) || "";
  const t = line.trim();
  return t.length > max ? t.slice(0, max - 1).trimEnd() + "…" : t;
}

/** shellArg quotes a value for a shell command, so a name with spaces stays
 *  one argument. Plain names stay as they are, to keep commands readable. */
export function shellArg(value) {
  const s = String(value);
  if (/^[A-Za-z0-9._\/:@%+=,-]+$/.test(s)) return s;
  return "'" + s.replaceAll("'", `'\\''`) + "'";
}

/** kindLabel names a connector kind as /connection-kinds does, or by its id
 *  while that has not answered. */
export function kindLabel(kind, kinds) {
  const d = (kinds || []).find((k) => k.kind === kind);
  return (d && d.label) || kind;
}

/** BUILTINS are the tools every agent may be given, each in one line. */
export const BUILTINS = {
  memory: ["Memory", "Remembers notes between tasks."],
  files: ["Files", "Reads and writes files in its own workspace."],
  shell: ["Shell", "Runs commands in its own machine."],
  web: ["Web", "Reads web pages."],
};

/** ACCESS is who may give an agent work. */
export const ACCESS = {
  owner: "Only its owner",
  org: "Everyone in the organisation",
  people: "Only the people listed",
};

/** MODES are what an agent does with a connector. */
export const MODES = {
  chat: ["Chat", "Answers messages in the chat."],
  watch: ["Watch", "Each new file in the folder is a task."],
  files: ["Files", "May read and write files in the folder."],
  repo: ["Repository", "May clone it, push keera/ branches and open pull requests."],
  read: ["Read", "May look things up, never change them."],
  write: ["Read and write", "May look things up and make changes."],
};

/** gives says in a few words what a connector kind gives agents. */
export function gives(kind) {
  if (kind.api) return `${(kind.tools || []).length} tools`;
  if (kind.code) return "Repositories";
  if (kind.chat && kind.files) return "Chats, folders";
  return kind.files ? "Folders" : "Chats";
}

/** TIERS name the isolation tiers a deployment may offer. */
export const TIERS = {
  standard: "Standard container",
  isolated: "gVisor isolated container",
  vm: "microVM",
};

/** machineFold is the Machine section of an agent's or a workspace's form.
 *  hints says what its memory and disk are for, and whether the disk is
 *  fixed, as once the machine exists. value() is the class to send. */
export function machineFold(me, cls, hints) {
  const tiers = (me.runtime && me.runtime.tiers) || ["standard"];
  // Tiers come weakest first. A new machine gets the strongest, as the server
  // gives it, since an agent runs code a model wrote.
  const chosen = cls.isolation || tiers[tiers.length - 1];
  // A tier taken away since keeps showing, so saving does not change it
  // unasked.
  const stale = cls.isolation && !tiers.includes(cls.isolation);
  const isolation = h(
    "select",
    { class: "select" },
    tiers.map((t) => h("option", { value: t, selected: t === chosen }, TIERS[t] || t)),
    stale
      ? h(
          "option",
          { value: cls.isolation, selected: true },
          `${TIERS[cls.isolation] || cls.isolation} (not offered here)`,
        )
      : null,
  );
  const cpu = h("input", { class: "input", value: cls.cpu || "", placeholder: "1", inputmode: "decimal" });
  const memory = h("input", { class: "input", value: cls.memory || "", placeholder: "2Gi" });
  const disk = h("input", {
    class: "input",
    value: cls.disk || "",
    placeholder: hints.diskSize,
    readonly: Boolean(hints.diskFixed),
  });
  // With nothing to choose, the field only takes room. Its value still goes.
  const isolationField = field("Isolation", isolation, "How strongly its machine is kept apart from others.");
  isolationField.hidden = tiers.length === 1 && !stale;
  const el = fold(
    "Machine",
    isolationField,
    h("div", { class: "field-row" }, field("CPU cores", cpu), field("Memory", memory, hints.memory)),
    field("Disk", disk, hints.disk),
    h("div", { class: "hint" }, "Leave a size empty for the deployment's default."),
  );
  const value = () => ({
    isolation: isolation.value,
    cpu: cpu.value.trim(),
    memory: memory.value.trim(),
    disk: disk.value.trim(),
  });
  return { el, value };
}

/** taskTotal counts an overview's tasks; cancelled ones are not counted. */
export function taskTotal(tasks) {
  return (tasks.queued || 0) + (tasks.running || 0) + (tasks.succeeded || 0) + (tasks.failed || 0);
}

/** tasksCard is tasks over time, with the share that failed, as on the
 *  dashboard. */
export function tasksCard({ tasks = {}, series, bucket }) {
  const total = taskTotal(tasks);
  const failRate = total ? (tasks.failed / total) * 100 : 0;
  return card("Tasks", areaChart(series, { bucket }), {
    more:
      tasks.failed > 0
        ? pill(`${num(tasks.failed)} failed (${failRate.toFixed(1)}%)`, "bad", { dot: true })
        : pill("No failures", "good", { dot: true }),
  });
}
