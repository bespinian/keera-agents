-- keera-agents' state. People, organisations, projects, models and keys live
-- in Keera Gateway; rows here name them by the gateway's ids.

-- One agent: a persistent assistant with its own container, workspace and
-- gateway key.
CREATE TABLE agents (
    id            text PRIMARY KEY,
    org_id        text NOT NULL,
    -- The gateway project its key is in: whose budget, rate limit, system
    -- prompt and model allow-list apply.
    project_id    text NOT NULL DEFAULT '',
    -- The person it acts for. Its key's spend is reported under them, and
    -- it stops when they leave.
    owner_id      text NOT NULL,
    owner_email   text NOT NULL,
    name          text NOT NULL,
    -- The icon shown next to its name, by the panel's name for it. Empty is
    -- the default.
    icon          text NOT NULL DEFAULT '',
    description   text NOT NULL DEFAULT '',
    instructions  text NOT NULL DEFAULT '',
    -- A gateway alias: a model or a router.
    model         text NOT NULL,
    -- The runtime's builtin tools this agent may use.
    builtins      text[] NOT NULL DEFAULT '{}',
    max_steps     integer NOT NULL DEFAULT 30,
    -- Who may give it work from a chat: 'owner', 'org', or 'people' (listed
    -- in allowed_emails).
    access        text NOT NULL DEFAULT 'org' CHECK (access IN ('owner', 'org', 'people')),
    allowed_emails text[] NOT NULL DEFAULT '{}',
    -- A monthly budget on its own key, in micro-CHF. 0 is none; the project's
    -- and organisation's budgets apply either way.
    budget_micros bigint NOT NULL DEFAULT 0,
    image         text NOT NULL DEFAULT '',
    class         jsonb NOT NULL DEFAULT '{}',
    -- What its container is doing, as last seen: stopped, starting, running,
    -- suspended, failed.
    state         text NOT NULL DEFAULT 'stopped',
    state_detail  text NOT NULL DEFAULT '',
    -- Whether it should run. Stopped agents take no tasks.
    enabled       boolean NOT NULL DEFAULT true,
    gateway_key_id text NOT NULL DEFAULT '',
    -- The key's secret, sealed with KEERA_AGENTS_SECRET_KEY under the agent's
    -- id. The runtime fetches it at start, so it is never in a pod spec.
    gateway_key   bytea,
    -- SHA-256 of the runtime token the container authenticates with.
    runtime_token_hash bytea UNIQUE,
    -- A workspace is a machine an engineer works in, with a coding agent. It
    -- is an agents row of its own kind, so it shares keys, connectors and the
    -- lifecycle, but it never takes tasks.
    kind          text NOT NULL DEFAULT 'agent' CHECK (kind IN ('agent', 'workspace')),
    -- What the engineer opens: 'terminal' (pi) or 'vscode' (code-server). Only
    -- a workspace has one.
    experience    text NOT NULL DEFAULT '' CHECK (experience IN ('', 'terminal', 'vscode')),
    last_active_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    CONSTRAINT agents_kind_experience_check CHECK ((kind = 'workspace') = (experience <> ''))
);
CREATE INDEX agents_org_idx ON agents (org_id) WHERE deleted_at IS NULL;
-- An agent's name is unique in its organisation, since a chat mentions it. A
-- workspace's is unique for its owner, who reaches it by name.
CREATE UNIQUE INDEX agents_org_name_idx ON agents (org_id, lower(name))
    WHERE deleted_at IS NULL AND kind = 'agent';
CREATE UNIQUE INDEX workspaces_owner_name_idx ON agents (org_id, owner_id, lower(name))
    WHERE deleted_at IS NULL AND kind = 'workspace';

-- One organisation's credentials for one service, such as its Slack app.
CREATE TABLE connections (
    id         text PRIMARY KEY,
    org_id     text NOT NULL,
    kind       text NOT NULL,
    name       text NOT NULL,
    config     jsonb NOT NULL DEFAULT '{}',
    -- The secret fields, as JSON sealed under the connection's id.
    secrets    bytea,
    -- Whether each person signs in to the service with their own account, so
    -- agents work with that person's permissions instead of a shared one's.
    per_user   boolean NOT NULL DEFAULT false,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX connections_org_idx ON connections (org_id);

-- Each person's sign-in to a connector that has per_user.
CREATE TABLE accounts (
    connection_id text NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    user_id       text NOT NULL,
    -- Who they signed in as at the service, such as their Google email.
    account       text NOT NULL DEFAULT '',
    -- The refresh token, sealed under the connection and the person.
    token         bytea NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, user_id)
);
CREATE INDEX accounts_user_idx ON accounts (user_id);

