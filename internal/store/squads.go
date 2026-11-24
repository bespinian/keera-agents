package store

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Card statuses, in the board's order.
const (
	CardBacklog    = "backlog"
	CardTodo       = "todo"
	CardInProgress = "in_progress"
	CardReview     = "review"
	CardDone       = "done"
)

var CardStatuses = []string{CardBacklog, CardTodo, CardInProgress, CardReview, CardDone}

// What a card holds, the same whether a person or an agent writes it: cards
// and comments go into other agents' prompts.
const (
	MaxCardTitle = 300
	MaxCardBody  = 64 << 10
	MaxComment   = 16 << 10
)

// A squad is agents that share a board and hand each other work on it.
// Everyone in the organisation may read and change it.
type Squad struct {
	ID    string `json:"id"`
	OrgID string `json:"-"`
	Name  string `json:"name"`
	Goal  string `json:"goal"`
	// LeadID takes the cards put in To do without an agent. Nil is none.
	LeadID    *string   `json:"lead_id"`
	Members   []Member  `json:"members"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Cards     int       `json:"cards"`
	// Open counts the cards in To do and In progress.
	Open int `json:"open"`
}

// Member is an agent in a squad.
type Member struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
}

// Limits on what agents hand out, so they cannot keep each other busy
// forever. People are not held to them.
const (
	// MaxCardDepth is how many hands a card may pass through: a card from a
	// person is 0, one an agent hands out while working on it is 1.
	MaxCardDepth = 3
	// MaxChildCards is how many cards an agent may hand out from one card.
	MaxChildCards = 20
	// MaxOpenCards is how many cards a squad may have in To do and In
	// progress for agents to create another.
	MaxOpenCards = 100
)

// Has reports whether the agent is a member.
func (q Squad) Has(agentID string) bool {
	return slices.ContainsFunc(q.Members, func(m Member) bool { return m.ID == agentID })
}

// MemberIDs are the members' ids, in the order of Members.
func (q Squad) MemberIDs() []string {
	ids := make([]string, len(q.Members))
	for i, m := range q.Members {
		ids[i] = m.ID
	}
	return ids
}

// CreateSquad stores a squad with its members, in one go.
func (s *Store) CreateSquad(ctx context.Context, q Squad, members []string) (Squad, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO squads (id, org_id, name, goal, created_by)
			VALUES ($1,$2,$3,$4,$5)`, q.ID, q.OrgID, q.Name, q.Goal, q.CreatedBy); err != nil {
			return err
		}
		return setMembers(ctx, tx, q, members)
	})
	if err != nil {
		return Squad{}, err
	}
	return s.Squad(ctx, q.ID)
}

// squadSelect reads a deleted agent as no lead: agents are only marked
// deleted.
const squadSelect = `SELECT q.id, q.org_id, q.name, q.goal, a.id, q.created_by, q.created_at,
	(SELECT count(*) FROM cards c WHERE c.squad_id = q.id),
	(SELECT count(*) FROM cards c WHERE c.squad_id = q.id AND c.status IN ('todo', 'in_progress'))
	FROM squads q LEFT JOIN agents a ON a.id = q.lead_id AND a.deleted_at IS NULL`

func scanSquad(row pgx.Row) (Squad, error) {
	var q Squad
	err := row.Scan(&q.ID, &q.OrgID, &q.Name, &q.Goal, &q.LeadID, &q.CreatedBy, &q.CreatedAt, &q.Cards, &q.Open)
	return q, notFound(err)
}

// withMembers fills in each squad's members.
func (s *Store) withMembers(ctx context.Context, squads []Squad) ([]Squad, error) {
	ids := make([]string, len(squads))
	at := make(map[string]int, len(squads))
	for i, q := range squads {
		ids[i] = q.ID
		at[q.ID] = i
		squads[i].Members = []Member{}
	}
	type row struct {
		squad string
		m     Member
	}
	rows, err := queryAll(ctx, s, func(r pgx.Row) (row, error) {
		var x row
		err := r.Scan(&x.squad, &x.m.ID, &x.m.Name, &x.m.Icon, &x.m.Description)
		return x, err
	}, `SELECT m.squad_id, a.id, a.name, a.icon, a.description
		FROM squad_members m JOIN agents a ON a.id = m.agent_id AND a.deleted_at IS NULL
		WHERE m.squad_id = ANY($1) ORDER BY lower(a.name)`, ids)
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		i := at[x.squad]
		squads[i].Members = append(squads[i].Members, x.m)
	}
	return squads, nil
}

