package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-agents/internal/runtime"
)

// Agent states, as last seen on its container.
const (
	AgentStopped   = "stopped"
	AgentStarting  = "starting"
	AgentRunning   = "running"
	AgentSuspended = "suspended"
	AgentFailed    = "failed"
)

// Kinds of row in agents. A workspace is a machine an engineer works in; it
// shares an agent's key, connectors and lifecycle, but never takes tasks.
const (
	KindAgent     = "agent"
	KindWorkspace = "workspace"
)

// What a workspace's owner opens in it.
const (
	ExperienceTerminal = "terminal"
	ExperienceVSCode   = "vscode"
)

// Who may give an agent work from a chat.
const (
	AccessOwner  = "owner"
	AccessOrg    = "org"
	AccessPeople = "people"
)

type Agent struct {
	ID string `json:"id"`
	// Kind is KindAgent or KindWorkspace. It never changes.
	Kind string `json:"kind"`
	// Experience is a workspace's ExperienceTerminal or ExperienceVSCode,
	// and empty for an agent.
	Experience string `json:"experience,omitempty"`
	OrgID      string `json:"org_id"`
	ProjectID  string `json:"project_id"`
	OwnerID    string `json:"owner_id"`
	OwnerEmail string `json:"owner_email"`
	Name       string `json:"name"`
	// Icon names one of the panel's agent icons. Empty is the default.
	Icon          string        `json:"icon"`
	Description   string        `json:"description"`
	Instructions  string        `json:"instructions"`
	Model         string        `json:"model"`
	Builtins      []string      `json:"builtins"`
	MaxSteps      int           `json:"max_steps"`
	Access        string        `json:"access"`
	AllowedEmails []string      `json:"allowed_emails"`
	BudgetMicros  int64         `json:"budget_micros"`
	Image         string        `json:"image"`
	Class         runtime.Class `json:"class"`
	State         string        `json:"state"`
	StateDetail   string        `json:"state_detail"`
	Enabled       bool          `json:"enabled"`
	GatewayKeyID  string        `json:"-"`
	// GatewayKey is sealed; only the runtime API opens it.
	GatewayKey []byte `json:"-"`
	// Key is what the gateway key carries, as it was minted.
	Key KeyFor `json:"-"`
	// HasDisk says whether its machine exists, as Reconcile last saw. Only
	// then is its disk billed.
	HasDisk          bool       `json:"-"`
	RuntimeTokenHash []byte     `json:"-"`
	LastActiveAt     *time.Time `json:"last_active_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// KeyFor is what a gateway key carries: whose it is, its project and its
// budget.
type KeyFor struct {
	OwnerID      string
	ProjectID    string
	BudgetMicros int64
}

// KeyFor is what a's key should carry now.
func (a Agent) KeyFor() KeyFor {
	return KeyFor{OwnerID: a.OwnerID, ProjectID: a.ProjectID, BudgetMicros: a.BudgetMicros}
}

// KeyStale reports whether a has a key minted for other settings, as when a
// change could not reach the gateway.
func (a Agent) KeyStale() bool {
	return a.GatewayKeyID != "" && a.Key != a.KeyFor()
}

const agentColumns = `id, kind, experience, org_id, project_id, owner_id, owner_email, name, icon, description,
	instructions, model, builtins, max_steps, access, allowed_emails, budget_micros, image,
	class, state, state_detail, enabled, gateway_key_id, gateway_key, runtime_token_hash,
	last_active_at, created_at, updated_at, key_owner_id, key_project_id, key_budget_micros, has_disk`

func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	var class []byte
	err := row.Scan(&a.ID, &a.Kind, &a.Experience, &a.OrgID, &a.ProjectID, &a.OwnerID, &a.OwnerEmail, &a.Name, &a.Icon,
		&a.Description, &a.Instructions, &a.Model, &a.Builtins, &a.MaxSteps, &a.Access,
		&a.AllowedEmails, &a.BudgetMicros, &a.Image, &class, &a.State, &a.StateDetail,
		&a.Enabled, &a.GatewayKeyID, &a.GatewayKey, &a.RuntimeTokenHash, &a.LastActiveAt,
		&a.CreatedAt, &a.UpdatedAt, &a.Key.OwnerID, &a.Key.ProjectID, &a.Key.BudgetMicros, &a.HasDisk)
	if err != nil {
		return a, notFound(err)
	}
	_ = json.Unmarshal(class, &a.Class)
	a.Builtins = emptyIfNil(a.Builtins)
	a.AllowedEmails = emptyIfNil(a.AllowedEmails)
	return a, nil
}

// CreateAgent makes an agent, or a workspace when a.Kind says so.
func (s *Store) CreateAgent(ctx context.Context, a Agent) (Agent, error) {
	class, _ := json.Marshal(a.Class)
	if a.Kind == "" {
		a.Kind = KindAgent
	}
	row := s.pool.QueryRow(ctx, `INSERT INTO agents (id, org_id, project_id, owner_id,
		owner_email, name, description, instructions, model, builtins, max_steps, access,
		allowed_emails, budget_micros, image, class, state, enabled, icon, kind, experience)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		RETURNING `+agentColumns,
		a.ID, a.OrgID, a.ProjectID, a.OwnerID, a.OwnerEmail, a.Name, a.Description,
		a.Instructions, a.Model, emptyIfNil(a.Builtins), a.MaxSteps, a.Access,
		emptyIfNil(a.AllowedEmails), a.BudgetMicros, a.Image, class, AgentStopped, a.Enabled, a.Icon,
		a.Kind, a.Experience)
	out, err := scanAgent(row)
	return out, conflict(err)
}

// UpdateAgent writes the fields people edit. Not enabled: SetEnabled writes
// that alone, so a save that read the row before a stop cannot undo it.
func (s *Store) UpdateAgent(ctx context.Context, a Agent) (Agent, error) {
	class, _ := json.Marshal(a.Class)
	row := s.pool.QueryRow(ctx, `UPDATE agents SET name = $2, description = $3,
		instructions = $4, model = $5, builtins = $6, max_steps = $7, access = $8,
		allowed_emails = $9, budget_micros = $10, image = $11, class = $12,
		project_id = $13, icon = $14, experience = $15, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL RETURNING `+agentColumns,
		a.ID, a.Name, a.Description, a.Instructions, a.Model, emptyIfNil(a.Builtins),
		a.MaxSteps, a.Access, emptyIfNil(a.AllowedEmails), a.BudgetMicros, a.Image, class,
		a.ProjectID, a.Icon, a.Experience)
	out, err := scanAgent(row)
	return out, conflict(err)
}

// SetEnabled says whether an agent should run. It writes only that, so it
// cannot undo an edit made since the agent was read.
func (s *Store) SetEnabled(ctx context.Context, id string, enabled bool) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `UPDATE agents SET enabled = $2, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL RETURNING `+agentColumns, id, enabled))
}

// DisableIfOwner stops an agent, but only while ownerID owns it: an
// administrator may have taken it over since it was read. It is
// ErrNotFound otherwise.
func (s *Store) DisableIfOwner(ctx context.Context, id, ownerID string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `UPDATE agents SET enabled = false, updated_at = now()
		WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL RETURNING `+agentColumns, id, ownerID))
}

// Agent loads an agent or a workspace by id; callers that serve only one
// kind check a.Kind.
func (s *Store) Agent(ctx context.Context, id string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE id = $1 AND deleted_at IS NULL`, id))
}

