// Package postgres lets agents query a PostgreSQL database with SQL, read
// its tables' structure, and, on a binding that may write, change data.
//
// A connector reaches what its database role may, so the role's grants are
// the limit: there is nothing to narrow a binding to. A binding that only
// reads runs every statement in a read-only transaction, in a session whose
// transactions are read-only too, so PostgreSQL itself refuses writes.
//
// Every call opens a connection of its own and runs one statement, with a
// timeout. The extended protocol takes only one statement, so a query
// cannot end the transaction and go on. Connections dial through the
// server's egress guard, by host name, as HTTP calls do.
//
// It has no endpoints to override: tests set the connection's host.
package postgres

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const (
	// timeout bounds each statement, and connecting.
	timeout = 30 * time.Second
	// maxCell bounds one value in a result, in bytes, so one large
	// JSON document does not fill the model's context.
	maxCell = 2000
	// maxResult bounds a result's cells together. Cut cells alone are not
	// enough: a query can return hundreds of wide columns, a thousand times.
	maxResult = 1 << 20
	// maxMessage bounds one message from the server, such as a row, before
	// it is cut. Without it a single huge value is read whole.
	maxMessage = 8 << 20
)

type Adapter struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	apitool.Set
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{dial: opts.Dial}
	if a.dial == nil {
		a.dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	a.Set = a.toolSet()
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Postgres,
		Label:    "PostgreSQL",
		Vendor:   "PostgreSQL",
		Category: "Data",
		Summary:  "Agents query a PostgreSQL database with SQL and read its tables' structure, and may change data.",
		Fields: []connectors.Field{
			{
				Name: "host", Label: "Host", Required: true, Endpoint: true,
				Help: "The database server's name or address, such as db.example.com.",
			},
			{Name: "port", Label: "Port", Endpoint: true, Help: "Empty for 5432."},
			{Name: "database", Label: "Database", Required: true},
			{Name: "user", Label: "User", Required: true, Help: "A role made for your agents."},
			{Name: "password", Label: "Password", Secret: true, Required: true},
			{
				Name: "sslmode", Label: "TLS", Endpoint: true,
				Help: "Empty or verify-full to require TLS and check the server's certificate, " +
					"or disable for a server inside your network without TLS.",
			},
			{
				Name: "ca_cert", Label: "CA certificate", Multiline: true, Endpoint: true,
				Help: "The CA of the server's certificate in PEM, if it is not from a public CA.",
			},
		},
		Setup: "1. Create a role for your agents, such as `CREATE ROLE keera_agents LOGIN PASSWORD '…'`.\n" +
			"2. Grant it only what agents may see: `GRANT USAGE ON SCHEMA shop TO keera_agents` and " +
			"`GRANT SELECT ON ALL TABLES IN SCHEMA shop TO keera_agents`. Grant INSERT, UPDATE or DELETE " +
			"only on the tables agents may change. Never make it a superuser.\n" +
			"3. Best, point the connector at a read replica, so a heavy query does not slow the primary.\n" +
			"4. If the server is inside your network, ask your operator to name its host in KEERA_AGENTS_UPSTREAM_PRIVATE.\n" +
			"5. A binding that may only read runs every query read-only. The role's grants are the limit for the rest.",
	}
}

// Target is always everything: a kind with no TargetLabel has nothing to
// narrow to, and the API binds it to AnyTarget.
func (a *Adapter) Target(context.Context, connectors.Connection, string) (string, error) {
	return connectors.AnyTarget, nil
}

// config reads a connection's settings into pgx's.
func (a *Adapter) config(conn connectors.Connection, readOnly bool) (*pgx.ConnConfig, error) {
	host := web.Value(conn, "host")
	port := cmp.Or(web.Value(conn, "port"), "5432")
	db, user, password := web.Value(conn, "database"), web.Value(conn, "user"), web.Value(conn, "password")
	mode := cmp.Or(web.Value(conn, "sslmode"), "verify-full")
	switch {
	case host == "" || db == "" || user == "" || password == "":
		return nil, errors.New("the host, database, user and password are all needed")
	case strings.ContainsAny(host, "/:, ") && net.ParseIP(host) == nil:
		return nil, fmt.Errorf("%q is not a host name; give the port on its own", web.Short(host, 80))
	case mode != "verify-full" && mode != "disable":
		return nil, fmt.Errorf("TLS must be verify-full or disable, not %q", web.Short(mode, 20))
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("%q is not a port", web.Short(port, 20))
	}
	// TLS is set below, from the connection alone, never from the
	// control plane's own PG* variables.
	u := url.URL{
		Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort(host, port),
		Path: "/" + db, RawQuery: "sslmode=disable",
	}
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		// pgx's error quotes the address, password and all.
		return nil, errors.New("the connection settings cannot be read")
	}
	cfg.Fallbacks = nil
	if mode == "verify-full" {
		cfg.TLSConfig = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if ca := web.Value(conn, "ca_cert"); ca != "" {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM([]byte(ca)) {
				return nil, errors.New("the CA certificate is not PEM")
			}
			cfg.TLSConfig.RootCAs = roots
		}
	}
	cfg.ConnectTimeout = timeout
	cfg.MaxProtocolMessageBodyLen = maxMessage
	// The egress guard checks the name before DNS, so it gets the name.
	cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	cfg.DialFunc = a.dial
	cfg.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.RuntimeParams = map[string]string{
		"application_name":                    "keera-agents",
		"statement_timeout":                   strconv.Itoa(int(timeout.Milliseconds())),
		"idle_in_transaction_session_timeout": strconv.Itoa(int(timeout.Milliseconds())),
	}
	if readOnly {
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return cfg, nil
}

