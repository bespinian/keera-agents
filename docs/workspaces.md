# Workspaces

A workspace is a machine of one person's to work in, with a coding agent. It
has the same parts as an agent: a gateway key in its owner's name, connectors,
a machine and a volume. It takes no tasks. Its owner works in it, with the
coding agent beside them.

When someone creates a workspace, they choose what to work in:

| Experience | What runs                                                                          |
| ---------- | ---------------------------------------------------------------------------------- |
| `terminal` | pi, the coding agent, in tmux, shown in the browser by ttyd                        |
| `vscode`   | [code-server](https://github.com/coder/code-server), VS Code, with the Cline agent |

Both come set up with the models the workspace's key may use and the
workspace's connectors. The experience can be changed later. That starts the
machine again and keeps the files.

## Getting in

People reach a workspace with the `keera` command line from Keera Gateway.
It signs in once with `keera login`, finds keera-agents through the
gateway, and uses the same sign-in there.

```sh
keera workspace create api            # asks: terminal or VS Code
keera workspace open api              # opens it in the browser
keera workspace forward api 3000      # a dev server inside, on this machine
keera workspace ssh-config            # once per machine
ssh api.keera                         # ssh, scp, VS Code Remote-SSH, JetBrains
```

`open` and `forward` listen on the loopback of the person's machine, as
`kubectl port-forward` does, and pass each connection through keera-agents.
VS Code's own port proxy, `/proxy/<port>/`, is off: any web page the person
visits could reach it. Use `forward` instead.
`ssh-config` makes `~/.ssh/keera_ed25519` if there is none, adds its public
half to the person's keys, and adds a `Host *.keera` entry whose
`ProxyCommand` is `keera workspace ssh-proxy`. SSH then runs end to end, from
the person's ssh to the workspace's sshd.

The terminal experience runs pi in a tmux session called `pi`. `ssh api.keera`
and `tmux attach` join the same session the browser shows.

**Only the owner gets in.** An administrator sees, changes, stops and
deletes every workspace in the organisation, and reads its logs to help when
it does not start. But they cannot connect to it, nor change its image, which
would run their code with the owner's keys. Other members do not see it at
all: its id answers 404.

## How a connection travels

```
 person's machine         keera-agents (any replica)          workspace pod
 browser ─▶ 127.0.0.1 ─┐
 ssh ─▶ ProxyCommand ──┴▶ POST /api/v1/workspaces/{id}/connect?port=web|ssh|N
     keera CLI            checks the owner, wakes it    ──▶  :7420 front ──▶ 127.0.0.1:N
                          dials the pod's IP                 ttyd, code-server, sshd
```

Each hop is an HTTP/1.1 request with `Upgrade: keera-stream`, answered 101,
then raw bytes both ways ([internal/stream](../internal/stream/stream.go)). A
proxy in front of keera-agents must pass Upgrade requests on
([install.md](install.md#one-listener-five-surfaces)).

**keera-agents dials the pod.** A container that dialled back would keep
workspaces as closed as agents. But with several replicas, the person's
request and the container's dial could land on different replicas. A dial
into the pod works from any replica. So a workspace's pod takes connections
on one port, 7420. Agents' pods still take none.

**The front checks keera-agents.** keera-agents sends an ingress secret, a
keyed hash of the workspace's id and its runtime token's hash. It is stored
nowhere, any replica can make it, and it changes at each start. The runtime
gets it in its bootstrap. Without it, the front refuses. It then joins the
connection to a port on the loopback, where everything else listens:

| Port | What                                                 |
| ---- | ---------------------------------------------------- |
| 2222 | sshd, as the workspace's user, with the owner's keys |
| 7681 | ttyd, the terminal experience                        |
| 8080 | code-server, the VS Code experience                  |
| 7417 | the git proxy, as for an agent                       |
| 7418 | the bridge, below                                    |

**The browser's address is checked.** ttyd and code-server let in whoever
reaches them, and `open` listens on the person's loopback, where any web page
they visit can try to connect. So for `web`, the front lets a connection
through only if its first request names the loopback (`127.0.0.1`,
`localhost`), which a page that points its own name there cannot fake. ttyd
also refuses WebSockets from another origin, and code-server does the same
itself. keera-agents follows no redirect from the front, since the owner
chooses the image that answers there.

## What a workspace holds

The same as an agent ([agents.md](agents.md#what-an-agent-holds)): a runtime
token and a gateway key. The runtime keeps both from what the person runs,
in the same way.

**The bridge.** The coding agent never gets the key. pi and Cline are pointed
at `http://127.0.0.1:7418`, the bridge, with any key. The bridge adds the real
one:

- `/v1/...` is the gateway's OpenAI-compatible API, for models.
  `OPENAI_BASE_URL` points there too, for other tools.
- `/mcp` is an MCP server. It offers the workspace's connectors, run by
  keera-agents as for an agent, and the gateway's MCP servers.

A key copied out of a workspace would work anywhere. The bridge only works
inside it. The person can still spend the key's budget from inside, as they
could with a key of their own. The key is in their name and its project's
guardrails apply.

**Connectors.** A workspace binds to connectors as an agent does, with the
modes `files`, `repo`, `read` and `write`. `chat` and `watch` give work, which
a workspace does not take. Its tools act for its owner. git reaches bound
repositories through the git proxy at any time, not only during a task. The
same rule applies to pushes: only branches starting with `keera/`, since the
pushes use the connector's credentials. To push in their own name, people
add their own credentials in the workspace.

## Lifecycle

A workspace's states are an agent's ([agents.md](agents.md#lifecycle)). It
differs in three ways:

- **Nothing but a connection wakes it.** A connect to a suspended workspace
  resumes it and waits up to three minutes for it to be up.
- **It is idle when nobody is connected.** While a connection is open,
  keera-agents notes it every minute. After
  `KEERA_AGENTS_WORKSPACE_IDLE_SUSPEND` (`2h`) without one, the workspace is
  suspended. Its files stay. Programs that were running stop.
- **When its owner leaves**, it is stopped and its key revoked, as an agent
  is.

## The image

`make workspace-image` builds the `workspace` stage of the
[Containerfile](../Containerfile) on Debian, since code-server is built for
glibc. It has pi, code-server, ttyd, tmux, sshd, git, Node, Python and a C
toolchain. Cline ships as a VSIX and is installed into the workspace at the
first start. A deployment that wants more builds its own image `FROM` it and
sets `KEERA_AGENTS_WORKSPACE_IMAGE`.

The root filesystem is read-only, as for agents
([agents.md](agents.md#hardening)), and there is no root. `HOME` is
`/workspace`, the volume, so `npm install -g` and `pip install` in a
virtualenv land there and last. System packages need a derived image.

The runtime writes its own entries into `~/.pi/agent/models.json`,
`settings.json` and `mcp.json`, and into Cline's `cline_mcp_settings.json`. It
leaves the rest of each file alone. **Cline's model** is the one thing it does
not set: Cline keeps its provider in its own storage. In Cline's settings,
choose **OpenAI Compatible**, base URL `http://127.0.0.1:7418/v1`, any API
key, and a model from the gateway.

## Kubernetes

Workspaces run in the agents' namespace. The pods carry
`app.kubernetes.io/component: workspace`. Let keera-agents reach their front,
and nothing else: the `workspace-front` NetworkPolicy in
[agents.md](agents.md#hardening) does that. keera-agents finds a pod's IP with
`get` on pods, which the Role there already allows.

With podman, the front is published on a free port of the host's loopback.
With `KEERA_AGENTS_PODMAN_NETWORK` set, keera-agents is taken to run in a
container on that network, and reaches the workspace by its container's
name.
