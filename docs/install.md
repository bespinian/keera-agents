# Installing Keera Agents

keera-agents needs three things: a **Keera Gateway** that knows it, a
**Postgres**, and somewhere to **run agents**: a Kubernetes cluster, with
nothing extra installed, or podman on one host for development.

## On the gateway

Set two variables on Keera Gateway, and restart it:

| Variable                   | Value                                                             |
| -------------------------- | ----------------------------------------------------------------- |
| `KEERA_AGENTS_URL`         | keera-agents' public origin, such as `https://agents.example.ch`  |
| `KEERA_AGENTS_SERVICE_KEY` | a random secret of at least 32 characters: `openssl rand -hex 32` |

The first lets the gateway hand a sign-in over to
`<KEERA_AGENTS_URL>/auth/callback`. The second lets keera-agents mint and
revoke agents' keys, look people up and read what an agent may use, and nothing
else. See Keera Gateway's `docs/agents.md`.

To bill organisations for their machines, set prices
([billing.md](billing.md)). The gateway needs nothing more.

## Configuration

Every setting is an environment variable.

### Required

| Variable                    | What it is                                                                    |
| --------------------------- | ----------------------------------------------------------------------------- |
| `KEERA_AGENTS_GATEWAY_URL`  | The gateway as a browser reaches it                                           |
| `KEERA_AGENTS_SERVICE_KEY`  | The same value as on the gateway                                              |
| `KEERA_AGENTS_DATABASE_URL` | Postgres, such as `postgres://keera:…@db:5432/keera_agents`                   |
| `KEERA_AGENTS_SECRET_KEY`   | Seals cookies, agents' keys and connector credentials: `openssl rand -hex 32` |

**Back up `KEERA_AGENTS_SECRET_KEY` with the database.** Without it, every
stored connector credential and agent key is unreadable: connectors must be
entered again, and agents get new keys at their next start.

### Where agents run

| Variable                              | Default                             | What it does                                                        |
| ------------------------------------- | ----------------------------------- | ------------------------------------------------------------------- |
| `KEERA_AGENTS_DRIVER`                 | -                                   | `kubernetes`, `podman`, or unset: agents can be set up but not run  |
| `KEERA_AGENTS_IMAGE`                  | `localhost/keera-agent:latest`      | The agent image, built with `make agent-image`                      |
| `KEERA_AGENTS_RUNTIME_URL`            | `KEERA_AGENTS_PUBLIC_URL`           | keera-agents **as an agent reaches it**: the in-cluster Service     |
| `KEERA_AGENTS_RUNTIME_GATEWAY_URL`    | `KEERA_AGENTS_GATEWAY_INTERNAL_URL` | The gateway as an agent reaches it                                  |
| `KEERA_AGENTS_IDLE_SUSPEND`           | `15m`                               | Suspend an agent with nothing to do for this long; `0` never        |
| `KEERA_AGENTS_WORKSPACE_IMAGE`        | `localhost/keera-workspace:latest`  | The workspace image, built with `make workspace-image`              |
| `KEERA_AGENTS_WORKSPACE_IDLE_SUSPEND` | `2h`                                | Suspend a workspace nobody is connected to for this long; `0` never |
| `KEERA_AGENTS_RUNTIME_STANDARD`       | -                                   | The runtime for the standard tier, if not the default               |
| `KEERA_AGENTS_RUNTIME_ISOLATED`       | -                                   | gVisor: a RuntimeClass, or `runsc` on podman                        |
| `KEERA_AGENTS_RUNTIME_VM`             | -                                   | Kata: a RuntimeClass, or `krun` on podman                           |
| `KEERA_AGENTS_MIN_ISOLATION`          | `standard`                          | The weakest tier agents may choose; it needs a runtime              |
| `KEERA_AGENTS_MAX_CPU`                | `8`                                 | The most CPU one agent may ask for                                  |
| `KEERA_AGENTS_MAX_MEMORY`             | `32Gi`                              | The most memory one agent may ask for                               |
| `KEERA_AGENTS_MAX_DISK`               | `100Gi`                             | The largest workspace one agent may ask for                         |
| `KEERA_AGENTS_NAMESPACE`              | required for Kubernetes             | Kubernetes: where agents run. Give them their own                   |
| `KEERA_AGENTS_STORAGE_CLASS`          | the cluster's                       | Kubernetes: what workspaces are provisioned from                    |
| `KEERA_AGENTS_SERVICE_ACCOUNT`        | the namespace's                     | Kubernetes: what agents run as; its token is never mounted          |
| `KEERA_AGENTS_IMAGE_PULL_SECRETS`     | -                                   | Kubernetes: comma-separated                                         |
| `KEERA_AGENTS_USER_NAMESPACES`        | `false`                             | Kubernetes: standard-tier pods in a user namespace of their own     |
| `KEERA_AGENTS_PODMAN_BINARY`          | `podman`                            | podman: the command                                                 |
| `KEERA_AGENTS_PODMAN_NETWORK`         | podman's default                    | podman: the network agents join                                     |

