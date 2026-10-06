package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SSHKey is a public key a person signs in to their workspaces with.
type SSHKey struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"-"`
	UserID      string    `json:"-"`
	Name        string    `json:"name"`
	PublicKey   string    `json:"public_key"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
}

const sshKeyColumns = `id, org_id, user_id, name, public_key, fingerprint, created_at`

func scanSSHKey(row pgx.Row) (SSHKey, error) {
	var k SSHKey
	err := row.Scan(&k.ID, &k.OrgID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.CreatedAt)
	return k, notFound(err)
}

// AddSSHKey stores a key. The same key twice for one person is ErrConflict.
func (s *Store) AddSSHKey(ctx context.Context, k SSHKey) (SSHKey, error) {
	out, err := scanSSHKey(s.pool.QueryRow(ctx, `INSERT INTO ssh_keys (id, org_id, user_id, name,
		public_key, fingerprint) VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+sshKeyColumns,
		k.ID, k.OrgID, k.UserID, k.Name, k.PublicKey, k.Fingerprint))
	return out, conflict(err)
}

// SSHKeys lists one person's keys in an organisation, oldest first.
func (s *Store) SSHKeys(ctx context.Context, orgID, userID string) ([]SSHKey, error) {
	return queryAll(ctx, s, scanSSHKey, `SELECT `+sshKeyColumns+` FROM ssh_keys
		WHERE org_id = $1 AND user_id = $2 ORDER BY created_at, id`, orgID, userID)
}

// DeleteSSHKey removes one of a person's keys. Someone else's is ErrNotFound.
func (s *Store) DeleteSSHKey(ctx context.Context, orgID, userID, id string) error {
	return one(s.pool.Exec(ctx, `DELETE FROM ssh_keys WHERE id = $1 AND org_id = $2 AND user_id = $3`,
		id, orgID, userID))
}