-- Where an agent listens or works: a channel of a chat connection, a folder
-- of a file connection, or a repository of a code connection.
CREATE TABLE bindings (
    id            text PRIMARY KEY,
    agent_id      text NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    connection_id text NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    -- A chat: a channel, space or chat id; '*' takes every conversation the
    -- bot is in, direct messages included. A file store: a folder id.
    target        text NOT NULL,
    label         text NOT NULL DEFAULT '',
    -- 'chat' takes messages; 'watch' turns new files into tasks; 'files'
    -- only lets the agent use the folder; 'repo' lets it work in a
    -- repository on GitHub or GitLab, named as owner/name or group/project;
    -- 'read' and 'write' let it use an API connector, such as Jira or AWS.
    mode          text NOT NULL
        CHECK (mode IN ('chat', 'watch', 'files', 'repo', 'read', 'write')),
    -- For a watch: what to do with each new file.
    prompt        text NOT NULL DEFAULT '',
    -- For a watch: where the store's change feed was last read.
    cursor        text NOT NULL DEFAULT '',
    checked_at    timestamptz,
    created_by    text NOT NULL,
    created_by_id text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_id, connection_id, target, mode)
);
CREATE INDEX bindings_connection_idx ON bindings (connection_id);

-- Files agents wrote through a connection, as the service reported them
-- after the write. A folder watch skips the writer's own change, so an agent
-- that writes to a folder it watches does not set itself off. Another agent
-- watching the folder, or a person's later edit, still starts a task.
CREATE TABLE written_files (
    agent_id      text NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    connection_id text NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    file_id       text NOT NULL,
    modified      timestamptz NOT NULL,
    written_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, connection_id, file_id)
);