### Everything else

| Variable                            | Default                      | What it does                                               |
| ----------------------------------- | ---------------------------- | ---------------------------------------------------------- |
| `KEERA_AGENTS_PUBLIC_URL`           | `http://127.0.0.1:<port>`    | keera-agents as browsers and chat services reach it        |
| `KEERA_AGENTS_ADDR`                 | `:8070`                      | The listener: panel, API, webhooks and runtime API         |
| `KEERA_AGENTS_GATEWAY_INTERNAL_URL` | `KEERA_AGENTS_GATEWAY_URL`   | The gateway as keera-agents reaches it                     |
| `KEERA_AGENTS_SECURE_COOKIES`       | on for an `https` public URL | Off allows plain HTTP on localhost                         |
| `KEERA_AGENTS_TASK_TIMEOUT`         | `30m`                        | The longest a task may run                                 |
| `KEERA_AGENTS_WATCH_INTERVAL`       | `5m`                         | How often watched folders are checked; at least `30s`      |
| `KEERA_AGENTS_LOG_LEVEL`            | `info`                       | `debug`, `info`, `warn` or `error`                         |
| `KEERA_AGENTS_LOG_FORMAT`           | `text`                       | `text` or `json`                                           |
| `KEERA_AGENTS_METRICS_TOKEN`        | unset: `/metrics` is off     | Reads `/metrics` and nothing else. See [Metrics](#metrics) |
| `KEERA_AGENTS_UPSTREAM_DENY`        | loopback and cloud metadata  | Addresses connectors never reach. See below                |
| `KEERA_AGENTS_UPSTREAM_PRIVATE`     | `none`                       | Hosts inside the network connectors may reach              |
| `KEERA_AGENTS_PRICE_*`              | unset: machines are free     | What machine time costs. See [billing.md](billing.md)      |

keera-agents refuses to start with a missing required setting, a service key
under 32 characters or a secret key under 16, a metrics token under 16 or the
same as the service or secret key, an unknown driver, log level or log format,
an address with a path, a duration it cannot read or below its minimum, an
address list it cannot read, or a price that is not from 0 to 1000, and says
which.

### Where connectors may connect

An organisation's administrator can point a GitHub or GitLab connector at a
server of their own. So keera-agents checks every address its connectors and
the agents' git proxy connect to. The two settings mean what
`KEERA_UPSTREAM_DENY` and `KEERA_UPSTREAM_PRIVATE` mean on Keera Gateway.

`KEERA_AGENTS_UPSTREAM_DENY` is never reached, by anyone. By default it is:

```text
127.0.0.0/8,::1/128,169.254.0.0/16,fe80::/10,0.0.0.0/8,::/128,fd00:ec2::254/128,100.100.100.200/32,168.63.129.16/32
```

That is loopback, link-local (where cloud metadata services answer), the
unspecified address and the cloud metadata addresses of AWS, Alibaba and
Azure. `none` turns it off.

`KEERA_AGENTS_UPSTREAM_PRIVATE` keeps connectors off the private network.
By default (`none`) they reach only public addresses. To use a GitLab or
GitHub Enterprise inside your network, name its host, such as
`gitlab.internal`; several are comma-separated. `all` allows the whole private
network. If you set `HTTPS_PROXY` to a private address, name the proxy host
too.

The check applies to the address a name resolves to, so a public name that
points into the network is blocked too. An IPv6 address that reaches IPv4
through NAT64 (`64:ff9b::/96`) or 6to4 (`2002::/16`) is checked as the IPv4
address it reaches. Behind a proxy, the proxy must filter.
Calls to the gateway are not checked: the operator sets its address.

## One listener, five surfaces

| Path                  | What it is                                | Who calls it             |
| --------------------- | ----------------------------------------- | ------------------------ |
| `/`, `/api`, `/auth`  | The panel, its API, sign-in               | people's browsers        |
| `/hooks`              | Chat services' webhooks                   | Microsoft, Google, Slack |
| `/runtime`            | The runtime API                           | agents' containers       |
| `/healthz`, `/readyz` | Liveness, and readiness with the database | the orchestrator         |
| `/metrics`            | Prometheus metrics                        | Prometheus               |

**Probes.** `/healthz` answers as soon as the process listens, before the
schema migrates, which can wait for another replica. Use it for liveness.
`/readyz` answers 503 until the control plane is ready, and while the
database does not answer: use it for readiness. In an image without curl,
`keera-agents health` checks `/readyz`.

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 8070 }
readinessProbe:
  httpGet: { path: /readyz, port: 8070 }
