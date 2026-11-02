# Channels

A **connector** is one organisation's credentials for one service: its Slack
app, its Azure Bot, its Drive service account, its GitHub token.
Administrators create connectors in the panel; anyone who may change an agent
then connects it to a channel, folder or repository of one. (The API calls
them connections.) This page explains how each service is reached and
why. The panel shows the same setup steps next to the form.

| Service                 | Kind         | Gives agents                | keera-agents reaches it with     |
| ----------------------- | ------------ | --------------------------- | -------------------------------- |
| Microsoft Teams         | `teams`      | messages in, replies out    | an Azure Bot (Bot Framework)     |
| Google Chat             | `googlechat` | messages in, replies out    | a Chat app with an HTTP endpoint |
| Slack                   | `slack`      | messages in, replies out    | a Slack app (Events API)         |
| Google Drive            | `gdrive`     | watched folders, file tools | a service account                |
| OneDrive and SharePoint | `onedrive`   | watched folders, file tools | an Entra app (Microsoft Graph)   |
| GitHub                  | `github`     | git, issues, pull requests  | a fine-grained access token      |
| GitLab                  | `gitlab`     | git, issues, merge requests | a project or group access token  |

**Credentials are sealed** with `KEERA_AGENTS_SECRET_KEY` and opened only to
make a call. The panel never shows them again; leaving a secret field empty
when editing keeps it, unless the server it is sent to changes, as with a new
GitHub or GitLab URL: then it must be entered again, so nobody can send a saved
token to a server of their own. A connector is checked with the service before it is
saved, so a typo fails at once with the service's own reason.

**The webhook address** of a chat connector is
`<KEERA_AGENTS_PUBLIC_URL>/hooks/<connector id>`. The panel shows it after
the connector is saved. Register it with the service. The services must reach
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

1. Create an app at api.slack.com/apps and install it to the workspace.
2. Bot scopes: `app_mentions:read`, `chat:write`, `im:history`, `users:read`,
   `users:read.email`. Add `channels:read`, `groups:read` and `im:read` to show
   channel names in the task log.
3. Under Event Subscriptions, set the request URL to the webhook address and
   subscribe to the bot events `app_mention` and `message.im`.
4. Under App Home, let people message the app. Invite the bot to its channels.

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

An agent connected to a repository gets its address in its tools, and works
with plain git in its own machine: it clones, changes the code, runs the tests,
commits and pushes. Then it opens a pull or merge request with `repo_open_change`
for a person to review. It can also list and read issues and comment on them.

The token never enters the agent's machine. Git there talks to
`http://127.0.0.1:7417`, which the agent's runtime serves and passes on to the
control plane with its runtime token. The control plane checks the agent is
connected to the repository and is running a task, adds the token, and passes
the request on to GitHub or GitLab.

**Agents push only to branches starting with `keera/`.** The control plane
reads the start of every push and refuses it if it names any other branch, or a
tag. So an agent cannot change `main` even with a token that could. Its
commits are authored as the agent, with an address ending in
`@agents.keera.invalid`.

## What an agent can reach

The connector's credentials can usually reach more than one agent should. So
keera-agents narrows it: an agent posts only in chats it is connected to, reads
and writes only in folders it is connected to, and in subfolders it found by
listing them during the same task, and works only in repositories it is
connected to. A member choosing a folder in the panel sees folder names only.

## Not built yet

- Tasks from GitHub or GitLab, such as an issue assigned to an agent. For now,
  give the task in a chat, on the board or on a schedule, and name the issue.
- GitHub Apps as an alternative to a token, and Git LFS.
- Long messages are not split, so a very long answer may be cut by the service.
- OneDrive uploads over 4 MiB, and Teams' government clouds.
- Tested against stand-ins of each service, not the live ones.
