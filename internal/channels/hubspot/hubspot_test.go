package hubspot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

type fake struct {
	// sent is each request's body, by method and path.
	sent map[string]map[string]any
	// query is each GET's query, by path.
	query map[string]string
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{sent: map[string]map[string]any{}, query: map[string]string{}}
	mux := http.NewServeMux()
	reply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.Header.Get("Authorization") {
			case "Bearer tok":
			case "Bearer noscope":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"status":"error","message":"This app hasn't been granted all required scopes","category":"MISSING_SCOPES"}`))
				return
			default:
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"error","message":"Authentication credentials not found","category":"INVALID_AUTHENTICATION"}`))
				return
			}
			key := r.Method + " " + r.URL.Path
			f.query[key] = r.URL.RawQuery
			if b, _ := io.ReadAll(r.Body); len(b) > 0 {
				var in map[string]any
				if err := json.Unmarshal(b, &in); err != nil {
					t.Errorf("%s: %v", key, err)
				}
				f.sent[key] = in
			}
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("GET /crm/v3/objects/contacts", reply(`{"results":[]}`))
	mux.HandleFunc("POST /crm/v3/objects/deals/search", reply(`{"total":2,
		"results":[{"id":"7","properties":{"dealname":"Big deal","amount":"1000","closedate":null,"hs_object_id":"7"}}],
		"paging":{"next":{"after":"1"}}}`))
	mux.HandleFunc("GET /crm/v3/objects/contacts/42", reply(`{"id":"42",
		"properties":{"firstname":"Ada","email":"ada@example.com","phone":"","notes":"`+strings.Repeat("x", 3000)+`"},
		"associations":{"companies":{"results":[{"id":"5","type":"contact_to_company"},{"id":"5","type":"contact_to_company_unlabeled"}]}}}`))
	mux.HandleFunc("GET /crm/v3/objects/contacts/404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":"error","message":"Object not found","category":"OBJECT_NOT_FOUND"}`))
	})
	mux.HandleFunc("POST /crm/v3/objects/companies", reply(`{"id":"9","properties":{"name":"Acme"}}`))
	mux.HandleFunc("PATCH /crm/v3/objects/tickets/3", reply(`{"id":"3","properties":{"hs_pipeline_stage":"4"}}`))
	mux.HandleFunc("POST /crm/v3/objects/notes", reply(`{"id":"77","properties":{}}`))
	mux.HandleFunc("GET /crm/v3/owners", reply(`{"results":[{"id":"11","email":"bob@example.com","firstName":"Bob","lastName":"Lee"}]}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, New(channels.Options{Endpoints: map[string]string{"api": srv.URL}})
}

func conn(token string) channels.Connection {
	return channels.Connection{Kind: channels.HubSpot, Secrets: map[string]string{"access_token": token}}
}

func TestDescriptor(t *testing.T) {
	d := New(channels.Options{}).Descriptor()
	if d.Kind != channels.HubSpot || d.Category != "CRM" || !d.AnyTarget || d.TargetLabel != "" || strings.Contains(d.Setup, "{webhook}") {
		t.Fatalf("descriptor = %+v", d)
	}
	write := map[string]bool{}
	for _, tool := range New(channels.Options{}).Tools() {
		write[tool.Name] = tool.Write
	}
	if write["search"] || write["get"] || write["list_owners"] || !write["create"] || !write["update"] || !write["add_note"] {
		t.Fatalf("write = %v", write)
	}
}

func TestCheck(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, conn("tok")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, conn("wrong")); err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Fatalf("wrong token: %v", err)
	}
	if err := a.Check(ctx, conn("noscope")); err == nil || !strings.Contains(err.Error(), "crm.objects.contacts.read") {
		t.Fatalf("missing scope: %v", err)
	}
	if err := a.Check(ctx, conn("")); err == nil {
		t.Fatal("no token passed")
	}
}

