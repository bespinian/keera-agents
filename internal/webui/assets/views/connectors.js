// Connectors: the organisation's accounts with chat services, file stores,
// code hosts and other services. Administrators add them from the catalogue;
// everyone may connect an agent to one.

import { api } from "../api.js";
import {
  h,
  icons,
  iconButton,
  field,
  formModal,
  fail,
  modal,
  confirm,
  kindMark,
  replace,
  table,
  crumb,
  go,
  listHead,
  empty,
  banner,
  checkButton,
  detailHead,
  gone,
  card,
  fact,
  sectionHead,
  rowLink,
  icon,
  date,
  secretField,
  isAdmin,
  num,
  plural,
  pill,
  stamp,
  busyButton,
  toast,
  modeTiles,
  fold,
} from "../ui.js";
import { render } from "../markdown.js";
import { kindLabel, gives, MODES, agentPath, workspacePath, connectorPath } from "../common.js";
import { targetText } from "./bindings.js";

const catalogue = "/connectors/catalogue";

// callbackURL is where a service sends people back after they sign in to a
// connector. Its setup steps name it, so it is kept when the kinds load.
let callbackURL = "";

export async function connectorsView(ctx) {
  const [conns, kindsRes] = await Promise.all([api.connections(), api.connectionKinds().catch(() => ({ data: [] }))]);
  const connections = conns.data || [];
  const kinds = kindsRes.data || [];
  callbackURL = kindsRes.callback_url || "";
  signInResult(conns.sign_in_result, connections);
  const admin = isAdmin(ctx);
  ctx.setSubtitle(`${connections.length} ${plural(connections.length, "connector")}`);
  const kindOf = (c) => kinds.find((k) => k.kind === c.kind) || {};

  const head = listHead(
    "A connector is the organisation's account with a service, such as a chat, a file store, a code host or a CRM. " +
      "Agents use it once it is added to them.",
    [
      admin
        ? null
        : h(
            "a",
            { class: "btn", href: catalogue, title: "Every service agents can work with", onClick: go(ctx, catalogue) },
            "Connector catalogue",
          ),
    ],
    admin ? { label: "Add connector", onClick: go(ctx, catalogue) } : null,
  );

  if (!connections.length) {
    return h(
      "div",
      {},
      head,
      h(
        "div",
        { class: "card" },
        empty("No connectors yet", [
          h("div", {}, "Connect the services your team uses so agents can work in them."),
          admin ? null : h("div", { class: "faint", style: { marginTop: "6px" } }, "An administrator adds them."),
        ]),
      ),
    );
  }

  const categories = [...new Set(connections.map((c) => kindOf(c).category).filter(Boolean))];
  const list = table(
    [
      {
        label: "Name",
        sortKey: (c) => c.name,
        cell: (c) =>
          h(
            "div",
            { class: "row" },
            h("span", { class: "tile-mark tile-mark-sm" }, kindMark(c.kind, kinds)),
            h(
              "div",
              { class: "stack" },
              h("strong", {}, c.name),
              h("span", { class: "faint small" }, kindLabel(c.kind, kinds)),
            ),
          ),
      },
      {
        label: "Gives agents",
        shrink: true,
        sortKey: (c) => gives(kindOf(c)),
        cell: (c) => gives(kindOf(c)),
      },
      {
        label: "Acts as",
        shrink: true,
        sortKey: (c) => (c.per_user ? 1 : 0),
        cell: (c) => (c.per_user ? accountControl(ctx, c) : h("span", { class: "faint" }, "One shared account")),
      },
      { label: "Uses", num: true, shrink: true, sortKey: (c) => c.bindings, cell: (c) => num(c.bindings) },
      {
        label: "Added",
        num: true,
        shrink: true,
        sortKey: (c) => c.created_at,
        sortDir: "desc",
        cell: (c) => stamp(c.created_at),
      },
      { label: "", shrink: true, cell: (c) => (admin ? actions(ctx, c, kinds) : null) },
    ],
    connections,
    {
      ctx,
      open: (c) => connectorPath(c.id),
      search: (c) => [c.name, kindLabel(c.kind, kinds), kindOf(c).vendor, kindOf(c).category].join(" "),
      searchLabel: "connectors",
      sortBy: "Added",
      sortDir: "desc",
      choices:
        categories.length > 1
          ? [
              {
                label: "Categories",
                options: categories.map((v) => ({ value: v, label: v })),
                test: (c, v) => kindOf(c).category === v,
              },
            ]
          : [],
    },
  );
  return h("div", {}, head, list);
}