-- A recurring task.
CREATE TABLE schedules (
    id          text PRIMARY KEY,
    agent_id    text NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    name        text NOT NULL,
    -- Five fields, as cron: minute hour day-of-month month day-of-week.
    cron        text NOT NULL,
    timezone    text NOT NULL DEFAULT 'Europe/Zurich',
    prompt      text NOT NULL,
    -- Where the result goes: a chat binding, or nowhere but the task log.
    reply_binding_id text REFERENCES bindings (id) ON DELETE SET NULL,
    enabled     boolean NOT NULL DEFAULT true,
    -- Claimed with a compare-and-set, so several replicas run it once.
    next_run_at timestamptz,
    last_run_at timestamptz,
    created_by  text NOT NULL,
    -- Whose rights it runs with: whoever last gave it, not whoever created
    -- it. Otherwise anyone could edit another person's schedule and have it
    -- run with that person's rights.
    requested_by    text NOT NULL DEFAULT '',
    requested_by_id text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX schedules_due_idx ON schedules (next_run_at) WHERE enabled;

CREATE TABLE boards (
    id         text PRIMARY KEY,
    org_id     text NOT NULL,
    name       text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX boards_org_idx ON boards (org_id);

CREATE TABLE cards (
    id          text PRIMARY KEY,
    board_id    text NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    title       text NOT NULL,
    body        text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'backlog'
        CHECK (status IN ('backlog', 'todo', 'in_progress', 'review', 'done')),
    -- Sorts cards within a column; gaps leave room to move one between two.
    position    double precision NOT NULL DEFAULT 0,
    -- An agent assigned to a card in 'todo' picks it up.
    agent_id    text REFERENCES agents (id) ON DELETE SET NULL,
    labels      text[] NOT NULL DEFAULT '{}',
    due_at      timestamptz,
    task_id     text,
    created_by  text NOT NULL,
    -- Whose rights it runs with, as for a schedule.
    requested_by    text NOT NULL DEFAULT '',
    requested_by_id text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cards_board_idx ON cards (board_id, status, position);
CREATE INDEX cards_pickup_idx ON cards (agent_id) WHERE status = 'todo';

CREATE TABLE card_comments (
    id         text PRIMARY KEY,
    card_id    text NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
    -- A person's email, or the agent's name for an agent.
    author     text NOT NULL,
    agent_id   text,
    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX card_comments_card_idx ON card_comments (card_id, created_at);

-- One piece of work for an agent, from a chat, a schedule, a card, a file or
-- a person in the panel.
CREATE TABLE tasks (
    id            text PRIMARY KEY,
    org_id        text NOT NULL,
    agent_id      text NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    source        text NOT NULL,
    -- Where it came from and where the result goes, as the source needs.
    origin        jsonb NOT NULL DEFAULT '{}',
    -- Redelivered webhooks and replayed changes are dropped on this.
    dedupe_key    text,
    conversation  text NOT NULL DEFAULT '',
    prompt        text NOT NULL,
    context       text NOT NULL DEFAULT '',
    requested_by  text NOT NULL DEFAULT '',
    requested_by_id text NOT NULL DEFAULT '',
    state         text NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    result        text NOT NULL DEFAULT '',
    error         text NOT NULL DEFAULT '',
    -- Whether the result went back to where the task came from.
    delivered     boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz,
    deadline      timestamptz
);
CREATE UNIQUE INDEX tasks_dedupe_idx ON tasks (agent_id, dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX tasks_agent_idx ON tasks (agent_id, created_at DESC);
CREATE INDEX tasks_org_idx ON tasks (org_id, created_at DESC);
-- /metrics counts running tasks at every scrape. Without this, that count
-- reads every task kept.
CREATE INDEX tasks_running_idx ON tasks (agent_id) WHERE state = 'running';
CREATE INDEX tasks_queue_idx ON tasks (agent_id, created_at) WHERE state = 'queued';
CREATE INDEX tasks_undelivered_idx ON tasks (finished_at) WHERE NOT delivered AND state IN ('succeeded', 'failed', 'cancelled');

CREATE TABLE task_events (
    id      bigserial PRIMARY KEY,
    task_id text NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    kind    text NOT NULL,
    text    text NOT NULL,
    data    jsonb,
    at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX task_events_task_idx ON task_events (task_id, id);

-- Who changed what, in keera-agents. Inference is in the gateway's log.
CREATE TABLE audit (
    id        bigserial PRIMARY KEY,
    at        timestamptz NOT NULL DEFAULT now(),
    org_id    text NOT NULL,
    actor     text NOT NULL,
    action    text NOT NULL,
    target    text NOT NULL DEFAULT '',
    detail    jsonb
);
-- Read newest first by id, a page at a time.
CREATE INDEX audit_org_id_idx ON audit (org_id, id DESC);

-- The public keys a person signs in to their workspaces with over SSH.
CREATE TABLE ssh_keys (
    id          text PRIMARY KEY,
    org_id      text NOT NULL,
    user_id     text NOT NULL,
    name        text NOT NULL DEFAULT '',
    -- One line of authorized_keys: type, key and comment.
    public_key  text NOT NULL,
    -- SHA256:..., as ssh-keygen -l prints it.
    fingerprint text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, user_id, fingerprint)
);

-- Machine time, for billing: what each agent's and workspace's machine used
-- in one hour. CPU and memory count while the machine runs, disk while it
-- exists. Kept while billing is on (see docs/billing.md). The gateway's bill
-- is the record; these rows say which machine used what.
CREATE TABLE machine_usage (
    agent_id           text NOT NULL,
    org_id             text NOT NULL,
    -- The hour the time was metered in, in UTC.
    hour               timestamptz NOT NULL,
    -- Millicores, and MiB, times the seconds they ran or existed.
    cpu_milli_seconds  bigint NOT NULL DEFAULT 0,
    memory_mib_seconds bigint NOT NULL DEFAULT 0,
    disk_mib_seconds   bigint NOT NULL DEFAULT 0,
    -- When the hour went to the gateway's bill. Null until then.
    billed_at          timestamptz,
    PRIMARY KEY (agent_id, hour)
);
CREATE INDEX machine_usage_unbilled_idx ON machine_usage (hour) WHERE billed_at IS NULL;
CREATE INDEX machine_usage_org_hour_idx ON machine_usage (org_id, hour);

-- How far machine time has been metered. One row. Each round meters the time
-- since then, so a round that runs late, or on another replica, neither
-- skips time nor counts it twice.
CREATE TABLE metering (
    id            boolean PRIMARY KEY DEFAULT true CHECK (id),
    metered_until timestamptz NOT NULL
);
