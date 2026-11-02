# AGENTS.md

Keera Agents runs AI agents for businesses on top of Keera Gateway. See
[README.md](README.md).

The repository is one Go module and one binary, `cmd/keera-agents`: the
control plane by default, and `keera-agents agent`, the runtime inside each
agent's container. Keera Gateway lives in a sibling repository; this one
calls its control API and never imports its code.

## Layout

| Path                    | What it is                                                                |
| ----------------------- | ------------------------------------------------------------------------- |
| `cmd/keera-agents`      | The binary: serve, `agent`, `health`, `version`.                          |
| `internal/server`       | Wiring, lifecycle and the periodic work.                                  |
| `internal/config`       | Every setting, read from the environment.                                 |
| `internal/api`          | The panel's JSON API and sign-in through the gateway.                     |
| `internal/gateway`      | Client for the gateway's control API: sign-in and service routes.         |
| `internal/store`        | Postgres schema and queries.                                              |
| `internal/fleet`        | Keeps each agent's container in the state it should be in; its keys.      |
| `internal/runtime`      | The drivers: podman and Kubernetes (StatefulSets).                        |
| `internal/runner`       | The runtime API agents' containers call.                                  |
| `internal/protocol`     | The types of that API, shared by both ends.                               |
| `internal/agent`        | The runtime: agent loop, builtin tools, MCP client.                       |
| `internal/dispatch`     | Tasks from chats, schedules, cards and folders; delivery; tools.          |
| `internal/schedule`     | Cron expressions.                                                         |
| `internal/channels`     | The adapters: Teams, Google Chat, Slack, Drive, OneDrive, GitHub, GitLab. |
| `internal/channels/all` | Registers every adapter.                                                  |
| `internal/webui`        | The panel, embedded. Same design as the gateway's.                        |
| `internal/secret`       | Sealing for cookies, agents' keys and connector credentials.              |
| `internal/egress`       | Where connectors may connect: not loopback, nor inside unless allowed.    |
| `internal/id`           | Prefixed identifiers, sortable by creation time.                          |
| `internal/version`      | The version the binary was built as.                                      |
| `Containerfile`         | Both images: the control plane, and the agent's (`keera-agents agent`).   |
| `scripts`               | The licence notices the images ship.                                      |
| `.github`               | CI, and the release: images signed with SLSA Build L3 provenance.         |
| `compose`               | The database for the development loop, and the panel in a container.      |

## Commands

```sh
make build             # the binary, to build/
make check             # test + vet + fmt-check, and check-js when node is there
make test-integration  # store and end-to-end tests, against a throwaway Postgres
make check-all         # check + test-integration
make lint              # golangci-lint
make dev               # Postgres in a container, keera-agents under air, podman driver
make image             # the control plane's image
make agent-image       # the image agents run in
make sbom              # both images and an SBOM of each, to build/sbom
```

Run `make check` before handing work back.

## Conventions

Follow Keera Gateway's: format with `golangci-lint fmt` (gofumpt); standard
library first, no extra dependencies unless asked; comments explain why, not
what; tests beside the code; secrets are never logged and keys never stored in
plain text.

- Every API handler checks permissions itself. Rows of another organisation
  answer 404, not 403.
- Credentials a service needs stay in the control plane. An agent's container
  holds only its runtime token and its gateway key; platform tools run in
  `internal/dispatch` and check what the agent may do.
- `internal/store/migrations` holds one file per release that changes the
  schema. Once a release has shipped, its file is never edited.
- The panel has no build step: plain ES modules and one stylesheet. Its
  colours and components are shared with Keera Gateway and Keera Chat; change
  them in all three.

## Docs

`docs/` explains how each piece works and why, for operators and
contributors. Explain each thing in one place and link to it. Update the docs
when behaviour changes.

## Instructions

- Always keep code comments, documentation, and user-facing texts concise,
  easy to read, and in simple language.
