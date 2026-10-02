// Connectors: the organisation's accounts with chat services, file stores
// and code hosts. Administrators add them; everyone may connect an agent to
// one.

import { api } from "../api.js";
import {
  h,
  icon,
  icons,
  iconButton,
  field,
  formModal,
  fail,
  modal,
  confirm,
  serviceMark,
  modeTiles,
  replace,
  listHead,
  empty,
  banner,
  checkButton,
  secretField,
  isAdmin,
  date,
  num,
  plural,
} from "../ui.js";
import { render } from "../markdown.js";
import { kindLabel } from "../common.js";

export async function connectorsView(ctx) {
  const [conns, kindsRes] = await Promise.all([api.connections(), api.connectionKinds().catch(() => ({ data: [] }))]);
  const connections = conns.data || [];
  const kinds = kindsRes.data || [];
  const admin = isAdmin(ctx);
  ctx.setSubtitle(`${connections.length} ${plural(connections.length, "connector")}`);
  const add = admin && kinds.length ? () => openEditor(ctx, kinds, null) : null;

  return h(
    "div",
    {},
    listHead(
      "A connector is the organisation's account with a chat service, a file store or a code host. " +
        "Agents use it through their channels.",
      [],
      add ? { label: "Add connector", onClick: add } : null,
    ),
    connections.length
      ? h(
          "div",
          { class: "table-tiles" },
          connections.map((c) => tile(ctx, c, kinds, admin)),
        )
      : h(
          "div",
          { class: "card" },
          empty("No connectors yet", [
            h("div", {}, "Connect the services your team uses so agents can work in them."),
            add
              ? h(
                  "button",
                  { class: "btn btn-primary", style: { marginTop: "14px" }, onClick: add },
                  icon(icons.plus),
                  "Add connector",
                )
              : h("div", { class: "faint", style: { marginTop: "6px" } }, "An administrator adds them."),
          ]),
        ),
  );
}

function tile(ctx, c, kinds, admin) {
  const kind = kinds.find((k) => k.kind === c.kind) || {};
  return h(
    "div",
    { class: "model-tile" },
    h(
      "div",
      { class: "model-tile-head" },
      h("span", { class: "tile-mark" }, serviceMark(c.kind) || icon(icons.connections)),
      h(
        "div",
        { class: "stack", style: { gap: "1px", minWidth: "0" } },
        h("span", { class: "model-tile-name" }, c.name),
        h("span", { class: "faint small" }, kindLabel(c.kind, kinds)),
      ),
    ),
    c.webhook_url
      ? h(
          "div",
          { class: "stack" },
          h("span", { class: "faint small" }, "Webhook URL"),
          secretField(c.webhook_url, `Webhook URL of ${c.name}`, { compact: true }),
        )
      : null,
    h(
      "dl",
      { class: "model-tile-facts" },
      h("div", {}, h("dt", {}, "Channels"), h("dd", {}, num(c.bindings))),
      h(
        "div",
        {},
        h("dt", {}, "Kind"),
        h("dd", {}, kind.code ? "Code" : kind.chat && kind.files ? "Chat, files" : kind.files ? "Files" : "Chat"),
      ),
      h("div", {}, h("dt", {}, "Added"), h("dd", {}, date(c.created_at))),
    ),
    admin
      ? h(
          "div",
          { class: "row" },
          checkButton(
            "Check that the service still accepts its credentials",
            () => api.checkConnection(c.id),
            () => ({ label: "Works", tone: "good", title: "The service accepted the credentials" }),
          ),
          h("div", { class: "spacer" }),
          iconButton(icons.pencil, "Edit this connector", () => openEditor(ctx, kinds, c), {
            label: `Edit ${c.name}`,
          }),
          iconButton(
            icons.trash,
            "Delete this connector",
            () =>
              confirm({
                title: `Delete ${c.name}?`,
                body: c.bindings
                  ? `${c.bindings} ${plural(c.bindings, "channel")} use it and go with it. Agents stop answering there.`
                  : "No channel uses it.",
                confirmLabel: "Delete connector",
                danger: true,
                onConfirm: () => api.deleteConnection(c.id),
                done: `${c.name} deleted`,
                ctx,
              }),
            { danger: true, label: `Delete ${c.name}` },
          ),
        )
      : null,
  );
}

/** setupText fills in the webhook URL, or says where it will be. */
export function setupText(setup, webhook) {
  return String(setup || "").replaceAll("{webhook}", webhook || "the webhook URL (shown once saved)");
}

/** openEditor adds a connector, or changes one. Secrets are never shown:
 *  one left empty keeps its value. */
