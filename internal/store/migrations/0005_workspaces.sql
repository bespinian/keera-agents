-- A workspace is a machine an engineer works in, with a coding agent. It is
-- an agents row of its own kind, so it shares keys, connectors and the
-- lifecycle, but it never takes tasks.
ALTER TABLE agents ADD COLUMN kind text NOT NULL DEFAULT 'agent'
    CHECK (kind IN ('agent', 'workspace'));
-- What the engineer opens: 'terminal' (pi) or 'vscode' (code-server). Only a
-- workspace has one.
ALTER TABLE agents ADD COLUMN experience text NOT NULL DEFAULT ''
    CHECK (experience IN ('', 'terminal', 'vscode'));
ALTER TABLE agents ADD CONSTRAINT agents_kind_experience_check
    CHECK ((kind = 'workspace') = (experience <> ''));

-- An agent's name is unique in its organisation, since a chat mentions it. A
-- workspace's is unique for its owner, who reaches it by name.
DROP INDEX agents_org_name_idx;
CREATE UNIQUE INDEX agents_org_name_idx ON agents (org_id, lower(name))
    WHERE deleted_at IS NULL AND kind = 'agent';
CREATE UNIQUE INDEX workspaces_owner_name_idx ON agents (org_id, owner_id, lower(name))
    WHERE deleted_at IS NULL AND kind = 'workspace';

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
