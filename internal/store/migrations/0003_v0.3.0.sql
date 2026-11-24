-- What v0.3.0 changes in the schema.

ALTER TABLE agents
    -- Other models of the gateway an agent may use as tools: one to turn
    -- audio into text, and chat models it may ask. See docs/agents.md.
    ADD COLUMN transcription_model text NOT NULL DEFAULT '',
    ADD COLUMN ask_models text[] NOT NULL DEFAULT '{}',
    -- The gateway's MCP servers an agent may call. NULL is every one its
    -- project allows. Its key carries them, so key_mcp_servers is what it
    -- was minted with, as for the budget. See docs/agents.md.
    ADD COLUMN mcp_servers text[],
    ADD COLUMN key_mcp_servers text[],
    -- The owner's name, for the panel. Empty until set; the email stands in.
    ADD COLUMN owner_name text NOT NULL DEFAULT '',
    -- Runs a machine started since the last metering round. A run that
    -- starts and stops between two rounds is billed a minute through it. See
    -- Meter.
    ADD COLUMN unmetered_runs bigint NOT NULL DEFAULT 0;

-- Boards become squads: agents that share a board and hand each other work
-- on it. See docs/tasks.md.
ALTER TABLE boards RENAME TO squads;
ALTER INDEX boards_pkey RENAME TO squads_pkey;
ALTER INDEX boards_org_idx RENAME TO squads_org_idx;
ALTER TABLE squads
    ADD COLUMN goal text NOT NULL DEFAULT '',
    -- Takes the cards put in To do without an agent.
    ADD COLUMN lead_id text REFERENCES agents (id) ON DELETE SET NULL;

CREATE TABLE squad_members (
    squad_id   text NOT NULL REFERENCES squads (id) ON DELETE CASCADE,
    agent_id   text NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (squad_id, agent_id)
);
CREATE INDEX squad_members_agent_idx ON squad_members (agent_id);

ALTER TABLE cards RENAME COLUMN board_id TO squad_id;
ALTER TABLE cards RENAME CONSTRAINT cards_board_id_fkey TO cards_squad_id_fkey;
ALTER INDEX cards_board_idx RENAME TO cards_squad_idx;
ALTER TABLE cards
    -- The card whose agent handed this one out, and how many hands it went
    -- through: deep chains are refused, so agents cannot hand work around
    -- forever.
    ADD COLUMN parent_id text REFERENCES cards (id) ON DELETE SET NULL,
    ADD COLUMN depth int NOT NULL DEFAULT 0,
    -- The card's agent finished and waits for the cards it handed out. The
    -- card goes back to To do once they are finished.
    ADD COLUMN waiting boolean NOT NULL DEFAULT false;
CREATE INDEX cards_parent_idx ON cards (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX cards_waiting_idx ON cards (id) WHERE waiting;

-- The audit log names them squads too, so old entries read as new ones.
UPDATE audit SET action = 'squad.' || substring(action FROM 7) WHERE action LIKE 'board.%';

-- Every agent with a card on a board is a member of its squad, so nothing
-- assigned stops being picked up.
INSERT INTO squad_members (squad_id, agent_id)
SELECT DISTINCT c.squad_id, c.agent_id FROM cards c
JOIN agents a ON a.id = c.agent_id AND a.deleted_at IS NULL;
