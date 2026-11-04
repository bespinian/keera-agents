package store

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Task states.
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)

// Origin is where a task came from and where its result goes. Only the
// fields of its source are set.
type Origin struct {
	// Label is what the panel shows, such as "Slack #sales".
	Label string `json:"label,omitempty"`
	// A chat message: the connection and the place to reply.
	ConnectionID string            `json:"connection_id,omitempty"`
	Conversation string            `json:"conversation,omitempty"`
	Thread       string            `json:"thread,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
	// A schedule, and the chat binding its result goes to.
	ScheduleID     string `json:"schedule_id,omitempty"`
	ReplyBindingID string `json:"reply_binding_id,omitempty"`
	// A card.
	CardID string `json:"card_id,omitempty"`
	// A file in a watched folder.
	BindingID string `json:"binding_id,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

// KeeraChat labels a chat task from Keera Chat. It has no connection:
// Keera Chat reads the result itself.
const KeeraChat = "Keera Chat"

type Task struct {
	ID        string `json:"id"`
	OrgID     string `json:"-"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	AgentIcon string `json:"agent_icon"`
	// AgentOwnerID is empty once the agent is deleted: nobody owns it then.
	AgentOwnerID  string     `json:"-"`
	Source        string     `json:"source"`
	Origin        Origin     `json:"-"`
	OriginLabel   string     `json:"origin_label"`
	DedupeKey     *string    `json:"-"`
	Conversation  string     `json:"-"`
	Prompt        string     `json:"prompt"`
	Context       string     `json:"context"`
	RequestedBy   string     `json:"requested_by"`
	RequestedByID string     `json:"-"`
	State         string     `json:"state"`
	Result        string     `json:"result"`
	Error         string     `json:"error"`
	Delivered     bool       `json:"delivered"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	Deadline      *time.Time `json:"-"`
}

func (t Task) Finished() bool {
	return t.State == TaskSucceeded || t.State == TaskFailed || t.State == TaskCancelled
}

const taskSelect = `SELECT t.id, t.org_id, t.agent_id, coalesce(a.name, ''), coalesce(a.icon, ''),
	coalesce(CASE WHEN a.deleted_at IS NULL THEN a.owner_id END, ''), t.source, t.origin,
	t.dedupe_key, t.conversation, t.prompt, t.context, t.requested_by, t.requested_by_id, t.state,
	t.result, t.error, t.delivered, t.created_at, t.started_at, t.finished_at, t.deadline
	FROM tasks t LEFT JOIN agents a ON a.id = t.agent_id`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	var origin []byte
	err := row.Scan(&t.ID, &t.OrgID, &t.AgentID, &t.AgentName, &t.AgentIcon, &t.AgentOwnerID, &t.Source,
		&origin, &t.DedupeKey,
		&t.Conversation, &t.Prompt, &t.Context, &t.RequestedBy, &t.RequestedByID, &t.State,
		&t.Result, &t.Error, &t.Delivered, &t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.Deadline)
	if err != nil {
		return t, notFound(err)
	}
	_ = json.Unmarshal(origin, &t.Origin)
	t.OriginLabel = t.Origin.Label
	return t, nil
}

func (s *Store) queryTasks(ctx context.Context, tail string, args ...any) ([]Task, error) {
	return queryAll(ctx, s, scanTask, taskSelect+` `+tail, args...)
}

