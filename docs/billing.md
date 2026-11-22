# Billing

A deployment can charge organisations for their agents' and workspaces'
machines. keera-agents meters the time and sends it to Keera Gateway's bill.
The gateway holds each organisation's credit and takes its payments, so an
organisation has one bill and one balance for models and machines.
keera-agents takes no money itself.

Model calls are billed by the gateway as they always are: every call an
agent makes goes through it, on a key in its owner's name
([agents.md](agents.md#what-an-agent-holds)). This page is about the machines.

Without prices, nothing is billed and nothing changes.

## Set it up

Set the prices, in CHF, and restart keera-agents. The gateway needs nothing
more than the service key it already has. Its `docs/billing.md` explains what
it does with the lines.

```sh
KEERA_AGENTS_PRICE_CPU_HOUR=0.05
KEERA_AGENTS_PRICE_MEMORY_GIB_HOUR=0.006
KEERA_AGENTS_PRICE_DISK_GIB_MONTH=0.12
```

| Setting                              | What it costs                                         |
| ------------------------------------ | ----------------------------------------------------- |
| `KEERA_AGENTS_PRICE_CPU_HOUR`        | One CPU for an hour, while the machine runs           |
| `KEERA_AGENTS_PRICE_MEMORY_GIB_HOUR` | One GiB of memory for an hour, while the machine runs |
| `KEERA_AGENTS_PRICE_DISK_GIB_MONTH`  | One GiB of disk for a month, while the machine exists |

Each is from 0 to 1000. Unset or 0 is free. Any price above 0 turns billing
on. A price that is not a number in that range stops the start.

If the gateway cannot be reached, keera-agents keeps the metered time and
sends it once it can, up to 39 days back.

## What is metered

Machines are metered by their size in their settings, not by what they use:
that is what they hold on the node.

- **CPU and memory** count while the machine is running. A machine still
  starting, such as a pod waiting for a node, is not charged for them, nor is
  a suspended or stopped one.
- **Disk** counts while the machine exists, suspended and stopped too, since
  its workspace is kept. It stops when the agent or workspace is deleted. A
  machine never made, as when its organisation had no credit, has no disk to
  count.
- **A month** of disk is 730 hours, a year's twelfth, so every month costs
  the same.

Each minute, one replica meters every machine for the time since its last
round, into one row per machine and hour. A round that runs late loses
nothing. While no replica runs, at most 15 minutes are counted, since nobody
saw what ran.

## What the gateway gets

Once an hour is over, keera-agents sends it to the gateway: one line per
organisation and item (CPU, memory, disk), with the quantity and the price.
Each line has an id, such as `agents/org_x/2026-11-17T10:00:00Z/cpu`, and the
gateway records each id once. So a line sent twice, as after an answer that was
lost, is billed once.

The gateway's monthly report shows the lines as **Keera Agents** rows, next to
model use. When organisations pay in advance, each line is taken from the
credit, as a model call is.

The panel's **Billing** screen, for administrators, shows each machine's time
and cost this month, at today's prices, with a link to the gateway's Billing.
That is what was metered here. The gateway's bill is the record. keera-agents
keeps its own rows for 100 days.

The page of an agent or workspace shows its owner and administrators what
its machine cost this month, and what an hour of it running costs at its
size: CPU, memory and its share of the disk. The disk costs while it is
stopped too.

## When an organisation may not run machines

Before a machine starts or resumes, keera-agents asks the gateway whether its
organisation may run one. The gateway says no when:

| Code               | Means                                                                      |
| ------------------ | -------------------------------------------------------------------------- |
| `credit_exhausted` | It pays in advance and its credit is used up.                              |
| `charge_due`       | A recurring charge is due that no saved card can pay.                      |
| `org_limited`      | It signed itself up and is limited, on a gateway without payments by card. |

Then:

- **No machine starts or resumes.** A task waiting for one fails, with the
  gateway's reason, so whoever asked hears why. Starting an agent or opening a
  workspace answers `402` with the code.
- **Running machines are suspended** once idle: an agent after its task, a
  workspace at once. The workspaces' disks are kept, and are still billed.
- **Every screen of the panel says so**, with a link to the gateway's Billing
  for administrators.

The answer is kept for a minute, so a payment lets machines start within a
minute. When the gateway cannot be asked, its last answer counts. Without one,
the machine starts: its time is still metered and billed.

## Keep in mind

- **Prices apply when an hour is sent.** A changed price applies to every
  hour not yet sent, and the Billing screen shows past months at today's
  prices. The gateway's bill keeps what each line was billed at.
- **Credit overshoots a little.** An hour is billed once it is over, and
  running machines stop only when idle. So credit can go slightly below zero.
- **Back up the gateway's database.** It holds the bill. keera-agents' rows
  are for the Billing screen only.
