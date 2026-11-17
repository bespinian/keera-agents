// Schedules: the same task given to an agent at set times, such as a summary
// every weekday morning. The list and the editor are shared with an agent's
// Schedules tab.

import { api } from "../api.js";
import {
  h,
  table,
  field,
  formModal,
  fail,
  confirm,
  toast,
  iconButton,
  icons,
  busyButton,
  listHead,
  replace,
  stamp,
  dash,
  link,
  checkRow,
  debounce,
  plural,
  pad,
} from "../ui.js";
import { agentPath, taskPath } from "../common.js";

export async function schedulesView(ctx) {
  const [s, a] = await Promise.all([api.schedules(), api.agents()]);
  const schedules = s.data || [];
  const agents = a.data || [];
  ctx.setSubtitle(`${schedules.length} ${plural(schedules.length, "schedule")}`);
  const editable = agents.filter((x) => x.can_edit);
  return h(
    "div",
    {},
    listHead(
      null,
      [],
      editable.length
        ? { label: "New schedule", onClick: () => openScheduleEditor(ctx, { agents: editable, onSaved: ctx.reload }) }
        : null,
    ),
    scheduleTable(ctx, schedules, { agents, showAgent: true, onChange: ctx.reload }),
    h(
      "div",
      { class: "hint", style: { marginTop: "14px" } },
      "A schedule gives an agent the same task at set times. Its result can go to a chat.",
    ),
  );
}

/** scheduleTable lists schedules with their next run, a switch, and Run now.
 *  agents are those the editor may offer; agent is the one owner, on an
 *  agent's screen. */
export function scheduleTable(ctx, schedules, { agents = [], agent = null, showAgent = false, onChange }) {
  const canEdit = (s) => (agent ? agent.can_edit : (agents.find((a) => a.id === s.agent_id) || {}).can_edit);
  return table(
    [
      {
        label: "Name",
        cell: (s) =>
          h(
            "div",
            { class: "stack" },
            h("strong", {}, s.name),
            s.prompt ? h("div", { class: "faint clamp-2" }, s.prompt) : null,
          ),
        sortKey: (s) => s.name,
      },
      showAgent
        ? {
            label: "Agent",
            shrink: true,
            cell: (s) => link(ctx, agentPath(s.agent_id), s.agent_name),
            sortKey: (s) => s.agent_name,
          }
        : null,
      {
        label: "When",
        cell: (s) =>
          h(
            "div",
            { class: "stack" },
            h("span", { title: s.cron }, s.description || s.cron),
            h("span", { class: "faint small" }, s.timezone),
          ),
      },
      {
        label: "Next run",
        shrink: true,
        num: true,
        cell: (s) =>
          s.enabled && s.next_run_at ? stamp(s.next_run_at, { ahead: true }) : h("span", { class: "faint" }, "Off"),
        sortKey: (s) => (s.enabled ? s.next_run_at : ""),
      },
      {
        label: "Last run",
        shrink: true,
        num: true,
        cell: (s) => (s.last_run_at ? stamp(s.last_run_at) : dash()),
      },
      {
        label: "On",
        shrink: true,
        cell: (s) =>
          h("input", {
            type: "checkbox",
            checked: s.enabled,
            disabled: !canEdit(s),
            title: s.enabled ? "Runs on schedule. Untick to pause it." : "Paused. Tick to run it on schedule.",
            "aria-label": `Run ${s.name} on schedule`,
            onChange: async (e) => {
              const input = e.target;
              input.disabled = true;
              try {
                await api.updateSchedule(s.id, { enabled: input.checked });
                toast(input.checked ? `${s.name} is on` : `${s.name} is paused`, "good");
                onChange();
              } catch (err) {
                input.checked = !input.checked;
                input.disabled = false;
                toast(err.message, "bad");
              }
            },
          }),
      },
      {
        label: "",
        shrink: true,
        cell: (s) =>
          canEdit(s)
            ? h(
                "div",
                { class: "row-tight" },
                busyButton({ class: "btn btn-sm", title: "Give the agent this task now" }, "Run now", async () => {
                  const task = await api.runSchedule(s.id);
                  toast(`${s.name} started`, "good");
                  ctx.navigate(taskPath(task.id));
                }),
                iconButton(
                  icons.pencil,
                  "Edit this schedule",
                  () => openScheduleEditor(ctx, { schedule: s, agent, agents, onSaved: onChange }),
                  { label: `Edit ${s.name}` },
                ),
                iconButton(
                  icons.trash,
                  "Delete this schedule",
                  () =>
                    confirm({
                      title: `Delete ${s.name}?`,
                      body: "The agent no longer gets this task. Tasks it already ran stay in Tasks.",
                      confirmLabel: "Delete",
                      danger: true,
                      onConfirm: async () => {
                        await api.deleteSchedule(s.id);
                        onChange();
                      },
                      done: `${s.name} deleted`,
                    }),
                  { danger: true, label: `Delete ${s.name}` },
                ),
              )
            : null,
      },
    ].filter(Boolean),
    schedules,
    {
      search: schedules.length > 8 ? (s) => `${s.name} ${s.agent_name} ${s.prompt}` : null,
      searchLabel: "schedules",
      sortBy: "Next run",
      rowClass: (s) => (s.enabled ? "" : "row-faint"),
      emptyTitle: "No schedules yet",
      emptyBody: agent
        ? "Give this agent the same task at set times, such as a summary every weekday at 08:00."
        : "A schedule gives an agent the same task at set times, such as a summary every weekday at 08:00.",
    },
  );
}

