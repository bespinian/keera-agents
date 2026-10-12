// Package gsheets lets agents find, read and write Google Sheets as a
// service account, as a user of the domain through domain-wide delegation,
// or as each person who signs in with their own account.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "drive": the base of the Drive API, default https://www.googleapis.com
//   - "sheets": the base of the Sheets API, default https://sheets.googleapis.com
package gsheets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
	"github.com/bespinian/keera-agents/internal/channels/internal/workspace"
)

const (
	scope    = "https://www.googleapis.com/auth/spreadsheets"
	mimeType = "application/vnd.google-apps.spreadsheet"
	// maxCells and maxBytes bound what read_range returns, so a whole sheet
	// does not fill the model's context, and fits in the 64 KiB a tool may
	// answer. maxCells bounds a write too.
	maxCells = 5_000
	maxBytes = 50_000
)

type Adapter struct {
	files  workspace.Files
	sheets string
	apitool.Set
	workspace.Accounts
}

const idParam = `"spreadsheet_id":{"type":"string","description":"The spreadsheet's id or link. Optional when the connector is bound to one spreadsheet."}`

const rowsParam = `"rows":{"type":"array","description":"Rows of cells, as you would type them: numbers, dates and formulas starting with = are read as such.",
	"items":{"type":"array","items":{"type":"string"}}}`

