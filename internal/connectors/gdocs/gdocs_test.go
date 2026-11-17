package gdocs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace/workspacetest"
)

const docJSON = `{"documentId":"DOC","title":"Plan","tabs":[{"tabProperties":{"title":"Tab 1"},"documentTab":{
	"lists":{"L1":{"listProperties":{"nestingLevels":[{"glyphType":"DECIMAL"}]}},"L2":{"listProperties":{"nestingLevels":[{"glyphSymbol":"●"}]}}},
	"body":{"content":[
		{"sectionBreak":{}},
		{"paragraph":{"paragraphStyle":{"namedStyleType":"HEADING_1"},"elements":[{"textRun":{"content":"Goals\n"}}]}},
		{"paragraph":{"paragraphStyle":{"namedStyleType":"NORMAL_TEXT"},"elements":[{"textRun":{"content":"Ship it "}},{"person":{"personProperties":{"name":"Ada","email":"ada@acme.com"}}},{"textRun":{"content":"\n"}}]}},
		{"paragraph":{"bullet":{"listId":"L1"},"elements":[{"textRun":{"content":"First\n"}}]}},
		{"paragraph":{"bullet":{"listId":"L2","nestingLevel":1},"elements":[{"textRun":{"content":"Point\n"}}]}},
		{"table":{"tableRows":[{"tableCells":[
			{"content":[{"paragraph":{"elements":[{"textRun":{"content":"a\n"}}]}}]},
			{"content":[{"paragraph":{"elements":[{"textRun":{"content":"b\n"}}]}}]}]}]}}
	]}}}]}`

func setup(t *testing.T) (*workspacetest.Fake, *Adapter, *[]map[string]any) {
	f := workspacetest.New(t)
	f.Files["DOC"] = map[string]any{"id": "DOC", "name": "Plan", "mimeType": mimeType, "webViewLink": "https://docs.google.com/document/d/DOC/edit"}
	f.Files["OTHER"] = map[string]any{"id": "OTHER", "name": "Notes", "mimeType": mimeType}
	f.Files["SHEET"] = map[string]any{"id": "SHEET", "name": "Budget", "mimeType": "application/vnd.google-apps.spreadsheet"}
	var updates []map[string]any
	f.Handle("GET /v1/documents/DOC", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("includeTabsContent") != "true" {
			t.Error("tabs not asked for")
		}
		_, _ = w.Write([]byte(docJSON))
	})
	f.Handle("POST /v1/documents/{rest}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutSuffix(r.PathValue("rest"), ":batchUpdate")
		if !ok || f.Files[id] == nil {
			workspacetest.Error(w, http.StatusNotFound, "Requested entity was not found.")
			return
		}
		in := workspacetest.JSON(r)
		in["id"] = id
		updates = append(updates, in)
		_, _ = w.Write([]byte(`{"replies":[{"replaceAllText":{"occurrencesChanged":3}}]}`))
	})
	return f, New(f.Options()), &updates
}

func call(t *testing.T, a *Adapter, conn connectors.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func TestCheck(t *testing.T) {
	f, a, _ := setup(t)
	if err := a.Check(context.Background(), f.Conn(connectors.GoogleDocs, "")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Scope, "auth/documents") || !strings.Contains(f.Scope, "drive.metadata.readonly") {
		t.Errorf("scope = %q", f.Scope)
	}
	err := a.Check(context.Background(), f.Conn(connectors.GoogleDocs, workspacetest.Outsider))
	if err == nil || !strings.Contains(err.Error(), "domain-wide delegation") {
		t.Errorf("outsider: %v", err)
	}
}

// A connector people sign in to works as each of them, with their token.
func TestPerUser(t *testing.T) {
	f, a, _ := setup(t)
	ctx := context.Background()
	var _ connectors.PerUser = a

	// Before anyone signs in, a check proves the OAuth client is known.
	client := f.UserConn(connectors.GoogleDocs, "")
	if err := a.CheckClient(ctx, client); err != nil {
		t.Fatalf("CheckClient = %v", err)
	}
	wrong := f.UserConn(connectors.GoogleDocs, "")
	wrong.Secrets["client_secret"] = "nope"
	if err := a.CheckClient(ctx, wrong); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Errorf("wrong secret: %v", err)
	}

	u, err := a.SignInURL(client, "https://k/cb", "st", "ch")
	if err != nil || !strings.HasPrefix(u, "https://accounts.example/auth?") ||
		!strings.Contains(u, "access_type=offline") || !strings.Contains(u, "auth%2Fdocuments") ||
		!strings.Contains(u, "openid+email") {
		t.Errorf("SignInURL = %s, %v", u, err)
	}
	g, err := a.Exchange(ctx, client, "https://k/cb", workspacetest.Code, "verifier")
	if err != nil || g.Refresh != workspacetest.Refresh || g.Access != "ws-token" || g.Account != "ada@acme.com" {
		t.Fatalf("Exchange = %+v, %v", g, err)
	}
	if g, err := a.Refresh(ctx, client, workspacetest.Refresh); err != nil || g.Access != "ws-token" {
		t.Fatalf("Refresh = %+v, %v", g, err)
	}
	if _, err := a.Refresh(ctx, client, "revoked"); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("revoked refresh: %v", err)
	}

	// Calls carry the person's token, and errors name them.
	conn := f.UserConn(connectors.GoogleDocs, "ws-token")
	if out, err := call(t, a, conn, "*", "find", `{}`); err != nil || !strings.Contains(out, "Plan") {
		t.Errorf("find = %s, %v", out, err)
	}
	if _, err := a.Target(ctx, conn, "GONE"); err == nil || !strings.Contains(err.Error(), "ada@acme.com") {
		t.Errorf("missing: %v", err)
	}
	if _, err := call(t, a, client, "*", "find", `{}`); err == nil {
		t.Error("a call without anyone's token passed")
	}
}

