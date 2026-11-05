package store

import (
	"context"
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

type Board struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"-"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Cards     int       `json:"cards"`
}

func (s *Store) CreateBoard(ctx context.Context, b Board) (Board, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO boards (id, org_id, name, created_by)
		VALUES ($1,$2,$3,$4) RETURNING created_at`, b.ID, b.OrgID, b.Name, b.CreatedBy).
		Scan(&b.CreatedAt)
	return b, err
}

const boardSelect = `SELECT b.id, b.org_id, b.name, b.created_by, b.created_at,
	(SELECT count(*) FROM cards c WHERE c.board_id = b.id)
	FROM boards b`

func scanBoard(row pgx.Row) (Board, error) {
	var b Board
	err := row.Scan(&b.ID, &b.OrgID, &b.Name, &b.CreatedBy, &b.CreatedAt, &b.Cards)
	return b, notFound(err)
}

func (s *Store) Boards(ctx context.Context, orgID string) ([]Board, error) {
	return queryAll(ctx, s, scanBoard, boardSelect+` WHERE b.org_id = $1 ORDER BY b.created_at`, orgID)
}

func (s *Store) Board(ctx context.Context, id string) (Board, error) {
	return scanBoard(s.pool.QueryRow(ctx, boardSelect+` WHERE b.id = $1`, id))
}

func (s *Store) RenameBoard(ctx context.Context, id, name string) error {
	return one(s.pool.Exec(ctx, `UPDATE boards SET name = $2 WHERE id = $1`, id, name))
}

func (s *Store) DeleteBoard(ctx context.Context, id string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM boards WHERE id = $1`, id))
}

type Card struct {
	ID        string     `json:"id"`
	BoardID   string     `json:"board_id"`
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
	// RequestedBy is who last gave the card's agent work: its task runs for
	// them.
	RequestedBy   string    `json:"requested_by"`
	RequestedByID string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Comments      int       `json:"comments"`
	OrgID         string    `json:"-"`
}

const cardSelect = `SELECT c.id, c.board_id, c.title, c.body, c.status, c.position, c.agent_id,
	coalesce(a.name, ''), c.labels, c.due_at, c.task_id, coalesce(t.state, ''), c.created_by,
	c.requested_by, c.requested_by_id, c.created_at, c.updated_at,
	(SELECT count(*) FROM card_comments cc WHERE cc.card_id = c.id), b.org_id
	FROM cards c JOIN boards b ON b.id = c.board_id
	LEFT JOIN agents a ON a.id = c.agent_id AND a.deleted_at IS NULL
	LEFT JOIN tasks t ON t.id = c.task_id`

func scanCard(row pgx.Row) (Card, error) {
	var c Card
	err := row.Scan(&c.ID, &c.BoardID, &c.Title, &c.Body, &c.Status, &c.Position, &c.AgentID,
		&c.AgentName, &c.Labels, &c.DueAt, &c.TaskID, &c.TaskState, &c.CreatedBy,
		&c.RequestedBy, &c.RequestedByID, &c.CreatedAt, &c.UpdatedAt, &c.Comments, &c.OrgID)
	c.Labels = emptyIfNil(c.Labels)
	return c, notFound(err)
}

func (s *Store) queryCards(ctx context.Context, tail string, args ...any) ([]Card, error) {
	return queryAll(ctx, s, scanCard, cardSelect+` `+tail, args...)
}

// CreateCard puts the card at the end of its column.
func (s *Store) CreateCard(ctx context.Context, c Card) (Card, error) {
	_, err := s.pool.Exec(ctx, `INSERT INTO cards (id, board_id, title, body, status, position,
		agent_id, labels, due_at, created_by, requested_by, requested_by_id)
		VALUES ($1,$2,$3,$4,$5,
			(SELECT coalesce(max(position), 0) + 1 FROM cards WHERE board_id = $2 AND status = $5),
			$6,$7,$8,$9,$10,$11)`,
		c.ID, c.BoardID, c.Title, c.Body, c.Status, c.AgentID, emptyIfNil(c.Labels), c.DueAt,
		c.CreatedBy, c.RequestedBy, c.RequestedByID)
	if err != nil {
		return Card{}, err
	}
	return s.Card(ctx, c.ID)
}

