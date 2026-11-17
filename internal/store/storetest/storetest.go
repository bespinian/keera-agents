// Package storetest opens the test database for other packages' integration
// tests.
package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-agents/internal/store"
)

// Open connects to KEERA_AGENTS_TEST_DATABASE_URL, or skips. It starts from
// an empty schema, so a test sees only its own rows. The address comes back
// too, for tests that run SQL of their own.
func Open(t testing.TB) (*store.Store, string) {
	t.Helper()
	dsn := os.Getenv("KEERA_AGENTS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KEERA_AGENTS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	_ = db.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st, dsn
}
