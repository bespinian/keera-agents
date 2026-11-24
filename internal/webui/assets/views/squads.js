// Squads: agents that share a board and hand each other work on it. The
// board has cards in five columns, for people and agents alike. A card in
// To do with an agent is picked up by it; the agent moves it on as it works.

import { api } from "../api.js";
import {
  h,
  icon,
  icons,
  iconButton,
  menu,
  formModal,
  table,
  rowLink,
  checkRow,
  gone,
  detailHead,
  fail,
  confirm,
  field,
  toast,
  replace,
  listHead,
  banner,
  busyButton,
  date,
  ago,
  stamp,
  pill,
  avatarOf,
  splitList,
  go,
  link,
  every,
  plural,
  pad,
} from "../ui.js";
import { taskStatePill, taskPath, squadPath, agentPath } from "../common.js";
import { render } from "../markdown.js";

/** STATUSES are the columns, in order. */
export const STATUSES = [
  ["backlog", "Backlog"],
  ["todo", "To do"],
  ["in_progress", "In progress"],
  ["review", "Review"],
  ["done", "Done"],
];

const statusLabel = (s) => (STATUSES.find(([v]) => v === s) || [s, s])[1];

/** positionBetween is the position for a card dropped between two others:
 *  their midpoint, or one past the only neighbour. */
export function positionBetween(before, after) {
  const has = (v) => typeof v === "number" && Number.isFinite(v);
  if (has(before) && has(after)) return (before + after) / 2;
  if (has(before)) return before + 1;
  if (has(after)) return after - 1;
  return 1;
}

/** columnOf is a column's cards in order, without the card being moved. */
export function columnOf(cards, status, without = "") {
  return cards.filter((c) => c.status === status && c.id !== without).sort((a, b) => a.position - b.position);
}

/** placeCard is where a card lands when put at index among the other cards
 *  of a column: { status, position }. */
export function placeCard(cards, id, status, index) {
  const column = columnOf(cards, status, id);
  const i = Math.max(0, Math.min(index, column.length));
  return { status, position: positionBetween(column[i - 1]?.position, column[i]?.position) };
}

/** dueISO turns a date field's day into the end of that day, here. */
export function dueISO(day) {
  if (!day) return "";
  return new Date(`${day}T23:59:00`).toISOString();
}