```

**Publish `/runtime` only inside the cluster.** It authenticates every call
with a runtime token, but nothing outside needs it. The chat services need
`/hooks/` from the internet; people need the rest.

**Pass Upgrade requests on.** The `keera` command line reaches workspaces
through `/api/v1/workspaces/{id}/connect`, which takes the connection over as
a WebSocket does ([workspaces.md](workspaces.md#how-a-connection-travels)). A
proxy in front of keera-agents must let it, over HTTP/1.1, and not cut idle
connections short. nginx, Traefik and HAProxy pass Upgrade requests on;
Envoy needs it allowed.

## Metrics

`/metrics` is the Prometheus exposition. It is off until
`KEERA_AGENTS_METRICS_TOKEN` is set, and then it needs that token as
`Authorization: Bearer <token>`. Generate one with `openssl rand -hex 32`.
Nothing on it names an organisation, agent or person.

```yaml
scrape_configs:
  - job_name: keera-agents
    authorization:
      credentials_file: /etc/prometheus/keera-agents-token
    static_configs:
      - targets: ["keera-agents.keera.svc:8070"]
```

**Counters** count what one replica did, so sum them across replicas:

| Metric                                           | Labels          | What it counts                                                                   |
| ------------------------------------------------ | --------------- | -------------------------------------------------------------------------------- |
| `keera_agents_tasks_total`                       | source, state   | finished tasks, counted when their result is handed back                         |
| `keera_agents_task_duration_seconds`             | source, state   | how long a task ran                                                              |
| `keera_agents_task_wait_seconds`                 | source          | how long a task waited in the queue                                              |
| `keera_agents_deliveries_total`                  | source, outcome | results handed back: `ok`, `error`, or `retry` when nothing was sent             |
| `keera_agents_tool_calls_total`                  | kind, outcome   | platform and connector tool calls, such as `jira` or `board`                     |
| `keera_agents_tool_call_duration_seconds`        | kind, outcome   | how long a tool call took                                                        |
| `keera_agents_webhooks_total`                    | kind, outcome   | webhooks: `accepted`, `unverified`, `unreadable` or `unconfigured`               |
| `keera_agents_machine_starts_total`              | kind, outcome   | containers created for agents and workspaces                                     |
| `keera_agents_folder_checks_total`               | kind, outcome   | checks of watched folders                                                        |
| `keera_agents_gateway_calls_total`               | op, outcome     | calls to the gateway's control API: `ok`, `4xx`, `5xx`, `unreachable` or `error` |
| `keera_agents_gateway_call_duration_seconds`     | op, outcome     | how long a call to the gateway took                                              |
| `keera_agents_background_round_duration_seconds` |                 | how long a round of the periodic work took                                       |

`keera_agents_background_last_round_timestamp_seconds` is when the replica
last finished a round of the periodic work, which runs on one replica at a
time: take the max across replicas.

**Gauges** are read from the database at each scrape, so every replica shows
the same values. Take the max, not the sum:

| Metric                                    | Labels      | What it shows                                     |
| ----------------------------------------- | ----------- | ------------------------------------------------- |
| `keera_agents_machines`                   | kind, state | agents and workspaces in each state               |
| `keera_agents_tasks_queued`               |             | tasks waiting for their agent                     |
| `keera_agents_tasks_running`              |             | tasks an agent is working on                      |
| `keera_agents_oldest_queued_task_seconds` |             | how long the oldest queued task has waited        |
| `keera_agents_results_undelivered`        |             | finished tasks whose result has not gone back yet |
| `keera_agents_build_info`                 | version     | always 1                                          |

Worth an alert: `keera_agents_oldest_queued_task_seconds` over a few minutes,
since a task fails after an hour of waiting; `keera_agents_machines` with
`state="failed"` above zero; a rise in `keera_agents_webhooks_total` with
`outcome="unverified"`, which may be someone probing, or a changed secret at
the chat service; `keera_agents_results_undelivered` that keeps growing;
`keera_agents_gateway_calls_total` with `outcome="unreachable"` or `"5xx"`,
since sign-ins and agents' keys need the gateway; and
`time() - max(keera_agents_background_last_round_timestamp_seconds)` over five
minutes, since schedules, cards and deliveries then wait.

Model calls, tokens and spend are on Keera Gateway's own `/metrics`. Each task
is one session there, so its Sessions screen shows the model side of a task.

## Shapes

**Kubernetes.** Run keera-agents as a Deployment in the cluster where agents
run, with Postgres beside it or managed. Several replicas are fine: periodic
work takes an advisory lock, and schedules, cards and deliveries are claimed
in the database. Give agents their own namespace with a default-deny
NetworkPolicy that allows DNS, the gateway and keera-agents' Service, and the
Role, as in [agents.md](agents.md#hardening). No Helm chart is in this
repository.

**Single host.** `compose/compose.yaml` runs Postgres, and with
`--profile full` keera-agents in a container too. That container runs no
agents: podman cannot run inside it, and the Kubernetes driver works only in a
cluster. To run agents on one host, run keera-agents on the host with the
podman driver, as [run-locally.md](run-locally.md) does. The container reaches
the gateway at `http://host.containers.internal:8080` unless
`KEERA_AGENTS_GATEWAY_INTERNAL_URL` says otherwise. Two settings are for
compose only: `KEERA_AGENTS_DB_PASSWORD`, the database's password (`keera` by
default), and `KEERA_AGENTS_BIND`, the interface the port is published on
(`127.0.0.1` by default).

