// An agent's connectors: the chats it answers in, the folders it watches or
// works in, its repositories and the services it calls, each through one of
// the organisation's connectors. The API calls each of these a binding.

import { api } from "../api.js";
import {
  h,
  table,
  field,
  formModal,
  fail,
  confirm,
  iconButton,
  icon,
  icons,
  kindMark,
  replace,
  sectionHead,
  pill,
  stamp,
  link,
  plural,
  banner,
  isAdmin,
} from "../ui.js";
import { kindLabel, MODES } from "../common.js";

export async function bindingsTab(ctx, agent) {
  const [conns, kindsRes] = await Promise.all([
    api.connections().catch(() => ({ data: [] })),
    api.connectionKinds().catch(() => ({ data: [] })),
  ]);
  const kinds = kindsRes.data || [];
  // Only connectors it can use: a workspace has no use for a chat.
  const connections = (conns.data || []).filter(
    (c) =>
      agent.kind !== "workspace" ||
      modesFor(
        kinds.find((k) => k.kind === c.kind),
        true,
      ).length,
  );
  const bindings = agent.bindings || [];
  const admin = isAdmin(ctx);
  const noShell = lacksShell(agent) && bindings.some((b) => b.mode === "repo");

  const add =
    agent.can_edit && connections.length
      ? h(
          "button",
          { class: "btn btn-primary", onClick: () => addBinding(ctx, agent, connections, kinds) },
          icon(icons.plus),
          "Add connector",
        )
      : null;

  return h(
    "div",
    { class: "stack", style: { gap: "12px" } },
    sectionHead(`${bindings.length} ${plural(bindings.length, "connector")}`, add, { margin: "0" }),
    !connections.length
      ? banner(
          "info",
          "There are no connectors yet, such as to a chat, a file store or a CRM. ",
          admin ? [link(ctx, "/connectors", "Add a connector"), " first."] : "An administrator adds connectors.",
        )
      : null,
    noShell ? banner("warn", NO_SHELL) : null,
    table(
      [
        {
          label: "Where",
          sortKey: (b) => (b.label || targetText(b, kinds)).toLowerCase(),
          cell: (b) =>
            h(
              "div",
              { class: "row" },
              h("span", { class: "tile-mark tile-mark-sm" }, kindMark(b.kind, kinds)),
              h(
                "div",
                { class: "stack" },
                h("strong", {}, b.label || targetText(b, kinds)),
                h(
                  "span",
                  { class: "faint small" },
                  b.connection_name,
                  " · ",
                  kindLabel(b.kind, kinds),
                  b.label && b.target !== b.label ? [" · ", h("span", { class: "mono" }, targetText(b, kinds))] : null,
                ),
              ),
            ),
        },
        {
          label: "Mode",
          shrink: true,
          sortKey: (b) => b.mode,
          cell: (b) =>
            pill((MODES[b.mode] || [b.mode])[0], b.mode === "chat" ? "accent" : b.mode === "write" ? "warn" : ""),
        },
        {
          label: "Task",
          cell: (b) =>
            b.mode === "watch" && b.prompt
              ? h("div", { class: "faint clamp-2" }, b.prompt)
              : h(
                  "span",
                  { class: "faint" },
                  b.mode === "chat" && b.target === "*"
                    ? "Answers wherever it is asked, but cannot post on its own."
                    : (MODES[b.mode] || ["", ""])[1],
                ),
        },
        {
          label: "Added",
          shrink: true,
          num: true,
          sortKey: (b) => b.created_at,
          sortDir: "desc",
          cell: (b) => stamp(b.created_at),
        },
        {
          label: "",
          shrink: true,
          cell: (b) =>
            agent.can_edit
              ? iconButton(
                  icons.trash,
                  "Remove this connector",
                  () =>
                    confirm({
                      title: `Remove ${b.label || targetText(b, kinds)}?`,
                      body:
                        b.mode === "chat"
                          ? `${agent.name} stops answering there. Schedules that reply there keep their results in Tasks.`
                          : b.mode === "repo"
                            ? `${agent.name} can no longer clone or push to it. Its branches and pull requests stay.`
                            : b.mode === "read" || b.mode === "write"
                              ? `${agent.name} loses the tools of this connector. What it changed there stays.`
                              : `${agent.name} stops using this folder. Its files stay where they are.`,
                      confirmLabel: "Remove",
                      danger: true,
                      onConfirm: () => api.deleteBinding(b.id),
                      done: "Connector removed",
                      ctx,
                    }),
                  { danger: true, label: `Remove ${b.label || targetText(b, kinds)}` },
                )
              : null,
        },
      ],
      bindings,
      {
        sortBy: "Added",
        sortDir: "desc",
        emptyTitle: "No connectors yet",
        emptyBody:
          "Connectors are where the agent works: a chat it answers in, a folder it watches, a repository it codes in, " +
          "or a service it calls, such as Jira or AWS.",
      },
    ),
  );
}