// connect opens a connection for one call.
func (a *Adapter) connect(ctx context.Context, conn connectors.Connection, readOnly bool) (*pgx.Conn, error) {
	cfg, err := a.config(conn, readOnly)
	if err != nil {
		return nil, err
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, connectError(err)
	}
	return c, nil
}

// connectError says why connecting failed in words an administrator can act
// on. pgx's own message names the user and host, never the password.
func connectError(err error) error {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pe.Code {
		case "28P01", "28000":
			return fmt.Errorf("PostgreSQL refused the user or password (%s)", pe.Message)
		case "3D000":
			return fmt.Errorf("PostgreSQL has no such database (%s)", pe.Message)
		}
		return describe(pe)
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return fmt.Errorf("the server's TLS certificate is not trusted; give its CA certificate: %w", err)
	}
	return fmt.Errorf("could not reach PostgreSQL: %w", err)
}

// describe turns a server error into what the model needs to fix its SQL.
func describe(pe *pgconn.PgError) error {
	msg := "PostgreSQL error " + pe.Code + ": " + pe.Message
	if pe.Detail != "" {
		msg += ". " + pe.Detail
	}
	if pe.Hint != "" {
		msg += ". Hint: " + pe.Hint
	}
	if pe.Position > 0 {
		msg += fmt.Sprintf(" (at character %d)", pe.Position)
	}
	return errors.New(msg)
}

// sqlError describes a failed statement.
func sqlError(err error) error {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return describe(pe)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("the statement took longer than %s; ask for less", timeout)
	}
	if _, ok := errors.AsType[*pgproto3.ExceededMaxBodyLenErr](err); ok {
		return fmt.Errorf("a row is larger than %d MiB; select fewer or shorter columns", maxMessage>>20)
	}
	return err
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	c, err := a.connect(ctx, conn, true)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var super bool
	if err := c.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		return sqlError(err)
	}
	if super {
		return errors.New("the user is a superuser, who may do anything, even with a read-only binding; give agents a role of their own")
	}
	return nil
}

// Result is what a statement returned: its columns and rows as text, as
// psql shows them, with NULL as null.
type Result struct {
	Columns []Column `json:"columns,omitempty"`
	Rows    [][]any  `json:"rows,omitempty"`
	// More says rows were left out after the limit.
	More bool `json:"more,omitempty"`
	// Command is the server's summary, such as UPDATE 3, or SELECT 250
	// for a query cut at its limit.
	Command string `json:"command,omitempty"`
}

type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// run runs one statement in a transaction, read-only unless write, and
// reads at most limit rows. A write is committed only if it succeeded.
func (a *Adapter) run(ctx context.Context, conn connectors.Connection, write bool, sql string, limit int, args ...any) (Result, error) {
	if strings.TrimSpace(sql) == "" {
		return Result{}, errors.New("give the SQL to run")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	c, err := a.connect(ctx, conn, !write)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	mode := pgx.ReadOnly
	if write {
		mode = pgx.ReadWrite
	}
	tx, err := c.BeginTx(ctx, pgx.TxOptions{AccessMode: mode})
	if err != nil {
		return Result{}, sqlError(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	res, err := read(ctx, tx, sql, limit, args)
	if err != nil {
		return Result{}, sqlError(err)
	}
	if write {
		if err := tx.Commit(ctx); err != nil {
			return Result{}, sqlError(err)
		}
	}
	return res, nil
}

// read runs sql and reads its rows as text.
func read(ctx context.Context, tx pgx.Tx, sql string, limit int, args []any) (Result, error) {
	rows, err := tx.Query(ctx, sql, append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, args...)...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	var res Result
	size := 0
	for _, f := range rows.FieldDescriptions() {
		typ := "oid " + strconv.FormatUint(uint64(f.DataTypeOID), 10)
		if t, ok := tx.Conn().TypeMap().TypeForOID(f.DataTypeOID); ok {
			typ = t.Name
		}
		res.Columns = append(res.Columns, Column{Name: f.Name, Type: typ})
	}
	for rows.Next() {
		if len(res.Rows) == limit || size > maxResult {
			res.More = true
			break
		}
		row := make([]any, len(res.Columns))
		for i, v := range rows.RawValues() {
			if v != nil {
				cell := web.Cut(string(v[:min(len(v), maxCell+1)]), maxCell, "…")
				size += len(cell)
				row[i] = cell
			}
		}
		res.Rows = append(res.Rows, row)
	}
	// Closing ends the statement when the rest is not read, and gives its
	// error and command tag.
	rows.Close()
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	res.Command = rows.CommandTag().String()
	return res, nil
}