## Images

All three come from one [Containerfile](../Containerfile), which builds the
binary once. `make image` builds the control plane's image, `FROM scratch`
(`--target control-plane`). `make agent-image` builds the agent image
(`--target agent`): Alpine with bash, git, curl, python3, jq and ripgrep, user
1000, the workspace at `/workspace`, and `keera-agents agent` under tini. It
is a base: a deployment builds its own `FROM` it, with the tools its agents
need, and names it in `KEERA_AGENTS_IMAGE` or per agent. Agents run it with a
read-only root filesystem, so install tools in the image, not at run time
([Hardening](agents.md#hardening)).
Setting `KEERA_AGENT_LOG_LEVEL=debug` in such an image makes the runtime log
more. `make workspace-image` builds the image workspaces run in
([workspaces.md](workspaces.md#the-image)). It checks each file it downloads,
ttyd, code-server and Cline, against a SHA-256 pinned in the Containerfile:
change those with the versions.

Each release publishes all three, built by GitHub Actions from the tag:
`ghcr.io/bespinian/keera-agents:<tag>`, `ghcr.io/bespinian/keera-agent:<tag>`
and `ghcr.io/bespinian/keera-workspace:<tag>`. They are for `linux/amd64`
only. The Containerfile builds for `arm64` too: on such a machine, build them
with the `make` targets above. The release lists an SBOM for
each. Every image carries the licences of what is compiled in, in
`/THIRD_PARTY_NOTICES`.

Each image is signed with build provenance (SLSA Build Level 3). To check
that the release workflow built it from this repository:

```sh
gh attestation verify oci://ghcr.io/bespinian/keera-agent:v0.1.0 --repo bespinian/keera-agents \
  --signer-workflow bespinian/keera-agents/.github/workflows/release-build.yml
```

`--signer-workflow` checks that the release workflow signed the image, not
some other workflow in the repository. Verify the image you base your own
agent image on, too. `make sbom` writes the SBOMs of locally built images to
`build/sbom`.

## Backups

Three things hold state. Back up all three, on the same schedule.

| What                      | Holds                                              | Without it                                             |
| ------------------------- | -------------------------------------------------- | ------------------------------------------------------ |
| The database              | agents, connectors, tasks, schedules, the board    | everything is gone                                     |
| `KEERA_AGENTS_SECRET_KEY` | the key the database's credentials are sealed with | connectors must be entered again, people sign in again |
| Agents' workspaces        | `MEMORY.md`, chat histories, files and clones      | agents forget, and unpushed work is lost               |

Back up the database with `pg_dump`, and keep the secret key apart from it,
in your secret store. A workspace is a volume per agent: on Kubernetes, the
PVCs labelled `app.kubernetes.io/managed-by=keera-agents`, backed up with
volume snapshots or a tool such as Velero; on podman, the volumes named
`keera-agent-…-workspace`, with `podman volume export`.

To restore, restore the database and the volumes, and start keera-agents with
the same secret key. Agents pick up their workspaces when they start.

## Upgrading

Take a backup first ([Backups](#backups)). The schema migrates at start, under
an advisory lock. Upgrade the agent image with the control plane: agents pick
up a new image when they are restarted or their settings change.

**Rolling back.** A release refuses to start against a database that a newer
release migrated, since it would misread it. To go back, restore the database
from the backup taken before the upgrade, then start the older release. During
a rolling update, replicas of the old release that restart after the new one
has migrated stay down until the update finishes.
