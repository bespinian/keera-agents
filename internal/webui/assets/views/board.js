// The board: cards in five columns, for people and agents alike. A card in
// To do with an agent is picked up by it; the agent moves it on as it works.

import { api } from "../api.js";
import {
  h,
  icon,
  icons,
  iconButton,
  menu,
  formModal,
  fail,
  confirm,
  field,
  toast,
  replace,
  listHead,
  remembered,
  remember,
  banner,
  empty,
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
import { taskStatePill, taskPath } from "../common.js";
import { code } from "../markdown.js";

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

export async function boardView(ctx) {
  const [boardsRes, agentsRes] = await Promise.all([api.boards(), api.agents().catch(() => ({ data: [] }))]);
  let boards = boardsRes.data || [];
  const agents = agentsRes.data || [];
  const iconOf = (id) => (agents.find((a) => a.id === id) || {}).icon;
  if (!boards.length) {
    return h(
      "div",
      {},
      listHead(null, null, { label: "New board", onClick: () => newBoard(ctx) }),
      h("div", { class: "card" }, empty("No board yet", "A board holds cards for people and agents.")),
    );
  }
  const saved = remembered("keera.board", { lasting: true });
  const boardID = boards.some((b) => b.id === saved) ? saved : boards[0].id;
  const board = await api.board(boardID);
  let cards = board.cards || [];
  ctx.setTitle(boards.length > 1 ? board.name : "Board", boards.length > 1 ? "Board" : board.name);

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
        c.task_state ? taskStatePill(c.task_state) : null,
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
      { label: "Open", icon: icons.pencil, onClick: () => openCard(c.id) },
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
    const fresh = await api.board(board.id);
    // Unchanged, the board is left alone, so nothing flickers.
    if (JSON.stringify(fresh.cards || []) === JSON.stringify(cards)) return;
    cards = fresh.cards || [];
    draw();
  }

  // Agents move cards as they work, so the board follows along.
  every(ctx, POLL_MS, () => {
    if (dragging || saving || document.querySelector(".menu")) return;
    return refresh();
  });

  function agentSelect(selected) {
    return h(
      "select",
      { class: "select" },
      h("option", { value: "" }, "Nobody yet"),
      agents.map((a) => h("option", { value: a.id, selected: a.id === selected }, a.name)),
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
      agent: agentSelect(c.agent_id || ""),
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
        field("Agent", f.agent, "In To do, the agent picks it up on its own."),
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
      subtitle: board.name,
      wide: true,
      body: h("form", {}, f.el),
      submitLabel: "Add card",
      done: "Card added",
      submit: async () => {
        const created = await api.createCard(board.id, f.values());
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

    const body = h(
      "div",
      {},
      h("form", {}, f.el),
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
        h("div", { class: "comment-body" }, code(m.body)),
      ),
    );
  }

  const picker =
    boards.length > 1
      ? h(
          "select",
          {
            class: "select toolbar-select",
            "aria-label": "Board",
            onChange: (e) => {
              remember("keera.board", e.target.value, { lasting: true });
              ctx.reload();
            },
          },
          boards.map((b) => h("option", { value: b.id, selected: b.id === board.id }, b.name)),
        )
      : null;

  const boardMenu = h(
    "button",
    {
      class: "btn",
      title: "Board actions",
      "aria-label": "Board actions",
      "aria-haspopup": "menu",
      onClick: (e) =>
        menu(e.currentTarget, [
          { label: "Rename board", icon: icons.pencil, onClick: () => renameBoard(ctx, board) },
          { label: "New board", icon: icons.plus, onClick: () => newBoard(ctx) },
          boards.length > 1 ? { separator: true } : null,
          boards.length > 1
            ? { label: "Delete board", icon: icons.trash, onClick: () => deleteBoard(ctx, board, cards.length) }
            : null,
        ]),
    },
    icon(icons.more),
  );

  draw();
  return h(
    "div",
    {},
    listHead(
      null,
      [picker, boardMenu],
      { label: "New card", onClick: () => newCard("backlog") },
    ),
    agents.length
      ? null
      : banner(
          "info",
          "No agents yet, so cards are for people only. ",
          link(ctx, "/agents", "Create an agent"),
          " to hand them work.",
        ),
    kanban,
    h(
      "div",
      { class: "hint", style: { marginTop: "14px" } },
      "A card in To do with an agent is picked up by it. It comments with its result and moves the card to Review, or to Backlog if it could not finish.",
    ),
  );
}

function newBoard(ctx) {
  const name = h("input", { class: "input", placeholder: "Finance" });
  formModal({
    title: "New board",
    body: h("form", {}, field("Name", name)),
    submitLabel: "Create board",
    done: "Board created",
    submit: async () => {
      if (!name.value.trim()) fail(name, "Give the board a name.");
      const b = await api.createBoard(name.value.trim());
      remember("keera.board", b.id, { lasting: true });
      ctx.reload();
      return b;
    },
  });
}

function renameBoard(ctx, board) {
  const name = h("input", { class: "input", value: board.name });
  formModal({
    title: "Rename board",
    body: h("form", {}, field("Name", name)),
    done: "Board renamed",
    ctx,
    submit: () => {
      if (!name.value.trim()) fail(name, "Give the board a name.");
      return api.renameBoard(board.id, name.value.trim());
    },
  });
}

function deleteBoard(ctx, board, count) {
  confirm({
    title: `Delete ${board.name}?`,
    body: count ? `Its ${count} ${plural(count, "card")} go with it. Their tasks stay in Tasks.` : "It has no cards.",
    confirmLabel: "Delete board",
    danger: true,
    onConfirm: async () => {
      await api.deleteBoard(board.id);
      remember("keera.board", null, { lasting: true });
    },
    done: `${board.name} deleted`,
    ctx,
  });
}
