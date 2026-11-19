// Workspaces: machines of one's own to work in, with a coding agent. Each
// person sees their own; an administrator can see everyone's. People reach
// them with the keera command line, which also sets up SSH.

import { api } from "../api.js";
import {
  h,
  table,
  rowLink,
  field,
  formModal,
  listHead,
  stamp,
  fail,
  dash,
  icon,
  icons,
  plural,
  toast,
  isAdmin,
  sectionHead,
  iconButton,
  confirm,
  modeTiles,
  pill,
} from "../ui.js";
import { agentStatePill, workspacePath, machineFold, EXPERIENCES, shellArg } from "../common.js";
import { noRuntime } from "./agents.js";
import { codeBlock } from "../markdown.js";

export async function workspacesView(ctx) {
  const all = isAdmin(ctx) && new URLSearchParams(location.search).get("all") === "1";
  const [list, keys] = await Promise.all([api.workspaces(all), api.sshKeys()]);
  const workspaces = list.data || [];
  ctx.setSubtitle(`${workspaces.length} ${plural(workspaces.length, "workspace")}`);

  return h(
    "div",
    { class: "stack", style: { gap: "16px" } },
    listHead(null, null, { label: "New workspace", onClick: () => openNewWorkspace(ctx) }),
    noRuntime(ctx.state.me),
    workspaceTable(ctx, workspaces, all),
    sshKeysCard(ctx, keys.data || []),
  );
}

function workspaceTable(ctx, workspaces, all) {
  return table(
    [
      {
        label: "Name",
        cell: (w) =>
          h(
            "div",
            { class: "row" },
            h("span", { class: "tile-mark tile-mark-sm" }, icon(EXPERIENCES[w.experience]?.icon || icons.workspace)),
            h(
              "div",
              { class: "stack" },
              rowLink(ctx, workspacePath(w.id), w.name),
              h("span", { class: "faint small" }, w.model),
              w.description ? h("div", { class: "faint small clamp-2" }, w.description) : null,
            ),
          ),
        sortKey: (w) => w.name,
      },
      {
        label: "State",
        shrink: true,
        cell: (w) => h("div", { class: "row-tight" }, w.enabled === false ? pill("Stopped") : agentStatePill(w.state)),
        sortKey: (w) => w.state,
      },
      {
        label: "Experience",
        shrink: true,
        cell: (w) => h("span", { class: "muted" }, EXPERIENCES[w.experience]?.name || w.experience),
        sortKey: (w) => w.experience,
      },
      all
        ? {
            label: "Owner",
            shrink: true,
            cell: (w) => h("span", { class: "muted" }, w.owner_email),
            sortKey: (w) => w.owner_email,
          }
        : null,
      {
        label: "Last used",
        shrink: true,
        num: true,
        cell: (w) => (w.last_active_at ? stamp(w.last_active_at) : dash()),
        sortKey: (w) => w.last_active_at || "",
        sortDir: "desc",
      },
    ].filter(Boolean),
    workspaces,
    {
      ctx,
      open: (w) => workspacePath(w.id),
      sortBy: "Name",
      rowClass: (w) => (w.enabled === false ? "row-faint" : ""),
      emptyTitle: all ? "No workspaces in the organisation" : "No workspaces yet",
      emptyBody: all
        ? null
        : "A workspace is a machine of your own to work in, with a coding agent that uses your models " +
          "and the connectors you give it. Open it in your browser or over SSH.",
      // Another list, fetched again, as the control plane sends only one's
      // own unless asked.
      toggles: isAdmin(ctx)
        ? [
            {
              label: "Show everyone's",
              on: all,
              onChange: (v) => ctx.navigate("/workspaces" + (v ? "?all=1" : "")),
            },
          ]
        : [],
    },
  );
}

/** openNewWorkspace asks for a new workspace and opens it once created. */
async function openNewWorkspace(ctx) {
  let form;
  try {
    form = await workspaceForm(ctx, null);
  } catch (err) {
    toast(err.message, "bad");
    return;
  }
  formModal({
    title: "New workspace",
    subtitle: "A machine of your own, with a coding agent. Only you can get in.",
    wide: true,
    body: form.el,
    submitLabel: "Create workspace",
    submit: () => api.createWorkspace(form.values()),
    done: (w) => {
      ctx.navigate(workspacePath(w.id));
      return `${w.name} created`;
    },
  });
  form.first.focus();
}

/** workspaceForm is a workspace's settings, for a new one or an existing one.
 *  values() returns the body to send, or throws after focusing the field at
 *  fault. */