func (s *Store) querySquads(ctx context.Context, tail string, args ...any) ([]Squad, error) {
	squads, err := queryAll(ctx, s, scanSquad, squadSelect+` `+tail, args...)
	if err != nil {
		return nil, err
	}
	return s.withMembers(ctx, squads)
}

func (s *Store) Squads(ctx context.Context, orgID string) ([]Squad, error) {
	return s.querySquads(ctx, `WHERE q.org_id = $1 ORDER BY q.created_at`, orgID)
}

// memberOf limits a query to the squads agent $1 is in, of its own
// organisation only.
const memberOf = `q.id IN (SELECT m.squad_id FROM squad_members m WHERE m.agent_id = $1)
	AND q.org_id = (SELECT org_id FROM agents WHERE id = $1)`

// AgentSquads are the squads an agent is in.
func (s *Store) AgentSquads(ctx context.Context, agentID string) ([]Squad, error) {
	return s.querySquads(ctx, `WHERE `+memberOf+` ORDER BY q.created_at`, agentID)
}

func (s *Store) Squad(ctx context.Context, id string) (Squad, error) {
	squads, err := s.querySquads(ctx, `WHERE q.id = $1`, id)
	if err != nil {
		return Squad{}, err
	}
	if len(squads) == 0 {
		return Squad{}, ErrNotFound
	}
	return squads[0], nil
}

// UpdateSquad writes a squad's name, goal, lead and members.
func (s *Store) UpdateSquad(ctx context.Context, q Squad, members []string) (Squad, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := one(tx.Exec(ctx, `UPDATE squads SET name = $2, goal = $3 WHERE id = $1`,
			q.ID, q.Name, q.Goal)); err != nil {
			return err
		}
		return setMembers(ctx, tx, q, members)
	})
	if err != nil {
		return Squad{}, err
	}
	return s.Squad(ctx, q.ID)
}

// setMembers writes a squad's members and lead. A member who leaves takes no
// work from it any more: their cards that are not in progress or done lose
// their agent, and they stop being the lead.
func setMembers(ctx context.Context, tx pgx.Tx, q Squad, members []string) error {
	members = emptyIfNil(members)
	if _, err := tx.Exec(ctx, `DELETE FROM squad_members WHERE squad_id = $1
		AND NOT (agent_id = ANY($2))`, q.ID, members); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO squad_members (squad_id, agent_id)
		SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`, q.ID, members); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE cards SET agent_id = NULL, updated_at = now()
		WHERE squad_id = $1 AND status IN ('backlog', 'todo', 'review')
		AND agent_id IS NOT NULL AND NOT (agent_id = ANY($2))`, q.ID, members); err != nil {
		return err
	}
	lead := q.LeadID
	if lead != nil && !slices.Contains(members, *lead) {
		lead = nil
	}
	_, err := tx.Exec(ctx, `UPDATE squads SET lead_id = $2 WHERE id = $1`, q.ID, lead)
	return err
}

func (s *Store) DeleteSquad(ctx context.Context, id string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM squads WHERE id = $1`, id))
}

type Card struct {
	ID        string     `json:"id"`
	SquadID   string     `json:"squad_id"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Status    string     `json:"status"`
	Position  float64    `json:"position"`
	AgentID   *string    `json:"agent_id"`
	AgentName string     `json:"agent_name"`
	Labels    []string   `json:"labels"`
	DueAt     *time.Time `json:"due_at"`
	TaskID    *string    `json:"task_id"`
	TaskState string     `json:"task_state"`
	CreatedBy string     `json:"created_by"`
	// ParentID is the card whose agent handed this one out; Depth counts the
	// hands it went through.
	ParentID *string `json:"parent_id"`
	Depth    int     `json:"depth"`
	// Waiting is a card whose agent waits for the cards it handed out.
	Waiting bool `json:"waiting"`
	// RequestedBy is who last gave the card's agent work: its task runs for
	// them.
	RequestedBy   string    `json:"requested_by"`
	RequestedByID string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Comments      int       `json:"comments"`
	OrgID         string    `json:"-"`
	SquadName     string    `json:"-"`
}

// cardSelect reads a deleted agent's card as unassigned: agents are only
// marked deleted, so the card's agent_id outlives them.
const cardSelect = `SELECT c.id, c.squad_id, c.title, c.body, c.status, c.position, a.id,
	coalesce(a.name, ''), c.labels, c.due_at, c.task_id, coalesce(t.state, ''), c.created_by,
	c.parent_id, c.depth, c.waiting, c.requested_by, c.requested_by_id, c.created_at, c.updated_at,
	(SELECT count(*) FROM card_comments cc WHERE cc.card_id = c.id), q.org_id, q.name
	FROM cards c JOIN squads q ON q.id = c.squad_id
	LEFT JOIN agents a ON a.id = c.agent_id AND a.deleted_at IS NULL
	LEFT JOIN tasks t ON t.id = c.task_id`

