package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Connection is one organisation's credentials for one service. Secrets are
// sealed here; the caller opens them with the connection's id.
type Connection struct {
	ID        string            `json:"id"`
	OrgID     string            `json:"org_id"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Config    map[string]string `json:"config"`
	Secrets   []byte            `json:"-"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

const connectionColumns = `id, org_id, kind, name, config, secrets, created_by, created_at, updated_at`

func scanConnection(row pgx.Row) (Connection, error) {
	var c Connection
	var cfg []byte
	err := row.Scan(&c.ID, &c.OrgID, &c.Kind, &c.Name, &cfg, &c.Secrets, &c.CreatedBy,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, notFound(err)
	}
	_ = json.Unmarshal(cfg, &c.Config)
	if c.Config == nil {
		c.Config = map[string]string{}
	}
	return c, nil
}

func (s *Store) CreateConnection(ctx context.Context, c Connection) (Connection, error) {
	cfg, _ := json.Marshal(c.Config)
	return scanConnection(s.pool.QueryRow(ctx, `INSERT INTO connections
		(id, org_id, kind, name, config, secrets, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+connectionColumns, c.ID, c.OrgID, c.Kind, c.Name, cfg, c.Secrets, c.CreatedBy))
}

func (s *Store) UpdateConnection(ctx context.Context, c Connection) (Connection, error) {
	cfg, _ := json.Marshal(c.Config)
	return scanConnection(s.pool.QueryRow(ctx, `UPDATE connections SET name = $2, config = $3,
		secrets = $4, updated_at = now() WHERE id = $1 RETURNING `+connectionColumns,
		c.ID, c.Name, cfg, c.Secrets))
}

func (s *Store) Connection(ctx context.Context, id string) (Connection, error) {
	return scanConnection(s.pool.QueryRow(ctx,
		`SELECT `+connectionColumns+` FROM connections WHERE id = $1`, id))
}

func (s *Store) Connections(ctx context.Context, orgID string) ([]Connection, error) {
	return queryAll(ctx, s, scanConnection, `SELECT `+connectionColumns+` FROM connections
		WHERE org_id = $1 ORDER BY kind, lower(name)`, orgID)
}

func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM connections WHERE id = $1`, id))
}

// Binding modes.
const (
	ModeChat  = "chat"
	ModeWatch = "watch"
	ModeFiles = "files"
	// ModeRepo lets an agent clone a repository, push branches and open
	// pull or merge requests.
	ModeRepo = "repo"
	// ModeRead and ModeWrite let an agent call an API connector's tools:
	// those that only read, or all of them.
	ModeRead  = "read"
	ModeWrite = "write"
)

// AnyConversation binds every conversation a chat connection's bot is in.
const AnyConversation = "*"

type Binding struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	AgentName      string     `json:"agent_name"`
	ConnectionID   string     `json:"connection_id"`
	ConnectionName string     `json:"connection_name"`
	Kind           string     `json:"kind"`
	Target         string     `json:"target"`
	Label          string     `json:"label"`
	Mode           string     `json:"mode"`
	Prompt         string     `json:"prompt"`
	Cursor         string     `json:"-"`
	CheckedAt      *time.Time `json:"checked_at"`
	CreatedBy      string     `json:"created_by"`
	CreatedByID    string     `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
	// OrgID is the connection's, for checks.
	OrgID string `json:"-"`
}

const bindingSelect = `SELECT b.id, b.agent_id, a.name, b.connection_id, c.name, c.kind, b.target,
	b.label, b.mode, b.prompt, b.cursor, b.checked_at, b.created_by, b.created_by_id,
	b.created_at, c.org_id
	FROM bindings b JOIN connections c ON c.id = b.connection_id
	JOIN agents a ON a.id = b.agent_id AND a.deleted_at IS NULL`

func scanBinding(row pgx.Row) (Binding, error) {
	var b Binding
	err := row.Scan(&b.ID, &b.AgentID, &b.AgentName, &b.ConnectionID, &b.ConnectionName, &b.Kind, &b.Target,
		&b.Label, &b.Mode, &b.Prompt, &b.Cursor, &b.CheckedAt, &b.CreatedBy, &b.CreatedByID,
		&b.CreatedAt, &b.OrgID)
	return b, notFound(err)
}

func (s *Store) queryBindings(ctx context.Context, where string, args ...any) ([]Binding, error) {
	return queryAll(ctx, s, scanBinding, bindingSelect+` WHERE `+where+` ORDER BY b.created_at`, args...)
}

func (s *Store) CreateBinding(ctx context.Context, b Binding) (Binding, error) {
	_, err := s.pool.Exec(ctx, `INSERT INTO bindings (id, agent_id, connection_id, target,
		label, mode, prompt, created_by, created_by_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		b.ID, b.AgentID, b.ConnectionID, b.Target, b.Label, b.Mode, b.Prompt, b.CreatedBy,
		b.CreatedByID)
	if err != nil {
		return Binding{}, conflict(err)
	}
	return s.Binding(ctx, b.ID)
}