/** accountControl connects the viewer's own account to a connector people
 *  sign in to, or disconnects it. Agents act as that account for the work
 *  the viewer gives them. */
function accountControl(ctx, c) {
  if (c.account) {
    return h(
      "div",
      { class: "row" },
      h(
        "span",
        { class: "small", title: "Agents act as this account in the work you give them" },
        `You: ${c.account.account || "connected"}`,
      ),
      busyButton({ class: "btn btn-sm", title: "Agents stop acting as you here" }, "Disconnect", async () => {
        await api.disconnectAccount(c.id);
        toast(`Your account is disconnected from ${c.name}`, "good");
        ctx.reload();
      }),
    );
  }
  return busyButton(
    { class: "btn btn-sm btn-primary", title: "Sign in, so agents can work here as you" },
    "Connect your account",
    async () => {
      const { url } = await api.connectAccount(c.id);
      location.href = url;
    },
  );
}

/** signInResult says how the viewer's last sign-in to a connector went. */
function signInResult(result, connections) {
  if (!result) return;
  const c = connections.find((x) => x.id === result.connection_id);
  const name = c ? c.name : "the connector";
  if (result.error) toast(`Your account was not connected to ${name}: ${result.error}`, "bad");
  else toast(`Your account is connected to ${name}`, "good");
}

/** actions are an administrator's buttons for one connector in the list.
 *  Checking it is on its own screen. */
function actions(ctx, c, kinds) {
  return h(
    "div",
    { class: "row-tight" },
    iconButton(icons.pencil, "Edit this connector", () => openEditor(ctx, kinds, c), {
      label: `Edit ${c.name}`,
    }),
    iconButton(icons.trash, "Delete this connector", () => deleteConnector(ctx, c), {
      danger: true,
      label: `Delete ${c.name}`,
    }),
  );
}

/** deleteConnector asks first, then goes back to the list. */
function deleteConnector(ctx, c) {
  confirm({
    title: `Delete ${c.name}?`,
    body: c.bindings
      ? `It is used in ${c.bindings} ${plural(c.bindings, "place")}. Agents stop working there.`
      : "No agent uses it.",
    confirmLabel: "Delete connector",
    danger: true,
    onConfirm: () => api.deleteConnection(c.id),
    done: () => {
      if (location.pathname === "/connectors") ctx.reload();
      else ctx.navigate("/connectors");
      return `${c.name} deleted`;
    },
  });
}