func TestTarget(t *testing.T) {
	_, a := newFake(t)
	for _, in := range []string{"", "*", " * "} {
		if got, err := a.Target(context.Background(), conn("tok"), in); err != nil || got != channels.AnyTarget {
			t.Errorf("Target(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := a.Target(context.Background(), conn("tok"), "deals"); err == nil {
		t.Fatal("a narrower target passed")
	}
	if _, err := a.Call(context.Background(), conn("tok"), "deals", "list_owners", nil); err == nil {
		t.Fatal("a call with a narrower target ran")
	}
}

func call(t *testing.T, a *Adapter, tool, args string) any {
	t.Helper()
	out, err := a.Call(context.Background(), conn("tok"), channels.AnyTarget, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out
}

func TestSearch(t *testing.T) {
	f, a := newFake(t)
	out := call(t, a, "search", `{"object_type":"deals","filters":{"dealstage":"closedwon"},"limit":500}`).(SearchResult)
	if out.Total != 2 || out.Next != "1" || len(out.Records) != 1 {
		t.Fatalf("search = %+v", out)
	}
	r := out.Records[0]
	if r.ID != "7" || r.Properties["dealname"] != "Big deal" || len(r.Properties) != 2 {
		t.Fatalf("record = %+v", r)
	}
	sent := f.sent["POST /crm/v3/objects/deals/search"]
	if sent["limit"] != float64(100) || sent["sorts"] == nil {
		t.Fatalf("sent %v", sent)
	}
	filter := sent["filterGroups"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)
	if filter["propertyName"] != "dealstage" || filter["operator"] != "EQ" || filter["value"] != "closedwon" {
		t.Fatalf("filter = %v", filter)
	}
	if props := sent["properties"].([]any); len(props) != len(types["deals"]) {
		t.Fatalf("default properties = %v", props)
	}
	if _, err := a.Call(context.Background(), conn("tok"), "*", "search", json.RawMessage(`{"object_type":"users"}`)); err == nil {
		t.Fatal("an unknown object type passed")
	}
}

func TestGet(t *testing.T) {
	f, a := newFake(t)
	r := call(t, a, "get", `{"object_type":"contacts","id":"42","properties":["firstname","email","phone","notes"]}`).(Record)
	if r.Properties["firstname"] != "Ada" || r.Properties["phone"] != "" || len([]rune(r.Properties["notes"])) != maxValue+1 {
		t.Fatalf("record = %+v", r)
	}
	if got := r.Associations["companies"]; len(got) != 1 || got[0] != "5" {
		t.Fatalf("associations = %v", r.Associations)
	}
	if q := f.query["GET /crm/v3/objects/contacts/42"]; !strings.Contains(q, "associations=companies%2Cdeals%2Ctickets") {
		t.Fatalf("query = %s", q)
	}
	_, err := a.Call(context.Background(), conn("tok"), "*", "get", json.RawMessage(`{"object_type":"contacts","id":"404"}`))
	if err == nil || !strings.Contains(err.Error(), "no contacts record") {
		t.Fatalf("missing record: %v", err)
	}
	if _, err := a.Call(context.Background(), conn("tok"), "*", "get", json.RawMessage(`{"object_type":"contacts","id":"../owners"}`)); err == nil {
		t.Fatal("a path as id passed")
	}
}

func TestWrite(t *testing.T) {
	f, a := newFake(t)
	r := call(t, a, "create", `{"object_type":"companies","properties":{"name":"Acme"}}`).(Record)
	if r.ID != "9" || f.sent["POST /crm/v3/objects/companies"]["properties"].(map[string]any)["name"] != "Acme" {
		t.Fatalf("create = %+v, sent %v", r, f.sent)
	}
	r = call(t, a, "update", `{"object_type":"tickets","id":"3","properties":{"hs_pipeline_stage":"4"}}`).(Record)
	if r.ID != "3" || f.sent["PATCH /crm/v3/objects/tickets/3"] == nil {
		t.Fatalf("update = %+v", r)
	}
	out := call(t, a, "add_note", `{"object_type":"deals","id":"7","body":"Called <them>.\nAll good."}`).(map[string]string)
	if out["note_id"] != "77" {
		t.Fatalf("add_note = %v", out)
	}
	note := f.sent["POST /crm/v3/objects/notes"]
	if body := note["properties"].(map[string]any)["hs_note_body"]; body != "Called &lt;them&gt;.<br>All good." {
		t.Fatalf("note body = %v", body)
	}
	assoc := note["associations"].([]any)[0].(map[string]any)
	typ := assoc["types"].([]any)[0].(map[string]any)
	if assoc["to"].(map[string]any)["id"] != "7" || typ["associationTypeId"] != float64(214) {
		t.Fatalf("association = %v", assoc)
	}
}

func TestListOwners(t *testing.T) {
	f, a := newFake(t)
	out := call(t, a, "list_owners", `{"email":"bob@example.com"}`)
	b, _ := json.Marshal(out)
	if string(b) != `{"owners":[{"id":"11","name":"Bob Lee","email":"bob@example.com"}]}` {
		t.Fatalf("owners = %s", b)
	}
	if q := f.query["GET /crm/v3/owners"]; !strings.Contains(q, "email=bob%40example.com") {
		t.Fatalf("query = %s", q)
	}
}

func TestAPIError(t *testing.T) {
	_, a := newFake(t)
	_, err := a.Call(context.Background(), conn("wrong"), "*", "list_owners", nil)
	if err == nil || !strings.Contains(err.Error(), "HubSpot answered 401") || !strings.Contains(err.Error(), "Authentication credentials not found") {
		t.Fatalf("error = %v", err)
	}
}