const NO_SHELL =
  "This agent has no shell, so it cannot clone repositories, only work with their issues and pull requests. " +
  "Turn on Shell in its settings.";

export function targetText(b, kinds) {
  if (b.target !== "*") return b.target;
  return b.mode === "chat" ? "Every chat the bot is in" : `All of ${kindLabel(b.kind, kinds)}`;
}

/** lacksShell is true for an agent without a shell, which cannot clone a
 *  repository. A workspace's person has a terminal of their own. */
function lacksShell(agent) {
  return agent.kind !== "workspace" && !(agent.builtins || []).includes("shell");
}

/** modesFor lists what an agent can do with a kind's connector. A
 *  workspace takes no tasks, so it neither answers chats nor watches
 *  folders. */
export function modesFor(kind, workspace = false) {
  if (!kind) return [];
  return [
    ...(kind.chat && !workspace ? ["chat"] : []),
    ...(kind.files ? (workspace ? ["files"] : ["watch", "files"]) : []),
    ...(kind.code ? ["repo"] : []),
    ...(kind.api ? ["read", "write"] : []),
  ];
}

function addBinding(ctx, agent, connections, kinds) {
  const kindOf = (c) => kinds.find((k) => k.kind === c.kind) || { kind: c.kind, chat: true };
  const connection = h(
    "select",
    { class: "select" },
    connections.map((c) => h("option", { value: c.id }, `${c.name} · ${kindLabel(c.kind, kinds)}`)),
  );
  const mode = h("select", { class: "select" });
  const target = h("input", { class: "input", autocomplete: "off", spellcheck: "false" });
  const label = h("input", { class: "input", placeholder: "Sales team chat" });
  const prompt = h("textarea", {
    class: "input",
    rows: 4,
    placeholder: "Read the invoice, check it against its purchase order, and move it to /Checked.",
  });

  const targetHint = h("div", { class: "hint" });
  const actsAs = h("div", { class: "hint" });
  const targetLabel = h("label", {}, "Target");
  const browse = h("button", {
    type: "button",
    class: "btn",
    onClick: () => (kindOf(chosen()).channels ? openChannels() : openFolders()),
  });
  const browser = h("div", { class: "field", hidden: true });
  const targetField = h(
    "div",
    { class: "field" },
    targetLabel,
    h("div", { class: "row-tight" }, target, browse),
    targetHint,
  );
  target.id = "binding-target";
  targetLabel.setAttribute("for", target.id);
  const promptField = field("What to do with each new file", prompt, "The file comes with the task.");
  const shellHint = banner("warn", NO_SHELL);

  const chosen = () => connections.find((c) => c.id === connection.value);

  function syncConnection() {
    const kind = kindOf(chosen());
    const c = chosen() || {};
    actsAs.hidden = !c.per_user;
    replace(
      actsAs,
      c.per_user
        ? "It acts as each person who gives the agent work, with their own account, and a folder watch as you. " +
            "Each needs to connect theirs under Connectors" +
            (c.account ? "." : ", you too, before adding it.")
        : "",
    );
    const keep = mode.value;
    const modes = modesFor(kind, agent.kind === "workspace");
    replace(
      mode,
      modes.map((m) => h("option", { value: m, selected: m === keep }, `${MODES[m][0]}: ${MODES[m][1]}`)),
    );
    targetLabel.textContent = kind.target_label || (kind.code ? "Repository" : kind.files ? "Folder" : "Conversation");
    // An API kind with nothing to narrow to reaches all it can.
    targetField.hidden = !!kind.api && !kind.target_label;
    browse.hidden = !kind.files && !kind.channels;
    replace(browse, icon(kind.channels ? icons.chat : icons.folder), "Browse");
    browser.hidden = true;
    syncMode();
  }

  function syncMode() {
    const kind = kindOf(chosen());
    const chat = mode.value === "chat";
    const any = chat || (kind.api && kind.any_target);
    promptField.hidden = mode.value !== "watch";
    shellHint.hidden = !(mode.value === "repo" && lacksShell(agent));
    target.placeholder = any ? "*" : "";
    replace(
      targetHint,
      kind.target_help || "",
      any
        ? [
            kind.target_help ? " " : "",
            h("code", {}, "*"),
            chat
              ? " is every conversation the bot is in, direct messages included. It only answers there. " +
                  "To have it post on its own, such as a schedule's results, name a channel" +
                  (kind.channels ? " or your direct messages (Browse)." : ".")
              : " is everything the connector reaches.",
          ]
        : null,
    );
    if (chat && !target.value) target.value = "*";
    if (!any && target.value === "*") target.value = "";
  }

  connection.addEventListener("change", () => {
    target.value = "";
    syncConnection();
  });
  mode.addEventListener("change", syncMode);
  syncConnection();

  /** useTarget fills in what a picker chose. */
  function useTarget(id, name) {
    target.value = id;
    if (!label.value) label.value = name;
    browser.hidden = true;
    target.focus();
  }

  /** openChannels lists where the bot can post: direct messages with you,
   *  then its channels. */
  async function openChannels() {
    browser.hidden = false;
    replace(browser, h("div", { class: "faint small" }, "Loading channels…"));
    let res;
    try {
      res = await api.channels(connection.value);
    } catch (err) {
      replace(browser, banner("bad", err.message));
      return;
    }
    const items = res.data || [];
    replace(
      browser,
      res.notice ? banner("warn", res.notice) : null,
      items.length
        ? h(
            "div",
            { class: "folder-list" },
            items.map((c) =>
              h(
                "button",
                {
                  type: "button",
                  class: "folder-item",
                  onClick: () => useTarget(c.id, c.name),
                },
                icon(c.direct ? icons.person : icons.chat),
                h("span", {}, c.name),
                h("span", { class: "spacer" }),
                c.private ? h("span", { class: "faint small" }, "private") : null,
              ),
            ),
          )
        : h("div", { class: "faint small" }, "The bot is in no channels yet. Invite it to one first."),
    );
  }

  /** openFolders is a small file browser over the connector's folders. */
  async function openFolders() {
    const trail = [{ id: "", name: "Top" }];
    browser.hidden = false;
    async function list() {
      const here = trail[trail.length - 1];
      replace(browser, h("div", { class: "faint small" }, "Loading folders…"));
      let items;
      try {
        items = ((await api.folders(connection.value, here.id)).data || []).filter((f) => f.folder);
      } catch (err) {
        replace(browser, banner("bad", err.message));
        return;
      }
      const path =
        "/" +
        trail
          .slice(1)
          .map((t) => t.name)
          .join("/");
      replace(
        browser,
        h(
          "div",
          { class: "row", style: { marginBottom: "6px", flexWrap: "wrap" } },
          trail.length > 1
            ? iconButton(icons.back, "Up one folder", () => {
                trail.pop();
                list();
              })
            : null,
          h("span", { class: "mono small" }, path),
          h("div", { class: "spacer" }),
          // The top is where every folder lives, not one to watch.
          trail.length === 1
            ? null
            : h(
                "button",
                {
                  type: "button",
                  class: "btn btn-sm btn-primary",
                  onClick: () => useTarget(here.id, path),
                },
                "Use this folder",
              ),
        ),
        items.length
          ? h(
              "div",
              { class: "folder-list" },
              items.map((f) =>
                h(
                  "button",
                  {
                    type: "button",
                    class: "folder-item",
                    onClick: () => {
                      trail.push({ id: f.id, name: f.name });
                      list();
                    },
                  },
                  icon(icons.folder),
                  h("span", {}, f.name),
                  h("span", { class: "spacer" }),
                  icon(icons.right),
                ),
              ),
            )
          : h("div", { class: "faint small" }, "No folders in here."),
      );
    }
    list();
  }

  formModal({
    title: `Add a connector to ${agent.name}`,
    wide: true,
    body: h(
      "form",
      {},
      field("Connector", connection, actsAs),
      field("Mode", mode),
      shellHint,
      targetField,
      browser,
      field("Display name", label, "Optional. How it shows here, such as #sales or Invoices."),
      promptField,
    ),
    submitLabel: "Add connector",
    done: "Connector added",
    ctx,
    submit: () => {
      if (targetField.hidden) target.value = "*";
      if (!target.value.trim()) fail(target, `Fill in ${targetLabel.textContent.toLowerCase()}.`);
      if (mode.value === "watch" && !prompt.value.trim()) {
        fail(prompt, "Say what the agent should do with each new file.");
      }
      const add = agent.kind === "workspace" ? api.addWorkspaceBinding : api.addBinding;
      return add(agent.id, {
        connection_id: connection.value,
        target: target.value.trim(),
        label: label.value.trim(),
        mode: mode.value,
        prompt: mode.value === "watch" ? prompt.value.trim() : "",
      });
    },
  });
}
