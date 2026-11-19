# Connectors

A **connector** is one organisation's credentials for one service: its Slack
app, its Azure Bot, its Drive service account, its GitHub token, its Jira API
token. Administrators add connectors from the panel's **connector catalogue**,
which lists every service as tiles or a table, to search and filter by
category and vendor. Anyone who may change an agent then adds a connector on
the agent's **Connectors** tab, naming the chat, folder, repository or service
it may reach there. Each of these is a **binding**. (The API calls connectors
connections.) This page explains how each service is reached and why. The
panel shows the same setup steps next to the form.

| Service                 | Kind              | Gives agents                           | keera-agents reaches it with          |
| ----------------------- | ----------------- | -------------------------------------- | ------------------------------------- |
| Microsoft Teams         | `teams`           | messages in, replies out               | an Azure Bot (Bot Framework)          |
| Google Chat             | `googlechat`      | messages in, replies out               | a Chat app with an HTTP endpoint      |
| Slack                   | `slack`           | messages in, replies out               | a Slack app (Events API)              |
| Google Drive            | `gdrive`          | watched folders, file tools            | a service account                     |
| OneDrive and SharePoint | `onedrive`        | watched folders, file tools            | an Entra app (Microsoft Graph)        |
| GitHub                  | `github`          | git, issues, pull requests             | a fine-grained access token           |
| GitLab                  | `gitlab`          | git, issues, merge requests            | a project or group access token       |
| Jira                    | `jira`            | issue tools                            | an API token or personal access token |
| Confluence              | `confluence`      | page tools                             | an API token or personal access token |
| Trello                  | `trello`          | card tools                             | a Power-Up's API key and a token      |
| Google Docs             | `gdocs`           | find, read and write documents         | a service account                     |
| Google Sheets           | `gsheets`         | read and write spreadsheets            | a service account                     |
| Google Slides           | `gslides`         | read and change presentations          | a service account                     |
| Google Calendar         | `gcalendar`       | events and free time                   | a service account                     |
| Gmail                   | `gmail`           | mail tools                             | a service account with delegation     |
| Outlook Mail            | `outlook`         | mail tools                             | an Entra app (Microsoft Graph)        |
| Outlook Calendar        | `outlookcalendar` | events and free time                   | an Entra app (Microsoft Graph)        |
| HubSpot                 | `hubspot`         | CRM record tools                       | a private app's access token          |
| Salesforce              | `salesforce`      | CRM record tools                       | an app's client credentials           |
| Freshdesk               | `freshdesk`       | ticket tools                           | an agent's API key                    |
| PostgreSQL              | `postgres`        | SQL queries, tables, changes           | a database role and its password      |
| Kubernetes              | `kubernetes`      | workloads, logs, events, apply         | a service account token               |
| AWS                     | `aws`             | EC2, S3, CloudWatch, logs, costs       | an access key, and a role to assume   |
| Azure                   | `azure`           | resource, cost and VM tools            | an Entra app (Resource Manager)       |
| Google Cloud            | `gcp`             | resources, logs, metrics, storage, VMs | a service account                     |

**Credentials are sealed** with `KEERA_AGENTS_SECRET_KEY` and opened only to
make a call. The panel never shows them again; leaving a secret field empty
when editing keeps it, unless the server it is sent to changes, as with a new
GitHub or GitLab URL, or its TLS settings change: then it must be entered
again, so nobody can send a saved token to a server of their own. On a connector people sign in to, such a
change also signs everyone out, and they sign in again. A connector is checked with the service before it is
saved, so a typo fails at once with the service's own reason.

**The webhook address** of a chat connector is
`<KEERA_AGENTS_PUBLIC_URL>/hooks/<connector id>`. The panel shows it once
the connector is saved, and when an administrator edits it, hidden until they
press **Show**. Register it with the service. The services must reach
it from the internet, so the public URL must be reachable from there, at least
for `/hooks/`.

**Every webhook is verified** before it is read: Slack's signature, the Bot
Framework's or Google's signed token. An address alone does nothing.

## Microsoft Teams

Settings: `app_id`, `tenant_id` (empty for a multi-tenant bot),
`allowed_tenants`, `app_password`.

1. In the Azure portal, create an **Azure Bot**, single tenant if you can, and
   note its Microsoft App ID.
2. In its app registration, create a client secret.
3. In the bot's Configuration, set the messaging endpoint to the webhook
   address. Under Channels, add Microsoft Teams.
4. Add the bot to Teams with an app package, for example from the Teams
   Developer Portal, and install it for the people and teams that use it.

**Only your tenants are trusted.** Anyone can add a bot to their own tenant
and give their account any email there. So a message is refused unless it
comes from `tenant_id` or one of `allowed_tenants` (comma-separated). A
multi-tenant bot needs `allowed_tenants`.

The sender's email comes from the conversation's member list. Replies go to the
`serviceUrl` the activity named, but only to Microsoft's hosts, so a forged
activity cannot send the bot's token elsewhere. A message that answers
nothing, such as a schedule's result, goes to Microsoft's global address,
`https://smba.trafficmanager.net/teams/`. The bot must be installed in that
team. In a channel, the bot must be mentioned.

## Google Chat

Settings: `service_account_json`, `audience`.

1. In a Google Cloud project, enable the Google Chat API and create a service
   account with a JSON key.
2. In the Chat API's Configuration: name the app, allow 1:1 messages and
   joining spaces, choose **HTTP endpoint URL**, enter the webhook address,
   and set the **authentication audience to the project number**.