/** connectorView is one connector: what it is, and where agents use it. */
export async function connectorView(ctx) {
  let c;
  try {
    c = await api.connection(ctx.param);
  } catch (err) {
    if (err.status === 404) {
      return gone(
        ctx,
        "/connectors",
        "Connectors",
        "No such connector",
        "It was deleted, or it belongs to another organisation.",
      );
    }
    throw err;
  }
  const kindsRes = await api.connectionKinds().catch(() => ({ data: [] }));
  const kinds = kindsRes.data || [];
  callbackURL = kindsRes.callback_url || "";
  const kind = kinds.find((k) => k.kind === c.kind) || {};
  const admin = isAdmin(ctx);
  ctx.setTitle(c.name, kindLabel(c.kind, kinds), h("span", { class: "tile-mark" }, kindMark(c.kind, kinds)));

  const controls = admin
    ? [
        checkButton(
          "Check that the service still accepts its credentials",
          () => api.checkConnection(c.id),
          () => ({ label: "Works", tone: "good", title: "The service accepted the credentials" }),
        ),
        h("button", { class: "btn", onClick: () => openEditor(ctx, kinds, c) }, icon(icons.pencil), "Edit"),
        h("button", { class: "btn btn-danger", onClick: () => deleteConnector(ctx, c) }, icon(icons.trash), "Delete"),
      ]
    : [];

  const uses = c.uses || [];
  const hiddenUses = c.bindings - uses.length;
  const tools = toolList(kind);

  return h(
    "div",
    { class: "grid" },
    detailHead(
      ctx,
      ["/connectors", "Connectors"],
      [kind.category ? pill(kind.category) : null],
      h("div", { class: "row-tight", style: { flexWrap: "wrap" } }, controls),
    ),
    h(
      "div",
      { class: tools ? "grid grid-2" : "grid" },
      card(
        "Details",
        [
          fact("Service", kindLabel(c.kind, kinds)),
          fact("Gives agents", gives(kind)),
          fact("Acts as", c.per_user ? accountControl(ctx, c) : "One shared account"),
          fact("Uses", num(c.bindings)),
          fact("Added", c.created_by ? `${date(c.created_at)} by ${c.created_by}` : date(c.created_at)),
          admin && c.webhook_url
            ? fact("Webhook URL", secretField(c.webhook_url, `Webhook URL of ${c.name}`, { hidden: true }))
            : null,
        ],
        { style: { alignSelf: "start" }, bodyClass: "card-body facts" },
      ),
      tools ? card("What agents can do", tools, { style: { alignSelf: "start" } }) : null,
    ),
    sectionHead(
      "Where agents use it",
      hiddenUses > 0 ? `and in ${hiddenUses} ${plural(hiddenUses, "workspace")} of other people` : null,
      { margin: "0" },
    ),
    table(
      [
        {
          label: "Agent",
          sortKey: (b) => b.agent_name,
          cell: (b) =>
            h(
              "div",
              { class: "stack" },
              rowLink(ctx, usePath(b), b.agent_name),
              b.agent_kind === "workspace" ? h("span", { class: "faint small" }, "Workspace") : null,
            ),
        },
        {
          label: "Where",
          sortKey: (b) => b.label || b.target,
          cell: (b) =>
            h(
              "div",
              { class: "stack" },
              h("span", {}, b.label || targetText(b, kinds)),
              b.label && b.target !== b.label ? h("span", { class: "faint small mono" }, targetText(b, kinds)) : null,
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
          label: "Added",
          shrink: true,
          num: true,
          sortKey: (b) => b.created_at,
          sortDir: "desc",
          cell: (b) => stamp(b.created_at),
        },
      ],
      uses,
      {
        ctx,
        open: usePath,
        sortBy: "Added",
        sortDir: "desc",
        emptyTitle: "No agent uses it yet",
        emptyBody: "Add it to an agent on the agent's Connectors tab.",
      },
    ),
  );
}

/** usePath is the Connectors tab of the agent or workspace using it. */
function usePath(b) {
  return b.agent_kind === "workspace" ? workspacePath(b.agent_id, "connectors") : agentPath(b.agent_id, "connectors");
}

/** catalogueView lists every kind of connector, and the organisation's
 *  connectors of each. An administrator adds one by picking its kind. */
export async function catalogueView(ctx) {
  const admin = isAdmin(ctx);
  const [kindsRes, conns] = await Promise.all([api.connectionKinds(), api.connections().catch(() => ({ data: [] }))]);
  const kinds = kindsRes.data || [];
  callbackURL = kindsRes.callback_url || "";
  const connections = conns.data || [];
  // Alphabetical first, so sorting by another column keeps names in order.
  const rows = kinds
    .map((k) => ({ k, mine: connections.filter((c) => c.kind === k.kind) }))
    .sort((a, b) => a.k.label.localeCompare(b.k.label));
  ctx.setSubtitle(`${rows.length} ${plural(rows.length, "service")}`);
  const unique = (f) => [...new Set(kinds.map(f).filter(Boolean))].sort();

  const list = table(
    [
      {
        label: "Name",
        sortKey: (r) => r.k.label,
        cell: (r) =>
          h(
            "div",
            { class: "row" },
            h("span", { class: "tile-mark tile-mark-sm" }, kindMark(r.k.kind, kinds)),
            h(
              "div",
              { class: "stack" },
              h("strong", {}, r.k.label),
              r.k.summary ? h("span", { class: "faint small" }, r.k.summary) : null,
            ),
          ),
      },
      { label: "Vendor", shrink: true, sortKey: (r) => r.k.vendor, cell: (r) => r.k.vendor || "" },
      { label: "Category", shrink: true, sortKey: (r) => r.k.category, cell: (r) => pill(r.k.category || "Other") },
      { label: "Gives agents", shrink: true, cell: (r) => h("span", { class: "nowrap" }, gives(r.k)) },
      {
        label: "In use",
        shrink: true,
        sortKey: (r) => r.mine.length || null,
        sortDir: "desc",
        cell: (r) => inUse(r.mine),
      },
    ],
    rows,
    {
      pick: admin ? (r) => openEditor(ctx, kinds, null, r.k) : (r) => showKind(r.k),
      pickTitle: admin ? "Add a connector to this service" : "What agents can do with it",
      emptyTitle: "No services",
      emptyBody: "This build knows no connectors.",
      search: (r) =>
        [r.k.label, r.k.kind, r.k.vendor, r.k.category, r.k.summary, ...(r.k.tools || []).map((t) => t.name)].join(" "),
      searchLabel: "services",
      sortBy: "Name",
      tile: (r) => catalogueTile(r, kinds),
      choices: [
        {
          label: "Categories",
          options: unique((k) => k.category).map((v) => ({ value: v, label: v })),
          test: (r, v) => r.k.category === v,
        },
        {
          label: "Vendors",
          options: unique((k) => k.vendor).map((v) => ({ value: v, label: v })),
          test: (r, v) => r.k.vendor === v,
        },
      ].filter((c) => c.options.length > 1),
    },
  );

  return h("div", {}, h("div", { class: "detail-head row" }, crumb(ctx, "/connectors", "Connectors")), list);
}

function inUse(mine) {
  if (!mine.length) return h("span", { class: "faint" }, "Not yet");
  return h("span", { class: "small", title: mine.map((c) => c.name).join(", ") }, mine.map((c) => c.name).join(", "));
}

function catalogueTile(r, kinds) {
  const fact = (label, value) => h("div", {}, h("dt", {}, label), h("dd", {}, value));
  return h(
    "article",
    { class: "model-tile" },
    h(
      "div",
      { class: "model-tile-head" },
      h("span", { class: "tile-mark" }, kindMark(r.k.kind, kinds)),
      h(
        "div",
        { class: "stack" },
        h("span", { class: "model-tile-name" }, r.k.label),
        h("span", { class: "faint small" }, [r.k.vendor, r.k.category].filter(Boolean).join(" · ")),
      ),
    ),
    r.k.summary ? h("p", { class: "model-tile-what" }, r.k.summary) : null,
    h(
      "dl",
      { class: "model-tile-facts" },
      fact("Gives agents", gives(r.k)),
      fact("Narrows to", r.k.target_label || "Nothing"),
      fact("In use", r.mine.length ? num(r.mine.length) : "Not yet"),
    ),
  );
}

/** toolList says what an API kind's tools do, those that change something
 *  marked, so an administrator knows what agents may do before adding it. */
function toolList(kind) {
  if (!kind.api || !(kind.tools || []).length) return null;
  return h(
    "div",
    { class: "field" },
    h("label", {}, "What agents can do"),
    h(
      "ul",
      { class: "tool-list" },
      kind.tools.map((t) =>
        h(
          "li",
          {},
          h("span", { class: "mono small" }, `${kind.kind}_${t.name}`),
          t.write ? pill("Writes", "warn") : null,
          h("div", { class: "faint small" }, t.description),
        ),
      ),
    ),
    h(
      "div",
      { class: "hint" },
      "An agent that may only read never gets the tools that write. ",
      kind.target_label
        ? `Adding it to an agent names one ${kind.target_label.toLowerCase()}, and its tools stay in it.`
        : "The service's own permissions decide what it reaches.",
    ),
  );
}

/** showKind tells a member what a service does, since only an
 *  administrator can add it. */
function showKind(kind) {
  modal({
    title: kind.label,
    subtitle: kind.summary,
    wide: true,
    body: h(
      "div",
      {},
      toolList(kind),
      banner(
        "info",
        { style: { marginBottom: "0" } },
        "An administrator adds connectors.",
        kind.sign_in ? " They can let each person sign in with their own account." : "",
      ),
    ),
  });
}

/** setupText says where the webhook URL is, and fills in the address
 *  people come back to after signing in. The URL itself stays out of the
 *  text, so it shows only when someone reveals it. */
export function setupText(setup, saved, callback = "") {
  return String(setup || "")
    .replaceAll("{webhook}", saved ? "the webhook URL above" : "the webhook URL (shown once saved)")
    .replaceAll("{callback}", callback ? "`" + callback + "`" : "this panel's /auth/connector/callback");
}

// ACCESS is whom a connector of a kind people may sign in to acts as.
const ACCESS = [
  {
    value: "shared",
    icon: icons.orgs,
    name: "One shared account",
    what: "Agents act as one account you set up, such as a service account, whoever gives them work.",
  },
  {
    value: "per_user",
    icon: icons.person,
    name: "Each person's own account",
    what: "Each person signs in. Agents act as whoever gives them work, with only that person's access.",
  },
];

/** openEditor adds a connector of kind, or changes conn. Secrets are never
 *  shown: one left empty keeps its value. */
function openEditor(ctx, kinds, conn, kind = null) {
  if (conn) kind = kinds.find((k) => k.kind === conn.kind) || null;
  const name = h("input", { class: "input", value: conn ? conn.name : "" });
  const fieldsHost = h("div");
  const inputs = {};
  // Whom it acts as is chosen once, when it is added.
  let perUser = !!(conn && conn.per_user);
  const access =
    kind && kind.sign_in && !conn
      ? modeTiles(ACCESS, null, (value) => {
          perUser = value === "per_user";
          drawFields();
        })
      : null;

  function drawFields() {
    for (const k of Object.keys(inputs)) delete inputs[k];
    name.placeholder = kind ? `My ${kind.label}` : "";
    if (!kind || (access && !access.tiles.hidden)) {
      replace(fieldsHost);
      return;
    }
    const config = (conn && conn.config) || {};
    const set = (conn && conn.secrets_set) || [];
    const signIn = perUser && kind.sign_in;
    const setup = signIn ? kind.sign_in.setup : kind.setup;
    const fields = (signIn ? kind.sign_in.fields : kind.fields) || [];
    const fieldOf = (f) => {
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
      return field(f.label, input, f.help || null);
    };
    // Optional settings are folded, so what every connector needs comes
    // first. The fold opens when one is set, so an edit shows what is saved.
    const optional = fields.filter((f) => !f.required);
    const advanced = optional.length ? fold("Advanced options", optional.map(fieldOf)) : null;
    if (advanced) advanced.open = optional.some((f) => (f.secret ? set.includes(f.name) : !!config[f.name]));
    replace(
      fieldsHost,
      conn && conn.per_user
        ? field("Acts as", h("div", {}, "Each person's own account"), "Each person connects theirs on the connector.")
        : null,
      field("Name", name, "Shown when adding it to an agent."),
      fields.filter((f) => f.required).map(fieldOf),
      advanced,
      conn && conn.webhook_url
        ? field("Webhook URL", secretField(conn.webhook_url, "Webhook URL", { hidden: true }))
        : null,
      setup
        ? h(
            "div",
            { class: "field" },
            h("label", {}, `Setting up ${kind.label}`),
            h("div", { class: "preview" }, render(setupText(setup, !!(conn && conn.webhook_url), callbackURL))),
            kind.chat && !conn
              ? h(
                  "div",
                  { class: "hint" },
                  "The webhook URL is known once the connector is saved. It shows then, and when you edit it.",
                )
              : null,
          )
        : null,
    );
  }

  drawFields();

  formModal({
    title: conn ? `Edit ${conn.name}` : `Add ${kind.label}`,
    subtitle: conn ? kindLabel(conn.kind, kinds) : `${kind.label}. It is checked with the service before it is saved.`,
    wide: true,
    body: h(
      "form",
      {},
      access ? h("div", { class: "field" }, h("label", {}, "Agents act as"), access.tiles, access.chosenRow) : null,
      fieldsHost,
      toolList(kind),
    ),
    submitLabel: conn ? "Save" : "Add connector",
    submit: async () => {
      if (access && !access.tiles.hidden) fail(access.tiles.firstChild, "Choose whom agents act as.");
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
        : api.createConnection({ kind: kind.kind, name: name.value.trim(), config, secrets, per_user: perUser });
    },
    done: (saved) => {
      // A new connector shows among the others.
      if (conn) ctx.reload();
      else ctx.navigate("/connectors");
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
      h(
        "div",
        { class: "field" },
        h("label", {}, "Webhook URL"),
        secretField(conn.webhook_url, "Webhook URL", { hidden: true }),
      ),
      kind.setup
        ? h(
            "div",
            { class: "field" },
            h("label", {}, `Setting up ${kind.label}`),
            h("div", { class: "preview" }, render(setupText(kind.setup, true))),
          )
        : null,
      banner("info", { style: { marginBottom: "0" } }, "It also shows when you edit the connector."),
    ),
  });
}