func scanCard(row pgx.Row) (Card, error) {
	var c Card
	err := row.Scan(&c.ID, &c.SquadID, &c.Title, &c.Body, &c.Status, &c.Position, &c.AgentID,
		&c.AgentName, &c.Labels, &c.DueAt, &c.TaskID, &c.TaskState, &c.CreatedBy,
		&c.ParentID, &c.Depth, &c.Waiting, &c.RequestedBy, &c.RequestedByID, &c.CreatedAt, &c.UpdatedAt, &c.Comments, &c.OrgID,
		&c.SquadName)
	c.Labels = emptyIfNil(c.Labels)
	return c, notFound(err)
}

func (s *Store) queryCards(ctx context.Context, tail string, args ...any) ([]Card, error) {
	return queryAll(ctx, s, scanCard, cardSelect+` `+tail, args...)
}

// CreateCard puts the card at the end of its column.
func (s *Store) CreateCard(ctx context.Context, c Card) (Card, error) {
	_, err := s.pool.Exec(ctx, `INSERT INTO cards (id, squad_id, title, body, status, position,
		agent_id, labels, due_at, created_by, requested_by, requested_by_id, parent_id, depth)
		VALUES ($1,$2,$3,$4,$5,
			(SELECT coalesce(max(position), 0) + 1 FROM cards WHERE squad_id = $2 AND status = $5),
			$6,$7,$8,$9,$10,$11,$12,$13)`,
		c.ID, c.SquadID, c.Title, c.Body, c.Status, c.AgentID, emptyIfNil(c.Labels), c.DueAt,
		c.CreatedBy, c.RequestedBy, c.RequestedByID, c.ParentID, c.Depth)
	if err != nil {
		return Card{}, err
	}
	return s.Card(ctx, c.ID)
}

// UpdateCard writes c. Its status and position are written only if they
// differ from before, the card as it was read: an agent finishing the card
// meanwhile moves it, and saving an edit must not move it back. A move
// ends any wait: whoever moved it decides what comes next.
func (s *Store) UpdateCard(ctx context.Context, before, c Card) (Card, error) {
	moved := c.Status != before.Status || c.Position != before.Position
	if err := one(s.pool.Exec(ctx, `UPDATE cards SET title = $2, body = $3,
		status = CASE WHEN $11 THEN $4 ELSE status END,
		position = CASE WHEN $11 THEN $5 ELSE position END,
		waiting = waiting AND NOT $11,
		agent_id = $6, labels = $7, due_at = $8, requested_by = $9,
		requested_by_id = $10, updated_at = now() WHERE id = $1`,
		c.ID, c.Title, c.Body, c.Status, c.Position, c.AgentID, emptyIfNil(c.Labels), c.DueAt,
		c.RequestedBy, c.RequestedByID, moved)); err != nil {
		return Card{}, err
	}
	return s.Card(ctx, c.ID)
}

// MoveCard changes a card's status, to the end of the new column, as an
// agent does when it takes or finishes a card. It ends any wait.
func (s *Store) MoveCard(ctx context.Context, id, status string, taskID *string) error {
	return one(s.pool.Exec(ctx, `UPDATE cards SET status = $2, waiting = false,
		position = (SELECT coalesce(max(c2.position), 0) + 1 FROM cards c2
			WHERE c2.squad_id = cards.squad_id AND c2.status = $2),
		task_id = coalesce($3, task_id), updated_at = now() WHERE id = $1`, id, status, taskID))
}

func (s *Store) Card(ctx context.Context, id string) (Card, error) {
	return scanCard(s.pool.QueryRow(ctx, cardSelect+` WHERE c.id = $1`, id))
}

func (s *Store) SquadCards(ctx context.Context, squadID string) ([]Card, error) {
	return s.queryCards(ctx, `WHERE c.squad_id = $1
		ORDER BY array_position($2::text[], c.status), c.position`, squadID, CardStatuses)
}

// CardFilter narrows the cards an agent's board tools list.
type CardFilter struct {
	Squad  string // a squad's id, or "" for all of the agent's
	Status string
	Agent  string // the assignee's id, or "" for anyone
	Limit  int
}

// MemberCards lists the cards in the squads the agent is in, newest change
// first.
func (s *Store) MemberCards(ctx context.Context, agentID string, f CardFilter) ([]Card, error) {
	return s.queryCards(ctx, `WHERE `+memberOf+`
		AND ($2 = '' OR c.squad_id = $2) AND ($3 = '' OR c.status = $3)
		AND ($4 = '' OR c.agent_id = $4) ORDER BY c.updated_at DESC LIMIT $5`,
		agentID, f.Squad, f.Status, f.Agent, f.Limit)
}