/** dueDay is the day a due time falls on, here, for a date field. */
export function dueDay(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

const POLL_MS = 10_000;

export async function squadsView(ctx) {
  const [res, agentsRes] = await Promise.all([api.squads(), api.agents().catch(() => ({ data: [] }))]);
  const squads = res.data || [];
  const agents = agentsRes.data || [];
  ctx.setSubtitle(`${squads.length} ${plural(squads.length, "squad")}`);
  return h(
    "div",
    { class: "grid" },
    listHead(null, null, { label: "New squad", onClick: () => squadForm(ctx, agents) }),
    table(
      [
        {
          label: "Name",
          cell: (q) =>
            h(
              "div",
              { class: "stack" },
              rowLink(ctx, squadPath(q.id), q.name),
              q.goal ? h("div", { class: "faint small clamp-2" }, q.goal) : null,
            ),
          sortKey: (q) => q.name,
        },
        {
          label: "Members",
          cell: (q) =>
            q.members.length ? memberChips(ctx, q, { small: true }) : h("span", { class: "faint" }, "None yet"),
          sortKey: (q) => q.members.length,
        },
        {
          label: "Open cards",
          shrink: true,
          cell: (q) => h("span", { title: `${q.cards} in all` }, String(q.open)),
          sortKey: (q) => q.open,
        },
      ],
      squads,
      {
        ctx,
        open: (q) => squadPath(q.id),
        emptyTitle: "No squad yet",
        emptyBody: "A squad is agents that share a board. They hand each other work on it as cards.",
      },
    ),
  );
}

/** memberChips shows a squad's members, its lead first. */
function memberChips(ctx, q, { small = false } = {}) {
  const lead = q.members.find((m) => m.id === q.lead_id);
  const rest = q.members.filter((m) => m !== lead);
  return h(
    "div",
    { class: "wrap-chips" },
    [lead, ...rest].filter(Boolean).map((m) =>
      link(
        ctx,
        agentPath(m.id),
        [
          avatarOf(m.name, { agent: true, icon: m.icon }),
          small ? null : h("span", {}, m.name),
          m === lead && !small ? pill("Lead", "accent") : null,
        ],
        { class: "who", title: m === lead ? `${m.name}, the lead` : m.description || m.name },
      ),
    ),
  );
}

/** squadForm creates a squad, or changes the one given. */
function squadForm(ctx, agents, squad) {
  const name = h("input", { class: "input", value: squad?.name || "", placeholder: "Sales" });
  const goal = h(
    "textarea",
    { class: "input", rows: 3, placeholder: "Answer every lead within a day, and keep the pipeline up to date." },
    squad?.goal || "",
  );
  const chosen = new Set((squad?.members ?? []).map((m) => m.id));
  const lead = h("select", { class: "select" });
  const paintLead = (selected) =>
    replace(lead, [
      h("option", { value: "" }, "No lead"),
      agents
        .filter((a) => chosen.has(a.id))
        .map((a) => h("option", { value: a.id, selected: a.id === selected }, a.name)),
    ]);
  paintLead(squad?.lead_id || "");
  const checks = agents.map((a) => {
    const row = checkRow(a.name, a.description, chosen.has(a.id), {
      onChange: (e) => {
        if (e.target.checked) chosen.add(a.id);
        else chosen.delete(a.id);
        paintLead(lead.value);
      },
    });
    return row.el;
  });
  formModal({
    title: squad ? `Edit ${squad.name}` : "New squad",
    wide: true,
    body: h(
      "form",
      {},
      field("Name", name),
      field("Goal", goal, "What the squad is for. Every member reads it with each card."),
      field(
        "Members",
        agents.length ? h("div", { class: "stack" }, checks) : h("div", { class: "faint" }, "No agents yet."),
        "Members hand each other work as cards. An agent can be in several squads.",
      ),
      field("Lead", lead, "Takes the cards put in To do without an agent, and hands out the work."),
    ),
    submitLabel: squad ? "Save" : "Create squad",
    done: (res) => `${res.name} ${squad ? "saved" : "created"}`,
    ctx: squad ? ctx : undefined,
    submit: async () => {
      if (!name.value.trim()) fail(name, "Give the squad a name.");
      const body = {
        name: name.value.trim(),
        goal: goal.value.trim(),
        members: agents.filter((a) => chosen.has(a.id)).map((a) => a.id),
        lead_id: lead.value || null,
      };
      if (squad) return api.updateSquad(squad.id, body);
      const created = await api.createSquad(body);
      ctx.navigate(squadPath(created.id));
      return created;
    },
  });
}

export async function squadView(ctx) {
  // Every agent, for names and icons in comments: an agent may have left the
  // squad since it wrote one.
  const agentsRes = api.agents().catch(() => ({ data: [] }));
  let squad;
  try {
    squad = await api.squad(ctx.param);
  } catch (err) {
    if (err.status === 404) return gone(ctx, "/squads", "Squads", "No such squad", "It was deleted.");
    throw err;
  }
  const agents = (await agentsRes).data || [];
  const members = squad.members;
  const iconOf = (id) => (agents.find((a) => a.id === id) || {}).icon;
  let cards = squad.cards_list || [];
  ctx.setTitle(squad.name, squad.goal || "", avatarOf(squad.name, { agent: true, glyph: icons.squads, large: true }));

  // Moves in flight. A poll then would show the card back where it was.
  let saving = 0;
  let dragging = null;
  const kanban = h("div", { class: "kanban" });
  const drop = h("div", { class: "kanban-drop", "aria-hidden": "true" });

  function draw() {
    // Redrawing must not lose the reader's place on a card.
    const focused = document.activeElement?.closest?.(".kanban-card")?.dataset.id;
    replace(
      kanban,
      STATUSES.map(([status, label]) => column(status, label)),
    );
    if (focused) kanban.querySelector(`.kanban-card[data-id="${CSS.escape(focused)}"]`)?.focus({ preventScroll: true });
  }

  function column(status, label) {
    const list = columnOf(cards, status);
    const holder = h("div", { class: "kanban-cards" }, list.map(cardEl));
    const col = h(
      "section",
      { class: "kanban-col", dataset: { status }, "aria-label": label },
      h(
        "div",
        { class: "kanban-head" },
        h("span", { class: "dot", "aria-hidden": "true" }),
        h("h2", {}, label),
        h("span", { class: "kanban-count" }, String(list.length)),
        h("div", { class: "spacer" }),
        iconButton(icons.plus, `Add a card to ${label}`, () => newCard(status), { quiet: true }),
      ),
      holder,
    );

    col.addEventListener("dragover", (e) => {
      if (!dragging) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "move";
      col.classList.add("drop-target");
      const { before } = dropPoint(holder, e.clientY);
      if (before) holder.insertBefore(drop, before);
      else holder.append(drop);
    });
    col.addEventListener("dragleave", (e) => {
      if (col.contains(e.relatedTarget)) return;
      col.classList.remove("drop-target");
      drop.remove();
    });
    col.addEventListener("drop", (e) => {
      if (!dragging) return;
      e.preventDefault();
      const { index } = dropPoint(holder, e.clientY);
      const id = dragging;
      endDrag();
      move(id, status, index);
    });
    return col;
  }

  /** dropPoint is where a card dropped at y lands in a column: its index
   *  among the other cards, and the card it goes before. */
  function dropPoint(holder, y) {
    const others = [...holder.querySelectorAll(".kanban-card:not(.dragging)")];
    const i = others.findIndex((el) => {
      const r = el.getBoundingClientRect();
      return y < r.top + r.height / 2;
    });
    return i < 0 ? { index: others.length, before: null } : { index: i, before: others[i] };
  }

  function endDrag() {
    dragging = null;
    drop.remove();
    for (const el of kanban.querySelectorAll(".drop-target, .dragging")) el.classList.remove("drop-target", "dragging");
  }

  function cardEl(c) {
    const overdue = c.due_at && c.status !== "done" && new Date(c.due_at) < new Date();
    const el = h(
      "article",
      {
        class: "kanban-card",
        draggable: "true",
        tabindex: "0",
        dataset: { id: c.id },
        "aria-label": `${c.title}, ${statusLabel(c.status)}`,
        title: "Open the card. Drag it, or press Alt and an arrow key, to move it.",
        onClick: (e) => {
          if (e.target.closest("button, a")) return;
          openCard(c.id);
        },
        onKeydown: (e) => {
          if (e.target !== el) return;
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            openCard(c.id);
          } else if (e.altKey && e.key.startsWith("Arrow")) {
            e.preventDefault();
            nudge(c, e.key);
          }
        },
        onDragstart: (e) => {
          dragging = c.id;
          e.dataTransfer.effectAllowed = "move";
          e.dataTransfer.setData("text/plain", c.title);
          // After this event, or the drag image would be faded too.
          setTimeout(() => el.classList.add("dragging"), 0);
        },
        onDragend: endDrag,
      },
      h("div", { class: "kanban-title" }, c.title),
      h(
        "div",
        { class: "kanban-meta" },
        c.agent_id
          ? h(
              "span",
              { class: "who" },
              avatarOf(c.agent_name, { agent: true, icon: iconOf(c.agent_id) }),
              h("span", {}, c.agent_name),
            )
          : null,
        c.waiting
          ? h("span", { class: "pill", title: "Waits for the cards its agent handed out" }, "Waiting")
          : c.task_state
            ? taskStatePill(c.task_state)
            : null,
        c.due_at
          ? h(
              "span",
              { class: "pill" + (overdue ? " pill-bad" : ""), title: overdue ? "Overdue" : "Due" },
              icon(icons.calendar),
              date(c.due_at),
            )
          : null,
        (c.labels || []).map((l) => pill(l, "accent")),
        c.comments ? h("span", { class: "row-tight", title: "Comments" }, icon(icons.chat), String(c.comments)) : null,
      ),
      iconButton(icons.more, "Card actions", (e) => cardMenu(e.currentTarget, c), {
        quiet: true,
        cls: "kanban-menu",
        label: `Actions for ${c.title}`,
        "aria-haspopup": "menu",
      }),
    );
    return el;
  }

  /** cardMenu moves a card without dragging, for the keyboard and for touch. */
  function cardMenu(anchor, c) {
    const column = columnOf(cards, c.status);
    const at = column.findIndex((x) => x.id === c.id);
    menu(anchor, [
      { label: "Edit", icon: icons.pencil, onClick: () => openCard(c.id) },
      { separator: true },
      at > 0 ? { label: "Move up", icon: icons.up, onClick: () => move(c.id, c.status, at - 1) } : null,
      at < column.length - 1
        ? { label: "Move down", icon: icons.down, onClick: () => move(c.id, c.status, at + 1) }
        : null,
      ...STATUSES.filter(([s]) => s !== c.status).map(([s, label]) => ({
        label: `Move to ${label}`,
        icon: icons.move,
        onClick: () => move(c.id, s, Infinity),
      })),
      { separator: true },
      { label: "Delete", icon: icons.trash, onClick: () => removeCard(c) },
    ]);
  }

  /** nudge moves a card with Alt and an arrow: up and down in its column,
   *  left and right to the end of the next column. */
  function nudge(c, key) {
    const at = columnOf(cards, c.status).findIndex((x) => x.id === c.id);
    const col = STATUSES.findIndex(([s]) => s === c.status);
    if (key === "ArrowUp" && at > 0) move(c.id, c.status, at - 1);
    else if (key === "ArrowDown") move(c.id, c.status, at + 1);
    else if (key === "ArrowLeft" && col > 0) move(c.id, STATUSES[col - 1][0], Infinity);
    else if (key === "ArrowRight" && col < STATUSES.length - 1) move(c.id, STATUSES[col + 1][0], Infinity);
  }

  /** move shows the card in its new place at once, and puts it back if the
   *  server refuses. */
  async function move(id, status, index) {
    const card = cards.find((c) => c.id === id);
    if (!card) return;
    const at = columnOf(cards, card.status, id).findIndex((c) => c.position > card.position);
    const current = at < 0 ? columnOf(cards, card.status, id).length : at;
    if (status === card.status && Math.min(index, columnOf(cards, status, id).length) === current) return;

    const before = { status: card.status, position: card.position };
    const next = placeCard(cards, id, status, index);
    Object.assign(card, next);
    saving++;
    draw();
    kanban.querySelector(`.kanban-card[data-id="${CSS.escape(id)}"]`)?.classList.add("saving");
    try {
      Object.assign(card, await api.updateCard(id, next));
    } catch (err) {
      Object.assign(card, before);
      toast(err.message || "The card could not be moved.", "bad");
    } finally {
      saving--;
      draw();
    }
  }

  async function refresh() {
    const fresh = await api.squad(squad.id);
    // Unchanged, the board is left alone, so nothing flickers.
    if (JSON.stringify(fresh.cards_list || []) === JSON.stringify(cards)) return;
    cards = fresh.cards_list || [];
    draw();
  }

  // Agents move cards as they work, so the board follows along.
  every(ctx, POLL_MS, () => {
    if (dragging || saving || document.querySelector(".menu")) return;
    return refresh();
  });

  /** agentSelect offers the members, and the card's agent if it has left
   *  the squad since. */
  function agentSelect(selected, selectedName) {
    const left = selected && !members.some((m) => m.id === selected);
    return h(
      "select",
      { class: "select" },
      h("option", { value: "" }, squad.lead_id ? "The lead, in To do" : "Nobody yet"),
      members.map((a) => h("option", { value: a.id, selected: a.id === selected }, a.name)),
      left ? h("option", { value: selected, selected: true }, selectedName || "Not a member") : null,
    );
  }

  function statusSelect(selected) {
    return h(
      "select",
      { class: "select" },
      STATUSES.map(([v, label]) => h("option", { value: v, selected: v === selected }, label)),
    );
  }

  function cardFields(c) {
    const f = {
      title: h("input", { class: "input", value: c.title || "", placeholder: "Answer the open supplier questions" }),
      body: h(
        "textarea",
        { class: "input", rows: 5, placeholder: "What needs doing, and what done looks like." },
        c.body || "",
      ),
      status: statusSelect(c.status || "backlog"),
      agent: agentSelect(c.agent_id || "", c.agent_name),
      labels: h("input", { class: "input", value: (c.labels || []).join(", "), placeholder: "finance, urgent" }),
      due: h("input", { class: "input", type: "date", value: dueDay(c.due_at) }),
    };
    f.el = [
      field("Title", f.title),
      field("Details", f.body, "The agent reads this as its task."),
      h(
        "div",
        { class: "field-row" },
        field("Column", f.status),
        field("Agent", f.agent, "In To do, a member picks it up on its own."),
      ),
      h("div", { class: "field-row" }, field("Labels", f.labels, "Comma separated."), field("Due", f.due)),
    ];
    f.values = () => {
      if (!f.title.value.trim()) fail(f.title, "Give the card a title.");
      return {
        title: f.title.value.trim(),
        body: f.body.value.trim(),
        status: f.status.value,
        agent_id: f.agent.value,
        labels: splitList(f.labels.value),
        due_at: dueISO(f.due.value),
      };
    };
    return f;
  }

  function newCard(status) {
    const f = cardFields({ status });
    formModal({
      title: "New card",
      subtitle: squad.name,
      wide: true,
      body: h("form", {}, f.el),
      submitLabel: "Add card",
      done: "Card added",
      submit: async () => {
        const created = await api.createCard(squad.id, f.values());
        cards.push(created);
        draw();
        return created;
      },
    });
  }

  function removeCard(c) {
    confirm({
      title: `Delete ${c.title}?`,
      body: c.task_id ? "Its task stays in Tasks." : "Its comments go with it.",
      confirmLabel: "Delete card",
      danger: true,
      onConfirm: async () => {
        await api.deleteCard(c.id);
        cards = cards.filter((x) => x.id !== c.id);
        draw();
      },
      done: "Card deleted",
    });
  }

  async function openCard(id) {
    let c;
    try {
      c = await api.card(id);
    } catch (err) {
      toast(err.message, "bad");
      return;
    }
    const f = cardFields(c);
    const thread = h("div", { class: "comments" });
    const paintComments = () =>
      replace(
        thread,
        (c.comments_list || []).length
          ? c.comments_list.map((m) => comment(m))
          : h("div", { class: "faint small" }, "No comments yet."),
      );
    paintComments();
    const reply = h("textarea", { class: "input", rows: 2, placeholder: "Write a comment" });
    const send = busyButton({ class: "btn", type: "button" }, "Comment", async () => {
      const text = reply.value.trim();
      if (!text) return reply.focus();
      const m = await api.comment(c.id, text);
      c.comments_list = [...(c.comments_list || []), m];
      reply.value = "";
      paintComments();
      refresh().catch(() => {});
    });
    // A textarea takes Enter for new lines, so Ctrl+Enter sends.
    reply.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        send.click();
      }
    });

    const parent = c.parent_id ? cards.find((x) => x.id === c.parent_id) : null;
    const children = cards.filter((x) => x.parent_id === c.id);
    const cardLink = (x) =>
      h(
        "a",
        {
          href: "#",
          onClick: (e) => {
            e.preventDefault();
            close();
            openCard(x.id);
          },
        },
        x.title,
      );
    const body = h(
      "div",
      {},
      h("form", {}, f.el),
      parent ? h("div", { class: "field" }, h("label", {}, "Handed out from"), h("div", {}, cardLink(parent))) : null,
      children.length
        ? h(
            "div",
            { class: "field" },
            h("label", {}, "Handed out"),
            h(
              "div",
              { class: "stack" },
              children.map((x) =>
                h(
                  "div",
                  { class: "row-tight" },
                  cardLink(x),
                  h(
                    "span",
                    { class: "faint small" },
                    `${statusLabel(x.status)}${x.agent_name ? ", " + x.agent_name : ""}`,
                  ),
                ),
              ),
            ),
          )
        : null,
      c.task
        ? h(
            "div",
            { class: "field" },
            h("label", {}, "Task"),
            h(
              "div",
              { class: "row" },
              taskStatePill(c.task.state),
              h(
                "a",
                {
                  href: taskPath(c.task.id),
                  // The dialog closes first, or it would stay over the task.
                  onClick: (e) => {
                    close();
                    go(ctx, taskPath(c.task.id))(e);
                  },
                },
                "Open the task",
              ),
              h(
                "span",
                { class: "faint small" },
                c.task.finished_at ? `finished ${ago(c.task.finished_at)}` : `given ${ago(c.task.created_at)}`,
              ),
            ),
          )
        : null,
      h(
        "div",
        { class: "field" },
        h("label", {}, "Comments"),
        thread,
        reply,
        h("div", { class: "row", style: { marginTop: "6px" } }, h("div", { class: "spacer" }), send),
      ),
    );

    const { close } = formModal({
      title: c.title,
      subtitle: `${statusLabel(c.status)} · added ${ago(c.created_at)}`,
      wide: true,
      body,
      // Not btn-danger: the dialog wires Enter to the first of those.
      lead: (close) =>
        h(
          "button",
          {
            class: "btn btn-delete",
            onClick: () => {
              close();
              removeCard(c);
            },
          },
          "Delete",
        ),
      done: "Card saved",
      submit: async () => {
        const values = f.values();
        // A new column puts it at the bottom there.
        if (values.status !== c.status) Object.assign(values, placeCard(cards, c.id, values.status, Infinity));
        const updated = await api.updateCard(c.id, values);
        const i = cards.findIndex((x) => x.id === c.id);
        if (i >= 0) cards[i] = { ...cards[i], ...updated };
        draw();
      },
    });
  }

  function comment(m) {
    const agent = m.agent_id ? agents.find((a) => a.id === m.agent_id) : null;
    const who = agent ? agent.name : m.author || "Someone";
    return h(
      "div",
      { class: "comment" },
      avatarOf(who, { agent: !!m.agent_id, icon: agent && agent.icon }),
      h(
        "div",
        {},
        h("div", { class: "comment-head" }, h("strong", {}, who), stamp(m.created_at, { cls: "faint" })),
        h("div", { class: "comment-body" }, render(m.body)),
      ),
    );
  }

  const squadMenu = h(
    "button",
    {
      class: "btn",
      title: "Squad actions",
      "aria-label": "Squad actions",
      "aria-haspopup": "menu",
      onClick: (e) =>
        menu(e.currentTarget, [
          { label: "Edit squad", icon: icons.pencil, onClick: () => squadForm(ctx, agents, squad) },
          { separator: true },
          { label: "Delete squad", icon: icons.trash, onClick: () => deleteSquad(ctx, squad, cards.length) },
        ]),
    },
    icon(icons.more),
  );

  draw();
  return h(
    "div",
    {},
    detailHead(
      ctx,
      ["/squads", "Squads"],
      members.length ? [memberChips(ctx, squad)] : [],
      h(
        "div",
        { class: "row-tight" },
        squadMenu,
        h("button", { class: "btn btn-primary", onClick: () => newCard("backlog") }, icon(icons.plus), "New card"),
      ),
    ),
    members.length
      ? null
      : banner(
          "info",
          "No members yet, so cards are for people only. ",
          h(
            "a",
            {
              href: "#",
              onClick: (e) => {
                e.preventDefault();
                squadForm(ctx, agents, squad);
              },
            },
            "Add agents",
          ),
          " to hand them work.",
        ),
    kanban,
    h(
      "div",
      { class: "hint", style: { marginTop: "14px" } },
      "A card in To do goes to its agent, or to the lead if it has none. The agent comments with the result and moves it to Review. It may hand out parts as new cards and wait for them.",
    ),
  );
}

function deleteSquad(ctx, squad, count) {
  confirm({
    title: `Delete ${squad.name}?`,
    body: count
      ? `Its ${count} ${plural(count, "card")} go with it. Their tasks stay in Tasks, and its agents stay.`
      : "Its agents stay.",
    confirmLabel: "Delete squad",
    danger: true,
    onConfirm: async () => {
      await api.deleteSquad(squad.id);
      ctx.navigate("/squads");
    },
    done: `${squad.name} deleted`,
  });
}