/** cronProblem catches what is plainly not a cron expression before asking
 *  the server, which has the last word. */
export function cronProblem(cron) {
  const s = String(cron || "").trim();
  if (!s) return "Say when it runs, such as 0 8 * * 1-5 for weekdays at 08:00.";
  if (s.startsWith("@")) return "";
  const n = s.split(/\s+/).length;
  if (n !== 5) return `A cron expression has five fields: minute, hour, day, month and weekday. This one has ${n}.`;
  return "";
}

// A rule is when a schedule runs, as the picker shows it. It is stored as
// cron; one the picker cannot show stays as cron, under Custom.

const REPEATS = [
  ["day", "Every day"],
  ["week", "Every week"],
  ["month", "Every month"],
  ["year", "Every year"],
  ["often", "Several times a day"],
  ["custom", "Custom (cron)"],
];

/** EVERY are the gaps, in minutes, for several times a day. Each divides the
 *  hour or the day, so the runs are evenly spaced. */
export const EVERY = [5, 10, 15, 20, 30, 60, 120, 180, 240, 360, 480, 720];

// Monday first, as the server writes a week.
const WEEK = [1, 2, 3, 4, 5, 6, 0];
const DAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const MONTH_NAMES = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
];
const DOW_NAMES = { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 };
const MACROS = {
  "@yearly": "0 0 1 1 *",
  "@annually": "0 0 1 1 *",
  "@monthly": "0 0 1 * *",
  "@weekly": "0 0 * * 0",
  "@daily": "0 0 * * *",
  "@midnight": "0 0 * * *",
  "@hourly": "0 * * * *",
};

function everyLabel(minutes) {
  if (minutes < 60) return `${minutes} minutes`;
  return minutes === 60 ? "hour" : `${minutes / 60} hours`;
}

/** daysField writes days of the week as cron: ranges where three or more
 *  follow each other, * for all of them. */
export function daysField(days) {
  const set = [...new Set(days)].sort((a, b) => a - b);
  if (set.length === 7) return "*";
  const parts = [];
  for (let i = 0; i < set.length;) {
    let j = i;
    while (j + 1 < set.length && set[j + 1] === set[j] + 1) j++;
    if (j - i >= 2) parts.push(`${set[i]}-${set[j]}`);
    else parts.push(...set.slice(i, j + 1));
    i = j + 1;
  }
  return parts.join(",");
}

