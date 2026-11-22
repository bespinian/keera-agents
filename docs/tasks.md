# Tasks

A task is one piece of work for one agent. This page explains where tasks
come from, who they are attributed to, and where their results go.

| Source     | Made when                                       | Attributed to            | Result goes to                        |
| ---------- | ----------------------------------------------- | ------------------------ | ------------------------------------- |
| `chat`     | someone writes to the agent in a connected chat | the sender               | a reply in the same thread            |
| `chat`     | someone writes to the agent in Keera Chat       | the sender               | Keera Chat reads it from the task     |
| `schedule` | a schedule is due                               | who last changed it      | the schedule's chat, if it has one    |
| `board`    | a card assigned to the agent sits in To do      | who last gave it work    | a comment, and the card moves on      |
| `file`     | a file is added or changed in a watched folder  | who connected the folder | the task log                          |
| `manual`   | someone types a task in the panel               | that person              | the panel                             |

Every task is in the panel's task log with what the agent did: its messages,
tool calls and results. The gateway sees each task as one session, named by
the task's id.

## Chats

**Who is asking.** The chat service tells keera-agents the sender's email.
The gateway says whether that address is a member of the agent's organisation.
Then the agent's rule decides: its owner only, everyone in the organisation, or
listed people. Anyone else gets a short refusal in the thread, and nothing is
queued. A sender without an email, as when the app may not read addresses, is
refused too: an agent never works for someone it cannot name.

**Which agent.** Several agents can share one bot. A message that starts with
an agent's name, such as `Scout: …` or `@Scout …`, goes to that agent; the
longest name that fits wins. In a channel only one agent listens on, it goes
to that one. An agent connected to a channel beats one connected to every
conversation (`*`).

People cannot see which agent answers, since all of them use the same bot. So
a member may only connect their agent to a conversation where no one else's
agent listens, there or on `*`. An administrator can add more.

**Quiet by default.** In a channel, an agent only sees messages that mention
the bot. In a channel nobody listens on, keera-agents says nothing.

**Slow answers say so.** If a task has not finished 10 seconds after the
message, the agent replies in the thread that it is working on it, or that it
is finishing another task first. A quicker answer comes alone. This note is
not saved anywhere, so a restart in those 10 seconds skips it.

**Redelivered webhooks** are dropped by message id, so a service retrying a
slow webhook does not make two tasks.

## Keera Chat

When the gateway has both Keera Chat and Keera Agents connected
(`KEERA_CHAT_URL` and `KEERA_AGENTS_URL`), Keera Chat offers people their
agents next to the models. It finds keera-agents through the gateway, so
neither needs a setting for the other.

Keera Chat calls keera-agents with the person's own gateway sign-in, so the
agent's access rule applies as in any chat. It lists only the started agents
that take work from the person. Each message is a chat task. Keera Chat shows
its answer, with the tools the agent used. Stopping the reply
cancels the task. An agent's page in the panel has a **Chat** button that
starts a chat with it in Keera Chat.

The agent keeps the chat's history in its workspace, one per person and chat,
as for any other chat. So Keera Chat sends only the new message. Agents take
text only: no images or files.

## Schedules

The panel asks when a schedule repeats as a calendar does: every day, week,
month or year at a time, or several times a day, such as every 30 minutes
between 08:00 and 18:00 on weekdays. It shows the schedule in words and the
next five runs.

Underneath, a schedule is five cron fields - minute, hour, day of month,
month, day of week - in a time zone, `Europe/Zurich` unless chosen. `@daily`,
`@weekly` and the like work too. A cron the picker cannot show, such as
`0 8,17 * * *`, is kept as it is, under Custom.

When the clocks go forward, a run in the hour that never happens runs right
after the change. When they go back, a run in the hour that comes twice runs
once. A schedule that runs every hour keeps its usual minutes either way.

Exactly one replica starts each run: the run is claimed by moving the
schedule's next time with a compare-and-set, in the same transaction that
queues its task. So a run is never claimed without its task: if queueing
fails, the next round tries again. A run missed by more than a day,
as after a long outage, is skipped rather than run late. An expression that
never matches again turns the schedule off.

A schedule runs for whoever changed it last, and a run started from the panel
runs for whoever started it. So changing someone else's schedule never
borrows their rights.

## The board

Five columns: Backlog, To do, In progress, Review, Done. Boards belong to the
organisation; everyone in it can read and change them.

A card in To do with an agent is picked up within seconds: it moves to In
progress, the agent works on it with its title, description and comments, and
the result is added as a comment. A card that went well moves to Review, for a
person to check. One that failed moves to Backlog, so it is not picked up
again in a loop. A card a person moved meanwhile stays where they put it, and
saving an edit to a card does not move it back.

Agents can use the board too: list, create, update and comment on cards. An
agent can hand work to another by creating a card assigned to it.

Assigning a card, moving an assigned card to To do, and rewriting or
commenting on an assigned card in any column give its agent work: the title,
description and comments become the task once the card reaches To do. So
whoever does it must be allowed to by that agent's `access`, and an agent
only for the person its task is for. The rule is checked again at pickup: a
card whose giver may no longer give the agent work goes back to Backlog,
with a comment saying why. The task then
runs for whoever gave the work, not whoever created the card, so editing
someone else's card never borrows their rights.

Comments are Markdown, rendered as task results are.

## Folders

A watched folder is checked every `KEERA_AGENTS_WATCH_INTERVAL`. Each file
added or changed since the last check becomes a task with the folder's prompt
and the file's name and id. Turning a watch on does not replay what is already
there. A file an agent wrote itself does not start a task for that agent, so
an agent can write to a folder it watches. Another agent watching the folder
still gets it, and so does a later change by anyone.

## Delivery

A result is delivered once, by whichever replica claims it first. A service
that answers 429 or 503 is tried up to three times in all, waiting as it asks
for up to 30 seconds: both mean it did not take the message. Other failures
are not retried but written to the task's log, so a broken chat connector
does not post the same answer ten times once it works again. What fails
before anything is sent is tried again at the next round: a card's result,
which is our own write, results a round had no time left for, and sends that
never reached the service (its name did not resolve, or it took no
connection) or never started (the database did not answer). Each retry waits
as long as the task has been finished, from 30 seconds up to 10 minutes, so
results that keep failing do not hold up the others. Retries stop an hour
after the task finished, and the task's log says why.

On shutdown, keera-agents waits up to 30 seconds for results being sent and
for messages it has already accepted.

A task runs at most `KEERA_AGENTS_TASK_TIMEOUT`, and fails after that. A task
can be cancelled in the panel by whoever asked for it and whoever may change
the agent; the runtime stops at its next report.

Finished tasks are deleted after 90 days, with their logs.
