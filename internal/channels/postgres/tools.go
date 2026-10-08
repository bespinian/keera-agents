package postgres

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
)

// tables lists what the role may read, outside PostgreSQL's own schemas.
const tables = `SELECT n.nspname AS schema, c.relname AS name,
	CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view'
		WHEN 'm' THEN 'materialized view' WHEN 'f' THEN 'foreign table' END AS kind,
	CASE WHEN c.reltuples >= 0 THEN c.reltuples::bigint END AS estimated_rows,
	obj_description(c.oid, 'pg_class') AS comment
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
		AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'
		AND NOT c.relispartition
		AND has_table_privilege(c.oid, 'SELECT')
		AND ($1 = '' OR n.nspname = $1)
	ORDER BY 1, 2`

// columns describes a table's columns. $1 is resolved as a name, never
// spliced into SQL.
const columns = `SELECT a.attname AS name, format_type(a.atttypid, a.atttypmod) AS type,
	NOT a.attnotnull AS nullable, pg_get_expr(d.adbin, d.adrelid) AS default,
	col_description(a.attrelid, a.attnum) AS comment
	FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
	WHERE a.attrelid = $1::text::regclass AND a.attnum > 0 AND NOT a.attisdropped
	ORDER BY a.attnum`

// constraints are a table's keys and checks, as PostgreSQL writes them.
const constraints = `SELECT conname AS name, pg_get_constraintdef(oid) AS definition
	FROM pg_constraint WHERE conrelid = $1::text::regclass ORDER BY contype, conname`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{Tool: channels.Tool{
			Name:        "list_tables",
			Description: "List the tables and views you may read, with their estimated row counts and comments.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"schema":{"type":"string","description":"Only this schema, such as public."}}}`),
		}, Run: a.listTables},
		{Tool: channels.Tool{
			Name:        "describe_table",
			Description: "Describe a table or view: its columns with their types, nullability, defaults and comments, and its keys and checks.",
			Parameters: apitool.Schema(`{"type":"object","required":["table"],"properties":{
				"table":{"type":"string","description":"The table's name, with its schema if it is not on the search path, such as shop.orders."}}}`),
		}, Run: a.describeTable},
		{Tool: channels.Tool{
			Name: "query",
			Description: "Run one read-only SQL statement, such as a SELECT, and return its rows as text. " +
				"Values are as psql shows them, and NULL is null. Add ORDER BY and LIMIT, and aggregate in SQL rather than reading many rows.",
			Parameters: apitool.Schema(`{"type":"object","required":["sql"],"properties":{
				"sql":{"type":"string","description":"One statement, without a trailing semicolon needed."},
				"limit":{"type":"integer","description":"Rows to return at most, 100 by default, up to 1000."}}}`),
		}, Run: a.query},
		{Tool: channels.Tool{
			Name: "execute",
			Description: "Run one SQL statement that changes data, such as an INSERT, UPDATE or DELETE, and commit it. " +
				"It returns how many rows it changed, and the rows of a RETURNING clause. Check with query first what it will change.",
			Parameters: apitool.Schema(`{"type":"object","required":["sql"],"properties":{
				"sql":{"type":"string","description":"One statement."}}}`),
			Write: true,
		}, Run: a.execute},
	}
}

func (a *Adapter) listTables(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Schema string `json:"schema"`
	}](args)
	if err != nil {
		return nil, err
	}
	res, err := a.run(ctx, conn, false, tables, 1000, strings.TrimSpace(in.Schema))
	return objects(res), err
}

// objects are a result's rows as objects by column, for the catalogue's
// own queries, whose columns the model does not choose.
func objects(res Result) []map[string]any {
	out := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		o := map[string]any{}
		for i, v := range row {
			if v != nil {
				o[res.Columns[i].Name] = v
			}
		}
		out = append(out, o)
	}
	return out
}

func (a *Adapter) describeTable(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Table string `json:"table"`
	}](args)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("table", in.Table); err != nil {
		return nil, err
	}
	table := strings.TrimSpace(in.Table)
	cols, err := a.run(ctx, conn, false, columns, 1000, table)
	if err != nil {
		return nil, err
	}
	keys, err := a.run(ctx, conn, false, constraints, 1000, table)
	if err != nil {
		return nil, err
	}
	return map[string]any{"table": table, "columns": objects(cols), "constraints": objects(keys)}, nil
}

func (a *Adapter) query(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		SQL   string `json:"sql"`
		Limit int    `json:"limit"`
	}](args)
	if err != nil {
		return nil, err
	}
	return a.run(ctx, conn, false, in.SQL, apitool.Limit(in.Limit, 100, 1000))
}

func (a *Adapter) execute(ctx context.Context, conn channels.Connection, _ string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		SQL string `json:"sql"`
	}](args)
	if err != nil {
		return nil, err
	}
	return a.run(ctx, conn, true, in.SQL, 100)
}
