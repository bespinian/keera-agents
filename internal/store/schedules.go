package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Schedule struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	AgentName      string     `json:"agent_name"`
	Name           string     `json:"name"`
	Cron           string     `json:"cron"`
	Timezone       string     `json:"timezone"`
	Prompt         string     `json:"prompt"`
	ReplyBindingID *string    `json:"reply_binding_id"`
	Enabled        bool       `json:"enabled"`
	NextRunAt      *time.Time `json:"next_run_at"`
	LastRunAt      *time.Time `json:"last_run_at"`
	CreatedBy      string     `json:"created_by"`
	CreatedByID    string     `json:"-"`
	// RequestedBy is who last changed the schedule: it runs for them.
	RequestedBy   string    `json:"requested_by"`
	RequestedByID string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	OrgID         string    `json:"-"`
}

const scheduleSelect = `SELECT s.id, s.agent_id, a.name, s.name, s.cron, s.timezone, s.prompt,
	s.reply_binding_id, s.enabled, s.next_run_at, s.last_run_at, s.created_by, s.created_by_id,
	s.requested_by, s.requested_by_id, s.created_at, a.org_id
	FROM schedules s JOIN agents a ON a.id = s.agent_id AND a.deleted_at IS NULL`

func scanSchedule(row pgx.Row) (Schedule, error) {
	var s Schedule
	err := row.Scan(&s.ID, &s.AgentID, &s.AgentName, &s.Name, &s.Cron, &s.Timezone, &s.Prompt,
		&s.ReplyBindingID, &s.Enabled, &s.NextRunAt, &s.LastRunAt, &s.CreatedBy, &s.CreatedByID,
		&s.RequestedBy, &s.RequestedByID, &s.CreatedAt, &s.OrgID)
	return s, notFound(err)
}

func (s *Store) querySchedules(ctx context.Context, tail string, args ...any) ([]Schedule, error) {
	return queryAll(ctx, s, scanSchedule, scheduleSelect+` `+tail, args...)
}

func (s *Store) CreateSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	_, err := s.pool.Exec(ctx, `INSERT INTO schedules (id, agent_id, name, cron, timezone,
		prompt, reply_binding_id, enabled, next_run_at, created_by, created_by_id, requested_by,
		requested_by_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		sc.ID, sc.AgentID, sc.Name, sc.Cron, sc.Timezone, sc.Prompt, sc.ReplyBindingID,
		sc.Enabled, sc.NextRunAt, sc.CreatedBy, sc.CreatedByID, sc.RequestedBy, sc.RequestedByID)
	if err != nil {
		return Schedule{}, conflict(err)
	}
	return s.Schedule(ctx, sc.ID)
}

func (s *Store) UpdateSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	if err := one(s.pool.Exec(ctx, `UPDATE schedules SET name = $2, cron = $3, timezone = $4,
		prompt = $5, reply_binding_id = $6, enabled = $7, next_run_at = $8, requested_by = $9,
		requested_by_id = $10 WHERE id = $1`,
		sc.ID, sc.Name, sc.Cron, sc.Timezone, sc.Prompt, sc.ReplyBindingID, sc.Enabled,
		sc.NextRunAt, sc.RequestedBy, sc.RequestedByID)); err != nil {
		return Schedule{}, err
	}
	return s.Schedule(ctx, sc.ID)
}

func (s *Store) Schedule(ctx context.Context, id string) (Schedule, error) {
	return scanSchedule(s.pool.QueryRow(ctx, scheduleSelect+` WHERE s.id = $1`, id))
}

// Schedules lists an organisation's schedules, or one agent's.
func (s *Store) Schedules(ctx context.Context, orgID, agentID string) ([]Schedule, error) {
	return s.querySchedules(ctx, `WHERE a.org_id = $1 AND ($2 = '' OR s.agent_id = $2)
		ORDER BY s.next_run_at NULLS LAST, lower(s.name)`, orgID, agentID)
}

// DueSchedules are enabled schedules of enabled agents whose time has come.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	return s.querySchedules(ctx, `WHERE s.enabled AND a.enabled AND s.next_run_at <= $1
		ORDER BY s.next_run_at LIMIT 100`, now)
}

// Upcoming lists the next runs in an organisation.
func (s *Store) Upcoming(ctx context.Context, orgID string, limit int) ([]Schedule, error) {
	return s.querySchedules(ctx, `WHERE a.org_id = $1 AND s.enabled AND a.enabled
		AND s.next_run_at IS NOT NULL ORDER BY s.next_run_at LIMIT $2`, orgID, limit)
}

// ClaimRun moves a schedule from the run it was due for to the next, and
// reports whether this caller won. Only the winner starts the task, so
// several replicas run each schedule once.
func (s *Store) ClaimRun(ctx context.Context, id string, due time.Time, next *time.Time) (bool, error) {
	// A schedule that never runs again is switched off.
	tag, err := s.pool.Exec(ctx, `UPDATE schedules SET next_run_at = $3, last_run_at = now(),
		enabled = enabled AND $3::timestamptz IS NOT NULL
		WHERE id = $1 AND next_run_at = $2`, id, due, next)
	return tag.RowsAffected() == 1, err
}

func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM schedules WHERE id = $1`, id))
}