func TestTarget(t *testing.T) {
	f, a, _ := setup(t)
	conn := f.Conn(connectors.GoogleDocs, "")
	ctx := context.Background()
	if got, err := a.Target(ctx, conn, "https://docs.google.com/document/d/DOC/edit"); err != nil || got != "DOC" {
		t.Errorf("Target = %q, %v", got, err)
	}
	if got, err := a.Target(ctx, conn, "*"); err != nil || got != "*" {
		t.Errorf("AnyTarget = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, conn, "SHEET"); err == nil || !strings.Contains(err.Error(), "not a Google document") {
		t.Errorf("spreadsheet: %v", err)
	}
	if _, err := a.Target(ctx, conn, "GONE"); err == nil || !strings.Contains(err.Error(), "sa@p.iam.gserviceaccount.com") {
		t.Errorf("missing: %v", err)
	}
}

func TestFind(t *testing.T) {
	f, a, _ := setup(t)
	conn := f.Conn(connectors.GoogleDocs, "")
	out, err := call(t, a, conn, "*", "find", `{"name":"Pl'an"}`)
	if err != nil || !strings.Contains(out, `"Plan"`) || strings.Contains(out, "Budget") {
		t.Fatalf("find = %s, %v", out, err)
	}
	if q := f.Queries[0]; !strings.Contains(q, `name contains 'Pl\'an'`) || !strings.Contains(q, mimeType) {
		t.Errorf("query = %s", q)
	}
	out, err = call(t, a, conn, "DOC", "find", `{}`)
	if err != nil || strings.Contains(out, "Notes") || len(f.Queries) != 1 {
		t.Errorf("bound find = %s, %v", out, err)
	}
}

func TestRead(t *testing.T) {
	f, a, _ := setup(t)
	out, err := a.Call(context.Background(), f.Conn(connectors.GoogleDocs, ""), "DOC", "read", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Goals\nShip it Ada <ada@acme.com>\n1. First\n  - Point\n| a | b |"
	if v := out.(docView); v.Text != want || v.Title != "Plan" {
		t.Errorf("read = %+v, want text\n%s", v, want)
	}
}

func TestWrites(t *testing.T) {
	f, a, updates := setup(t)
	conn := f.Conn(connectors.GoogleDocs, "")
	if _, err := call(t, a, conn, "DOC", "append", `{"text":"Done.\n"}`); err != nil {
		t.Fatal(err)
	}
	insert := (*updates)[0]["requests"].([]any)[0].(map[string]any)["insertText"].(map[string]any)
	if insert["text"] != "\nDone." || insert["endOfSegmentLocation"] == nil {
		t.Errorf("append sent %v", insert)
	}
	out, err := call(t, a, conn, "*", "replace_text", `{"document_id":"OTHER","find":"x","replace":"y"}`)
	if err != nil || out != `{"id":"OTHER","occurrences_changed":3}` {
		t.Errorf("replace_text = %s, %v", out, err)
	}
	rep := (*updates)[1]["requests"].([]any)[0].(map[string]any)["replaceAllText"].(map[string]any)
	if rep["containsText"].(map[string]any)["matchCase"] != true {
		t.Errorf("replace sent %v", rep)
	}
	out, err = call(t, a, conn, "*", "create", `{"title":"Minutes","text":"Hello","folder_id":"https://drive.google.com/drive/folders/FOLDER"}`)
	if err != nil || !strings.Contains(out, `"id":"NEW"`) {
		t.Fatalf("create = %s, %v", out, err)
	}
	if c := f.Created[0]; c["name"] != "Minutes" || c["mimeType"] != mimeType || c["parents"].([]any)[0] != "FOLDER" {
		t.Errorf("created %v", c)
	}
	if (*updates)[2]["id"] != "NEW" {
		t.Errorf("text went to %v", (*updates)[2]["id"])
	}
}

func TestTargetEnforced(t *testing.T) {
	f, a, updates := setup(t)
	conn := f.Conn(connectors.GoogleDocs, "")
	for tool, args := range map[string]string{
		"read":         `{"document_id":"OTHER"}`,
		"append":       `{"document_id":"OTHER","text":"x"}`,
		"replace_text": `{"document_id":"https://docs.google.com/document/d/OTHER/edit","find":"a","replace":"b"}`,
		"create":       `{"title":"New","folder_id":"F"}`,
	} {
		if _, err := call(t, a, conn, "DOC", tool, args); err == nil || !strings.Contains(err.Error(), "only the document DOC") {
			t.Errorf("%s outside the target: %v", tool, err)
		}
	}
	if len(*updates) > 0 || len(f.Created) > 0 {
		t.Error("a call outside the target reached Google")
	}
}

func TestAPIError(t *testing.T) {
	f, a, _ := setup(t)
	_, err := call(t, a, f.Conn(connectors.GoogleDocs, ""), "*", "append", `{"document_id":"MISSING","text":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "Google answered 404 Not Found") {
		t.Errorf("err = %v", err)
	}
}