// AgentByName finds an agent in an organisation, ignoring case. Workspaces
// are not agents to give work to, so it never finds one.
func (s *Store) AgentByName(ctx context.Context, orgID, name string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents
		WHERE org_id = $1 AND lower(name) = lower($2) AND kind = 'agent' AND deleted_at IS NULL`,
		orgID, name))
}

// WorkspaceByName finds one of a person's workspaces, ignoring case.
func (s *Store) WorkspaceByName(ctx context.Context, orgID, ownerID, name string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents
		WHERE org_id = $1 AND owner_id = $2 AND lower(name) = lower($3) AND kind = 'workspace'
		AND deleted_at IS NULL`, orgID, ownerID, name))
}

// AgentByRuntimeToken authenticates a container.
func (s *Store) AgentByRuntimeToken(ctx context.Context, hash []byte) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents
		WHERE runtime_token_hash = $1 AND deleted_at IS NULL`, hash))
}

// Agents lists an organisation's agents, without its workspaces.
func (s *Store) Agents(ctx context.Context, orgID string) ([]Agent, error) {
	return queryAll(ctx, s, scanAgent, `SELECT `+agentColumns+` FROM agents
		WHERE org_id = $1 AND kind = 'agent' AND deleted_at IS NULL ORDER BY lower(name)`, orgID)
}

// Workspaces lists an organisation's workspaces, or only one person's when
// ownerID is set.
func (s *Store) Workspaces(ctx context.Context, orgID, ownerID string) ([]Agent, error) {
	return queryAll(ctx, s, scanAgent, `SELECT `+agentColumns+` FROM agents
		WHERE org_id = $1 AND ($2 = '' OR owner_id = $2) AND kind = 'workspace'
		AND deleted_at IS NULL ORDER BY lower(name)`, orgID, ownerID)
}

// Machines lists every agent and workspace of every organisation, for the
// control plane's own loops.
func (s *Store) Machines(ctx context.Context) ([]Agent, error) {
	return queryAll(ctx, s, scanAgent, `SELECT `+agentColumns+` FROM agents
		WHERE deleted_at IS NULL ORDER BY id`)
}

func (s *Store) SetAgentState(ctx context.Context, id, state, detail string) error {
	return one(s.pool.Exec(ctx, `UPDATE agents SET state = $2, state_detail = $3
		WHERE id = $1 AND deleted_at IS NULL`, id, state, detail))
}

// SetHasDisk records whether the agent's machine exists.
func (s *Store) SetHasDisk(ctx context.Context, id string, has bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET has_disk = $2 WHERE id = $1 AND deleted_at IS NULL`, id, has)
	return err
}

