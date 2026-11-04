package gsheets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/workspace/workspacetest"
)

type sent struct {
	method, rng, query, rawPath string
	body                        map[string]any
}

func setup(t *testing.T) (*workspacetest.Fake, *Adapter, *[]sent) {
	f := workspacetest.New(t)
	f.Files["S1"] = map[string]any{"id": "S1", "name": "Budget", "mimeType": mimeType}
	f.Files["S2"] = map[string]any{"id": "S2", "name": "Other", "mimeType": mimeType}
	f.Files["DOC"] = map[string]any{"id": "DOC", "name": "Plan", "mimeType": "application/vnd.google-apps.document"}
	var calls []sent
	f.Handle("GET /v4/spreadsheets/S1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spreadsheetId":"S1","spreadsheetUrl":"https://docs.google.com/spreadsheets/d/S1/edit",
			"properties":{"title":"Budget"},"sheets":[{"properties":{"title":"2026","gridProperties":{"rowCount":1000,"columnCount":26}}}]}`))
	})
	f.Handle("/v4/spreadsheets/{id}/values/{rest}", func(w http.ResponseWriter, r *http.Request) {
		if f.Files[r.PathValue("id")] == nil {
			workspacetest.Error(w, http.StatusNotFound, "Requested entity was not found.")
			return
		}
		rng := r.PathValue("rest")
		calls = append(calls, sent{r.Method, rng, r.URL.RawQuery, r.URL.EscapedPath(), workspacetest.JSON(r)})
		switch {
		case r.Method == http.MethodGet && rng == "Big":
			rows := make([][]int, 3000)
			for i := range rows {
				rows[i] = []int{i, i}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"range": "Big!A1:B3000", "values": rows})
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"range":"2026!A1:B2","values":[["Item","Cost"],["Coffee","4.50"]]}`))
		case strings.HasSuffix(rng, ":append"):
			_, _ = w.Write([]byte(`{"updates":{"updatedRange":"2026!A3:B3","updatedRows":1,"updatedCells":2}}`))
		default:
			_, _ = w.Write([]byte(`{"updatedRange":"2026!A1:B1","updatedRows":1,"updatedCells":2}`))
		}
	})
	return f, New(f.Options()), &calls
}