// UpdateCard writes c. Its status and position are written only if they
// differ from before, the card as it was read: an agent finishing the card
// meanwhile moves it, and saving an edit must not move it back.
func (s *Store) UpdateCard(ctx context.Context, before, c Card) (Card, error) {
	moved := c.Status != before.Status || c.Position != before.Position
	if err := one(s.pool.Exec(ctx, `UPDATE cards SET title = $2, body = $3,
		status = CASE WHEN $11 THEN $4 ELSE status END,
		position = CASE WHEN $11 THEN $5 ELSE position END,
		agent_id = $6, labels = $7, due_at = $8, requested_by = $9,
		requested_by_id = $10, updated_at = now() WHERE id = $1`,
		c.ID, c.Title, c.Body, c.Status, c.Position, c.AgentID, emptyIfNil(c.Labels), c.DueAt,
		c.RequestedBy, c.RequestedByID, moved)); err != nil {
		return Card{}, err
	}
	return s.Card(ctx, c.ID)
}

// MoveCard changes a card's status, to the end of the new column, as an
// agent does when it takes or finishes a card.
func (s *Store) MoveCard(ctx context.Context, id, status string, taskID *string) error {
	return one(s.pool.Exec(ctx, `UPDATE cards SET status = $2,
		position = (SELECT coalesce(max(c2.position), 0) + 1 FROM cards c2
			WHERE c2.board_id = cards.board_id AND c2.status = $2),
		task_id = coalesce($3, task_id), updated_at = now() WHERE id = $1`, id, status, taskID))
}

func (s *Store) Card(ctx context.Context, id string) (Card, error) {
	return scanCard(s.pool.QueryRow(ctx, cardSelect+` WHERE c.id = $1`, id))
}

func (s *Store) BoardCards(ctx context.Context, boardID string) ([]Card, error) {
	return s.queryCards(ctx, `WHERE c.board_id = $1
		ORDER BY array_position($2::text[], c.status), c.position`, boardID, CardStatuses)
}

// OrgCards lists an organisation's cards on every board, for an agent's
// board tools.
func (s *Store) OrgCards(ctx context.Context, orgID, status, agentID string, limit int) ([]Card, error) {
	return s.queryCards(ctx, `WHERE b.org_id = $1 AND ($2 = '' OR c.status = $2)
		AND ($3 = '' OR c.agent_id = $3) ORDER BY c.updated_at DESC LIMIT $4`,
		orgID, status, agentID, limit)
}

// PickupCards are cards waiting in To do for an agent that has no task for
// them yet.
func (s *Store) PickupCards(ctx context.Context) ([]Card, error) {
	return s.queryCards(ctx, `WHERE c.status = 'todo' AND c.agent_id IS NOT NULL
		AND a.enabled AND (t.id IS NULL OR t.state IN ('succeeded', 'failed', 'cancelled'))
		ORDER BY c.position LIMIT 100`)
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
}

func (s *Store) AddComment(ctx context.Context, c Comment) (Comment, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO card_comments (id, card_id, author, agent_id, body)
		VALUES ($1,$2,$3,$4,$5) RETURNING created_at`, c.ID, c.CardID, c.Author, c.AgentID, c.Body).
		Scan(&c.CreatedAt)
	if err == nil {
		_, _ = s.pool.Exec(ctx, `UPDATE cards SET updated_at = now() WHERE id = $1`, c.CardID)
	}
	return c, err
}

func (s *Store) Comments(ctx context.Context, cardID string) ([]Comment, error) {
	return queryAll(ctx, s, func(row pgx.Row) (Comment, error) {
		var c Comment
		err := row.Scan(&c.ID, &c.CardID, &c.Author, &c.AgentID, &c.Body, &c.CreatedAt)
		return c, err
	}, `SELECT id, card_id, author, agent_id, body, created_at
		FROM card_comments WHERE card_id = $1 ORDER BY created_at`, cardID)
}
