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

| Variable                           | Default                             | What it does                                                       |
| ---------------------------------- | ----------------------------------- | ------------------------------------------------------------------ |
| `KEERA_AGENTS_DRIVER`              | -                                   | `kubernetes`, `podman`, or unset: agents can be set up but not run |
| `KEERA_AGENTS_IMAGE`               | `localhost/keera-agent:latest`      | The agent image, built with `make agent-image`                     |
| `KEERA_AGENTS_RUNTIME_URL`         | `KEERA_AGENTS_PUBLIC_URL`           | keera-agents **as an agent reaches it**: the in-cluster Service    |
| `KEERA_AGENTS_RUNTIME_GATEWAY_URL` | `KEERA_AGENTS_GATEWAY_INTERNAL_URL` | The gateway as an agent reaches it                                 |
| `KEERA_AGENTS_IDLE_SUSPEND`        | `15m`                               | Suspend an agent with nothing to do for this long; `0` never       |
| `KEERA_AGENTS_RUNTIME_STANDARD`    | -                                   | The runtime for the standard tier, if not the default              |
| `KEERA_AGENTS_RUNTIME_ISOLATED`    | -                                   | gVisor: a RuntimeClass, or `runsc` on podman                       |
| `KEERA_AGENTS_RUNTIME_VM`          | -                                   | Kata: a RuntimeClass, or `krun` on podman                          |
| `KEERA_AGENTS_MIN_ISOLATION`       | `standard`                          | The weakest tier agents may choose; it needs a runtime             |
| `KEERA_AGENTS_MAX_CPU`             | `8`                                 | The most CPU one agent may ask for                                 |
| `KEERA_AGENTS_MAX_MEMORY`          | `32Gi`                              | The most memory one agent may ask for                              |
| `KEERA_AGENTS_MAX_DISK`            | `100Gi`                             | The largest workspace one agent may ask for                        |
| `KEERA_AGENTS_NAMESPACE`           | keera-agents' own                   | Kubernetes: where agents run. Give them their own                  |
| `KEERA_AGENTS_STORAGE_CLASS`       | the cluster's                       | Kubernetes: what workspaces are provisioned from                   |
| `KEERA_AGENTS_SERVICE_ACCOUNT`     | the namespace's                     | Kubernetes: what agents run as; its token is never mounted         |
| `KEERA_AGENTS_IMAGE_PULL_SECRETS`  | -                                   | Kubernetes: comma-separated                                        |
| `KEERA_AGENTS_USER_NAMESPACES`     | `false`                             | Kubernetes: standard-tier pods in a user namespace of their own    |
| `KEERA_AGENTS_PODMAN_BINARY`       | `podman`                            | podman: the command                                                |
| `KEERA_AGENTS_PODMAN_NETWORK`      | podman's default                    | podman: the network agents join                                    |

### Everything else

| Variable                            | Default                      | What it does                                          |
| ----------------------------------- | ---------------------------- | ----------------------------------------------------- |
| `KEERA_AGENTS_PUBLIC_URL`           | `http://127.0.0.1:<port>`    | keera-agents as browsers and chat services reach it   |
| `KEERA_AGENTS_ADDR`                 | `:8070`                      | The listener: panel, API, webhooks and runtime API    |
| `KEERA_AGENTS_GATEWAY_INTERNAL_URL` | `KEERA_AGENTS_GATEWAY_URL`   | The gateway as keera-agents reaches it                |
| `KEERA_AGENTS_SECURE_COOKIES`       | on for an `https` public URL | Off allows plain HTTP on localhost                    |
| `KEERA_AGENTS_TASK_TIMEOUT`         | `30m`                        | The longest a task may run                            |
| `KEERA_AGENTS_WATCH_INTERVAL`       | `5m`                         | How often watched folders are checked; at least `30s` |
| `KEERA_AGENTS_LOG_LEVEL`            | `info`                       | `debug`, `info`, `warn` or `error`                    |
| `KEERA_AGENTS_LOG_FORMAT`           | `text`                       | `text` or `json`                                      |
| `KEERA_AGENTS_UPSTREAM_DENY`        | loopback and cloud metadata  | Addresses connectors never reach. See below           |
| `KEERA_AGENTS_UPSTREAM_PRIVATE`     | `none`                       | Hosts inside the network connectors may reach         |

keera-agents refuses to start with a missing required setting, a service key
under 32 characters or a secret key under 16, an unknown driver, log level or
log format, an address with a path, a duration it cannot read or below its
minimum, or an address list it cannot read, and says which.

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
points into the network is blocked too. Behind a proxy, the proxy must filter.
Calls to the gateway are not checked: the operator sets its address.

## One listener, four surfaces

| Path                  | What it is                                | Who calls it             |
| --------------------- | ----------------------------------------- | ------------------------ |
| `/`, `/api`, `/auth`  | The panel, its API, sign-in               | people's browsers        |
| `/hooks`              | Chat services' webhooks                   | Microsoft, Google, Slack |
| `/runtime`            | The runtime API                           | agents' containers       |
| `/healthz`, `/readyz` | Liveness, and readiness with the database | the orchestrator         |

**Publish `/runtime` only inside the cluster.** It authenticates every call
with a runtime token, but nothing outside needs it. The chat services need
`/hooks/` from the internet; people need the rest.

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

Both come from one [Containerfile](../Containerfile), which builds the binary
once. `make image` builds the control plane's image, `FROM scratch`
(`--target control-plane`). `make agent-image` builds the agent image
(`--target agent`): Alpine with bash, git, curl, python3, jq and ripgrep, user
1000, the workspace at `/workspace`, and `keera-agents agent` under tini. It
is a base: a deployment builds its own `FROM` it, with the tools its agents
need, and names it in `KEERA_AGENTS_IMAGE` or per agent. Agents run it with a
read-only root filesystem, so install tools in the image, not at run time
([Hardening](agents.md#hardening)).
Setting `KEERA_AGENT_LOG_LEVEL=debug` in such an image makes the runtime log
more.

Each release publishes both, built by GitHub Actions from the tag:
`ghcr.io/bespinian/keera-agents:<tag>` and `ghcr.io/bespinian/keera-agent:<tag>`.
The release lists an SBOM for each. Both images carry the licences of what is
compiled in, in `/THIRD_PARTY_NOTICES`.

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

## Upgrading

The schema migrates at start, under an advisory lock. Upgrade the agent image
with the control plane: agents pick up a new image when they are restarted or
their settings change.