func call(t *testing.T, a *Adapter, conn channels.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func TestCheckAndTarget(t *testing.T) {
	f, a, _ := setup(t)
	conn := f.Conn(channels.GoogleSheets, "")
	ctx := context.Background()
	if err := a.Check(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Scope, "auth/spreadsheets") {
		t.Errorf("scope = %q", f.Scope)
	}
	if got, err := a.Target(ctx, conn, "https://docs.google.com/spreadsheets/d/S1/edit#gid=0"); err != nil || got != "S1" {
		t.Errorf("Target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, conn, "DOC"); err == nil {
		t.Error("a document passed as a spreadsheet")
	}
}

func TestReads(t *testing.T) {
	f, a, calls := setup(t)
	conn := f.Conn(channels.GoogleSheets, "")
	out, err := call(t, a, conn, "S1", "get", `{}`)
	if err != nil || !strings.Contains(out, `"tabs":[{"title":"2026","rows":1000,"columns":26}]`) {
		t.Errorf("get = %s, %v", out, err)
	}
	out, err = call(t, a, conn, "S1", "read_range", `{"range":"2026!A1:B2"}`)
	if err != nil || out != `{"range":"2026!A1:B2","rows":[["Item","Cost"],["Coffee","4.50"]]}` {
		t.Errorf("read_range = %s, %v", out, err)
	}
	if c := (*calls)[0]; c.rng != "2026!A1:B2" || !strings.HasSuffix(c.rawPath, "2026%21A1%3AB2") && !strings.HasSuffix(c.rawPath, "2026!A1%3AB2") {
		t.Errorf("read sent %+v", c)
	}
	out, err = call(t, a, conn, "S1", "read_range", `{"range":"Big"}`)
	if err != nil || !strings.Contains(out, "only the first 2500 of 3000 rows") {
		t.Errorf("big read = %.200s, %v", out, err)
	}
	out, err = call(t, a, conn, "*", "find", `{"name":"Bud"}`)
	if err != nil || !strings.Contains(out, "Budget") || strings.Contains(out, "Plan") {
		t.Errorf("find = %s, %v", out, err)
	}
}

func TestWrites(t *testing.T) {
	f, a, calls := setup(t)
	conn := f.Conn(channels.GoogleSheets, "")
	out, err := call(t, a, conn, "S1", "write_range", `{"range":"2026!A1","rows":[["Total","=SUM(B2:B9)"]]}`)
	if err != nil || out != `{"updated_range":"2026!A1:B1","updated_rows":1,"updated_cells":2}` {
		t.Errorf("write_range = %s, %v", out, err)
	}
	if c := (*calls)[0]; c.method != http.MethodPut || !strings.Contains(c.query, "valueInputOption=USER_ENTERED") || c.body["majorDimension"] != "ROWS" {
		t.Errorf("write sent %+v", c)
	}
	out, err = call(t, a, conn, "S1", "append_rows", `{"range":"2026","rows":[["Tea",3]]}`)
	if err != nil || !strings.Contains(out, `"updated_range":"2026!A3:B3"`) {
		t.Errorf("append_rows = %s, %v", out, err)
	}
	if c := (*calls)[1]; c.method != http.MethodPost || c.rng != "2026:append" || !strings.Contains(c.query, "insertDataOption=INSERT_ROWS") {
		t.Errorf("append sent %+v", c)
	}
	out, err = call(t, a, conn, "*", "create", `{"title":"Q4","folder_id":"F","rows":[["a"]]}`)
	if err != nil || !strings.Contains(out, `"id":"NEW"`) || f.Created[0]["mimeType"] != mimeType {
		t.Errorf("create = %s, %v", out, err)
	}
	if c := (*calls)[2]; c.rng != "A1" || c.method != http.MethodPut {
		t.Errorf("create's rows sent %+v", c)
	}
	big := fmt.Sprintf(`{"range":"A1","rows":[%s["x"]]}`, strings.Repeat(`["x"],`, maxCells))
	if _, err := call(t, a, conn, "S1", "write_range", big); err == nil {
		t.Error("too many cells written")
	}
}

func TestTargetEnforced(t *testing.T) {
	f, a, calls := setup(t)
	conn := f.Conn(channels.GoogleSheets, "")
	for tool, args := range map[string]string{
		"get":         `{"spreadsheet_id":"S2"}`,
		"read_range":  `{"spreadsheet_id":"S2","range":"A1"}`,
		"write_range": `{"spreadsheet_id":"S2","range":"A1","rows":[["x"]]}`,
		"append_rows": `{"spreadsheet_id":"S2","range":"A1","rows":[["x"]]}`,
		"create":      `{"title":"x","folder_id":"F"}`,
	} {
		if _, err := call(t, a, conn, "S1", tool, args); err == nil || !strings.Contains(err.Error(), "only the spreadsheet S1") {
			t.Errorf("%s outside the target: %v", tool, err)
		}
	}
	if len(*calls) > 0 || len(f.Created) > 0 {
		t.Error("a call outside the target reached Google")
	}
	if _, err := call(t, a, conn, "S1", "read_range", `{"spreadsheet_id":"../S1","range":"A1"}`); err == nil {
		t.Error("a bad id passed")
	}
}

func TestAPIError(t *testing.T) {
	f, a, _ := setup(t)
	_, err := call(t, a, f.Conn(channels.GoogleSheets, ""), "*", "read_range", `{"spreadsheet_id":"GONE","range":"A1"}`)
	if err == nil || !strings.Contains(err.Error(), "reading the range: Google answered 404 Not Found") {
		t.Errorf("err = %v", err)
	}
}
