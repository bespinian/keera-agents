# The panel API

What the panel calls. It is not a stable public API yet; automation should go
through the gateway. The runtime API agents call is in
[internal/protocol](../internal/protocol/protocol.go).

Same origin as the panel. JSON everywhere. Errors: `{"error":{"type":"...","code":"...","message":"..."}}`
(message is for people; show it as-is). The session is an HttpOnly cookie. Unsafe methods
(POST/PATCH/PUT/DELETE) need header `X-CSRF-Token: <csrf from /api/v1/me>`. 401 with code
`unauthenticated` means show the sign-in screen. Times are RFC 3339. Money: `*_micros` are micro-CHF.

Instead of the cookie, a caller may send a gateway sign-in as
`Authorization: Bearer <token>`, as Keera Chat does. It needs no CSRF token,
since a browser never adds the header on its own.

## Who may do what

Every member sees the organisation's agents, schedules, squads and connector
names, and handlers check the rest themselves. Another organisation's things
answer 404.

- An agent's owner and administrators change, delete, start, stop or restart
  it, read its logs, and add or remove its connectors and schedules.
- Whoever the agent's `access` lets gives it work: a task, a chat message, or
  a card assigned to it (see [tasks.md](tasks.md#squads)).
- A task is seen by whoever asked, the agent's owner and administrators.
- Only administrators manage connectors, read the audit log and what
  machines cost, take an agent over, and choose an agent's project or image.
- Everyone connects and disconnects their own account to a connector people
  sign in to, and only their own.
- A workspace is its owner's. Administrators also see, change, stop and
  delete it. Nobody else sees it: it answers 404. Only the owner connects.

## Sign-in (like Keera Chat)

- `GET /auth/config` → `{providers:[{name,label}], passkeys:bool, gateway_url, sign_in_error}` (sign_in_error shown once)
- `GET /auth/login?provider=<name>` → browser navigation (redirects to the gateway). Passkeys: `provider=passkey`.
- `POST /auth/logout` → `{ok:true}`

## Me

- `GET /api/v1/me` →
  `{user_id,email,name,role:"operator"|"admin"|"member",org_id,org_name,can_admin:bool,csrf,gateway_url,chat_url,
orgs:[{id,name}] (only when >1), runtime:{driver:"podman"|"kubernetes"|"", tiers:["standard","isolated","vm"]},
builtins:["memory","files","shell","web"], timezone_default:"Europe/Zurich",
billing:true (only when machines are billed), credit_refusal:{code,message} (only when no machine may start)}`
- `POST /api/v1/me/org {org_id}` → switches organisation; reload after.
- `GET /api/v1/catalog?project_id=` → `{project_id, projects:[{id,name}], models:[{alias,kind:"model"|"router",capabilities}], transcription_models:[{alias,kind}], mcp_servers:[{alias}]}`
  What an agent in that project may use. `projects` is only filled for admins (members get the default project).
- `GET /api/v1/members` → `{data:[{id,email,name}]}` (administrators: the organisation's active members, whom they may create an agent or a workspace for)

## Overview

What the panel's dashboard shows.

- `GET /api/v1/overview?since=168h` →
  `{agents:{total,running,suspended,stopped,failed}, tasks:{queued,running,succeeded,failed},
series:[{at,tasks,failed}], bucket:"hour"|"day",
recent_tasks:[Task] (7), upcoming:[{schedule_id,schedule_name,agent_id,agent_name,next_run_at}] (7)}`

`since` is from `1h` to `2160h`, `168h` by default. `tasks` counts the tasks
that came in since then, and any still queued or running. `series` counts
those that came in, per hour up to two days and per day beyond, with days in
`Europe/Zurich`.

- `GET /api/v1/setup` → `{agents, connections, started:bool}`

What the panel's getting started guide ticks off. It takes the dashboard's
place until `started`: once the organisation has had a task, or has an agent
older than the 90 days finished tasks are kept. So the guide does not come
back when old tasks are deleted.

## Agents

Agent:

```
{id,name,icon,description,instructions,model,transcription_model,ask_models:[...],mcp_servers:[...]|null,builtins:[...],
 max_steps,access:"owner"|"org"|"people",
 allowed_emails:[...],budget_micros,class:{isolation,cpu,memory,disk},image,
 state:"stopped"|"starting"|"running"|"suspended"|"failed",state_detail,enabled,
 owner_id,owner_email,owner_name,project_id,last_active_at,created_at,updated_at,
 queued:int,running_task_id:string, can_edit:bool, can_give:bool, binding_count:int}
```

`icon` names one of the panel's agent icons, such as `code`; empty is the
default robot. A name the panel does not know shows the default.

- `GET /api/v1/agents` → `{data:[Agent]}`
- `POST /api/v1/agents` body: `{name,icon,description,instructions,model,transcription_model,ask_models,mcp_servers,builtins,max_steps,access,allowed_emails,budget_micros,class,image,project_id,owner_id}` → 201 Agent
  (name: 1–60 chars; unique in the org, case-insensitive; max_steps: 0, for no limit, to 10000;
  transcription_model: one of the catalogue's transcription_models, or empty; ask_models: from its models;
  mcp_servers: aliases from its mcp_servers, or null, the default, for all it allows, also later ones;
  access: "owner" by default; owner_id: administrators only, an active member; the caller by default)
- `GET /api/v1/agents/{id}` → Agent + `{bindings:[Binding], schedules:[Schedule], stats:{last_task_at}}`
- `GET /api/v1/agents/{id}/activity?since=168h` → `{tasks:{queued,running,succeeded,failed}, series:[{at,tasks,failed}], bucket:"hour"|"day"}` (the overview's counts and chart, for one agent)
- `PATCH /api/v1/agents/{id}` (any subset of the POST fields, but not another owner_id: see take-over) → Agent
- `DELETE /api/v1/agents/{id}` → 204 (stops its container, deletes its volume, revokes its key)
- `POST /api/v1/agents/{id}/start` | `/stop` | `/restart` → Agent
- `POST /api/v1/agents/{id}/take-over` → Agent (administrators: become the owner, with a new key)
- `GET /api/v1/agents/{id}/logs?follow=1` → `text/plain` stream (the container's output)
- `POST /api/v1/agents/{id}/tasks {prompt}` → 201 Task (a task given in the panel)

Binding:

```
{id,agent_id,agent_name,agent_kind:"agent"|"workspace",connection_id,connection_name,kind,target,label,mode:"chat"|"watch"|"files"|"repo"|"read"|"write",prompt,checked_at,created_by,created_at}
```

- `POST /api/v1/agents/{id}/bindings {connection_id,target,label,mode,prompt}` → 201 Binding
  (`target` "*" for a chat binding = every conversation the bot is in, DMs included;
  for an API kind with `any_target` = everything the connector reaches; ignored for
  an API kind without `target_label`. `read` and `write` are only for API kinds, and
  their target is checked with the service first: 400 with its reason)
- `DELETE /api/v1/bindings/{id}` → 204

## Schedules

Schedule:

```
{id,agent_id,agent_name,name,cron,timezone,prompt,reply_binding_id,enabled,next_run_at,last_run_at,description,created_by,requested_by,created_at}
```

`description` is the cron in words, e.g. "Every weekday at 08:00".

- `GET /api/v1/schedules?agent_id=` → `{data:[Schedule]}`
- `POST /api/v1/schedules {agent_id,name,cron,timezone,prompt,reply_binding_id,enabled}` → 201 Schedule
- `PATCH /api/v1/schedules/{id}` (subset) → Schedule
- `DELETE /api/v1/schedules/{id}` → 204
- `POST /api/v1/schedules/{id}/run` → 201 Task (run now)
- `POST /api/v1/schedules/preview {cron,timezone}` → `{description, next:[5 times]}` or 400 with the reason

## Squads and their boards

A squad is agents that share a board; see [tasks.md](tasks.md#squads).
Statuses in order: `backlog`, `todo`, `in_progress`, `review`, `done`
(labels: Backlog, To do, In progress, Review, Done).

Squad:

```
{id,name,goal,lead_id,members:[{id,name,icon,description}],created_by,created_at,
 cards:int,open:int}
```

Card:

```
{id,squad_id,title,body,status,position,agent_id,agent_name,labels:[...],due_at,task_id,task_state,
 created_by,parent_id,depth,waiting,requested_by,created_at,updated_at,comments:int}
```

`parent_id` is the card an agent handed this one out from; `waiting` is a card
waiting for the cards its agent handed out.

- `GET /api/v1/squads` → `{data:[Squad]}`
- `POST /api/v1/squads {name,goal,members:[agent ids],lead_id}` → 201 Squad. Members are the
  organisation's agents, not workspaces; the lead is a member or null.
- `PATCH /api/v1/squads/{id} {name,goal,members,lead_id}` → Squad (only what is given changes).
- `DELETE /api/v1/squads/{id}` → 204, with its cards
- `GET /api/v1/squads/{id}` → Squad + `{cards_list:[Card]}` (sorted by status order then position)
- `POST /api/v1/squads/{id}/cards {title,body,status,agent_id,labels,due_at}` → 201 Card (appended
  to the column). `agent_id` must be a member.
- `GET /api/v1/cards/{id}` → Card + `{comments_list:[{id,author,agent_id,body,created_at}], task: Task|null}`
- `PATCH /api/v1/cards/{id} {title,body,status,position,agent_id,labels,due_at}` → Card
  (drag and drop: send `status` and `position`; position = midpoint between neighbours, or neighbour ± 1)
- `DELETE /api/v1/cards/{id}` → 204
- `POST /api/v1/cards/{id}/comments {body}` → 201 comment (403 on an assigned card in To do, for
  whoever may not give its agent work)

## Tasks

Task:

```
{id,agent_id,agent_name,agent_icon,source:"chat"|"schedule"|"board"|"file"|"manual",origin_label,prompt,context,
 requested_by,state:"queued"|"running"|"succeeded"|"failed"|"cancelled",result,error,delivered,
 created_at,started_at,finished_at}
```

`origin_label` is e.g. "Slack #sales", "Schedule Daily digest", "Card Fix onboarding doc", "Google Drive Invoices", "Panel".

- `GET /api/v1/tasks?agent_id=&state=&source=&limit=50&before=<created_at>` → `{data:[Task]}`
- `GET /api/v1/tasks/facets?agent_id=` → `{agents:[{id,name}], states:[...], sources:[...]}`: what the
  caller's tasks have, so the panel offers only filters that find something
- `GET /api/v1/tasks/{id}` → Task + `{events:[{id,kind:"message"|"tool_call"|"tool_result"|"log",text,data,at}], attached?:{images,files:[name]}}`.
  `attached` names what came with a message from Keera Chat.
  (`data`, on tool events only: `{tool,call_id,arguments}` or `{tool,call_id,is_error}`)
- `GET /api/v1/tasks/{id}/events?after=<id>` → `{events:[...], task}` (poll every 2s while running)
- `POST /api/v1/tasks/{id}/cancel` → Task

## Keera Chat

What Keera Chat calls, with the person's gateway sign-in. See
[tasks.md](tasks.md#keera-chat).

- `GET /api/v1/chat/agents` → `{data:[{id,name,icon,description,state,vision}]}`: the started agents the caller may give work to.
  `vision` says the agent's model reads images.
- `POST /api/v1/chat/agents/{id}/messages {conversation,text,images?,files?}` → 201 Task (`source` `chat`, `origin_label` "Keera Chat").
  `conversation` is Keera Chat's id of the chat: 1–64 letters, digits, `-` or `_`.
  `images` are up to 10 PNG, JPEG, GIF or WebP data URLs, only for an agent with `vision`; `files` up to 10 `{name,text}` of 512 KB.
  A message may be only attachments. The request may be up to 32 MB.
  Follow it with `GET /api/v1/tasks/{id}/events`, and stop it with `POST /api/v1/tasks/{id}/cancel`.

## Connectors (admins create/change; everyone may list names to bind)

The panel calls them connectors; the API keeps the name connections.

Kind descriptor (`GET /api/v1/connection-kinds` → `{data:[Descriptor],callback_url}`):

```
{kind,label,vendor,category,summary,chat:bool,files:bool,code:bool,api:bool,
 tools:[{name,description,write}] (API kinds),
 fields:[{name,label,help,secret,required,multiline,endpoint}],setup (text; "{webhook}" placeholder),
 target_label,target_help,any_target:bool,
 sign_in:{fields,setup} (kinds people may sign in to; setup has a "{callback}" placeholder)}
```

The kinds are listed in [connectors.md](connectors.md).

Connection:

```
{id,kind,name,config:{...non-secret fields},secrets_set:["field",...],webhook_url (chat kinds),
 per_user:bool,account:{account,created_at,updated_at} (the caller's own sign-in, when per_user),
 created_by,created_at,updated_at,bindings:int}
```

`per_user` connectors act as each person, who signs in with their own account
(see [connectors.md](connectors.md#each-persons-own-account)). It is set when the
connector is created, from `sign_in.fields` instead of `fields`, and kept.

- `GET /api/v1/connections` → `{data:[Connection], sign_in_result?:{connection_id,error}}` (how the caller's last sign-in went, shown once)
- `POST /api/v1/connections {kind,name,config:{},secrets:{},per_user}` → 201 Connection (checked with the service first; 400 with the service's reason)
- `GET /api/v1/connections/{id}` → Connection + `uses:[Binding]`: where agents use it. Other people's workspaces are left out for a member; `bindings` still counts them.
- `PATCH /api/v1/connections/{id} {name,config,secrets}` (a secret left out or empty keeps its value, unless an `endpoint` field changes; then 400 until it is sent again)
- `DELETE /api/v1/connections/{id}` → 204
- `POST /api/v1/connections/{id}/check` → `{ok:true}` or 400
- `GET /api/v1/connections/{id}/folders?parent=<id>` → `{data:[{id,name,folder,web_url,modified}]}` (file kinds; for a folder picker)
- `GET /api/v1/connections/{id}/channels` → `{data:[{id,name,private,direct}],notice}` (chat kinds that list channels, such as Slack; for a channel picker). The first entry is your direct messages with the bot; `notice` says why it is missing.
- `POST /api/v1/connections/{id}/account` → `{url}`: where the browser goes to sign in. The service sends it back to
  `GET /auth/connector/callback`, which keeps the sign-in and redirects to `/connectors`.
- `DELETE /api/v1/connections/{id}/account` → 204 (404 when the caller has not connected one)

## Workspaces

A workspace is a row of agents with `kind:"workspace"` and an `experience`
([workspaces.md](workspaces.md)). `{id}` is a workspace's id, or the name of
one of the caller's own.

- `GET /api/v1/workspaces?all=true` → `{data:[Workspace]}`, the caller's, or with `all` every one in the
  organisation (admins). Workspace = Agent fields + `{can_edit, can_connect}`.
- `POST /api/v1/workspaces {name, experience:"terminal"|"vscode", description?, model?, class?, project_id?, image?, owner_id?}`
  → 201 Workspace. Without a model, the project's first. owner_id: administrators only, an active member; the
  caller by default. An administrator cannot choose the image of someone else's.
- `GET /api/v1/workspaces/{id}` → Workspace + `bindings:[Binding]`
- `PATCH /api/v1/workspaces/{id}` → Workspace. Its owner never changes. A new experience, image or class starts the machine again.
- `DELETE /api/v1/workspaces/{id}`, `POST …/start`, `POST …/stop`, `GET …/logs?follow=1`
- `POST /api/v1/workspaces/{id}/bindings` → as for an agent, with the modes `files`, `repo`, `read` or `write`.
- `POST /api/v1/workspaces/{id}/connect?port=web|ssh|<n>` with `Connection: Upgrade`, `Upgrade: keera-stream`
  → 101, then raw bytes to that port in the workspace. Owner only. Wakes a suspended workspace first, waiting
  up to three minutes. 426 without the upgrade.

## SSH keys

Each person's own, in their current organisation.

- `GET /api/v1/me/ssh-keys` → `{data:[{id,name,public_key,fingerprint,created_at}]}`
- `POST /api/v1/me/ssh-keys {public_key, name?}` → 201. One authorized_keys line, without options. 409 if added already.
- `DELETE /api/v1/me/ssh-keys/{id}` → 204

## Audit (admins)

- `GET /api/v1/audit?limit=100&before=<id>` → `{data:[{id,at,actor,action,target,detail}]}`

## Billing

Only on a deployment that bills machine time; otherwise 404 `billing_off`. See [billing.md](billing.md).

- `GET /api/v1/billing?month=YYYY-MM` →
  `{month, from, to, prices:{cpu_hour_micros,memory_gib_hour_micros,disk_gib_month_micros},
machines:[{id,name,kind,owner_email,deleted,cpu_milli_seconds,memory_mib_seconds,disk_mib_seconds,
cpu_hours,memory_gib_hours,disk_gib_months,cost_micros}], total_micros,
credit:{payments,balance_micros,refusal:{code,message}|null}|null}`
  Months are UTC, like the gateway's bill. Costs are at today's prices. Admins only.
- `GET /api/v1/agents/{id}/cost` →
  `{month, month_micros, hour_micros}`
  What one agent's machine cost this month, and an hour of it running. Its owner and admins.
- `GET /api/v1/workspaces/{id}/cost` → the same, for a workspace.
- Starting an agent or workspace, or connecting to one, answers 402 with `credit_exhausted`, `charge_due` or
  `org_limited` when its organisation may not run machines.
