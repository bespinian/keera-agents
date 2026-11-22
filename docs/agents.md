# Agents

An agent is a persistent assistant: instructions, a model, tools, and a
machine of its own with a workspace that outlives each task. This page
explains how one runs and why.

## Two binaries

`keera-agents` serves the control plane. `keera-agents-runtime agent` is the
runtime inside an agent's container, and `keera-agents-runtime workspace` the
runtime inside a workspace's ([workspaces.md](workspaces.md)). The
[Containerfile](../Containerfile) builds both from the same source, so the
runtime always speaks the protocol of the control plane it came with.

**The runtime is its own binary** so a container holds none of the control
plane's code or modules: no database driver, no panel, no connectors. It uses
only the standard library, so a flaw in one of the control plane's modules
does not show up in the agents' images, and it is half the size. A depguard rule in `.golangci.yml` keeps it that way:
the runtime's packages may import only each other and the standard library.

The runtime calls the control plane, never the other way round
([internal/protocol](../internal/protocol/protocol.go)). So a container needs
no open port, and the control plane needs no route into the agents'
namespace, only out of it. Workspaces are the one exception: keera-agents
passes their owner's connections into them on one port
([workspaces.md](workspaces.md#how-a-connection-travels)).

## What an agent holds

| Credential    | What it is                                            | Where it lives                                     |
| ------------- | ----------------------------------------------------- | -------------------------------------------------- |
| Runtime token | Proves to the control plane which agent is calling    | The container's environment; its hash in Postgres  |
| Gateway key   | The agent's own Keera key, for models and MCP servers | Sealed in Postgres; fetched by the runtime at boot |

**Nothing else.** A chat service's bot token or a Drive service account never
reaches a container. Posting a message or reading a file is a _platform tool_:
the runtime asks the control plane, which checks that the agent may and makes
the call itself. Its answer is cut at 64 KiB, and the agent is told to ask for
less. What that lets an agent reach is in
[connectors.md](connectors.md#what-an-agent-can-reach).

**The gateway key is not in the pod spec.** Anyone who can read pods in the
agents' namespace sees the runtime token, which is useless outside that
container's calls to the control plane. The key is handed over at boot, and
the runtime removes the token from its own environment and makes its memory
unreadable to the commands it runs. The image does the same for tini, which
starts it ([Hardening](#hardening)).

**The key is attributed to the agent's owner** and named "Agent <name>". The
owner is whoever created it, or the member an administrator created it for.
Their gateway project decides
the budget, rate limit, system prompt and model allow-list, and the agent's
own monthly budget, if set, sits on the key. Every request carries
`X-Keera-Session: <task id>`, so the gateway's Sessions screen shows each task
as one session.

keera-agents mints and revokes these keys with `KEERA_AGENTS_SERVICE_KEY`,
which works only on the gateway's `/control/v1/service/` routes. How the
gateway limits it is in Keera Gateway's `docs/agents.md`.

## Lifecycle

| State       | Means                                                                           |
| ----------- | ------------------------------------------------------------------------------- |
| `starting`  | The container was created or resumed and has not asked for work yet             |
| `running`   | The runtime is up and polling for tasks                                         |
| `suspended` | Idle for `KEERA_AGENTS_IDLE_SUSPEND`; the compute is released, the volume kept  |
| `stopped`   | Someone stopped it, or its owner left; it takes no tasks                        |
| `failed`    | Its machine could not start; the reason is shown, and the next task tries again |

A queued task wakes a suspended agent. While a task waits for a `failed`
agent, keera-agents tries to start it again after 30 seconds, then less and
less often, up to every 10 minutes. A task that waited an hour fails, so the
person who asked hears back. The same goes for an agent whose machine has
waited an hour to start, as when no node has room for its pod. Each agent runs **one task at a time**:
it is one machine with one workspace, and two tasks writing the same files
would trip over each other. Tasks wait in the queue in order.

On a deployment that bills machine time, an organisation without credit
starts no machine, and its waiting tasks fail with the reason
([billing.md](billing.md#when-an-organisation-may-not-run-machines)).

**Changing settings.** Instructions, model and tools reach the runtime at its
next task, through the bootstrap's version. A changed project or budget gets a
new key at once; the runtime's next gateway call fails with 401 and it fetches
the new key. If the gateway cannot be reached, the old key is kept and
replaced in the background once it can, every 15 seconds. A changed image or machine size replaces the container and keeps
the workspace.

**The runtime token** is new at each start, and stored once the container has
it, so a start that fails leaves the old container working. A container whose
token no longer matches the stored one, as after a start whose answer was
lost, is started again within a minute. A stopped agent's token is refused
until the agent is started again, so a leaked token cannot fetch a new key.

**When the owner leaves.** Every ten minutes keera-agents asks the gateway
whether each owner is still an active member. An agent whose owner was
disabled or removed is stopped, its key revoked and its queue cancelled. Only
the gateway's own answer counts: a 404 from a proxy in between, or an error,
leaves the agent alone. An
administrator can take it over with **Take over** in the agent's Settings tab,
which makes them the owner and mints a key in their name. It stays stopped
until they start it. An administrator can take over any agent this way, not
only one whose owner left.

**Deleting** an agent removes its container and its workspace, and revokes its
key. Its tasks and audit entries stay.

## The runtime

Each task is a loop of model calls through the gateway's Chat Completions API,
with tools, until the model answers without a tool call, the agent's step limit
is hit, if it has one, or the task's deadline passes
(`KEERA_AGENTS_TASK_TIMEOUT`). If the model
writes a tool call as text instead of making one, it is told once; if it does it
again, the task fails rather than deliver the text. A model that keeps doing this
lacks tool calling, or its backend does not parse tool calls.

| Tool group | Tools                                                                                   |
| ---------- | --------------------------------------------------------------------------------------- |
| `memory`   | `memory_read`, `memory_write`, `memory_append` on `MEMORY.md`, which is in every prompt |
| `files`    | `file_list`, `file_read`, `file_write`, `file_search`, inside the workspace             |
| `shell`    | `shell`, a command in the workspace with a timeout                                      |
| `web`      | `web_fetch`, if egress allows it                                                        |
| MCP        | every MCP server the agent's key may call, as `<server>__<tool>`                        |
| Platform   | `send_message`, `board_*`, `files_*` for connected folders, `repo_*` for repositories   |
| Services   | `<kind>_<tool>` for each API connector, such as `jira_search_issues` or `aws_costs`     |

The prompt lists what the agent is connected to: each channel, folder,
repository and service, with the id its tools take and, for a repository, the
command that clones it. The same lists are in the tools' descriptions, but
models tend to miss them there.

A chat thread keeps its history in the workspace, so a follow-up in the same
thread has the context of the earlier messages.

The machine is the boundary: the agent may do anything inside it. Pick the
isolation tier with that in mind. A new agent or workspace gets the strongest
tier the deployment has, unless someone chooses another.

## Where agents run

|                   | `kubernetes`                                          | `podman`                      |
| ----------------- | ----------------------------------------------------- | ----------------------------- |
| For               | real workloads                                        | local development             |
| Runs on           | any cluster, as one StatefulSet per agent             | the host keera-agents runs on |
| Suspend           | `replicas: 0`                                         | `podman stop`                 |
| Workspace         | a PVC from the StatefulSet's claim template           | a named volume                |
| Isolation tiers   | RuntimeClasses: gVisor, Kata                          | OCI runtimes: `runsc`, `krun` |
| Network isolation | the namespace's NetworkPolicy, if the CNI enforces it | none                          |

**Isolation tiers.** `standard` shares the host kernel. `isolated` is gVisor.
`vm` is Kata or krun, a kernel of its own, and the best choice for an agent
with a shell. A tier with no runtime mapped is refused, never run with less.

**Limits.** Whoever may change an agent chooses its machine, so the operator
bounds it: `KEERA_AGENTS_MIN_ISOLATION` is the weakest tier allowed, and
`KEERA_AGENTS_MAX_CPU`, `_MAX_MEMORY` and `_MAX_DISK` the largest machine. An
agent outside them is refused at its next start, until someone changes it.
The smallest machine is 10m of CPU, 64Mi of memory and 64Mi of disk: less
would round down to no limit at all on podman.

**Kubernetes.** keera-agents must run in the cluster, and agents in a
namespace of their own, since whoever reads pods there reads runtime tokens.
`KEERA_AGENTS_NAMESPACE` names it; keera-agents does not start without it.
Nothing needs to be installed on the cluster. The service account needs the
Role in [Hardening](#hardening), in that namespace only. A changed spec patches the
StatefulSet's pod template, and the StatefulSet replaces the pod on the same
claim. Deleting an agent deletes its claim too. `volumeClaimTemplates` cannot
change, so an agent's disk is fixed once it exists, and billed at that size. The claim retention
policy that deletes the workspace needs Kubernetes 1.27 or later.

**Egress.** Agents need the gateway and keera-agents' runtime URL, nothing
else. Add hosts the agents' work needs, such as an internal package proxy, to
the namespace's NetworkPolicy ([example](#hardening)). With the podman driver,
containers reach everything the host does.

**Not tested against a real cluster yet.** The Kubernetes driver is tested
against a stand-in API server. Worth checking first: that a changed template
replaces a crash-looping pod, and that deleting a StatefulSet deletes its
claim.

## Hardening

The machine is the boundary, so it is locked down whatever the tier. The
tier decides how much of the host kernel the agent can reach; prefer
`isolated` or `vm`.

**The image** runs as user 1000, never as root. tini and the runtime are
execute-only: the kernel then makes their `/proc` files root's, so a command
the agent runs cannot read the runtime token from PID 1's environment. It is
built with Alpine's latest fixes; rebuild it, and images `FROM` it, regularly.

**Every container** the drivers make:

|                      | `kubernetes`                                      | `podman`                       |
| -------------------- | ------------------------------------------------- | ------------------------------ |
| User                 | 1000, `runAsNonRoot`                              | `--user 1000:1000`             |
| Capabilities         | all dropped                                       | `--cap-drop ALL`               |
| Privilege escalation | `allowPrivilegeEscalation: false`                 | `no-new-privileges`            |
| Seccomp              | `RuntimeDefault`                                  | podman's default               |
| Root filesystem      | read-only                                         | `--read-only`                  |
| `/tmp`               | an `emptyDir` of 1Gi                              | a tmpfs of 1 GB, in its memory |
| Node disk            | `ephemeral-storage` of 1Gi                        | -                              |
| CPU and memory       | requests equal limits                             | `--cpus`, `--memory`, no swap  |
| Processes            | the kubelet's `podPidsLimit`                      | `--pids-limit 4096`            |
| Kubernetes API       | no service account token, no service links in env | -                              |

Only `/workspace` and `/tmp` are writable. `HOME` is the workspace, so
`pip install --user`, a virtualenv or a git config land there and last.

**The agents' namespace.** On Kubernetes, some of it is the cluster's to set:

- **Pod Security `restricted`.** Agents' pods meet it, and it refuses any
  other pod in the namespace.
- **A default-deny NetworkPolicy** that allows DNS, the gateway and
  keera-agents. It also keeps agents from the cloud's metadata endpoint, other
  namespaces and each other. The CNI must enforce it.
- **`podPidsLimit` on the kubelet.** Kubernetes has no per-pod setting, and
  without one a fork bomb can use up the node's process ids.
- **User namespaces.** `KEERA_AGENTS_USER_NAMESPACES=true` runs standard-tier
  pods with `hostUsers: false`: the agent's user is an unprivileged one on the
  node, so an escape gains nothing there. It needs Kubernetes 1.33 or later
  and a container runtime that supports it, such as containerd 2 or CRI-O. The
  other tiers have a kernel of their own, and their runtimes may not support
  it, so they do not get it.

For agents in `agents` and keera-agents and the gateway in `keera`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: agents
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny
  namespace: agents
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - { protocol: UDP, port: 53 }
        - { protocol: TCP, port: 53 }
    # keera-agents' runtime API and the gateway.
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: keera
      ports:
        - { protocol: TCP, port: 8070 }
        - { protocol: TCP, port: 8080 }
---
# Workspaces only: keera-agents reaches their front. See workspaces.md.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: workspace-front
  namespace: agents
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: workspace
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: keera
      ports:
        - { protocol: TCP, port: 7420 }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: keera-agents
  namespace: agents
rules:
  - apiGroups: [apps]
    resources: [statefulsets]
    verbs: [get, list, create, patch, delete]
  - apiGroups: [""]
    resources: [pods, pods/log]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: keera-agents
  namespace: agents
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: keera-agents
subjects:
  - kind: ServiceAccount
    name: keera-agents
    namespace: keera
```