3. Choose who in your domain may use the app.

The audience can also be the endpoint URL. Then any Chat app pointed at the
address passes verification, so prefer the project number. Agents answer
after the webhook has returned, through the Chat API as the service account.

## Slack

Settings: `bot_token` (`xoxb-…`), `signing_secret`.

The panel shows the steps when you add the connector: create the app from
the manifest it shows, copy the bot token and signing secret into the
connector, then subscribe to the bot events `app_mention` and `message.im`
at the webhook address, and invite the bot to its channels. The manifest
turns on the Messages tab and asks for `app_mentions:read`, `chat:write`,
`im:history`, `users:read`, `users:read.email`, and `channels:read` and
`groups:read` to show channel names in the task log.

The manifest leaves out the events, since Slack wants a request URL with
them. The URL exists only once the connector is saved, and Slack checks it
straight away with a signed request, which passes only once the signing
secret is stored.

Without `users:read.email` Slack sends no address, and every message is
refused, since an agent never works for someone it cannot name.

People from other workspaces, such as guests in a Slack Connect channel, are
refused too: their own workspace sets their email, so it proves nothing here.
Workspaces of the same Enterprise Grid count as one.

## Google Drive

Settings: `service_account_json`, `subject` (optional), `shared_drive_id`
(optional).

Enable the Google Drive API and create a service account with a JSON key.
Then either **share the folders with the service account**, which is the
narrowest, or, as a Workspace administrator, grant it domain-wide delegation
for `https://www.googleapis.com/auth/drive` and name the user to act as.

Google Docs are read as text, Sheets as CSV and Slides as text. Watched folders
use Drive's change feed.

## OneDrive and SharePoint

Settings: `tenant_id`, `client_id`, `client_secret`, `drive_id`.

Register an app in Microsoft Entra with a client secret. Give it the Microsoft
Graph application permission **`Sites.Selected`** with access to one site, or
`Files.ReadWrite.All` or `Sites.ReadWrite.All` for more, and grant admin
consent. Find the drive's ID in Graph Explorer: `GET /users/{email}/drive` for
someone's OneDrive, `GET /sites/{host}:/sites/{site}:/drives` for a SharePoint
library.

Word documents are read as text. Files up to 4 MiB can be written. Watched
folders use the drive's delta feed, filtered to the folder.

## GitHub

Settings: `token`, `base_url` (only for GitHub Enterprise Server).

1. Create a GitHub user for your agents and give it access to the
   repositories they work in.
2. As that user, create a **fine-grained personal access token** for those
   repositories, with **Contents**, **Issues** and **Pull requests** set to
   Read and write.
3. Protect your main branches as you would for a person.

When an agent is connected to a repository, the panel checks the token can
see it and push to it.

## GitLab

Settings: `token`, `base_url` (only for GitLab on your own servers).

Create a **group or project access token** with the role **Developer** and the
scopes `api` and `write_repository`. A personal access token of a user made for
the agents works too. Protect your main branches as you would for a person.