function openEditor(ctx, kinds, conn) {
  let kind = conn ? kinds.find((k) => k.kind === conn.kind) : kinds.length === 1 ? kinds[0] : null;
  const name = h("input", { class: "input", value: conn ? conn.name : "" });
  const fieldsHost = h("div");
  const inputs = {};

  function drawFields() {
    for (const k of Object.keys(inputs)) delete inputs[k];
    name.placeholder = kind ? `My ${kind.label}` : "";
    if (!kind) {
      replace(fieldsHost);
      return;
    }
    const config = (conn && conn.config) || {};
    const set = (conn && conn.secrets_set) || [];
    replace(
      fieldsHost,
      field("Name", name, "Shown when adding a channel to an agent."),
      (kind.fields || []).map((f) => {
        const isSet = set.includes(f.name);
        const props = {
          class: "input" + (f.secret ? " mono" : ""),
          autocomplete: f.secret ? "new-password" : "off",
          spellcheck: "false",
        };
        let input;
        if (f.multiline) {
          input = h("textarea", { ...props, rows: 4 }, f.secret ? "" : config[f.name] || "");
        } else {
          input = h("input", {
            ...props,
            type: f.secret ? "password" : "text",
            value: f.secret ? "" : config[f.name] || "",
          });
        }
        if (f.secret && conn) input.placeholder = isSet ? "Set. Leave empty to keep it." : "Not set";
        inputs[f.name] = { f, input, isSet };
        return field(f.label + (f.required ? "" : " (optional)"), input, f.help || null);
      }),
      kind.setup
        ? h(
            "div",
            { class: "field" },
            h("label", {}, `Setting up ${kind.label}`),
            h("div", { class: "preview" }, render(setupText(kind.setup, conn && conn.webhook_url))),
            kind.chat && !conn
              ? h(
                  "div",
                  { class: "hint" },
                  "The webhook URL is known once the connector is saved. It shows then, and on the connector's card.",
                )
              : null,
          )
        : null,
    );
  }

  const picker = conn
    ? null
    : modeTiles(
        kinds.map((k) => ({
          value: k.kind,
          name: k.label,
          mark: () => serviceMark(k.kind),
          icon: icons.connections,
          what: k.code
            ? "Agents code in its repositories."
            : k.chat && k.files
              ? "Chats and files."
              : k.chat
                ? "Agents answer in its chats."
                : "Agents watch and use its folders.",
        })),
        kind ? kind.kind : null,
        (value) => {
          kind = kinds.find((k) => k.kind === value);
          drawFields();
          name.focus();
        },
      );
  if (picker && kind) picker.choose(kind.kind);
  drawFields();

  formModal({
    title: conn ? `Edit ${conn.name}` : "Add connector",
    subtitle: conn ? kindLabel(conn.kind, kinds) : "It is checked with the service before it is saved.",
    wide: true,
    body: h(
      "form",
      {},
      picker ? h("div", { class: "field" }, h("label", {}, "Service"), picker.tiles, picker.chosenRow) : null,
      fieldsHost,
    ),
    submitLabel: conn ? "Save" : "Add connector",
    submit: async () => {
      if (!kind) throw { message: "Choose a service." };
      if (!name.value.trim()) fail(name, "Give the connector a name.");
      const config = {};
      const secrets = {};
      for (const { f, input, isSet } of Object.values(inputs)) {
        const v = input.value.trim();
        if (f.required && !v && !(f.secret && isSet)) fail(input, `Fill in ${f.label}.`);
        if (f.secret) {
          if (v) secrets[f.name] = v;
        } else config[f.name] = v;
      }
      return conn
        ? api.updateConnection(conn.id, { name: name.value.trim(), config, secrets })
        : api.createConnection({ kind: kind.kind, name: name.value.trim(), config, secrets });
    },
    done: (saved) => {
      ctx.reload();
      if (!conn && saved.webhook_url) showWebhook(saved, kind);
      return conn ? `${saved.name} saved` : `${saved.name} added`;
    },
  });
}

/** showWebhook hands over the URL the service must call, now that it
 *  exists, with the set-up steps it goes into. */
function showWebhook(conn, kind) {
  modal({
    title: `${conn.name} is saved`,
    subtitle: `One step left: tell ${kind.label} where to send messages.`,
    wide: true,
    body: h(
      "div",
      {},
      h("div", { class: "field" }, h("label", {}, "Webhook URL"), secretField(conn.webhook_url, "Webhook URL")),
      kind.setup
        ? h(
            "div",
            { class: "field" },
            h("label", {}, `Setting up ${kind.label}`),
            h("div", { class: "preview" }, render(setupText(kind.setup, conn.webhook_url))),
          )
        : null,
      banner("info", { style: { marginBottom: "0" } }, "The URL also shows on the connector's card."),
    ),
  });
}
