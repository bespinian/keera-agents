-- What an agent's gateway key carries: whose it is, its project and its
-- budget. A key that differs from the agent's settings is replaced in the
-- background, so a change that could not reach the gateway at once still
-- does.
ALTER TABLE agents
    ADD COLUMN key_owner_id text NOT NULL DEFAULT '',
    ADD COLUMN key_project_id text NOT NULL DEFAULT '',
    ADD COLUMN key_budget_micros bigint NOT NULL DEFAULT 0;

-- Keys made before this release are taken to match.
UPDATE agents SET key_owner_id = owner_id, key_project_id = project_id,
    key_budget_micros = budget_micros
    WHERE gateway_key_id <> '';

-- When a result that could not be sent is tried again. Until then, others
-- go first, so one chat that cannot be reached does not hold up the rest.
ALTER TABLE tasks ADD COLUMN deliver_after timestamptz;

-- Whether the agent's machine, and so its disk, exists. Only an existing
-- disk is billed: a machine never started, as when its organisation had no
-- credit, has none. Reconcile keeps it in line with the runtime.
ALTER TABLE agents ADD COLUMN has_disk boolean NOT NULL DEFAULT false;
UPDATE agents SET has_disk = true WHERE deleted_at IS NULL;
