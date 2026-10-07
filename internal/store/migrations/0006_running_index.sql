-- /metrics counts running tasks at every scrape. Without this, that count
-- reads every task kept.
CREATE INDEX tasks_running_idx ON tasks (agent_id) WHERE state = 'running';