// ChildCards are the cards handed out from a card, oldest first.
func (s *Store) ChildCards(ctx context.Context, parentID string) ([]Card, error) {
	return s.queryCards(ctx, `WHERE c.parent_id = $1 ORDER BY c.created_at`, parentID)
}

// CountChildCards counts the cards handed out from a card since a time;
// the zero time counts all of them.
func (s *Store) CountChildCards(ctx context.Context, parentID string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM cards WHERE parent_id = $1 AND created_at >= $2`,
		parentID, since).Scan(&n)
	return n, err
}

// PickupCards are cards waiting in To do for a member of their squad that
// has no task for them yet.
func (s *Store) PickupCards(ctx context.Context) ([]Card, error) {
	return s.queryCards(ctx, `WHERE c.status = 'todo' AND c.agent_id IS NOT NULL
		AND a.enabled AND (t.id IS NULL OR t.state IN ('succeeded', 'failed', 'cancelled'))
		AND EXISTS (SELECT 1 FROM squad_members m
			WHERE m.squad_id = c.squad_id AND m.agent_id = c.agent_id)
		ORDER BY c.position LIMIT 100`)
}

// WaitCard keeps a card in progress after its task, until the cards its
// agent handed out are finished. It does nothing if the card has moved.
func (s *Store) WaitCard(ctx context.Context, id, taskID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE cards SET waiting = true, updated_at = now()
		WHERE id = $1 AND status = 'in_progress' AND task_id = $2`, id, taskID)
	return err
}

// WakeCards moves waiting cards whose handed-out cards are all finished,
// none in To do or In progress, back to To do, so their agent picks them up
// with the results.
func (s *Store) WakeCards(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE cards p SET status = 'todo', waiting = false, updated_at = now(),
			position = (SELECT coalesce(max(c2.position), 0) + 1 FROM cards c2
				WHERE c2.squad_id = p.squad_id AND c2.status = 'todo')
		WHERE p.waiting AND p.status = 'in_progress' AND NOT EXISTS (SELECT 1 FROM cards c
			WHERE c.parent_id = p.id AND c.status IN ('todo', 'in_progress'))`)
	return tag.RowsAffected(), err
}

func (s *Store) DeleteCard(ctx context.Context, id string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM cards WHERE id = $1`, id))
}

type Comment struct {
	ID        string    `json:"id"`
	CardID    string    `json:"-"`
	Author    string    `json:"author"`
	AgentID   *string   `json:"agent_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	// RequestedBy, when set, is who the comment gives the card's agent work
	// for: the card's task then runs for them, as after an edit.
	RequestedBy   string `json:"-"`
	RequestedByID string `json:"-"`
}

func (s *Store) AddComment(ctx context.Context, c Comment) (Comment, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO card_comments (id, card_id, author, agent_id, body)
			VALUES ($1,$2,$3,$4,$5) RETURNING created_at`, c.ID, c.CardID, c.Author, c.AgentID, c.Body).
			Scan(&c.CreatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE cards SET updated_at = now(),
			requested_by = CASE WHEN $2 <> '' THEN $2 ELSE requested_by END,
			requested_by_id = CASE WHEN $2 <> '' THEN $3 ELSE requested_by_id END
			WHERE id = $1`, c.CardID, c.RequestedBy, c.RequestedByID)
		return err
	})
	return c, err
}

// Results are the last comment by an agent on each of the cards, by card id:
// what the agent made of it.
func (s *Store) Results(ctx context.Context, cardIDs []string) (map[string]string, error) {
	type row struct{ card, body string }
	rows, err := queryAll(ctx, s, func(r pgx.Row) (row, error) {
		var x row
		return x, r.Scan(&x.card, &x.body)
	}, `SELECT DISTINCT ON (card_id) card_id, body FROM card_comments
		WHERE card_id = ANY($1) AND agent_id IS NOT NULL
		ORDER BY card_id, created_at DESC`, emptyIfNil(cardIDs))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, x := range rows {
		out[x.card] = x.body
	}
	return out, nil
}

func (s *Store) Comments(ctx context.Context, cardID string) ([]Comment, error) {
	return queryAll(ctx, s, func(row pgx.Row) (Comment, error) {
		var c Comment
		err := row.Scan(&c.ID, &c.CardID, &c.Author, &c.AgentID, &c.Body, &c.CreatedAt)
		return c, err
	}, `SELECT id, card_id, author, agent_id, body, created_at
		FROM card_comments WHERE card_id = $1 ORDER BY created_at`, cardID)
}