export async function workspaceForm(ctx, workspace) {
  const w = workspace || { experience: "" };
  const catalog = await api.catalog("");

  const name = h("input", { class: "input", value: w.name || "", maxlength: "60" });
  // The placeholder names what it opens in, such as My VS Code.
  const namePlaceholder = (exp) => (name.placeholder = EXPERIENCES[exp] ? `My ${EXPERIENCES[exp].name}` : "");
  namePlaceholder(w.experience);
  const description = h("input", {
    class: "input",
    value: w.description || "",
    placeholder: "The payments API",
  });

  // A new workspace asks first what it opens in, as the gateway asks a new
  // step's kind, and shows the rest once chosen.
  const rest = h("div", { hidden: !workspace, style: { marginTop: workspace ? "" : "14px" } });
  let experience = w.experience;
  const tiles = modeTiles(
    Object.entries(EXPERIENCES).map(([value, e]) => ({ value, ...e })),
    experience || null,
    (value) => {
      experience = value;
      namePlaceholder(value);
      if (!rest.hidden) return;
      rest.hidden = false;
      name.focus();
    },
  );
  if (experience) tiles.choose(experience);

  const model = h(
    "select",
    { class: "select" },
    (catalog.models || []).map((m) => h("option", { value: m.alias, selected: m.alias === w.model }, m.alias)),
  );

  const machine = machineFold(ctx.state.me, w.class || {}, {
    memory: "Such as 2Gi or 8Gi.",
    disk: "Your files and what you install. Fixed once the workspace exists.",
    diskSize: "5Gi",
  });

  rest.append(
    field("Name", name, "You reach it by this name, as in ssh api.keera. Unique among your workspaces."),
    field("Description", description, "Optional. What you work on in it."),
    field("Model", model, "The one the coding agent starts with. You can pick any other your key may use in it."),
    machine.el,
  );

  const el = workspace
    ? h(
        "form",
        {},
        h(
          "div",
          { class: "field" },
          h("label", {}, "What you work in"),
          tiles.tiles,
          tiles.chosenRow,
          h("div", { class: "hint" }, "Changing it later starts the machine again. Your files stay."),
        ),
        rest,
      )
    : h("form", {}, tiles.tiles, tiles.chosenRow, rest);

  function values() {
    // Before the name, which stays hidden until this is chosen.
    if (!experience) fail(tiles.tiles.firstChild, "Choose the terminal or VS Code.");
    const n = name.value.trim();
    if (!n) fail(name, "Give the workspace a name.");
    const body = {
      name: n,
      description: description.value.trim(),
      experience,
      class: machine.value(),
    };
    if (model.value) body.model = model.value;
    return body;
  }

  return { el, values, first: tiles.tiles.firstChild };
}

// ------------------------------------------------------------ SSH keys

function sshKeysCard(ctx, keys) {
  const add = h("button", { class: "btn", onClick: () => addSSHKey(ctx) }, icon(icons.plus), "Add key");
  return h(
    "div",
    { class: "stack", style: { gap: "12px" } },
    sectionHead("Your SSH keys", add, { margin: "0" }),
    keys.length
      ? table(
          [
            { label: "Name", cell: (k) => h("strong", {}, k.name || "Key") },
            { label: "Fingerprint", cell: (k) => h("span", { class: "mono muted small" }, k.fingerprint) },
            { label: "Added", shrink: true, num: true, cell: (k) => stamp(k.created_at) },
            {
              label: "",
              shrink: true,
              cell: (k) =>
                iconButton(
                  icons.trash,
                  "Remove this key",
                  () =>
                    confirm({
                      title: "Remove this key?",
                      body: "It no longer lets you into your workspaces, from the next minutes on.",
                      confirmLabel: "Remove",
                      danger: true,
                      onConfirm: () => api.deleteSSHKey(k.id),
                      done: "Key removed",
                      ctx,
                    }),
                  { danger: true, label: `Remove ${k.name || k.fingerprint}` },
                ),
            },
          ],
          keys,
          {},
        )
      : h(
          "div",
          { class: "card" },
          h(
            "div",
            { class: "empty" },
            "Your keys let you into your workspaces over SSH. ",
            h("code", {}, "keera workspace configure-ssh"),
            " adds this computer's key for you.",
          ),
        ),
  );
}

function addSSHKey(ctx) {
  const key = h("textarea", {
    class: "input mono",
    rows: 4,
    spellcheck: "false",
    placeholder: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... ada@laptop",
  });
  const name = h("input", { class: "input", placeholder: "Laptop" });
  formModal({
    title: "Add an SSH key",
    body: h(
      "form",
      {},
      field("Public key", key, "The line in a .pub file, such as ~/.ssh/id_ed25519.pub. Never the private key."),
      field("Name", name, "Optional. Which computer it is on."),
    ),
    submitLabel: "Add key",
    done: "Key added",
    ctx,
    submit: () => {
      const value = key.value.trim();
      if (!value) fail(key, "Paste the public key.");
      if (value.includes("PRIVATE KEY")) fail(key, "That is a private key. Paste the .pub file's line instead.");
      return api.addSSHKey({ public_key: value, name: name.value.trim() });
    },
  });
}

/** connectSteps says how to reach a workspace from a terminal. */
export function connectSteps(w) {
  const name = shellArg(w.name);
  const step = (title, command, hint) =>
    h(
      "div",
      { class: "stack", style: { gap: "4px" } },
      h("strong", {}, title),
      codeBlock(command, "sh"),
      hint ? h("div", { class: "faint small" }, hint) : null,
    );
  return h(
    "div",
    { class: "stack md", style: { gap: "14px" } },
    step(
      "Open it in your browser",
      `keera workspace open ${name}`,
      `It serves ${EXPERIENCES[w.experience]?.name || "the workspace"} on this computer until you stop it.`,
    ),
    step("Sign in over SSH", `keera workspace configure-ssh\nssh ${shellArg(w.name + ".keera")}`, "Once per computer, then any time."),
    step("Reach a port in it", `keera workspace forward ${name} 3000`, "Such as a dev server you started there."),
  );
}
