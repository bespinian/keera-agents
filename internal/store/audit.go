package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type AuditEntry struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	OrgID  string          `json:"-"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

func (s *Store) Audit(ctx context.Context, orgID, actor, action, target string, detail any) error {
	var raw []byte
	if detail != nil {
		raw, _ = json.Marshal(detail)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit (org_id, actor, action, target, detail)
		VALUES ($1,$2,$3,$4,$5)`, orgID, actor, action, target, raw)
	return err
}

func (s *Store) AuditLog(ctx context.Context, orgID string, before int64, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return queryAll(ctx, s, scanAudit, `SELECT id, at, org_id, actor, action, target, detail
		FROM audit WHERE org_id = $1 AND ($2 = 0 OR id < $2) ORDER BY id DESC LIMIT $3`,
		orgID, before, limit)
}

func scanAudit(row pgx.Row) (AuditEntry, error) {
	var e AuditEntry
	var detail []byte
	err := row.Scan(&e.ID, &e.At, &e.OrgID, &e.Actor, &e.Action, &e.Target, &detail)
	e.Detail = detail
	return e, err
}