A `base_url` server's redirects are followed only on that server, so the
token never goes anywhere else. A `base_url` inside your own network works
only once the operator names its host in `KEERA_AGENTS_UPSTREAM_PRIVATE`; see
[Where connectors may connect](install.md#where-connectors-may-connect).

## How agents code

An agent connected to a repository gets its address in its prompt and tools,
and works with plain git in its own machine: it clones, changes the code, runs
the tests, commits and pushes. It needs the **Shell** tool for that; without
it, it can only work with issues and pull or merge requests, and the panel
says so. Then it opens a pull or merge request with `repo_open_change`
for a person to review. It can also list and read issues and comment on them.

The token never enters the agent's machine. Git there talks to
`http://127.0.0.1:7417`, which the agent's runtime serves and passes on to the
control plane with its runtime token. The control plane checks the agent is
connected to the repository and is running a task, adds the token, or that of
the person the task is for ([Each person's own account](#each-persons-own-account)),
and passes the request on to GitHub or GitLab.

**Agents push only to branches starting with `keera/`.** The control plane
reads the start of every push and refuses it if it names any other branch, or a
tag. It also refuses push options (`git push -o`), since GitLab can act on them,
for example by merging the branch. So an agent cannot change `main` even with a
token that could. Its
commits are authored as the agent, with an address ending in
`@agents.keera.invalid`.

## Services

Jira, Salesforce, AWS and the other services below are not chats, file stores
or code hosts. An agent connected to one gets **tools** for it, named
`<kind>_<tool>`, such as `jira_search_issues`. Each takes a `connector`: the id
of the agent's binding to use. The control plane runs the tool with the
connector's credentials, so they never reach the agent. The panel lists each
service's tools in the connector's form.

**A binding names what it reaches**: a Jira project, a calendar, a
Kubernetes namespace, an AWS region. Every tool stays within it: a search is
narrowed to it, and an issue, event or resource outside it is refused. Where
the table says so, `*` reaches everything the connector does. HubSpot and
Salesforce name nothing: their own user and app permissions are the limit.

**A binding may only read, or read and write.** One that only reads never
gets the tools that change something, and is refused them. Give the
connector's credentials no more than the agents should have too: the service
enforces those itself.

Most services can also act as each person instead of one shared account; see
[Each person's own account](#each-persons-own-account).

## Each person's own account

A connector usually holds one account, and every agent connected to it acts as
that account, whoever gives it work. Every connector but the chats, HubSpot,
PostgreSQL, Trello and Freshdesk can instead let **each person sign in with their own account**. An agent then
works there as the person the task is for, and reaches only what they may. A
chat's bot stays the organisation's: who may talk to an agent is decided by the
sender. HubSpot's OAuth tokens reach the whole account, whoever signs in, so
they would not narrow anything. PostgreSQL, Trello and Freshdesk have no
sign-in of this kind.

The administrator chooses this when adding the connector, and enters an OAuth
client instead of a shared account. Its redirect URI, or callback URL, is
`<KEERA_AGENTS_PUBLIC_URL>/auth/connector/callback`. The panel shows the full
steps; in short:

- **Google** (Drive, Docs, Sheets, Slides, Calendar, Gmail, Google Cloud): enable the
  APIs the kind needs, set the Google Auth Platform's audience to **Internal**,
  and create an OAuth client of type **Web application**.
- **Microsoft** (OneDrive and SharePoint, Outlook, Azure): register an app for
  your tenant with the platform **Web**, and add the delegated permissions the
  panel names: `Files.ReadWrite.All` for OneDrive, `Mail.ReadWrite` and
  `Mail.Send` and their `.Shared` forms for Outlook Mail,
  `Calendars.ReadWrite` and its `.Shared` form for Outlook Calendar,
  `user_impersonation` of Azure Service Management for Azure.
- **Atlassian** (Jira, Confluence): create an OAuth 2.0 integration in the
  developer console, add the product's scopes and `read:me`, and set its
  distribution to **Sharing**. Only Atlassian Cloud signs people in. Their
  calls go through `api.atlassian.com`, not the site.
- **Salesforce**: create an External Client App with the scopes `api`,
  `refresh_token`, `openid` and `email`, and let people sign in to it. Agents
  then have the person's profile, permission sets and sharing.
- **GitHub**: create a GitHub App with **Expire user authorization tokens**
  on, Contents, Issues and Pull requests set to Read and write, and install it
  on the repositories. Agents reach what both the app and the person may.
- **GitLab**: add an OAuth application with the scopes `api`, `openid` and
  `email`.
- **Your own identity provider** (Kubernetes, AWS), such as Entra ID, Okta,
  Keycloak or Google: register a web app there, and enter its issuer URL. The
  person's **ID token** is what the service gets, so it must trust that
  provider. A Kubernetes API server takes it with `--oidc-issuer-url` and
  `--oidc-client-id` (or an OIDC identity provider on EKS), and its RBAC then
  decides what each person may. AWS takes it as an IAM OpenID Connect provider:
  each person assumes the connector's role with `AssumeRoleWithWebIdentity`, in
  a session named after their email, so CloudTrail shows them. The role's trust
  policy decides who may, but everyone who may gets the role's permissions;
  narrow them per person with conditions on the token's claims or with session
  tags.

Then each person presses **Connect your account** on the connector, signs in,
and agrees to what agents may do. **Disconnect** forgets it. The panel shows
who you are connected as.

An Outlook binding on such a connector may name the mailbox `me`: the mailbox
of whoever the task is for. A named mailbox must be shared with that person.
A Gmail binding on such a connector always names `me`: Gmail lets a person's
token reach only their own mailbox.
A OneDrive connector still names one drive, since a folder's ID belongs to it,
so use one people share, such as a SharePoint library. On Google Drive, the
folder `root` is the My Drive of whoever the task is for.

**Code is pushed as the person.** On GitHub and GitLab, git and the issue
tools use the person's token, so their pushes, comments and pull or merge
requests show as theirs. Commits are still authored as the agent, and agents
still push only to branches starting with `keera/`.

**Who a task is for** is who it is attributed to; see
[Tasks](tasks.md). A folder watch also reads the folder as whoever added it,
and a workspace acts as its owner. A person who has not connected their
account gets an error from the tool, which the agent passes on.

**Adding such a connector** to an agent works as you: you must have connected
your own account first, its target is checked as you, and the panel's folder
picker shows your folders.

**The tokens stay in the control plane.** Each person's refresh token is
sealed with `KEERA_AGENTS_SECRET_KEY`, and access tokens are only kept in
memory. Neither reaches an agent. Replicas take turns refreshing a person's
token, so a service that replaces the refresh token each time never sees an
old one. When someone leaves the organisation or is
disabled, their sign-ins are forgotten within ten minutes.

The binding's target and read-only mode still apply on top. Remember that the
agent's owner wrote its instructions: give work only to agents you trust to
act as you.

## Jira

Settings: `base_url` (the site, such as `https://acme.atlassian.net`, or your
Jira Data Center), `email` (only for Atlassian Cloud), `api_token`.

1. Create an Atlassian account for your agents and add it to the projects
   they work in. To read, it needs **Browse projects**; to write, also
   **Create**, **Edit**, **Assign** and **Transition issues** and **Add
   comments**.
2. On Atlassian Cloud, sign in as that account and create an **API token
   without scopes** at id.atlassian.com, under Security. Enter it with the
   account's email.
3. On Jira Data Center, sign in as that account and create a **personal access
   token** in the profile. Leave the email empty.

With an email, keera-agents signs in as Atlassian Cloud does, with the email
and token; without one, it sends the token as a bearer token, as Data Center
expects. It uses REST API version 2, which both have, so descriptions and
comments are plain text in Jira's wiki markup. A `base_url` inside your own
network works only once the operator names its host in
`KEERA_AGENTS_UPSTREAM_PRIVATE`; see
[Where connectors may connect](install.md#where-connectors-may-connect).

Agents get these tools:

- `jira_search_issues`: search with JQL, newest change first.
- `jira_get_issue`: an issue's fields, description and last 20 comments.
- `jira_list_projects`: the projects the agent may use.
- `jira_find_users`: people by name or email, for assigning.
- `jira_create_issue` (write): a new issue, a Task unless told otherwise.
- `jira_update_issue` (write): change the summary, description, labels or
  assignee.
- `jira_comment` (write): comment on an issue.
- `jira_transition` (write): move an issue to another status.

**A binding names a project**, such as `WEB`, or every project the account
sees. In one project, searches are wrapped as `project = "WEB" AND (…)`, and a
query whose parentheses or quotes do not balance is refused, so it cannot
reach past them. Issues of other projects are refused, new issues go in the
project, and only people who may be assigned there are found.

## Confluence

Settings: `base_url` (the site, such as `https://acme.atlassian.net`, or your
Confluence Data Center), `email` (only for Atlassian Cloud), `api_token`.

1. Create an Atlassian account for your agents and give it access to the
   spaces they work in: **View** to read; to write, also **Add pages** and
   **Add comments**.
2. On Atlassian Cloud, sign in as that account and create an **API token
   without scopes** at id.atlassian.com, under Security. Enter it with the
   account's email, and the site's address without `/wiki`.
3. On Confluence Data Center, sign in as that account and create a **personal
   access token** in the profile. Leave the email empty.

Credentials work as for [Jira](#jira). On Cloud, keera-agents calls
`<site>/wiki`; on Data Center, `base_url` as it is. It uses the REST API
version 1, which both have.

Agents get these tools:

- `confluence_search`: find pages and blog posts by text, or anything with
  CQL.
- `confluence_get_page`: a page as plain text, with headings, lists, tables
  and code kept, up to 50 KB.
- `confluence_list_spaces`: the spaces the agent may use.
- `confluence_create_page` (write): a new page, optionally under another.
- `confluence_update_page` (write): replace a page's title, body or both.
- `confluence_comment` (write): comment on a page.

Agents write pages in Confluence's storage format, its XHTML. Plain text,
as models often write for comments, becomes paragraphs.

**A binding names a space**, such as `DOCS`, or every space the account sees.
In one space, searches are wrapped as `space = "DOCS" AND (…)`, as for Jira,
pages of other spaces are refused, and new pages and their parents must be in
the space.

## Trello

Settings: `api_key`, `token`.

1. Create a Trello account for your agents and add it to the boards they work
   on.
2. At trello.com/power-ups/admin, create a **Power-Up** in one of your
   workspaces and generate its **API key**.
3. Signed in as the agents' account, open
   `https://trello.com/1/authorize?expiration=never&scope=read,write&response_type=token&name=Keera+Agents&key=`
   followed by the API key. Allow it and copy the **token**. Use
   `scope=read` if agents should only read.

keera-agents sends the key and token in the `Authorization` header, not in the
address as Trello's examples do, so they never show in an error.

Agents get these tools:

- `trello_list_boards`: the boards the agent may use.
- `trello_get_board`: a board's lists, labels and members, with their ids.
- `trello_list_cards`: the open cards on a board, or in one list.
- `trello_search_cards`: cards by text, with Trello's search operators.
- `trello_get_card`: a card's fields, description, checklists and last 20
  comments.
- `trello_create_card` (write): a new card in a list.
- `trello_update_card` (write): change a card, move it to another list on its
  board, or archive it.
- `trello_comment` (write): comment on a card.

**A binding names a board**, by its address or the code in it, or every board
the account is on. On one board, searches are narrowed to it, and cards and
lists of other boards are refused. A card moves only between the lists of its
own board.

## Google Docs

Settings: `service_account_json`, `subject` (optional).

1. In a Google Cloud project, enable the Google Docs API and the Google Drive
   API, and create a service account with a JSON key.
2. Either **share the documents**, or a folder holding them, with the service
   account's email, as Editor for agents that write, or, as a Workspace
   administrator, grant it domain-wide delegation for
   `https://www.googleapis.com/auth/documents`,
   `https://www.googleapis.com/auth/drive.metadata.readonly` and
   `https://www.googleapis.com/auth/drive.file`, and name the user to act as.

Agents get `gdocs_find` (documents by a part of their name), `gdocs_read` (a
document as text: headings as `#`, lists, and tables as `| a | b |` rows, up to
50 KB), and, to write, `gdocs_append` (paragraphs at the end),
`gdocs_replace_text` (every occurrence; it says how many changed) and
`gdocs_create`.

A binding names one document, by its link or ID, or `*` for every document the
connector can see. With one document, every tool works on that one only, and
`gdocs_create` is refused. A service account has no storage of its own, so a
new document goes into a folder the agent names, unless the connector acts as
a user. Drive is used only to find files and create them: `drive.file` reaches
only files the connector created or was shared.

## Google Sheets

Settings: `service_account_json`, `subject` (optional).

1. In a Google Cloud project, enable the Google Sheets API and the Google Drive
   API, and create a service account with a JSON key.
2. Either **share the spreadsheets**, or a folder holding them, with the
   service account's email, as Editor for agents that write, or, as a Workspace
   administrator, grant it domain-wide delegation for
   `https://www.googleapis.com/auth/spreadsheets`,
   `https://www.googleapis.com/auth/drive.metadata.readonly` and
   `https://www.googleapis.com/auth/drive.file`, and name the user to act as.

Agents get `gsheets_find` (spreadsheets by a part of their name), `gsheets_get`
(the title, and each tab's size) and `gsheets_read_range` (a range in A1
notation, as rows of the values shown, up to 5,000 cells). To write, they get
`gsheets_write_range`, `gsheets_append_rows` (below the table in a range) and
`gsheets_create`. Cells are written as if typed, so numbers, dates and
formulas work.

A binding names one spreadsheet, by its link or ID, or `*` for every
spreadsheet the connector can see. With one spreadsheet, every tool works on
that one only, and `gsheets_create` is refused. A new spreadsheet goes into a
folder the agent names, as with [Google Docs](#google-docs).

## Google Slides

Settings: `service_account_json`, `subject` (optional).

1. In a Google Cloud project, enable the Google Slides API and the Google
   Drive API, and create a service account with a JSON key.
2. Either **share the presentations**, or a folder holding them, with the
   service account's email, as Editor for agents that write, or, as a Workspace
   administrator, grant it domain-wide delegation for
   `https://www.googleapis.com/auth/presentations`,
   `https://www.googleapis.com/auth/drive.metadata.readonly` and
   `https://www.googleapis.com/auth/drive.file`, and name the user to act as.

Agents get `gslides_find` (presentations by a part of their name) and
`gslides_read` (each slide's number, ID, title, text and speaker notes). To
write, they get `gslides_replace_text` (every occurrence, or on some slides
only; it says how many changed) and `gslides_add_slide` (a title and a body, in
the presentation's title and body layout, at the end or at a position).

A binding names one presentation, by its link or ID, or `*` for every
presentation the connector can see. With one presentation, every tool works on
that one only.

## Google Calendar

Settings: `service_account_json`, `subject` (optional).

1. In a Google Cloud project, enable the Google Calendar API and create a
   service account with a JSON key.
2. Either **share each calendar** with the service account's email, with
   **Make changes to events** for agents that write, or, as a Workspace
   administrator, grant it domain-wide delegation for
   `https://www.googleapis.com/auth/calendar` and name the user to act as.

Agents get `gcalendar_list_events` (a time range, the next seven days by
default, optionally searched; recurring events one occurrence at a time),
`gcalendar_get_event` and `gcalendar_free_busy` (when the calendar is busy).
To write, they get `gcalendar_create_event`, `gcalendar_update_event` (only
the fields given) and `gcalendar_delete_event`. An event takes RFC 3339 times,
or dates for an all-day event. Guests are emailed only when the agent sets
`notify`. Google lets a service account invite guests only through
domain-wide delegation.

A binding names one calendar by its ID, from the calendar's settings: a
person's email, or a shared calendar's `…@group.calendar.google.com`. With
domain-wide delegation, `primary` is the user's own calendar, and is stored as
its ID. Every tool works on that calendar only; there is no `*`.

## Outlook Mail and Outlook Calendar

Settings: `tenant_id`, `client_id`, `client_secret`.

Mail and calendar are two connectors, so an agent may keep a calendar without
reading mail. Both may use the same app.

1. In Microsoft Entra, register an app and create a client secret.
2. Give it the Microsoft Graph application permissions it needs:
   `Mail.ReadWrite` and `Mail.Send` for mail, `Calendars.ReadWrite` for the
   calendar. Granted in Entra, they reach every mailbox in the tenant, so
   limit them to the agents' mailboxes.
3. Best, use Exchange Online's **RBAC for Applications**: create a management
   scope for the agents' mailboxes and assign the app the matching roles, such
   as Application Mail.ReadWrite, in it, without granting the permissions in
   Entra.
4. Or grant them in Entra with admin consent, and restrict the app with an
   **application access policy** (`New-ApplicationAccessPolicy`) to a
   mail-enabled security group of the agents' mailboxes.
5. Bind each agent to one mailbox, best one made for it.

The connector is checked by signing in. A mailbox is checked when an agent is
bound to it, by opening its inbox or its calendar, so a mailbox outside the
app's scope fails then.

**A binding names one mailbox**, by its address, and every call goes to that
mailbox alone. There is no `*`: the app's permissions reach every mailbox it
is allowed, so an agent gets only its own.
A member may bind only their own mailbox; an administrator may bind any.

Outlook Mail gives agents these tools:

- `outlook_list_messages`: list a folder's messages, newest first, by search
  text or unread only.
- `outlook_list_folders`: list the mail folders, with their ids.
- `outlook_read_message`: read a message as text, with its attachments' names.
- `outlook_send_message`: send a new email, as plain text. Needs write.
- `outlook_reply`: reply, or reply to all. Needs write.
- `outlook_move_message`: move a message to another folder. Needs write.

Outlook Calendar gives agents these tools:

- `outlookcalendar_list_events`: list events, by default the next 7 days.
- `outlookcalendar_get_event`: read an event, with its description and
  attendees' answers.
- `outlookcalendar_free_busy`: when the calendar is busy, without details.
- `outlookcalendar_create_event`: create an event, with invitations and a
  Teams meeting if asked. Needs write.
- `outlookcalendar_update_event`: change only the fields given. Attendees get
  the update. Needs write.
- `outlookcalendar_delete_event`: delete an event. If the mailbox organised
  it, attendees get a cancellation. Needs write.

Times are local times in a time zone the agent names, such as
`Europe/Zurich`, or RFC 3339. A range is at most 93 days.

## Gmail

Settings: `service_account_json`.

1. In a Google Cloud project, enable the Gmail API, and create a service
   account with a JSON key.
2. As a Workspace administrator, under Security, API controls, Domain-wide
   delegation, grant the service account's client ID the scope
   `https://www.googleapis.com/auth/gmail.modify`.
3. Bind each agent to one mailbox, best one made for it.

A service account has no mailbox of its own, so **a binding names one
mailbox** by its address, and the connector acts as it through domain-wide
delegation. Delegation lets the service account act as any mailbox in the
domain, so keep its key in the connector only, and only an administrator may
bind a mailbox other than their own. A mailbox is checked when an agent is
bound to it. `gmail.modify` reads, sends, drafts and labels mail, but
never deletes it for good.

Agents get these tools:

- `gmail_search_messages`: search with Gmail's search syntax, such as
  `from:ada@example.com is:unread newer_than:7d`, newest first, with a preview.
  Without a query, the inbox.
- `gmail_read_message`: read a message as text, with its attachments' names.
  An HTML-only message is turned into text.
- `gmail_read_thread`: read a conversation, each message cut short.
- `gmail_list_labels`: list the labels, the system ones and the mailbox's own.
- `gmail_send_message`: send a new email, as plain text. Needs write.
- `gmail_reply`: reply, or reply to all, in the thread, above the quoted
  original. Needs write.
- `gmail_create_draft`: save a draft for a person to check and send, or a
  draft reply. Needs write.
- `gmail_change_labels`: add or remove labels: archive by removing `INBOX`,
  mark read by removing `UNREAD`. Needs write.

## HubSpot

Settings: `access_token`.

1. In HubSpot, open **Settings → Integrations → Private apps** (legacy apps
   in newer accounts) and create an app for your agents.
2. Give it the scopes `crm.objects.contacts.read` and `.write`, the same for
   `companies` and `deals`, `tickets`, and `crm.objects.owners.read`. Leave
   out the write scopes if agents should only read.
3. Copy the app's access token into the connector.

The connector is checked by listing one contact, so a missing contacts scope
fails at once.

Agents get these tools, on contacts, companies, deals and tickets:

- `hubspot_search`: find records by text and exact property values.
- `hubspot_get`: read one record, with the ids of the records it is
  associated with.
- `hubspot_create`: create a record. Needs write.
- `hubspot_update`: change a record's properties. Needs write.
- `hubspot_add_note`: add a note to a record's timeline. Needs write.
- `hubspot_list_owners`: list the users records can be assigned to.

A HubSpot connector has nothing to narrow to: a binding reaches the whole CRM,
as far as the private app's scopes allow. Grant only the scopes agents need.

## Salesforce

Settings: `domain_url`, `client_id`, `client_secret`.

1. In Salesforce Setup, create a user for your agents, best with the
   **Salesforce Integration** user licence.
2. Give it a permission set with **API Enabled** and access to only the
   objects and fields agents may read or change.
3. Create an **External Client App** (or a Connected App) with OAuth enabled
   and the scope **Manage user data via APIs (`api`)**.
4. In its policies, enable the **Client Credentials Flow** and set the user
   from step 1 as the run-as user.
5. Copy the app's consumer key and secret, and the org's My Domain URL, such
   as `https://acme.my.salesforce.com`, into the connector.

keera-agents signs in at the My Domain URL with the client credentials flow
and keeps the token until Salesforce refuses it. The API is called on the
instance Salesforce names only if it is the My Domain server or another
`*.salesforce.com` or `*.force.com` server, so a forged answer cannot send the
token elsewhere.

Agents get these tools:

- `salesforce_query`: run a SOQL query.
- `salesforce_search`: find accounts, contacts, opportunities, leads and
  cases by text, or run a SOSL search.
- `salesforce_get`: read one record.
- `salesforce_describe`: list an object's fields, their types and picklist
  values.
- `salesforce_create`: create a record. Needs write.
- `salesforce_update`: change a record's fields. Needs write.

A Salesforce connector has nothing to narrow to: a binding reaches whatever
the run-as user may, so that user's permission set is the limit.

## Freshdesk

Settings: `domain` (such as `acme.freshdesk.com`), `api_key`.

1. In Freshdesk, add an agent for your agents to work as, in the groups they
   work in. Give it a role that may do what they should, such as reply to
   tickets.
2. Signed in as that agent, open **Profile settings** and copy the **API
   key**.
3. Enter your helpdesk's `freshdesk.com` address, not a custom domain.
   keera-agents sends the key only to a `freshdesk.com` address, so whoever
   edits the connector cannot send it elsewhere.

Agents get these tools:

- `freshdesk_search_tickets`: find tickets with a Freshdesk filter, such as
  `status:2 AND priority:4`, or the latest ones.
- `freshdesk_get_ticket`: a ticket's fields, requester, description and last
  20 replies and notes.
- `freshdesk_find_contacts`: customers by email or name.
- `freshdesk_list_groups`: the groups the agent may use.
- `freshdesk_list_agents`: the Freshdesk agents tickets can be assigned to.
- `freshdesk_create_ticket` (write): a new ticket for a customer.
- `freshdesk_update_ticket` (write): change the status, priority, agent, tags
  or subject.
- `freshdesk_reply` (write): reply to the customer, who gets it by email.
- `freshdesk_add_note` (write): add a note, private unless told otherwise.

**A binding names a group**, such as `Billing`, or every group. In one group,
filters are wrapped as `(…) AND group_id:5`, and one whose parentheses or
quotes do not balance is refused. Tickets of other groups are refused, and new
tickets go in the group. Contacts are the whole helpdesk's: an agent that
creates tickets must be able to find the customer.

## PostgreSQL

Settings: `host`, `port` (empty for 5432), `database`, `user`, `password`,
`sslmode` (empty or `verify-full`, or `disable`), `ca_cert` (optional).

1. Create a role for your agents, and grant it only what they may see:
   `USAGE` on the schemas and `SELECT` on the tables. Grant `INSERT`, `UPDATE`
   or `DELETE` only on the tables agents may change. Never use a superuser:
   the connector refuses one.
2. Best, point the connector at a read replica, so a heavy query does not slow
   the primary.
3. A server inside your network works only once the operator names its host in
   `KEERA_AGENTS_UPSTREAM_PRIVATE`; see
   [Where connectors may connect](install.md#where-connectors-may-connect).

**The role's grants are the limit.** A binding names nothing: it reaches what
the role may. **A binding that only reads** runs every statement in a read-only
transaction, in a session whose transactions are read-only too, so PostgreSQL
itself refuses a write.

**One statement per call.** Each call opens its own connection and runs one
statement, which may run for 30 seconds. PostgreSQL's extended protocol takes
one statement only, so a query cannot end its transaction and go on. TLS is
required and the certificate checked, unless `sslmode` is `disable`. Rows
come back as text, as `psql` shows them, with each value cut at 2,000
bytes.

Agents get these tools:

- `postgres_list_tables`: the tables and views the role may read, with
  estimated row counts and comments.
- `postgres_describe_table`: a table's columns, types, defaults and comments,
  and its keys and checks.
- `postgres_query`: run one read-only statement, up to 1,000 rows, 100 by
  default.
- `postgres_execute`: run one statement that changes data and commit it, with
  the rows it changed and those of a `RETURNING` clause. Needs write.

## Kubernetes

Settings: `server`, `token`, `ca_cert` (optional).

1. In each namespace agents work in, create a **ServiceAccount**, such as
   `keera-agents`.
2. Bind it to a role there with a **RoleBinding**: the ClusterRole `view` for
   read-only agents, or `edit` for agents that change things.
3. Create a long-lived token for it: a Secret of type
   `kubernetes.io/service-account-token` with the annotation
   `kubernetes.io/service-account.name` set to the ServiceAccount's name.
4. Copy the Secret's `token`, and its `ca.crt` into `ca_cert` if the API
   server's certificate is not from a public CA.

The `server` must be `https://`. Redirects are followed only on that server,
and a `ca_cert` is trusted for that connector alone; certificates are always
verified. An API server inside your own network works only once the operator
names its host in `KEERA_AGENTS_UPSTREAM_PRIVATE`; see
[Where connectors may connect](install.md#where-connectors-may-connect).

Agents get these tools:

- `kubernetes_list`: objects of a kind, with age and a short status, such as
  a pod's phase, ready containers and restarts.
- `kubernetes_get`: one object as JSON, without `managedFields`. A Secret's
  values are never returned, only its keys.
- `kubernetes_logs`: a pod's last log lines, 200 by default, or its previous
  run's.
- `kubernetes_events`: recent events, newest first, optionally about one
  object.
- `kubernetes_apply` (write): creates or updates an object with server-side
  apply, from a manifest given as a JSON object.
- `kubernetes_delete` (write): deletes one object.
- `kubernetes_scale` (write): sets a Deployment's or StatefulSet's replicas.
- `kubernetes_restart` (write): restarts a Deployment, StatefulSet or
  DaemonSet, as `kubectl rollout restart` does.

**A binding names one namespace**, and every call stays in it: a manifest
naming another namespace is refused, and cluster-scoped kinds, such as nodes
or namespaces, are refused too. The panel checks the token can read the
namespace when an agent is connected. Give the service account only the roles
agents need: the binding narrows, but the token's roles are the limit.

## AWS

Settings: `access_key_id`, `secret_access_key`, `role_arn` (optional).

1. In IAM, create a user for your agents, without console access, and an
   access key for it.
2. Give it what agents read: the AWS managed policy **ReadOnlyAccess**, or a
   narrower one allowing `ec2:DescribeRegions`, `ec2:DescribeInstances`,
   `s3:ListAllMyBuckets`, `s3:ListBucket`, `s3:GetObject`,
   `cloudwatch:DescribeAlarms`, `cloudwatch:GetMetricStatistics` and
   `logs:FilterLogEvents`.
3. Only if agents may change things, also allow `ec2:StartInstances`,
   `ec2:StopInstances`, `ec2:RebootInstances` and `s3:PutObject`, on just the
   instances and buckets they may change.
4. For costs, allow `ce:GetCostAndUsage`, and open Cost Explorer once in the
   Billing console to turn it on.
5. Optional: put these permissions on a role instead, trust the user in the
   role's trust policy, allow the user only `sts:AssumeRole` on it, and enter
   the role's ARN.

With a role, every call uses a one-hour session of it, taken again shortly
before it ends. keera-agents signs its requests itself (Signature Version 4);
the key never leaves the control plane. Saving the connector asks STS who the
key is, and assumes the role if there is one.

Agents get these tools. Those marked _write_ change something, so only a
binding that may write gets them.

- `aws_whoami`: the account and the ARN the agent acts as.
- `aws_ec2_instances`: instances, by id or EC2 filters such as `tag:Name`:
  name, state, type, zone, IP addresses, launch time.
- `aws_ec2_start`, `aws_ec2_stop`, `aws_ec2_reboot` (_write_): by instance id.
- `aws_s3_buckets`: the buckets in the region.
- `aws_s3_list`: a bucket's objects, one folder at a time or all under a
  prefix.
- `aws_s3_read`: a text object, 48 KiB at a time.
- `aws_s3_write` (_write_): a text object up to 4 MiB.
- `aws_cloudwatch_alarms`: metric alarms, by state or name: state, reason,
  metric, when the state changed.
- `aws_cloudwatch_metrics`: one metric's statistic over time, such as an
  instance's CPU over the last hour.
- `aws_logs_search`: a log group's events matching a filter pattern.
- `aws_costs`: unblended cost by month or day, optionally by service or
  usage type. Cost Explorer charges USD 0.01 per call.

**A binding names a region**, such as `eu-central-1`; connecting it checks the
region is enabled for the account. Every call goes to that region: EC2,
CloudWatch and logs see only what is there, a bucket in another region is
refused, and costs are filtered to the region, so global services such as
Route 53 are left out. A log group is named, not given as an ARN, so it cannot
point elsewhere. Cost Explorer itself is only in `us-east-1`, and keys are
checked and roles assumed there too.

## Azure

Settings: `tenant_id`, `client_id`, `client_secret`.

1. In Microsoft Entra, register an app and create a client secret.
2. In each subscription agents work in, open **Access control (IAM)** and
   assign the app the **Reader** role.
3. For costs, also assign it **Cost Management Reader**.
4. If agents may start and stop virtual machines, also assign it **Virtual
   Machine Contributor**, on the subscription or only on the resource groups
   they may touch.
5. Bind each agent to a subscription by its ID.

The connector is checked by signing in and listing subscriptions: it fails if
the app sees none.

Agents get these tools:

- `azure_list_resource_groups`: list the resource groups.
- `azure_list_resources`: list resources, in one resource group or of one
  type.
- `azure_get_resource`: read a resource's settings by its id; a virtual
  machine's include its power state.
- `azure_activity_log`: who changed what, by default in the last 24 hours.
- `azure_metrics`: a resource's metrics from Azure Monitor, such as a VM's
  CPU; without names, the metrics it has.
- `azure_costs`: actual costs for a date range, by default this month, in
  total, by resource group or by service, and per day if asked.
- `azure_vm_start`, `azure_vm_stop`, `azure_vm_restart`: start, stop or
  restart a virtual machine. Stopping deallocates it, so its compute stops
  costing. Azure does it in the background. Need write.

**A binding names one subscription.** Every resource id an agent passes must
be in it, or the call is refused. There is no `*`. Azure's roles limit the
rest: give the app Reader on the subscription, and a role that changes things
only where agents may.

## Google Cloud

Settings: `service_account_json`.

1. In the project agents work in, create a **service account**, such as
   `keera-agents`.
2. Grant it the roles **Viewer**, **Logs Viewer**, **Monitoring Viewer** and
   **Cloud Asset Viewer** on the project. Add **Compute Instance Admin (v1)**
   only if agents may start and stop VMs.
3. Create a JSON key for it.
4. Enable the Cloud Resource Manager, Cloud Asset, Compute Engine, Cloud
   Logging and Cloud Monitoring APIs in the service account's project.

One connector can serve several projects: grant the same roles in each.

Agents get these tools:

- `gcp_search_resources`: resources of every kind, such as VMs, buckets and
  databases, from Cloud Asset Inventory.
- `gcp_list_instances`: Compute Engine VMs in every zone, with status, machine
  type and IP addresses.
- `gcp_logs`: log entries from Cloud Logging, newest first, from the last
  hour unless asked otherwise.
- `gcp_metrics`: time series from Cloud Monitoring, from the last hour unless
  asked otherwise.
- `gcp_list_buckets`, `gcp_list_objects` and `gcp_read_object`: Cloud Storage
  buckets, their objects, and text objects, 48 KiB at a time.
- `gcp_instance_start` and `gcp_instance_stop` (write): start or stop a VM.

**A binding names one project ID**, and every call names that project. Bucket
names are global, so a bucket is used only after checking it belongs to the
project. The panel checks the service account can see the project when an
agent is connected.

## What an agent can reach

The connector's credentials can usually reach more than one agent should. So
keera-agents narrows it: an agent posts only in chats it is connected to, reads
and writes only in folders it is connected to, and in subfolders it found by
listing them during the same task, works only in repositories it is
connected to, and calls a service only within what its binding names
([Services](#services)). A member choosing a folder in the panel sees folder
names only.

That keeps file names out of the panel, not out of reach. A member may connect
their own agent to any folder a shared connector reaches, and the agent then
reads its files, which can include files the member could not open
themselves. So give a shared file connector only folders everyone in the
organisation may read, or use one people sign in to, which reaches only what
each person can.

## Not built yet

- Tasks from GitHub or GitLab, such as an issue assigned to an agent. For now,
  give the task in a chat, on the board or on a schedule, and name the issue.
- A GitHub App as the shared account instead of a token, and Git LFS.
- Long messages are not split, so a very long answer may be cut by the service.
- OneDrive uploads over 4 MiB, and Teams' government clouds.
- Atlassian's scoped API tokens, and AWS outside its standard partition, such
  as China or GovCloud.
- SharePoint lists and pages: SharePoint libraries are reached as OneDrive
  folders.
- Tested against stand-ins of each service, not the live ones.
