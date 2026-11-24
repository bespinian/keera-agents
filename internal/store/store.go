// Package store keeps keera-agents' state in Postgres: agents and
// workspaces, connections, bindings, schedules, squads, tasks, SSH keys and
// the audit log, people's sign-ins to connectors, the files agents wrote
// and the machine time to bill. People, organisations, projects, models and
// keys live in Keera Gateway; rows here name them by the gateway's ids.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// renamedMigrations maps a migration's shipped name to its current one.
var renamedMigrations = map[string]string{
	"0002_retries.sql": "0002_v0.2.0.sql",
}

var (
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is a unique constraint, such as an agent name taken in the
	// organisation.
	ErrConflict = errors.New("store: already exists")
	// ErrNewerSchema is a database migrated by a newer release.
	ErrNewerSchema = errors.New("store: the database is newer than this version")
)

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse the database URL: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 10 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies any embedded migration the database has not seen, under an
// advisory lock so several replicas can start at once. A migration file is
// never edited once released; the next release gets a new one.
func (s *Store) Migrate(ctx context.Context) (applied []string, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	const lockID = 0x6A6_0001 // arbitrary, but stable across versions
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockID)
		if err == nil && uerr != nil {
			err = fmt.Errorf("advisory unlock: %w", uerr)
		}
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	// Migration files are named after their release. Some shipped under
	// another name, so a database that ran them records the old one.
	for old, name := range renamedMigrations {
		if _, err := conn.Exec(ctx,
			"UPDATE schema_migrations SET version = $2 WHERE version = $1", old, name,
		); err != nil {
			return nil, fmt.Errorf("rename migration %s: %w", old, err)
		}
	}

	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)

	// A database a newer release migrated has tables this one does not
	// know, and rows it would misread. Running against it, as after
	// rolling back the binary alone, could damage it.
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	known, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, v := range known {
		if !slices.Contains(names, v) {
			return nil, fmt.Errorf("%w: the database has migration %s, which this version does not know. "+
				"Run the version that applied it, or restore the backup taken before upgrading", ErrNewerSchema, v)
		}
	}

	for _, name := range names {
		var seen bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name,
		).Scan(&seen); err != nil {
			return nil, err
		}
		if seen {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return applied, err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name)
			return err
		}); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}

// TryLock takes a session-level advisory lock if it is free and returns its
// release, so one replica runs a periodic job at a time.
func (s *Store) TryLock(ctx context.Context, id int64) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", id).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", id)
		conn.Release()
	}, true, nil
}

// notFound turns pgx's no-rows into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// conflict turns a unique violation into ErrConflict.
func conflict(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}

// one checks that a statement changed exactly one row.
func one(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return conflict(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// queryAll reads every row of a query with scan. It never returns a nil
// slice, so the API answers [] and not null.
func queryAll[T any](ctx context.Context, s *Store, scan func(pgx.Row) (T, error), sql string, args ...any) ([]T, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
	if err != nil {
		return nil, err
	}
	return emptyIfNil(out), nil
}

// countBy reads rows of an id and a count into a map.
func (s *Store) countBy(ctx context.Context, sql string, args ...any) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	var id string
	var n int
	_, err = pgx.ForEachRow(rows, []any{&id, &n}, func() error {
		out[id] = n
		return nil
	})
	return out, err
}

func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