func New(opts channels.Options) *Adapter {
	a := &Adapter{
		files: workspace.Files{
			Client:   workspace.NewClient(opts, scope, workspace.DriveMetadata, workspace.DriveFile),
			MimeType: mimeType, Noun: "spreadsheet", Arg: "spreadsheet_id",
		},
		sheets: web.Endpoint(opts, "sheets", "https://sheets.googleapis.com"),
	}
	a.Set = apitool.Set{
		a.files.FindTool(),
		{
			Tool: channels.Tool{
				Name:        "get",
				Description: "Get a Google Sheet's title and its tabs, with how many rows and columns each has.",
				Parameters:  apitool.Schema(`{"type":"object","properties":{` + idParam + `}}`),
			},
			Run: a.get,
		},
		{
			Tool: channels.Tool{
				Name:        "read_range",
				Description: fmt.Sprintf("Read the cells of a range, as rows of their shown values. At most %d cells, fewer when they are long.", maxCells),
				Parameters: apitool.Schema(`{"type":"object","properties":{` + idParam + `,
					"range":{"type":"string","description":"In A1 notation, such as Sheet1!A1:D20, or a tab's name for all of it."}},
					"required":["range"]}`),
			},
			Run: a.readRange,
		},
		{
			Tool: channels.Tool{
				Name:        "write_range",
				Description: "Write rows of cells into a range, starting at its top left cell, replacing what is there.",
				Parameters: apitool.Schema(`{"type":"object","properties":{` + idParam + `,
					"range":{"type":"string","description":"In A1 notation, such as Sheet1!A1, or Sheet1!A1:C3."},` + rowsParam + `},
					"required":["range","rows"]}`),
				Write: true,
			},
			Run: a.writeRange,
		},
		{
			Tool: channels.Tool{
				Name:        "append_rows",
				Description: "Add rows below the table in a range, such as a tab's name.",
				Parameters: apitool.Schema(`{"type":"object","properties":{` + idParam + `,
					"range":{"type":"string","description":"The table, in A1 notation, such as Sheet1 or Sheet1!A:D."},` + rowsParam + `},
					"required":["range","rows"]}`),
				Write: true,
			},
			Run: a.appendRows,
		},
		{
			Tool: channels.Tool{
				Name: "create",
				Description: "Create a Google Sheet. Only for a connector bound to every spreadsheet. " +
					"Give a folder unless the connector acts as a user: a service account has no storage of its own.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"title":{"type":"string"},
					"folder_id":{"type":"string","description":"The id or link of the Drive folder to create it in."},` +
					strings.Replace(rowsParam, "Rows of cells", "Optional first rows of cells", 1) + `},
					"required":["title"]}`),
				Write: true,
			},
			Run: a.create,
		},
	}
	a.Accounts = workspace.NewAccounts(a.files.Client, "Google Sheets API and the Google Drive API")
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.GoogleSheets,
		Label:    "Google Sheets",
		Vendor:   "Google",
		Category: "Documents",
		Summary:  "Agents find, read and write Google Sheets: read and write ranges, add rows and create spreadsheets.",
		Fields:   workspace.Fields("Google Sheets API"),
		Setup: workspace.Setup("Google Sheets API and the Google Drive API",
			"the spreadsheets, or a folder holding them, as Editor for agents that write,",
			scope, workspace.DriveMetadata, workspace.DriveFile),
		TargetLabel: "Spreadsheet",
		TargetHelp:  "A spreadsheet's link or id, or * for every spreadsheet the connector can see.",
		AnyTarget:   true,
	}
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	return a.files.Check(ctx, conn, "Google Drive API", nil)
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	return a.files.Target(ctx, conn, target)
}

type rangeArgs struct {
	SpreadsheetID string  `json:"spreadsheet_id"`
	Range         string  `json:"range"`
	Rows          [][]any `json:"rows"`
}

// open reads a tool's arguments and settings, and the spreadsheet they name.
func (a *Adapter) open(conn channels.Connection, target string, args json.RawMessage) (rangeArgs, workspace.Settings, string, error) {
	in, err := apitool.Decode[rangeArgs](args)
	if err != nil {
		return in, workspace.Settings{}, "", err
	}
	s, err := workspace.SettingsOf(conn)
	if err != nil {
		return in, s, "", err
	}
	id, err := a.files.Pick(target, in.SpreadsheetID)
	return in, s, id, err
}

// valuesURL is the values of rng. The range is a path segment, so every
// character with a meaning there is escaped, colons too.
func (a *Adapter) valuesURL(id, rng, suffix string, q url.Values) string {
	return a.sheets + "/v4/spreadsheets/" + id + "/values/" +
		strings.ReplaceAll(url.PathEscape(rng), ":", "%3A") + suffix + "?" + q.Encode()
}

type tabView struct {
	Title   string `json:"title"`
	Rows    int    `json:"rows"`
	Columns int    `json:"columns"`
}

func (a *Adapter) get(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
	_, s, id, err := a.open(conn, target, args)
	if err != nil {
		return nil, err
	}
	var r struct {
		SpreadsheetID  string `json:"spreadsheetId"`
		SpreadsheetURL string `json:"spreadsheetUrl"`
		Properties     struct {
			Title string `json:"title"`
		} `json:"properties"`
		Sheets []struct {
			Properties struct {
				Title          string `json:"title"`
				GridProperties struct {
					RowCount    int `json:"rowCount"`
					ColumnCount int `json:"columnCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	q := url.Values{"fields": {"spreadsheetId,spreadsheetUrl,properties.title,sheets.properties(title,gridProperties)"}}
	if err := a.files.Do(ctx, s, http.MethodGet, a.sheets+"/v4/spreadsheets/"+id+"?"+q.Encode(), nil, &r); err != nil {
		return nil, fmt.Errorf("reading the spreadsheet: %w", err)
	}
	tabs := []tabView{}
	for _, sh := range r.Sheets {
		p := sh.Properties
		tabs = append(tabs, tabView{Title: p.Title, Rows: p.GridProperties.RowCount, Columns: p.GridProperties.ColumnCount})
	}
	return map[string]any{"id": r.SpreadsheetID, "title": r.Properties.Title, "url": r.SpreadsheetURL, "tabs": tabs}, nil
}

func (a *Adapter) readRange(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
	in, s, id, err := a.open(conn, target, args)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("range", in.Range); err != nil {
		return nil, err
	}
	var r struct {
		Range  string  `json:"range"`
		Values [][]any `json:"values"`
	}
	if err := a.files.Do(ctx, s, http.MethodGet, a.valuesURL(id, in.Range, "", url.Values{"majorDimension": {"ROWS"}}), nil, &r); err != nil {
		return nil, fmt.Errorf("reading the range: %w", err)
	}
	rows, cells, size, cut := [][]any{}, 0, 0, false
	for _, row := range r.Values {
		b, _ := json.Marshal(row)
		if cells+len(row) > maxCells || size+len(b) > maxBytes {
			cut = true
			break
		}
		cells, size = cells+len(row), size+len(b)
		rows = append(rows, row)
	}
	out := map[string]any{"range": r.Range, "rows": rows}
	if cut {
		out["truncated"] = fmt.Sprintf("only the first %d of %d rows; read the rest in smaller ranges", len(rows), len(r.Values))
	}
	return out, nil
}

// checkRows refuses no rows, and more cells than a read could return.
func checkRows(rows [][]any) error {
	if len(rows) == 0 {
		return apitool.Need("rows", "")
	}
	n := 0
	for _, r := range rows {
		n += len(r)
	}
	if n > maxCells {
		return fmt.Errorf("that is %d cells; write at most %d at once", n, maxCells)
	}
	return nil
}

type update struct {
	UpdatedRange string `json:"updated_range"`
	UpdatedRows  int    `json:"updated_rows"`
	UpdatedCells int    `json:"updated_cells"`
}

type updateReply struct {
	UpdatedRange string `json:"updatedRange"`
	UpdatedRows  int    `json:"updatedRows"`
	UpdatedCells int    `json:"updatedCells"`
}

func (u updateReply) view() update {
	return update(u)
}

func (a *Adapter) write(ctx context.Context, s workspace.Settings, id, rng string, rows [][]any) (update, error) {
	var r updateReply
	err := a.files.Do(ctx, s, http.MethodPut, a.valuesURL(id, rng, "", url.Values{"valueInputOption": {"USER_ENTERED"}}),
		map[string]any{"range": rng, "majorDimension": "ROWS", "values": rows}, &r)
	return r.view(), err
}

func (a *Adapter) writeRange(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
	in, s, id, err := a.open(conn, target, args)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("range", in.Range); err != nil {
		return nil, err
	}
	if err := checkRows(in.Rows); err != nil {
		return nil, err
	}
	u, err := a.write(ctx, s, id, in.Range, in.Rows)
	if err != nil {
		return nil, fmt.Errorf("writing the range: %w", err)
	}
	return u, nil
}

func (a *Adapter) appendRows(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
	in, s, id, err := a.open(conn, target, args)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("range", in.Range); err != nil {
		return nil, err
	}
	if err := checkRows(in.Rows); err != nil {
		return nil, err
	}
	var r struct {
		Updates updateReply `json:"updates"`
	}
	q := url.Values{"valueInputOption": {"USER_ENTERED"}, "insertDataOption": {"INSERT_ROWS"}}
	err = a.files.Do(ctx, s, http.MethodPost, a.valuesURL(id, in.Range, ":append", q),
		map[string]any{"majorDimension": "ROWS", "values": in.Rows}, &r)
	if err != nil {
		return nil, fmt.Errorf("adding the rows: %w", err)
	}
	return r.Updates.view(), nil
}

func (a *Adapter) create(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Title    string  `json:"title"`
		FolderID string  `json:"folder_id"`
		Rows     [][]any `json:"rows"`
	}](args)
	if err != nil {
		return nil, err
	}
	s, err := workspace.SettingsOf(conn)
	if err != nil {
		return nil, err
	}
	if len(in.Rows) > 0 {
		if err := checkRows(in.Rows); err != nil {
			return nil, err
		}
	}
	f, err := a.files.Create(ctx, s, target, in.Title, in.FolderID)
	if err != nil {
		return nil, err
	}
	if len(in.Rows) > 0 {
		// A1 alone is the first tab's.
		if _, err := a.write(ctx, s, f.ID, "A1", in.Rows); err != nil {
			return nil, fmt.Errorf("created %s, but writing its rows failed: %w", f.ID, err)
		}
	}
	return f, nil
}
