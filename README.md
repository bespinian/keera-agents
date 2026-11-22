# Keera Agents - assistants that work where your business works

Keera Agents runs AI agents for a business. Each agent is a persistent
assistant with its own machine, workspace and memory. People give it work in
**Microsoft Teams**, **Google Chat**, **Slack** and **Keera Chat**, on a
**kanban board**, or on a **schedule**. It reads and writes files in **Google Drive** and
**OneDrive and SharePoint**, and can start work when a file lands in a folder.
It codes in **GitHub** and **GitLab** repositories: it clones, pushes a branch
and opens a pull or merge request for a person to review. And it works in the
business's other services through typed tools: **Jira**, **Confluence**,
**Trello**, **Google Docs, Sheets, Slides and Calendar**, **Gmail**,
**Outlook Mail and Calendar**, **HubSpot**, **Salesforce**, **Freshdesk**,
**PostgreSQL**, **Kubernetes**, **AWS**, **Azure** and **Google Cloud**.

It is built on **Keera Gateway**. People sign in with the gateway's sign-in.
Every model call an agent makes goes through the gateway, with a key of the
agent's own. So the gateway's models, guardrails, budgets, filters, MCP
servers and audit log apply to agents as they do to people.

```
 Teams · Google Chat · Slack ──webhooks──▶ ┌──────────── keera-agents ─────────────┐
 Google Drive · OneDrive ◀──── API ─────── │ panel · agents · tasks · board · cron │
                                           │ dispatcher ─▶ podman | Kubernetes     │
 browser ── sign-in through the gateway ─▶ │ runtime API ◀─────────────┐            │
                                           └───────┬───────────────────┼────────────┘
                  service key: agent keys, people  │                   │ runtime token
                                                   ▼                   │
                                            Keera Gateway ◀── models ── agent's container
                                            guardrails, budgets,        (keera-agents agent)
                                            filters, MCP, audit         workspace volume
```

## What is in this repository

One Go binary, `keera-agents`: the **control plane** by default,
`keera-agents agent`, the **runtime** inside each agent's container, and
`keera-agents workspace`, the runtime inside each workspace's
([docs/agents.md](docs/agents.md#one-binary-three-roles)). Agents run on
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
- **The board** has five columns. A card in To do assigned to an agent is
  picked up, worked on, commented on and moved to Review, or back to Backlog
  if it failed.
- **Folders.** A new or changed file in a watched Drive or OneDrive folder
  becomes a task.
  Agents read and write files only in the folders they are connected to.
- **Code.** An agent connected to a repository clones it with git, runs its
  tests in its own machine, pushes branches starting with `keera/` and opens
  pull or merge requests. It cannot push to any other branch.
- **Services.** An agent connected to a service, such as a Jira project or an
  AWS region, gets tools for it, such as searching issues or reading costs.
  A binding, an agent's link to a service, may only read or also write, and
  its tools stay within what it names ([docs/connectors.md](docs/connectors.md#services)).
- **Credentials stay out of agents.** The control plane makes the calls that
  need them ([docs/agents.md](docs/agents.md#what-an-agent-holds)).
- **Workspaces.** An engineer gets a machine of their own with a coding
  agent: pi in a terminal, or VS Code with Cline, in the browser. It has the
  models of their key and the connectors they give it. They reach it with the
  `keera` command line, in the browser or over SSH
  ([docs/workspaces.md](docs/workspaces.md)).

## Run it

You need Go, podman and a Keera Gateway. Set the gateway up once as
[docs/run-locally.md](docs/run-locally.md#once) says, then run `make dev` and
open <http://localhost:8070>. [docs/install.md](docs/install.md) has every
setting.

A new organisation sees **Getting started** in place of the dashboard: connect
your services, create an agent and give it a first task
([docs/api.md](docs/api.md#overview)).

## Docs

`docs/` is for operators and contributors:

- [install.md](docs/install.md) - every setting, and the deployment shapes
- [run-locally.md](docs/run-locally.md) - the development loop
- [agents.md](docs/agents.md) - how an agent runs, its keys and its lifecycle
- [workspaces.md](docs/workspaces.md) - machines to work in, with a coding agent
- [tasks.md](docs/tasks.md) - where tasks come from and where results go
- [billing.md](docs/billing.md) - billing machine time through the gateway
- [api.md](docs/api.md) - the panel's API
- [connectors.md](docs/connectors.md) - setting up the chat services, folders,
  repositories and service connectors
