// An agent's channels: the chats it answers in, the folders it watches or
// works in, its repositories and the services it calls, each on one of the
// organisation's connectors.

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

export async function channelsTab(ctx, agent) {
  const [conns, kindsRes] = await Promise.all([
    api.connections().catch(() => ({ data: [] })),
    api.connectionKinds().catch(() => ({ data: [] })),
  ]);
  const connections = conns.data || [];
  const kinds = kindsRes.data || [];
  const bindings = agent.bindings || [];
  const admin = isAdmin(ctx);

  const add =
    agent.can_edit && connections.length
      ? h(
          "button",
          { class: "btn btn-primary", onClick: () => addChannel(ctx, agent, connections, kinds) },
          icon(icons.plus),
          "Add channel",
        )
      : null;

  return h(
    "div",
    { class: "stack", style: { gap: "12px" } },
    sectionHead(`${bindings.length} ${plural(bindings.length, "channel")}`, add, { margin: "0" }),
    !connections.length
      ? banner(
          "info",
          "A channel needs a connector, such as to a chat, a file store or a CRM. ",
          admin ? [link(ctx, "/connectors", "Add a connector"), " first."] : "An administrator adds connectors.",
        )
      : null,
    table(
      [
        {
          label: "Where",
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
          cell: (b) =>
            pill((MODES[b.mode] || [b.mode])[0], b.mode === "chat" ? "accent" : b.mode === "write" ? "warn" : ""),
        },
        {
          label: "Task",
          cell: (b) =>
            b.mode === "watch" && b.prompt
              ? h("div", { class: "faint clamp-2" }, b.prompt)
              : h("span", { class: "faint" }, (MODES[b.mode] || ["", ""])[1]),
        },
        {
          label: "Added",
          shrink: true,
          num: true,
          cell: (b) => stamp(b.created_at),
        },
        {
          label: "",
          shrink: true,
          cell: (b) =>
            agent.can_edit
              ? iconButton(
                  icons.trash,
                  "Remove this channel",
                  () =>
                    confirm({
                      title: "Remove this channel?",
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
                      done: "Channel removed",
                      ctx,
                    }),
                  { danger: true, label: `Remove ${b.label || targetText(b, kinds)}` },
                )
              : null,
        },
      ],
      bindings,
      {
        emptyTitle: "No channels yet",
        emptyBody:
          "A channel is where the agent works: a chat it answers in, a folder it watches, a repository it codes in, " +
          "or a service it calls, such as Jira or AWS.",
      },
    ),
  );
}

function targetText(b, kinds) {
  if (b.target !== "*") return b.target;
  return b.mode === "chat" ? "Every chat the bot is in" : `All of ${kindLabel(b.kind, kinds)}`;
}

/** modesFor lists what an agent can do with a kind's connector. */
export function modesFor(kind) {
  if (!kind) return [];
  return [
    ...(kind.chat ? ["chat"] : []),
    ...(kind.files ? ["watch", "files"] : []),
    ...(kind.code ? ["repo"] : []),
    ...(kind.api ? ["read", "write"] : []),
  ];
}

function addChannel(ctx, agent, connections, kinds) {
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
  const targetLabel = h("label", {}, "Target");
  const browse = h(
    "button",
    { type: "button", class: "btn", onClick: () => openFolders() },
    icon(icons.folder),
    "Browse",
  );
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

  const chosen = () => connections.find((c) => c.id === connection.value);

  function syncConnection() {
    const kind = kindOf(chosen());
    const keep = mode.value;
    const modes = modesFor(kind);
    replace(
      mode,
      modes.map((m) => h("option", { value: m, selected: m === keep }, `${MODES[m][0]}: ${MODES[m][1]}`)),
    );
    targetLabel.textContent = kind.target_label || (kind.code ? "Repository" : kind.files ? "Folder" : "Conversation");
    // An API kind with nothing to narrow to reaches all it can.
    targetField.hidden = !!kind.api && !kind.target_label;
    browse.hidden = !kind.files;
    browser.hidden = true;
    syncMode();
  }

  function syncMode() {
    const kind = kindOf(chosen());
    const chat = mode.value === "chat";
    const any = chat || (kind.api && kind.any_target);
    promptField.hidden = mode.value !== "watch";
    target.placeholder = any ? "*" : "";
    replace(
      targetHint,
      kind.target_help || "",
      any
        ? [
            kind.target_help ? " " : "",
            h("code", {}, "*"),
            chat
              ? " is every conversation the bot is in, direct messages included."
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
                  onClick: () => {
                    target.value = here.id;
                    if (!label.value) label.value = path;
                    browser.hidden = true;
                    target.focus();
                  },
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
    title: `Add a channel to ${agent.name}`,
    wide: true,
    body: h(
      "form",
      {},
      field("Connector", connection),
      field("Mode", mode),
      targetField,
      browser,
      field("Name", label, "Optional. How the channel shows here, such as #sales or Invoices."),
      promptField,
    ),
    submitLabel: "Add channel",
    done: "Channel added",
    ctx,
    submit: () => {
      if (targetField.hidden) target.value = "*";
      if (!target.value.trim()) fail(target, `Fill in ${targetLabel.textContent.toLowerCase()}.`);
      if (mode.value === "watch" && !prompt.value.trim()) {
        fail(prompt, "Say what the agent should do with each new file.");
      }
      return api.addBinding(agent.id, {
        connection_id: connection.value,
        target: target.value.trim(),
        label: label.value.trim(),
        mode: mode.value,
        prompt: mode.value === "watch" ? prompt.value.trim() : "",
      });
    },
  });
}