// ReplaceAgentKey records a gateway key, sealed, and what it carries, in
// place of the key old, or of none when old is empty. It is ErrNotFound
// when the agent holds another key by now: another replica replaced it
// meanwhile, and that key stands. An empty keyID clears the key.
func (s *Store) ReplaceAgentKey(ctx context.Context, id, old, keyID string, sealed []byte, carries KeyFor) error {
	return one(s.pool.Exec(ctx, `UPDATE agents SET gateway_key_id = $3, gateway_key = $4,
		key_owner_id = $5, key_project_id = $6, key_budget_micros = $7
		WHERE id = $1 AND gateway_key_id = $2 AND deleted_at IS NULL`,
		id, old, keyID, sealed, carries.OwnerID, carries.ProjectID, carries.BudgetMicros))
}

// StaleKeys lists agents whose key was minted for other settings than
// theirs now.
func (s *Store) StaleKeys(ctx context.Context) ([]Agent, error) {
	return queryAll(ctx, s, scanAgent, `SELECT `+agentColumns+` FROM agents
		WHERE deleted_at IS NULL AND gateway_key_id <> ''
		AND (key_owner_id, key_project_id, key_budget_micros) IS DISTINCT FROM
			(owner_id, project_id, budget_micros)
		ORDER BY id LIMIT 100`)
}

// DeletedKeys are deleted agents whose key is not revoked yet, with only
// ID, OrgID and GatewayKeyID set.
func (s *Store) DeletedKeys(ctx context.Context) ([]Agent, error) {
	return queryAll(ctx, s, func(row pgx.Row) (Agent, error) {
		var a Agent
		return a, row.Scan(&a.ID, &a.OrgID, &a.GatewayKeyID)
	}, `SELECT id, org_id, gateway_key_id FROM agents
		WHERE deleted_at IS NOT NULL AND gateway_key_id <> '' ORDER BY id LIMIT 100`)
}

