# Running it locally

The development loop runs Postgres in a container and keera-agents on the
host, under [air](https://github.com/air-verse/air), so it rebuilds on save.
Agents run as podman containers next to it. Keera Gateway runs from its own
repository.

## Once

In Keera Gateway's `compose/.env`, add:

```sh
KEERA_PUBLIC_URL=http://localhost:8080
KEERA_AGENTS_URL=http://localhost:8070
KEERA_AGENTS_SERVICE_KEY=<from keera-agents' compose/.env>
```

`make dev` here creates `compose/.env` with a fresh service key and secret key
the first time, and prints the service key. Start the gateway with its own
`make dev`.

## Every day

```sh
make dev        # ^C stops keera-agents; the database keeps running
make dev-down   # stops the database, keeps its volume
```

Open <http://localhost:8070> and sign in through the gateway.

`make dev` builds the agent image the first time. After changing
`internal/agent` or the `Containerfile`, run `make agent-image` and restart
the agent.

**Use `localhost` everywhere**, not `127.0.0.1`. Identity providers often
accept only `localhost`, and the browser keeps cookies apart by host name, so
mixing the two loses the sign-in on its way back.

**Agents reach the host as `host.containers.internal`.** `make dev` sets
`KEERA_AGENTS_RUNTIME_URL` and `KEERA_AGENTS_RUNTIME_GATEWAY_URL` to it. Both
keera-agents and the gateway must listen on more than loopback, which `:8070`
and `:8080` do.

## Chat services on a laptop

Microsoft, Google and Slack must reach `/hooks/` from the internet. Use a
tunnel, such as `cloudflared tunnel --url http://localhost:8070`. Set its
address as `KEERA_AGENTS_PUBLIC_URL` here and as `KEERA_AGENTS_URL` on the
gateway, and open the panel there: sign-in returns to that address, and its
cookies are only kept there.

## Tests

```sh
make check             # unit tests, vet, formatting, the panel's JavaScript
make test-integration  # store and end-to-end tests, against a throwaway Postgres
```

The end-to-end test in `internal/server` runs the control plane with a
stand-in gateway and chat service, and runs each agent's runtime in the test
binary: a chat message has to come back as a reply, after the agent called a
platform tool.
