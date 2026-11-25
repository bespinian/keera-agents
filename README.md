# Keera Agents - assistants that work where your business works

Keera Agents runs AI agents for a business. Each agent is a persistent
assistant with its own machine, workspace and memory. People give it work in
**Microsoft Teams**, **Google Chat**, **Slack** and **Keera Chat**, on a
**squad's board**, or on a **schedule**. It reads and writes files in
**Google Drive** and **OneDrive and SharePoint**, and can start work when a
file lands in a folder.
It codes in **GitHub** and **GitLab** repositories: it clones, pushes a branch
and opens a pull or merge request for a person to review. And it works in the
business's other services through typed tools: **Jira**, **Confluence**,
**Trello**, **Google Docs, Sheets, Slides, Calendar and Meet**, **Gmail**,
**Outlook Mail and Calendar**, **HubSpot**, **Salesforce**, **Freshdesk**,
**PostgreSQL**, **Kubernetes**, **AWS**, **Azure** and **Google Cloud**. It
searches the web with **Brave Search**.

It is built on **Keera Gateway**. People sign in with the gateway's sign-in.
Every model call an agent makes goes through the gateway, with a key of the
agent's own. So the gateway's models, guardrails, budgets, filters, MCP
servers and audit log apply to agents as they do to people.

```
 Teams · Google Chat · Slack ──webhooks──▶ ┌──────────── keera-agents ──────────────┐
 Google Drive · OneDrive ◀──── API ─────── │ panel · agents · tasks · squads · cron │
                                           │ dispatcher ─▶ podman | Kubernetes      │
 browser ── sign-in through the gateway ─▶ │ runtime API ◀─────────────┐            │
                                           └───────┬───────────────────┼────────────┘
                  service key: agent keys, people  │                   │ runtime token
                                                   ▼                   │
                                            Keera Gateway ◀── models ── agent's container
                                            guardrails, budgets,        (keera-agents-runtime)
                                            filters, MCP, audit         workspace volume
```

## What is in this repository

Two Go binaries from one module: `keera-agents`, the **control plane**, and
`keera-agents-runtime`, the **runtime** inside each agent's container
(`keera-agents-runtime agent`) and each workspace's
(`keera-agents-runtime workspace`). Agents run on
**podman** for local development, or on any **Kubernetes** cluster, as one
StatefulSet each.

## What it does

- **Agents** have instructions, a model or router from the gateway, tools
  (memory, files, shell, web, the gateway's MCP servers), a monthly budget and
  a rule for who may give them work. Each runs one task at a time, is
  suspended when idle and resumes when work arrives.
- **Chats.** An agent listens on channels, spaces or direct messages. The
  sender's email maps them to a Keera user, and the agent's access rule
  decides whether it takes the work. The answer is posted in the thread.
  People can also chat with their agents in Keera Chat, when the gateway has
  it connected.
- **Schedules** in cron syntax, in a time zone, post their result to a chat.
- **Squads** are agents with a shared board of five columns. A card in To do
  assigned to a member is picked up, worked on, commented on and moved to
  Review, or back to Backlog if it failed. Members hand each other parts of
  the work as cards, and get the results back.
- **Folders.** A new or changed file in a watched Drive or OneDrive folder
  becomes a task.
  Agents read and write files only in the folders they are connected to.
- **Code.** An agent connected to a repository clones it with git, runs its
  tests in its own machine, pushes branches starting with `keera/` and opens
  pull or merge requests. It cannot push to any other branch.
- **Services.** An agent connected to a service, such as a Jira project or an
  AWS region, gets tools for it, such as searching issues or reading costs.
  A binding, an agent's link to a service, may only read or also write, and
  its tools stay within what it names
  ([Connectors](https://docs.keera.ch/manage/connectors/#connect-an-agent)).
- **Credentials stay out of agents.** The control plane makes the calls that
  need them.
- **Workspaces.** An engineer gets a machine of their own with a coding
  agent: pi in a terminal, or VS Code with Cline, in the browser. It has the
  models of their key and the connectors they give it. They reach it with the
  `keera` command line, in the browser or over SSH
  ([Workspaces](https://docs.keera.ch/use/workspaces/)).

## Run it

You need Go, podman and a Keera Gateway running from its own repository. In
the gateway's `compose/.env`, add:

```sh
KEERA_PUBLIC_URL=http://localhost:8080
KEERA_AGENTS_URL=http://localhost:8070
KEERA_AGENTS_SERVICE_KEY=<the service key `make dev` prints here>
```

Run `make dev` here, which creates `compose/.env` with fresh keys the first
time, then the gateway's own `make dev`. Open <http://localhost:8070>; use
`localhost` everywhere, not `127.0.0.1`, or the sign-in is lost.

A new organisation sees **Getting started** in place of the dashboard: create
an agent, connect your services and give it a first task
([Keera Agents](https://docs.keera.ch/use/agents/)).

## Docs

The docs are at [docs.keera.ch](https://docs.keera.ch):

- [Use Keera Agents](https://docs.keera.ch/use/agents/) and
  [workspaces](https://docs.keera.ch/use/workspaces/)
- [Connectors](https://docs.keera.ch/manage/connectors/) and
  [billing](https://docs.keera.ch/manage/billing/)
- [Run Keera Agents](https://docs.keera.ch/operate/agents/), and
  [on Kubernetes](https://docs.keera.ch/operate/kubernetes/#keera-agents)