// parseDays reads the weekday field, or returns null if it has steps or
// anything else the picker cannot show.
function parseDays(field) {
  if (field === "*" || field === "?") return [0, 1, 2, 3, 4, 5, 6];
  const day = (s) => {
    const v = s.toLowerCase() in DOW_NAMES ? DOW_NAMES[s.toLowerCase()] : /^\d$/.test(s) ? Number(s) : null;
    return v === null || v > 7 ? null : v % 7;
  };
  const days = new Set();
  for (const part of field.split(",")) {
    const [a, b = a, ...rest] = part.split("-");
    const lo = day(a);
    const hi = day(b);
    if (rest.length || lo === null || hi === null) return null;
    // 7 is Sunday too, so 5-7 is Friday to Sunday.
    const end = b === "7" ? 7 : hi;
    if (end < lo) return null;
    for (let d = lo; d <= end; d++) days.add(d % 7);
  }
  return [...days];
}

// hourWindow reads an hour field of *, one hour or a range as the hours a
// schedule runs between. single says whether one hour is allowed.
function hourWindow(field, single = true) {
  if (field === "*") return { from: 0, to: 24 };
  const m = /^(\d+)(?:-(\d+))?$/.exec(field);
  if (!m || (!m[2] && !single)) return null;
  const from = Number(m[1]);
  const last = m[2] ? Number(m[2]) : from;
  return last < from || last > 23 ? null : { from, to: last + 1 };
}

/** ruleFromCron reads a cron expression as a rule, under Custom when the
 *  picker cannot show it. */
export function ruleFromCron(cron) {
  const src = String(cron || "").trim();
  const rule = {
    repeat: "custom",
    time: "08:00",
    days: [1, 2, 3, 4, 5],
    day: 1,
    month: 1,
    every: 60,
    from: 0,
    to: 24,
    cron: src,
  };
  const fields = (MACROS[src.toLowerCase()] || src).split(/\s+/);
  if (fields.length !== 5) return rule;
  const [mi, ho, dom, mon, dow] = fields;
  const days = parseDays(dow);
  if (!days) return rule;
  const num = (s, lo, hi) => (/^\d+$/.test(s) && Number(s) >= lo && Number(s) <= hi ? Number(s) : null);
  const minute = num(mi, 0, 59);
  const hour = num(ho, 0, 23);
  const day = num(dom, 1, 31);
  const month = num(mon, 1, 12);

  if (minute !== null && hour !== null) {
    const time = `${pad(hour)}:${pad(minute)}`;
    if (dom === "*" && mon === "*") {
      return { ...rule, repeat: dow === "*" ? "day" : "week", time, days };
    }
    if (dow !== "*" || day === null) return rule;
    if (mon === "*") return { ...rule, repeat: "month", time, day };
    if (month !== null) return { ...rule, repeat: "year", time, day, month };
    return rule;
  }
  if (dom !== "*" || mon !== "*") return rule;

  const step = /^\*\/(\d+)$/.exec(mi);
  if (step && Number(step[1]) < 60 && EVERY.includes(Number(step[1]))) {
    const window = hourWindow(ho);
    return window ? { ...rule, repeat: "often", every: Number(step[1]), days, ...window } : rule;
  }
  if (minute === 0) {
    const [range, n] = ho.split("/");
    if (n !== undefined && !/^\d+$/.test(n)) return rule;
    const every = Number(n || 1) * 60;
    const window = hourWindow(range, n === undefined);
    if (window && EVERY.includes(every)) return { ...rule, repeat: "often", every, days, ...window };
  }
  return rule;
}