// CreateTask queues a task. A task with a dedupe key the agent has seen
// before returns ErrConflict, so a redelivered webhook does nothing.
func (s *Store) CreateTask(ctx context.Context, t Task) (Task, error) {
	origin, _ := json.Marshal(t.Origin)
	_, err := s.pool.Exec(ctx, `INSERT INTO tasks (id, org_id, agent_id, source, origin,
		dedupe_key, conversation, prompt, context, requested_by, requested_by_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		t.ID, t.OrgID, t.AgentID, t.Source, origin, t.DedupeKey, t.Conversation, t.Prompt,
		t.Context, t.RequestedBy, t.RequestedByID)
	if err != nil {
		return Task{}, conflict(err)
	}
	return s.Task(ctx, t.ID)
}

func (s *Store) Task(ctx context.Context, id string) (Task, error) {
	return scanTask(s.pool.QueryRow(ctx, taskSelect+` WHERE t.id = $1`, id))
}

// TaskQuery narrows a task list. Empty fields do not narrow.
type TaskQuery struct {
	OrgID   string
	AgentID string
	State   string
	Source  string
	Before  *time.Time
	// Limit is 50 when unset, and at most 200.
	Limit int
	// ViewerID, when set, keeps the tasks a member may see: those they asked
	// for, by id or by ViewerEmail, and those of agents they own.
	ViewerID    string
	ViewerEmail string
}

func (s *Store) Tasks(ctx context.Context, q TaskQuery) ([]Task, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	return s.queryTasks(ctx, `WHERE t.org_id = $1 AND ($2 = '' OR t.agent_id = $2)
		AND ($3 = '' OR t.state = $3) AND ($4 = '' OR t.source = $4)
		AND ($5::timestamptz IS NULL OR t.created_at < $5)
		AND ($7 = '' OR t.requested_by_id = $7 OR t.requested_by = $8
			OR (a.owner_id = $7 AND a.deleted_at IS NULL))
		ORDER BY t.created_at DESC LIMIT $6`,
		q.OrgID, q.AgentID, q.State, q.Source, q.Before, q.Limit, q.ViewerID, q.ViewerEmail)
}

// TaskFacets are the agents, states and sources a task list has tasks of,
// so the panel offers only filters that find something.
type TaskFacets struct {
	Agents  []TaskAgent `json:"agents"`
	States  []string    `json:"states"`
	Sources []string    `json:"sources"`
}

// TaskAgent names an agent that has tasks, deleted or not.
type TaskAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TaskFacets reads the facets of the tasks q would list, all pages of them.
// Only OrgID, AgentID and the viewer narrow them.
func (s *Store) TaskFacets(ctx context.Context, q TaskQuery) (TaskFacets, error) {
	f := TaskFacets{Agents: []TaskAgent{}, States: []string{}, Sources: []string{}}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT t.agent_id, coalesce(a.name, ''), t.state, t.source
		FROM tasks t LEFT JOIN agents a ON a.id = t.agent_id
		WHERE t.org_id = $1 AND ($2 = '' OR t.agent_id = $2)
		AND ($3 = '' OR t.requested_by_id = $3 OR t.requested_by = $4
			OR (a.owner_id = $3 AND a.deleted_at IS NULL))`,
		q.OrgID, q.AgentID, q.ViewerID, q.ViewerEmail)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var agent TaskAgent
		var state, source string
		if err := rows.Scan(&agent.ID, &agent.Name, &state, &source); err != nil {
			return f, err
		}
		if !slices.ContainsFunc(f.Agents, func(a TaskAgent) bool { return a.ID == agent.ID }) {
			f.Agents = append(f.Agents, agent)
		}
		if !slices.Contains(f.States, state) {
			f.States = append(f.States, state)
		}
		if !slices.Contains(f.Sources, source) {
			f.Sources = append(f.Sources, source)
		}
	}
	return f, rows.Err()
}

// ClaimTask starts an agent's oldest queued task, if it has none running.
// One task runs at a time per agent: its container is one machine with one
// workspace.
func (s *Store) ClaimTask(ctx context.Context, agentID string, timeout time.Duration) (Task, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialises claims per agent across replicas.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM agents WHERE id = $1 FOR UPDATE`, agentID); err != nil {
			return err
		}
		var running bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks
			WHERE agent_id = $1 AND state = 'running')`, agentID).Scan(&running); err != nil {
			return err
		}
		if running {
			return pgx.ErrNoRows
		}
		return tx.QueryRow(ctx, `UPDATE tasks SET state = 'running', started_at = now(),
			deadline = now() + $2::interval
			WHERE id = (SELECT id FROM tasks WHERE agent_id = $1 AND state = 'queued'
				ORDER BY created_at LIMIT 1)
			RETURNING id`, agentID, timeout.String()).Scan(&id)
	})
	if err != nil {
		return Task{}, notFound(err)
	}
	return s.Task(ctx, id)
}

// RunningTask is the task an agent works on now, so a runtime that
// restarted mid-task picks it up again instead of leaving it hanging.
func (s *Store) RunningTask(ctx context.Context, agentID string) (Task, error) {
	return scanTask(s.pool.QueryRow(ctx, taskSelect+` WHERE t.agent_id = $1 AND t.state = 'running'
		LIMIT 1`, agentID))
}

// FinishTask ends a running task. It reports false when the task was no
// longer running, such as one cancelled meanwhile.
func (s *Store) FinishTask(ctx context.Context, id, state, result, errText string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE tasks SET state = $2, result = $3, error = $4,
		finished_at = now() WHERE id = $1 AND state IN ('queued', 'running')`,
		id, state, result, errText)
	return tag.RowsAffected() == 1, err
}

// ExpireTasks fails running tasks past their deadline.
func (s *Store) ExpireTasks(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `UPDATE tasks SET state = 'failed', finished_at = now(),
		error = 'the task took longer than it may; ask again, or split it into smaller tasks'
		WHERE state = 'running' AND deadline < now() RETURNING id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// FailQueuedTasks fails an agent's tasks queued before a time, as when its
// container could not start for that long. It returns how many it failed.
func (s *Store) FailQueuedTasks(ctx context.Context, agentID string, before time.Time, reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE tasks SET state = 'failed', error = $3, finished_at = now()
		WHERE agent_id = $1 AND state = 'queued' AND created_at < $2`, agentID, before, reason)
	return tag.RowsAffected(), err
}

// Undelivered are finished tasks whose result has not gone back yet.
func (s *Store) Undelivered(ctx context.Context) ([]Task, error) {
	return s.queryTasks(ctx, `WHERE NOT t.delivered AND t.state IN ('succeeded', 'failed', 'cancelled')
		ORDER BY t.finished_at LIMIT 100`)
}

// MarkDelivered claims a delivery; only one replica gets true.
func (s *Store) MarkDelivered(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE tasks SET delivered = true WHERE id = $1 AND NOT delivered`, id)
	return tag.RowsAffected() == 1, err
}