func (s *Store) Binding(ctx context.Context, id string) (Binding, error) {
	return scanBinding(s.pool.QueryRow(ctx, bindingSelect+` WHERE b.id = $1`, id))
}

func (s *Store) AgentBindings(ctx context.Context, agentID string) ([]Binding, error) {
	return s.queryBindings(ctx, `b.agent_id = $1`, agentID)
}

// ChatBindings are the agents listening on a connection.
func (s *Store) ChatBindings(ctx context.Context, connectionID string) ([]Binding, error) {
	return s.queryBindings(ctx, `b.connection_id = $1 AND b.mode = 'chat' AND a.enabled`, connectionID)
}

// Watches are every folder watch of enabled agents.
func (s *Store) Watches(ctx context.Context) ([]Binding, error) {
	return s.queryBindings(ctx, `b.mode = 'watch' AND a.enabled`)
}

func (s *Store) BindingCounts(ctx context.Context, orgID string) (map[string]int, error) {
	return s.countBy(ctx, `SELECT b.connection_id, count(*) FROM bindings b
		JOIN connections c ON c.id = b.connection_id
		JOIN agents a ON a.id = b.agent_id AND a.deleted_at IS NULL
		WHERE c.org_id = $1 GROUP BY b.connection_id`, orgID)
}

// AgentBindingCounts counts each agent's connections, for the agent list.
func (s *Store) AgentBindingCounts(ctx context.Context, orgID string) (map[string]int, error) {
	return s.countBy(ctx, `SELECT b.agent_id, count(*) FROM bindings b
		JOIN agents a ON a.id = b.agent_id AND a.deleted_at IS NULL
		WHERE a.org_id = $1 GROUP BY b.agent_id`, orgID)
}

func (s *Store) SetWatchCursor(ctx context.Context, id, cursor string) error {
	_, err := s.pool.Exec(ctx, `UPDATE bindings SET cursor = $2, checked_at = now() WHERE id = $1`,
		id, cursor)
	return err
}

// RecordWrite notes a file an agent wrote through a connection, with the
// time the service gave it, so the agent's watches can skip the change.
func (s *Store) RecordWrite(ctx context.Context, agentID, connectionID, fileID string, modified time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO written_files (agent_id, connection_id, file_id, modified)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (agent_id, connection_id, file_id)
		DO UPDATE SET modified = excluded.modified, written_at = now()`,
		agentID, connectionID, fileID, modified)
	return err
}

// Wrote reports whether the agent's own write is the file's last change.
func (s *Store) Wrote(ctx context.Context, agentID, connectionID, fileID string, modified time.Time) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM written_files
		WHERE agent_id = $1 AND connection_id = $2 AND file_id = $3 AND modified = $4)`,
		agentID, connectionID, fileID, modified).Scan(&ok)
	return ok, err
}

// PruneWrites forgets writes older than before: a watch reads its folder's
// changes every few minutes, so it has long passed them.
func (s *Store) PruneWrites(ctx context.Context, before time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM written_files WHERE written_at < $1`, before)
	return err
}

func (s *Store) DeleteBinding(ctx context.Context, id string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM bindings WHERE id = $1`, id))
}