/** ruleToCron writes a rule as the cron expression the server keeps. */
export function ruleToCron(rule) {
  const [hour, minute] = String(rule.time || "08:00")
    .split(":")
    .map(Number);
  const days = daysField(rule.days);
  switch (rule.repeat) {
    case "day":
      return `${minute} ${hour} * * *`;
    case "week":
      return `${minute} ${hour} * * ${days}`;
    case "month":
      return `${minute} ${hour} ${rule.day} * *`;
    case "year":
      return `${minute} ${hour} ${rule.day} ${rule.month} *`;
    case "often": {
      const allDay = rule.from === 0 && rule.to === 24;
      const range = rule.to - rule.from === 1 ? `${rule.from}` : `${rule.from}-${rule.to - 1}`;
      const hours = allDay ? "*" : range;
      if (rule.every < 60) return `*/${rule.every} ${hours} * * ${days}`;
      const n = rule.every / 60;
      if (n === 1 || rule.to - rule.from === 1) return `0 ${hours} * * ${days}`;
      return `0 ${allDay ? "*" : range}/${n} * * ${days}`;
    }
    default:
      return String(rule.cron || "").trim();
  }
}

/** ruleProblem says what is missing from a rule, or "". */
export function ruleProblem(rule) {
  if (rule.repeat === "custom") return cronProblem(rule.cron);
  if ((rule.repeat === "week" || rule.repeat === "often") && !rule.days.length) return "Pick at least one day.";
  if (rule.repeat === "often" && rule.to <= rule.from) return "The end of the day comes before its start.";
  if (rule.repeat !== "often" && !/^\d\d:\d\d$/.test(rule.time)) return "Say at what time it runs.";
  return "";
}

/** recurrencePicker asks when a schedule runs, as a calendar asks when an
 *  event repeats. onChange runs after every change. */
function recurrencePicker(cron, onChange) {
  const options = (items, chosen) =>
    items.map(([value, label]) => h("option", { value, selected: String(value) === String(chosen) }, label));
  const hours = (from, to) => Array.from({ length: to - from + 1 }, (_, i) => [from + i, `${pad(from + i)}:00`]);

  const repeat = h("select", { class: "select" }, options(REPEATS));
  const time = h("input", { class: "input", type: "time", required: true });
  const every = h("select", { class: "select" }, options(EVERY.map((m) => [m, everyLabel(m)])));
  const month = h("select", { class: "select" }, options(MONTH_NAMES.map((name, i) => [i + 1, name])));
  const day = h("select", { class: "select" }, options(Array.from({ length: 31 }, (_, i) => [i + 1, String(i + 1)])));
  const dayHint = h("div", { class: "hint" });
  const from = h("select", { class: "select" }, options(hours(0, 23)));
  const to = h("select", { class: "select" }, options(hours(1, 24)));
  const custom = h("input", {
    class: "input mono",
    placeholder: "0 8 * * 1-5",
    spellcheck: "false",
    autocomplete: "off",
  });

  const chosenDays = new Set();
  const dayButtons = WEEK.map((d) =>
    h(
      "button",
      {
        type: "button",
        title: DAY_NAMES[d],
        "aria-label": DAY_NAMES[d],
        onClick: (e) => {
          if (chosenDays.has(d)) chosenDays.delete(d);
          else chosenDays.add(d);
          e.currentTarget.setAttribute("aria-pressed", String(chosenDays.has(d)));
          onChange();
        },
      },
      DAY_NAMES[d].slice(0, 3),
    ),
  );
  const days = h("div", { class: "seg", role: "group", "aria-label": "Days" }, dayButtons);

  const full = { gridColumn: "1 / -1" };
  const parts = {
    time: field("At", time),
    every: field("Every", every),
    days: h("div", { class: "field", style: full }, h("label", {}, "On"), h("div", {}, days)),
    month: field("In", month),
    day: field("On day", day, dayHint),
    from: field("From", from),
    to: field("Until", to),
    custom: field("Cron expression", custom, "Minute, hour, day of month, month, weekday."),
  };
  const shown = {
    day: ["time"],
    week: ["time", "days"],
    month: ["time", "day"],
    year: ["time", "month", "day"],
    often: ["every", "days", "from", "to"],
    custom: ["custom"],
  };

  function read() {
    return {
      repeat: repeat.value,
      time: time.value,
      days: [...chosenDays],
      day: Number(day.value),
      month: Number(month.value),
      every: Number(every.value),
      from: Number(from.value),
      to: Number(to.value),
      cron: custom.value,
    };
  }

  function load(rule) {
    repeat.value = rule.repeat;
    time.value = rule.time;
    day.value = String(rule.day);
    month.value = String(rule.month);
    every.value = String(rule.every);
    from.value = String(rule.from);
    to.value = String(rule.to);
    custom.value = rule.cron;
    chosenDays.clear();
    rule.days.forEach((d) => chosenDays.add(d));
    dayButtons.forEach((b, i) => b.setAttribute("aria-pressed", String(chosenDays.has(WEEK[i]))));
    show();
  }

  function show() {
    for (const [name, el] of Object.entries(parts)) el.hidden = !shown[repeat.value].includes(name);
    dayHint.textContent = Number(day.value) > 28 ? `Months without day ${day.value} are skipped.` : "";
  }

  // The picker keeps what was chosen when switching to Custom and back, so
  // the cron shown is the same schedule, and a cron that fits fills it in.
  let last = repeat.value;
  repeat.addEventListener("change", () => {
    if (repeat.value === "custom") custom.value = ruleToCron({ ...read(), repeat: last });
    else if (last === "custom") {
      const rule = ruleFromCron(custom.value);
      if (rule.repeat !== "custom") load({ ...rule, repeat: repeat.value });
    }
    last = repeat.value;
    show();
    onChange();
  });
  for (const el of [time, every, month, day, from, to, custom]) {
    el.addEventListener(el === custom || el === time ? "input" : "change", () => {
      show();
      onChange();
    });
  }

  load(ruleFromCron(cron));
  last = repeat.value;

  return {
    // Each field keeps its own space below, so rows need no gap of their own.
    el: h("div", { class: "field-row", style: { rowGap: "0" } }, field("Repeats", repeat), Object.values(parts)),
    cron: () => ruleToCron(read()),
    problem: () => ruleProblem(read()),
    focus: () => (repeat.value === "custom" ? custom : repeat).focus(),
  };
}

