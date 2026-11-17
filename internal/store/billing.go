package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MachineUse is what one machine used over a stretch of time: millicores
// and MiB, times seconds. See machine_usage in the schema.
type MachineUse struct {
	CPUMilliSeconds  int64 `json:"cpu_milli_seconds"`
	MemoryMiBSeconds int64 `json:"memory_mib_seconds"`
	DiskMiBSeconds   int64 `json:"disk_mib_seconds"`
}

func (u MachineUse) Zero() bool { return u == MachineUse{} }

// Meter adds the machine time since the last round to each machine's hour,
// and moves the mark to now. measure says what one machine used in so many
// seconds. The first round only sets the mark. A gap longer than maxGap,
// such as while no replica ran, counts as maxGap: nobody saw what ran then.
// It reports how many seconds it metered.
func (s *Store) Meter(ctx context.Context, now time.Time, maxGap time.Duration,
	measure func(a Agent, seconds int64) MachineUse,
) (int64, error) {
	var seconds int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var from time.Time
		err := tx.QueryRow(ctx, `SELECT metered_until FROM metering FOR UPDATE`).Scan(&from)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `INSERT INTO metering (metered_until) VALUES ($1)
				ON CONFLICT (id) DO NOTHING`, now)
			return err
		}
		if err != nil {
			return err
		}
		seconds = int64(min(now.Sub(from), maxGap) / time.Second)
		if seconds <= 0 {
			// Less than a second, or a clock behind the mark: leave the
			// mark, so no time is lost or counted twice.
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT `+agentColumns+` FROM agents WHERE deleted_at IS NULL`)
		if err != nil {
			return err
		}
		agents, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
		if err != nil {
			return err
		}
		// The time goes to the hour it started in, so an hour is complete
		// once the mark has passed its end. See UnbilledHours.
		hour := from.UTC().Truncate(time.Hour)
		batch := &pgx.Batch{}
		for _, a := range agents {
			u := measure(a, seconds)
			if u.Zero() {
				continue
			}
			batch.Queue(`INSERT INTO machine_usage (agent_id, org_id, hour,
				cpu_milli_seconds, memory_mib_seconds, disk_mib_seconds)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (agent_id, hour) DO UPDATE SET
					cpu_milli_seconds = machine_usage.cpu_milli_seconds + excluded.cpu_milli_seconds,
					memory_mib_seconds = machine_usage.memory_mib_seconds + excluded.memory_mib_seconds,
					disk_mib_seconds = machine_usage.disk_mib_seconds + excluded.disk_mib_seconds`,
				a.ID, a.OrgID, hour, u.CPUMilliSeconds, u.MemoryMiBSeconds, u.DiskMiBSeconds)
		}
		batch.Queue(`UPDATE metering SET metered_until = $1`, now)
		return tx.SendBatch(ctx, batch).Close()
	})
	if err != nil {
		return 0, err
	}
	return seconds, nil
}

// OrgHour is one organisation's machine time in one hour, summed over its
// machines.
type OrgHour struct {
	OrgID string
	Hour  time.Time
	MachineUse
}

// UnbilledHours lists complete hours not yet on the gateway's bill, oldest
// first. An hour is complete once metering has passed its end. Hours before
// since are left out: the gateway no longer takes them.
func (s *Store) UnbilledHours(ctx context.Context, since time.Time, limit int) ([]OrgHour, error) {
	return queryAll(ctx, s, func(r pgx.Row) (OrgHour, error) {
		var h OrgHour
		err := r.Scan(&h.OrgID, &h.Hour, &h.CPUMilliSeconds, &h.MemoryMiBSeconds, &h.DiskMiBSeconds)
		return h, err
	}, `SELECT org_id, hour, sum(cpu_milli_seconds)::bigint, sum(memory_mib_seconds)::bigint,
		       sum(disk_mib_seconds)::bigint
		FROM machine_usage
		WHERE billed_at IS NULL AND hour >= $1
		  AND hour + interval '1 hour' <= (SELECT metered_until FROM metering)
		GROUP BY org_id, hour ORDER BY hour, org_id LIMIT $2`, since, limit)
}

// MarkBilled records that these hours are on the gateway's bill.
func (s *Store) MarkBilled(ctx context.Context, hours []OrgHour) error {
	orgs := make([]string, len(hours))
	starts := make([]time.Time, len(hours))
	for i, h := range hours {
		orgs[i], starts[i] = h.OrgID, h.Hour
	}
	_, err := s.pool.Exec(ctx, `UPDATE machine_usage SET billed_at = now()
		WHERE billed_at IS NULL
		  AND (org_id, hour) IN (SELECT * FROM unnest($1::text[], $2::timestamptz[]))`, orgs, starts)
	return err
}

// MachineMonth is one machine's time in a window, for the panel.
type MachineMonth struct {
	AgentID    string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	OwnerEmail string `json:"owner_email"`
	Deleted    bool   `json:"deleted"`
	MachineUse
}

// MachineUsage sums each machine's time in [from, to) in an organisation,
// deleted machines too.
func (s *Store) MachineUsage(ctx context.Context, orgID string, from, to time.Time) ([]MachineMonth, error) {
	return queryAll(ctx, s, func(r pgx.Row) (MachineMonth, error) {
		var m MachineMonth
		err := r.Scan(&m.AgentID, &m.Name, &m.Kind, &m.OwnerEmail, &m.Deleted,
			&m.CPUMilliSeconds, &m.MemoryMiBSeconds, &m.DiskMiBSeconds)
		return m, err
	}, `SELECT u.agent_id, a.name, a.kind, a.owner_email, a.deleted_at IS NOT NULL,
		       sum(u.cpu_milli_seconds)::bigint, sum(u.memory_mib_seconds)::bigint,
		       sum(u.disk_mib_seconds)::bigint
		FROM machine_usage u JOIN agents a ON a.id = u.agent_id
		WHERE u.org_id = $1 AND u.hour >= $2 AND u.hour < $3
		GROUP BY u.agent_id, a.name, a.kind, a.owner_email, a.deleted_at
		ORDER BY a.kind, lower(a.name), u.agent_id`, orgID, from, to)
}

// AgentUsage sums one machine's time since from.
func (s *Store) AgentUsage(ctx context.Context, agentID string, from time.Time) (MachineUse, error) {
	var u MachineUse
	err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(cpu_milli_seconds), 0)::bigint,
		       coalesce(sum(memory_mib_seconds), 0)::bigint,
		       coalesce(sum(disk_mib_seconds), 0)::bigint
		FROM machine_usage WHERE agent_id = $1 AND hour >= $2`, agentID, from).
		Scan(&u.CPUMilliSeconds, &u.MemoryMiBSeconds, &u.DiskMiBSeconds)
	return u, err
}

// PruneUsage deletes machine time from before before. The gateway keeps the
// bill.
func (s *Store) PruneUsage(ctx context.Context, before time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM machine_usage WHERE hour < $1`, before)
	return err
}
