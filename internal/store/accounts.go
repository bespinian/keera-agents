package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Account is one person's sign-in to a connector people sign in to. Token is
// their refresh token, sealed; the caller opens it.
type Account struct {
	ConnectionID string    `json:"connection_id"`
	UserID       string    `json:"-"`
	Account      string    `json:"account"`
	Token        []byte    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// OrgID is the connection's, for checks.
	OrgID string `json:"-"`
}

const accountSelect = `SELECT a.connection_id, a.user_id, a.account, a.token, a.created_at, a.updated_at,
	c.org_id FROM accounts a JOIN connections c ON c.id = a.connection_id`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ConnectionID, &a.UserID, &a.Account, &a.Token, &a.CreatedAt, &a.UpdatedAt, &a.OrgID)
	return a, notFound(err)
}

// SaveAccount keeps a person's sign-in, replacing the one they had.
func (s *Store) SaveAccount(ctx context.Context, a Account) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO accounts (connection_id, user_id, account, token)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (connection_id, user_id)
		DO UPDATE SET account = excluded.account, token = excluded.token, updated_at = now()`,
		a.ConnectionID, a.UserID, a.Account, a.Token)
	return err
}

// SetAccountToken replaces the refresh token, as when the service rotates
// it.
func (s *Store) SetAccountToken(ctx context.Context, connectionID, userID string, token []byte) error {
	_, err := s.pool.Exec(ctx, `UPDATE accounts SET token = $3, updated_at = now()
		WHERE connection_id = $1 AND user_id = $2`, connectionID, userID, token)
	return err
}

func (s *Store) Account(ctx context.Context, connectionID, userID string) (Account, error) {
	return scanAccount(s.pool.QueryRow(ctx, accountSelect+` WHERE a.connection_id = $1 AND a.user_id = $2`,
		connectionID, userID))
}

// UserAccounts are a person's sign-ins in one organisation.
func (s *Store) UserAccounts(ctx context.Context, orgID, userID string) ([]Account, error) {
	return queryAll(ctx, s, scanAccount, accountSelect+` WHERE c.org_id = $1 AND a.user_id = $2`, orgID, userID)
}

// AccountHolders are everyone signed in to a connector, once per
// organisation, as accounts with only OrgID and UserID set.
func (s *Store) AccountHolders(ctx context.Context) ([]Account, error) {
	return queryAll(ctx, s, func(row pgx.Row) (Account, error) {
		var a Account
		return a, row.Scan(&a.OrgID, &a.UserID)
	}, `SELECT DISTINCT c.org_id, a.user_id FROM accounts a JOIN connections c ON c.id = a.connection_id`)
}

func (s *Store) DeleteAccount(ctx context.Context, connectionID, userID string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM accounts WHERE connection_id = $1 AND user_id = $2`,
		connectionID, userID))
}

// DeleteUserAccounts forgets every sign-in of a person in an organisation,
// as when they leave it, and returns how many there were.
func (s *Store) DeleteUserAccounts(ctx context.Context, orgID, userID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM accounts a USING connections c
		WHERE c.id = a.connection_id AND c.org_id = $1 AND a.user_id = $2`, orgID, userID)
	return tag.RowsAffected(), err
}
