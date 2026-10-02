// Words, tones and small cells that several screens share, so an agent's or
// a task's state reads the same everywhere.

import { h, pill, card, icon, icons, duration, num } from "./ui.js";
import { areaChart } from "./chart.js";

/** agentPath is an agent's screen, or one of its tabs. */
export function agentPath(id, tab) {
  return `/agents/${encodeURIComponent(id)}` + (tab ? `/${tab}` : "");
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

/** KIND_LABELS name connector kinds before /connection-kinds has answered. */
const KIND_LABELS = {
  teams: "Microsoft Teams",
  googlechat: "Google Chat",
  slack: "Slack",
  gdrive: "Google Drive",
  onedrive: "OneDrive",
  github: "GitHub",
  gitlab: "GitLab",
  jira: "Jira",
  confluence: "Confluence",
  gdocs: "Google Docs",
  gsheets: "Google Sheets",
  gslides: "Google Slides",
  gcalendar: "Google Calendar",
  outlook: "Outlook",
  hubspot: "HubSpot",
  salesforce: "Salesforce",
  kubernetes: "Kubernetes",
  aws: "AWS",
  azure: "Azure",
  gcp: "Google Cloud",
};

export function kindLabel(kind, kinds) {
  const d = (kinds || []).find((k) => k.kind === kind);
  return (d && d.label) || KIND_LABELS[kind] || kind;
}

/** BUILTINS are the tools every agent may be given, each in one line. */
export const BUILTINS = {
  memory: ["Memory", "Remembers notes between tasks."],
  files: ["Files", "Reads and writes files in its own workspace."],
  shell: ["Shell", "Runs commands in its own machine."],
  web: ["Web", "Searches the web and reads pages."],
};

/** ACCESS is who may give an agent work. */
export const ACCESS = {
  owner: "Only its owner",
  org: "Everyone in the organisation",
  people: "Only the people listed",
};

/** MODES are what an agent does with a channel. */
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
  standard: "Standard: a container",
  isolated: "Isolated: a container in a sandbox",
  vm: "VM: its own virtual machine",
};

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
