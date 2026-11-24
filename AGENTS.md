# AGENTS.md

Keera Agents runs AI agents for businesses on top of Keera Gateway. See
[README.md](README.md).

The repository is one Go module with two binaries: `cmd/keera-agents`, the
control plane, and `cmd/keera-agents-runtime`, the runtime inside each agent's
container (`agent`) and each workspace's (`workspace`). The runtime may import
only the standard library and its own few packages; depguard checks this. Keera Gateway lives in a sibling repository; this one
calls its control API and never imports its code.

## Layout

| Path                       | What it is                                                                 |
| -------------------------- | -------------------------------------------------------------------------- |
| `cmd/keera-agents`         | The control plane: serve, `health`, `version`.                             |
| `cmd/keera-agents-runtime` | The runtime in containers: `agent`, `workspace`, `version`.                |
| `internal/server`          | Wiring, lifecycle and the periodic work.                                   |
| `internal/config`          | Every setting, read from the environment.                                  |
| `internal/api`             | The panel's JSON API and sign-in through the gateway.                      |
| `internal/gateway`         | Client for the gateway's control API: sign-in and service routes.          |
| `internal/store`           | Postgres schema and queries.                                               |
| `internal/fleet`           | Keeps each agent's container in the state it should be in; its keys.       |
| `internal/billing`         | Meters machine time, bills it to the gateway, asks it about credit.        |
| `internal/runtime`         | The drivers: podman and Kubernetes (StatefulSets).                         |
| `internal/runner`          | The runtime API agents' containers call.                                   |
| `internal/protocol`        | The types of that API, shared by both ends.                                |
| `internal/agent`           | The runtimes: agent loop, builtin tools, MCP client; a workspace's bridge. |
| `internal/stream`          | Connections passed through HTTP, to a port inside a workspace.             |
| `internal/dispatch`        | Tasks from chats, schedules, cards and folders; delivery; tools.           |
| `internal/schedule`        | Cron expressions.                                                          |
| `internal/connectors`      | The adapters, one package per service; see `docs/connectors.md`.           |
| `internal/connectors/all`  | Registers every adapter.                                                   |
| `internal/webui`           | The panel, embedded. Same design as the gateway's.                         |
| `internal/metrics`         | The Prometheus exposition on `/metrics`.                                   |
| `internal/secret`          | Sealing for cookies, agents' keys and connector credentials.               |
| `internal/egress`          | Where connectors may connect: not loopback, nor inside unless allowed.     |
| `internal/id`              | Prefixed identifiers, sortable by creation time.                           |
| `internal/version`         | The version the binaries were built as.                                    |
| `Containerfile`            | The images: the control plane, the agent's, and the workspace's.           |
| `scripts`                  | The licence notices the images ship.                                       |
| `.github`                  | CI, and the release: images signed with SLSA Build L3 provenance.          |
| `compose`                  | The database for the development loop, and the panel in a container.       |

## Commands

```sh
make build             # both binaries, to build/
make check             # test + vet + fmt-check, and check-js when node is there
make test-integration  # tests that need Postgres, against a throwaway one
make check-all         # check + test-integration
make fmt               # format with golangci-lint fmt
make lint              # golangci-lint
make dev               # Postgres in a container, keera-agents under air, podman driver
make image             # the control plane's image
make agent-image       # the image agents run in
make workspace-image   # the image workspaces run in
make sbom              # the three images and an SBOM of each, to build/sbom
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
  schema, such as `0003_v0.3.0.sql`. A schema change goes in the file of the
  next release, which is created with the first change. Once a release has
  shipped, its file is never edited: the control plane runs each file once
  and remembers it by name.
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
- Never add yourself to commits or commit messages