/** timeZones lists the zones the browser knows, with extra ones included,
 *  such as the organisation's default and the schedule's own. */
export function timeZones(...extra) {
  let zones = [];
  try {
    zones = Intl.supportedValuesOf("timeZone");
  } catch {
    zones = [];
  }
  if (!zones.length) zones = ["Europe/Zurich", "Europe/Berlin", "Europe/London", "America/New_York", "Asia/Singapore"];
  return [...new Set(["UTC", ...zones, ...extra.filter(Boolean)])].sort((a, b) =>
    a === "UTC" ? -1 : b === "UTC" ? 1 : a.localeCompare(b),
  );
}

/** inZone writes a time as it reads in the schedule's own time zone. */
export function inZone(iso, timeZone) {
  const opts = { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" };
  try {
    return new Date(iso).toLocaleString(undefined, { ...opts, timeZone });
  } catch {
    return new Date(iso).toLocaleString(undefined, opts);
  }
}

/** openScheduleEditor creates or changes a schedule. agent fixes the agent;
 *  otherwise agents lists those to pick from. */
export function openScheduleEditor(ctx, { schedule = null, agent = null, agents = [], onSaved }) {
  const me = ctx.state.me;
  const s = schedule || {};
  const fallbackZone = me.timezone_default || Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  const zone = s.timezone || fallbackZone;

  const name = h("input", { class: "input", value: s.name || "", placeholder: "Morning summary", maxlength: "80" });
  const agentSelect =
    agent || schedule
      ? null
      : h(
          "select",
          { class: "select" },
          agents.map((a) => h("option", { value: a.id }, a.name)),
        );
  const when = recurrencePicker(s.cron || "0 8 * * 1-5", () => check());
  const tz = h(
    "select",
    { class: "select" },
    timeZones(zone, fallbackZone).map((z) => h("option", { value: z, selected: z === zone }, z.replaceAll("_", " "))),
  );
  const preview = h("div", { class: "preview", "aria-live": "polite" });
  const prompt = h(
    "textarea",
    {
      class: "input",
      rows: 5,
      placeholder: "Summarise yesterday's support tickets: what came in, what is still open, and anything urgent.",
    },
    s.prompt || "",
  );
  const reply = h("select", { class: "select" });
  const enabled = checkRow("On", "Runs on schedule. Untick to keep it without running it.", s.enabled !== false);

  // Previews can overlap while typing, so only the newest is shown.
  let inflight = null;
  async function runPreview() {
    const problem = when.problem();
    if (problem) {
      preview.className = "preview preview-bad";
      replace(preview, problem);
      return;
    }
    if (inflight) inflight.abort();
    const ac = (inflight = new AbortController());
    try {
      const res = await api.previewSchedule(when.cron(), tz.value, ac.signal);
      if (ac.signal.aborted) return;
      preview.className = "preview";
      replace(
        preview,
        h("strong", {}, res.description || "Valid"),
        (res.next || []).length
          ? h(
              "ol",
              {},
              res.next.map((t) => h("li", {}, inZone(t, tz.value))),
            )
          : null,
      );
    } catch (err) {
      if (err.name === "AbortError") return;
      preview.className = "preview preview-bad";
      replace(preview, err.message);
    }
  }
  const check = debounce(runPreview, 350);
  tz.addEventListener("change", check);
  runPreview();

  // The reply goes to one of the agent's chats, or nowhere.
  async function loadReplies() {
    const id = agent ? agent.id : schedule ? schedule.agent_id : agentSelect.value;
    let bindings = agent ? agent.bindings : null;
    if (!bindings && id) {
      try {
        bindings = (await api.agent(id)).bindings;
      } catch {
        bindings = [];
      }
    }
    // A chat that takes every conversation names none to post in.
    const chats = (bindings || []).filter((b) => b.mode === "chat" && b.target !== "*");
    replace(
      reply,
      h("option", { value: "" }, "Nowhere: keep the result in Tasks"),
      chats.map((b) =>
        h(
          "option",
          { value: b.id, selected: b.id === s.reply_binding_id },
          `${b.label || b.target} · ${b.connection_name}`,
        ),
      ),
    );
  }
  if (agentSelect) agentSelect.addEventListener("change", loadReplies);
  loadReplies();

  formModal({
    title: schedule ? `Edit ${s.name}` : "New schedule",
    subtitle: agent ? `For ${agent.name}` : schedule ? `For ${s.agent_name}` : null,
    wide: true,
    body: h(
      "form",
      {},
      field("Name", name),
      agentSelect ? field("Agent", agentSelect) : null,
      when.el,
      field("Time zone", tz, "The times are read in this zone, summer time included."),
      h("div", { class: "field" }, preview),
      field("Task", prompt, "What the agent is asked each time."),
      field("Send the result to", reply, "One of the agent's chats. Add one on the agent's Connectors tab."),
      h("div", { class: "field" }, enabled.el),
    ),
    submitLabel: schedule ? "Save" : "Create schedule",
    done: (res) => (schedule ? `${res.name} saved` : `${res.name} created`),
    submit: async () => {
      inflight?.abort();
      if (!name.value.trim()) fail(name, "Give the schedule a name.");
      if (when.problem()) fail(when, when.problem());
      if (!prompt.value.trim()) fail(prompt, "Say what the agent should do each time.");
      const body = {
        name: name.value.trim(),
        cron: when.cron(),
        timezone: tz.value,
        prompt: prompt.value.trim(),
        reply_binding_id: reply.value,
        enabled: enabled.input.checked,
      };
      const res = schedule
        ? await api.updateSchedule(schedule.id, body)
        : await api.createSchedule({ ...body, agent_id: agent ? agent.id : agentSelect.value });
      onSaved();
      return res;
    },
  });
}