// KeyRevoked records that a deleted agent's key is revoked.
func (s *Store) KeyRevoked(ctx context.Context, id, keyID string) error {
	return one(s.pool.Exec(ctx, `UPDATE agents SET gateway_key_id = ''
		WHERE id = $1 AND gateway_key_id = $2 AND deleted_at IS NOT NULL`, id, keyID))
}

func (s *Store) SetRuntimeToken(ctx context.Context, id string, hash []byte) error {
	return one(s.pool.Exec(ctx, `UPDATE agents SET runtime_token_hash = $2
		WHERE id = $1 AND deleted_at IS NULL`, id, hash))
}

// SetOwner hands the agent to another person. Its key must be replaced too.
func (s *Store) SetOwner(ctx context.Context, id, ownerID, ownerEmail string) error {
	return one(s.pool.Exec(ctx, `UPDATE agents SET owner_id = $2, owner_email = $3, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id, ownerID, ownerEmail))
}

func (s *Store) TouchAgent(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET last_active_at = now() WHERE id = $1`, id)
	return err
}

// SuspendIdle calls suspend if the agent has done nothing since idleSince
// and has no task. It holds the agent's row meanwhile, so ClaimTask waits
// and no task starts on a container that is stopping. New tasks can still
// be queued: their foreign key takes only a key-share lock, which NO KEY
// UPDATE lets through. It reports whether suspend ran.
func (s *Store) SuspendIdle(ctx context.Context, id string, idleSince time.Time, suspend func() error) (bool, error) {
	ran := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var idle bool
		err := tx.QueryRow(ctx, `SELECT coalesce(last_active_at < $2, false) AND NOT EXISTS (
			SELECT 1 FROM tasks WHERE agent_id = $1 AND state IN ('queued', 'running'))
			FROM agents WHERE id = $1 AND enabled AND deleted_at IS NULL FOR NO KEY UPDATE`,
			id, idleSince).Scan(&idle)
		if err != nil || !idle {
			return err
		}
		ran = true
		return suspend()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ran && err == nil, err
}

// DeleteAgent keeps the row, marked, so tasks and audit entries still name
// it, and frees its name and runtime token. The key's id stays until the
// gateway has revoked it (DeletedKeys).
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	return one(s.pool.Exec(ctx, `UPDATE agents SET deleted_at = now(), enabled = false,
		state = 'stopped', runtime_token_hash = NULL, gateway_key = NULL
		WHERE id = $1 AND deleted_at IS NULL`, id))
}

// AgentLoad is how much work an agent has. LastTaskAt is only set for one
// agent, by AgentLoad.
type AgentLoad struct {
	Queued        int
	RunningTaskID string
	LastTaskAt    *time.Time
}

// AgentLoads is each agent's work in an organisation, for the list.
func (s *Store) AgentLoads(ctx context.Context, orgID string) (map[string]AgentLoad, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id,
		count(*) FILTER (WHERE state = 'queued'),
		coalesce(max(id) FILTER (WHERE state = 'running'), '')
		FROM tasks WHERE org_id = $1 AND state IN ('queued', 'running')
		GROUP BY agent_id`, orgID)
	if err != nil {
		return nil, err
	}
	out := map[string]AgentLoad{}
	var id string
	var l AgentLoad
	_, err = pgx.ForEachRow(rows, []any{&id, &l.Queued, &l.RunningTaskID}, func() error {
		out[id] = l
		return nil
	})
	return out, err
}

// AgentLoad is one agent's work, and when it last got a task.
func (s *Store) AgentLoad(ctx context.Context, id string) (AgentLoad, error) {
	var l AgentLoad
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'queued'),
		coalesce(max(id) FILTER (WHERE state = 'running'), ''), max(created_at)
		FROM tasks WHERE agent_id = $1`, id).Scan(&l.Queued, &l.RunningTaskID, &l.LastTaskAt)
	return l, err
}