// UnmarkDelivered hands a claimed delivery back, for one that failed before
// anything went out.
func (s *Store) UnmarkDelivered(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE tasks SET delivered = false WHERE id = $1`, id)
	return err
}

// Busy reports whether an agent has queued or running tasks.
func (s *Store) Busy(ctx context.Context, agentID string) (bool, error) {
	var busy bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE agent_id = $1
		AND state IN ('queued', 'running'))`, agentID).Scan(&busy)
	return busy, err
}

// CancelAgentTasks cancels everything an agent has not finished, as when it
// is stopped or deleted.
func (s *Store) CancelAgentTasks(ctx context.Context, agentID, reason string) error {
	_, err := s.pool.Exec(ctx, `UPDATE tasks SET state = 'cancelled', error = $2, finished_at = now()
		WHERE agent_id = $1 AND state IN ('queued', 'running')`, agentID, reason)
	return err
}

type TaskCounts struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// TaskCounts counts an organisation's tasks since a time, or one agent's when
// agentID is set.
func (s *Store) TaskCounts(ctx context.Context, orgID, agentID string, since time.Time) (TaskCounts, error) {
	var c TaskCounts
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE state = 'queued'), count(*) FILTER (WHERE state = 'running'),
		count(*) FILTER (WHERE state = 'succeeded'), count(*) FILTER (WHERE state = 'failed')
		FROM tasks WHERE org_id = $1 AND ($2 = '' OR agent_id = $2)
		AND (created_at > $3 OR state IN ('queued', 'running'))`,
		orgID, agentID, since).Scan(&c.Queued, &c.Running, &c.Succeeded, &c.Failed)
	return c, err
}

// TaskPoint is how many tasks came in during one hour or day, and how many of
// them failed.
type TaskPoint struct {
	At     time.Time `json:"at"`
	Tasks  int       `json:"tasks"`
	Failed int       `json:"failed"`
}

// TaskSeries counts an organisation's tasks, or one agent's when agentID is
// set, per hour or per day since a time, with hours and days read in loc.
// Empty ones are there too, as zeroes, so a chart has no gaps.
func (s *Store) TaskSeries(ctx context.Context, orgID, agentID string, since time.Time, daily bool, loc *time.Location) ([]TaskPoint, error) {
	unit := "hour"
	since = since.In(loc)
	start := time.Date(since.Year(), since.Month(), since.Day(), since.Hour(), 0, 0, 0, loc)
	next := func(t time.Time) time.Time { return t.Add(time.Hour) }
	if daily {
		unit = "day"
		start = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, loc)
		next = func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc) }
	}
	rows, err := s.pool.Query(ctx, `SELECT date_trunc($2, created_at, $3), count(*),
		count(*) FILTER (WHERE state = 'failed')
		FROM tasks WHERE org_id = $1 AND ($5 = '' OR agent_id = $5) AND created_at >= $4 GROUP BY 1`,
		orgID, unit, loc.String(), start, agentID)
	if err != nil {
		return nil, err
	}
	counted := map[int64]TaskPoint{}
	var row TaskPoint
	if _, err := pgx.ForEachRow(rows, []any{&row.At, &row.Tasks, &row.Failed}, func() error {
		counted[row.At.Unix()] = row
		return nil
	}); err != nil {
		return nil, err
	}
	var out []TaskPoint
	for t, now := start, time.Now(); !t.After(now); t = next(t) {
		p := counted[t.Unix()]
		p.At = t
		out = append(out, p)
	}
	return out, nil
}

type Event struct {
	ID   int64           `json:"id"`
	Kind string          `json:"kind"`
	Text string          `json:"text"`
	Data json.RawMessage `json:"data,omitempty"`
	At   time.Time       `json:"at"`
}

func (s *Store) AddEvents(ctx context.Context, taskID string, events []Event) error {
	batch := &pgx.Batch{}
	for _, e := range events {
		at := e.At
		if at.IsZero() {
			at = time.Now()
		}
		var data any
		if len(e.Data) > 0 && json.Valid(e.Data) {
			data = []byte(e.Data)
		}
		batch.Queue(`INSERT INTO task_events (task_id, kind, text, data, at) VALUES ($1,$2,$3,$4,$5)`,
			taskID, e.Kind, e.Text, data, at)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

func (s *Store) Events(ctx context.Context, taskID string, after int64) ([]Event, error) {
	return queryAll(ctx, s, func(row pgx.Row) (Event, error) {
		var e Event
		var data []byte
		err := row.Scan(&e.ID, &e.Kind, &e.Text, &data, &e.At)
		e.Data = data
		return e, err
	}, `SELECT id, kind, text, data, at FROM task_events
		WHERE task_id = $1 AND id > $2 ORDER BY id LIMIT 1000`, taskID, after)
}

// PruneTasks deletes finished tasks older than the retention, with their
// events.
func (s *Store) PruneTasks(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tasks WHERE finished_at < $1`, before)
	return tag.RowsAffected(), err
}
