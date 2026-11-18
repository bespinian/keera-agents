package postgres

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-agents/internal/connectors"
)

// testDB connects to KEERA_AGENTS_TEST_DATABASE_URL as its superuser, makes
// a schema and a role of agents' own, and returns a connection as that role.
func testDB(t *testing.T) (connectors.Connection, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("KEERA_AGENTS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KEERA_AGENTS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS pgtest CASCADE`,
		`DROP ROLE IF EXISTS pgtest_agents`,
		`CREATE ROLE pgtest_agents LOGIN PASSWORD 'agents-pw'`,
		`CREATE SCHEMA pgtest`,
		`CREATE TABLE pgtest.orders (id int PRIMARY KEY, customer text NOT NULL, total numeric(10,2), note text)`,
		`COMMENT ON TABLE pgtest.orders IS 'Every order'`,
		`INSERT INTO pgtest.orders VALUES (1, 'Acme', 12.50, NULL), (2, 'Globex', 99.00, 'rush'), (3, 'Initech', 7.25, NULL)`,
		`CREATE TABLE pgtest.secrets (v text)`,
		`GRANT USAGE ON SCHEMA pgtest TO pgtest_agents`,
		`GRANT SELECT, UPDATE ON pgtest.orders TO pgtest_agents`,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return connAs(t, "pgtest_agents", "agents-pw"), admin
}

// connAs is a connection to the test database as user, or as its superuser
// when user is empty.
func connAs(t *testing.T, user, password string) connectors.Connection {
	t.Helper()
	u, err := url.Parse(os.Getenv("KEERA_AGENTS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if user == "" {
		user = u.User.Username()
		password, _ = u.User.Password()
	}
	return connectors.Connection{Kind: connectors.Postgres, Config: map[string]string{
		"host": u.Hostname(), "port": u.Port(), "database": strings.TrimPrefix(u.Path, "/"),
		"user": user, "sslmode": "disable",
	}, Secrets: map[string]string{"password": password}}
}

func call(t *testing.T, a *Adapter, conn connectors.Connection, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, "*", tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func TestDatabase(t *testing.T) {
	conn, admin := testDB(t)
	a := New(connectors.Options{})
	ctx := context.Background()

	if err := a.Check(ctx, conn); err != nil {
		t.Fatalf("Check = %v", err)
	}
	wrong := conn
	wrong.Secrets = map[string]string{"password": "nope"}
	if err := a.Check(ctx, wrong); err == nil || !strings.Contains(err.Error(), "refused the user or password") ||
		strings.Contains(err.Error(), "nope") {
		t.Errorf("a wrong password: %v", err)
	}
	if err := a.Check(ctx, connAs(t, "", "")); err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Errorf("a superuser: %v", err)
	}

	out, err := call(t, a, conn, "list_tables", `{"schema":"pgtest"}`)
	if err != nil || !strings.Contains(out, `"name":"orders"`) || !strings.Contains(out, "Every order") || strings.Contains(out, "secrets") {
		t.Errorf("list_tables = %s, %v", out, err)
	}
	out, err = call(t, a, conn, "describe_table", `{"table":"pgtest.orders"}`)
	if err != nil || !strings.Contains(out, `"type":"numeric(10,2)"`) || !strings.Contains(out, "PRIMARY KEY (id)") {
		t.Errorf("describe_table = %s, %v", out, err)
	}
	if _, err := call(t, a, conn, "describe_table", `{"table":"x'); DROP TABLE pgtest.orders; --"}`); err == nil {
		t.Error("a table name with SQL in it passed")
	}

	out, err = call(t, a, conn, "query", `{"sql":"SELECT id, total, note FROM pgtest.orders ORDER BY id","limit":2}`)
	if err != nil || out != `{"columns":[{"name":"id","type":"int4"},{"name":"total","type":"numeric"},{"name":"note","type":"text"}],`+
		`"rows":[["1","12.50",null],["2","99.00","rush"]],"more":true,"command":"SELECT 3"}` {
		t.Errorf("query = %s, %v", out, err)
	}
	// A result stops at its size, however many rows were asked for.
	out, err = call(t, a, conn, "query", `{"sql":"SELECT repeat('x', 5000) a, repeat('y', 5000) b FROM generate_series(1, 1000)","limit":1000}`)
	if err != nil || !strings.Contains(out, `"more":true`) || len(out) > 2*maxResult {
		t.Errorf("a large result: %d bytes, %v", len(out), err)
	}
	if _, err := call(t, a, conn, "query", `{"sql":"SELECT repeat('x', 10000000)"}`); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Errorf("a row past the message limit: %v", err)
	}
	// A query only reads, even one that tries to write or to end its
	// transaction first.
	for _, sql := range []string{
		"UPDATE pgtest.orders SET note = 'x'",
		"COMMIT; UPDATE pgtest.orders SET note = 'x'",
		"SET TRANSACTION READ WRITE; UPDATE pgtest.orders SET note = 'x'",
	} {
		if _, err := call(t, a, conn, "query", `{"sql":"`+sql+`"}`); err == nil {
			t.Errorf("query wrote: %s", sql)
		}
	}
	if _, err := call(t, a, conn, "query", `{"sql":"SELECT * FROM pgtest.secrets"}`); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("a table not granted: %v", err)
	}

	out, err = call(t, a, conn, "execute", `{"sql":"UPDATE pgtest.orders SET note = 'paid' WHERE id = 1 RETURNING id, note"}`)
	if err != nil || !strings.Contains(out, `"rows":[["1","paid"]]`) || !strings.Contains(out, `"command":"UPDATE 1"`) {
		t.Errorf("execute = %s, %v", out, err)
	}
	var note string
	if err := admin.QueryRow(ctx, `SELECT note FROM pgtest.orders WHERE id = 1`).Scan(&note); err != nil || note != "paid" {
		t.Errorf("the update was not committed: %q, %v", note, err)
	}
	if _, err := call(t, a, conn, "execute", `{"sql":"DELETE FROM pgtest.orders"}`); err == nil {
		t.Error("a delete the role may not do passed")
	}
}
