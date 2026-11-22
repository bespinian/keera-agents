package freshdesk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
)

type fake struct {
	// sent is each request's body, by method and path.
	sent map[string]map[string]any
	// query is each request's query, by method and path.
	query map[string]string
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{sent: map[string]map[string]any{}, query: map[string]string{}}
	mux := http.NewServeMux()
	reply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if user, pass, ok := r.BasicAuth(); !ok || user != "key" || pass != "X" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"invalid_credentials","message":"You have to be logged in to perform this action."}`))
				return
			}
			k := r.Method + " " + r.URL.Path
			f.query[k] = r.URL.RawQuery
			if b, _ := io.ReadAll(r.Body); len(b) > 0 {
				var in map[string]any
				if err := json.Unmarshal(b, &in); err != nil {
					t.Errorf("%s: %v", k, err)
				}
				f.sent[k] = in
			}
			_, _ = w.Write([]byte(body))
		}
	}
	ticket := func(id, group int) string {
		return fmt.Sprintf(`{"id":%d,"subject":"Refund","status":2,"priority":3,"group_id":%d,"responder_id":null,
			"requester_id":9,"tags":["refund"],"updated_at":"2026-11-01T10:00:00Z","description_text":"Please refund."}`, id, group)
	}
	mux.HandleFunc("GET /api/v2/agents/me", reply(`{"id":1}`))
	mux.HandleFunc("GET /api/v2/groups", reply(`[{"id":5,"name":"Billing"},{"id":6,"name":"Sales"}]`))
	mux.HandleFunc("GET /api/v2/groups/5", reply(`{"id":5,"name":"Billing","agent_ids":[1]}`))
	mux.HandleFunc("GET /api/v2/groups/7", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /api/v2/tickets", reply(`[`+ticket(1, 5)+`,`+ticket(2, 6)+`]`))
	mux.HandleFunc("GET /api/v2/search/tickets", reply(`{"results":[`+ticket(1, 5)+`,`+ticket(2, 6)+`],"total":2}`))
	mux.HandleFunc("GET /api/v2/tickets/1", reply(strings.Replace(ticket(1, 5), `"subject"`, `"requester":{"name":"Ada","email":"ada@example.com"},"subject"`, 1)))
	mux.HandleFunc("GET /api/v2/tickets/2", reply(ticket(2, 6)))
	mux.HandleFunc("GET /api/v2/tickets/404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	var convs []string
	for i := range 25 {
		convs = append(convs, fmt.Sprintf(`{"body_text":"message %d","incoming":%v,"private":true,"source":%d,"from_email":"x@example.com","created_at":"2026-11-0%dT00:00:00Z"}`,
			i, i%2 == 0, []int{0, 2}[i%2], 1+i%9))
	}
	mux.HandleFunc("GET /api/v2/tickets/1/conversations", reply(`[`+strings.Join(convs, ",")+`]`))
	mux.HandleFunc("GET /api/v2/contacts", reply(`[{"id":9,"name":"Ada","email":"ada@example.com"}]`))
	mux.HandleFunc("GET /api/v2/contacts/autocomplete", reply(`[{"id":9,"name":"Ada"}]`))
	mux.HandleFunc("GET /api/v2/agents", reply(`[{"id":1,"contact":{"name":"Bob","email":"bob@example.com"}},{"id":2,"contact":{"name":"Eve"}}]`))
	mux.HandleFunc("POST /api/v2/tickets", reply(ticket(3, 5)))
	mux.HandleFunc("PUT /api/v2/tickets/1", reply(ticket(1, 5)))
	mux.HandleFunc("POST /api/v2/tickets/1/reply", reply(`{"id":1}`))
	mux.HandleFunc("POST /api/v2/tickets/1/notes", reply(`{"id":1}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, New(connectors.Options{Endpoints: map[string]string{"api": srv.URL}})
}

func conn(key string) connectors.Connection {
	return connectors.Connection{
		Kind: connectors.Freshdesk, Config: map[string]string{"domain": "acme.freshdesk.com"}, Secrets: map[string]string{"api_key": key},
	}
}

func call(t *testing.T, a *Adapter, target, tool, args string) any {
	t.Helper()
	out, err := a.Call(context.Background(), conn("key"), target, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out
}

func callErr(t *testing.T, a *Adapter, target, tool, args string) error {
	t.Helper()
	_, err := a.Call(context.Background(), conn("key"), target, tool, json.RawMessage(args))
	if err == nil {
		t.Fatalf("%s %s: no error", tool, args)
	}
	return err
}

func TestDescriptor(t *testing.T) {
	d := New(connectors.Options{}).Descriptor()
	if d.Kind != connectors.Freshdesk || d.Vendor != "Freshworks" || d.TargetLabel == "" || !d.AnyTarget {
		t.Fatalf("descriptor = %+v", d)
	}
	if !d.Fields[0].Endpoint || d.Fields[0].Name != "domain" {
		t.Fatalf("the domain must be an endpoint field: %+v", d.Fields[0])
	}
	write := map[string]bool{}
	for _, tool := range New(connectors.Options{}).Tools() {
		write[tool.Name] = tool.Write
	}
	for _, name := range []string{"search_tickets", "get_ticket", "find_contacts", "list_groups", "list_agents"} {
		if w, ok := write[name]; !ok || w {
			t.Errorf("%s: write %v, present %v", name, w, ok)
		}
	}
	for _, name := range []string{"create_ticket", "update_ticket", "reply", "add_note"} {
		if !write[name] {
			t.Errorf("%s does not write", name)
		}
	}
}

func TestOpen(t *testing.T) {
	a := New(connectors.Options{})
	for domain, want := range map[string]string{
		"acme":                            "https://acme.freshdesk.com",
		"ACME.freshdesk.com":              "https://acme.freshdesk.com",
		"https://acme.freshdesk.com/":     "https://acme.freshdesk.com",
		"evil.example.com":                "",
		"https://evil.example.com":        "",
		"evil.example.com/.freshdesk.com": "",
		"http://acme.freshdesk.com":       "",
		"":                                "",
	} {
		c := conn("key")
		c.Config["domain"] = domain
		s, err := a.open(c)
		if s.base != want || (err == nil) != (want != "") {
			t.Errorf("open(%q) = %q, %v; want %q", domain, s.base, err, want)
		}
	}
}

func TestCheck(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, conn("key")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, conn("wrong")); err == nil || !strings.Contains(err.Error(), "does not work") {
		t.Fatalf("wrong key: %v", err)
	}
	if err := a.Check(ctx, conn("")); err == nil {
		t.Fatal("no key passed")
	}
}

func TestTarget(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	for in, want := range map[string]string{"billing": "5", "5": "5", connectors.AnyTarget: connectors.AnyTarget} {
		if got, err := a.Target(ctx, conn("key"), in); err != nil || got != want {
			t.Errorf("target %q = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"Support", "7"} {
		if _, err := a.Target(ctx, conn("key"), in); err == nil {
			t.Errorf("target %q passed", in)
		}
	}
}

func TestScope(t *testing.T) {
	for _, c := range []struct{ target, in, want string }{
		{"5", "", "group_id:5"},
		{"5", "status:2 OR priority:4", "(status:2 OR priority:4) AND group_id:5"},
		{"5", "tag:'a (b'", "(tag:'a (b') AND group_id:5"},
		{connectors.AnyTarget, "status:2", "status:2"},
		{connectors.AnyTarget, "", ""},
	} {
		if got, err := scope(c.target, c.in); err != nil || got != c.want {
			t.Errorf("scope(%q, %q) = %q, %v", c.target, c.in, got, err)
		}
	}
	for _, in := range []string{"status:2) OR (group_id:6", "status:2 OR (", `tag:"x"`, "tag:'x", `tag:'x\' ) OR (group_id:6 OR tag:'y'`} {
		if _, err := scope("5", in); err == nil {
			t.Errorf("scope(%q) passed", in)
		}
	}
}

func TestSearchTickets(t *testing.T) {
	f, a := newFake(t)
	out := call(t, a, "5", "search_tickets", `{"filter":"status:2"}`).(map[string]any)
	ts := out["tickets"].([]Brief)
	if len(ts) != 1 || ts[0].ID != 1 || ts[0].Status != "open" || ts[0].Priority != "high" || !strings.HasSuffix(ts[0].URL, "/a/tickets/1") {
		t.Fatalf("tickets = %+v", ts)
	}
	if q := f.query["GET /api/v2/search/tickets"]; !strings.Contains(q, "group_id%3A5") {
		t.Fatalf("query = %s", q)
	}
	ts = call(t, a, connectors.AnyTarget, "search_tickets", `{}`).(map[string]any)["tickets"].([]Brief)
	if len(ts) != 2 || !strings.Contains(f.query["GET /api/v2/tickets"], "order_by=updated_at") {
		t.Fatalf("latest = %+v", ts)
	}
}

func TestGetTicket(t *testing.T) {
	_, a := newFake(t)
	tk := call(t, a, "5", "get_ticket", `{"id":"1"}`).(Ticket)
	if tk.Requester != "Ada <ada@example.com>" || tk.Description != "Please refund." {
		t.Fatalf("ticket = %+v", tk)
	}
	if len(tk.Messages) != maxMessages || tk.EarlierMessages != 5 || tk.Messages[0].Text != "message 5" {
		t.Fatalf("messages = %d, earlier %d, first %+v", len(tk.Messages), tk.EarlierMessages, tk.Messages[0])
	}
	if tk.Messages[0].Kind != "private note" || tk.Messages[1].Kind != "reply" {
		t.Fatalf("kinds = %+v", tk.Messages[:2])
	}
	if err := callErr(t, a, "5", "get_ticket", `{"id":2}`); !strings.Contains(err.Error(), "not in this connector's group") {
		t.Fatalf("other group: %v", err)
	}
	if err := callErr(t, a, "5", "get_ticket", `{"id":404}`); !strings.Contains(err.Error(), "no ticket") {
		t.Fatalf("missing: %v", err)
	}
	if err := callErr(t, a, "5", "get_ticket", `{"id":-1}`); !strings.Contains(err.Error(), "not a ticket id") {
		t.Fatalf("bad id: %v", err)
	}
}

func TestPeople(t *testing.T) {
	f, a := newFake(t)
	cs := call(t, a, "5", "find_contacts", `{"query":"ada@example.com"}`).([]Contact)
	if len(cs) != 1 || f.query["GET /api/v2/contacts"] != "email=ada%40example.com" {
		t.Fatalf("by email = %+v", cs)
	}
	if cs := call(t, a, "5", "find_contacts", `{"query":"Ad"}`).([]Contact); len(cs) != 1 {
		t.Fatalf("by name = %+v", cs)
	}
	if as := call(t, a, "5", "list_agents", `{}`).([]Agent); len(as) != 1 || as[0].Name != "Bob" {
		t.Fatalf("group's agents = %+v", as)
	}
	if as := call(t, a, connectors.AnyTarget, "list_agents", `{}`).([]Agent); len(as) != 2 {
		t.Fatalf("every agent = %+v", as)
	}
	if gs := call(t, a, "5", "list_groups", `{}`).([]Group); len(gs) != 1 || gs[0].Name != "Billing" {
		t.Fatalf("group = %+v", gs)
	}
}

func TestCreateTicket(t *testing.T) {
	f, a := newFake(t)
	call(t, a, "5", "create_ticket", `{"email":"ada@example.com","subject":"Hi","description":"a <b>\nc","priority":"urgent"}`)
	sent := f.sent["POST /api/v2/tickets"]
	if sent["group_id"] != 5.0 || sent["priority"] != 4.0 || sent["status"] != 2.0 || sent["description"] != "a &lt;b&gt;<br>c" {
		t.Fatalf("sent = %v", sent)
	}
	if err := callErr(t, a, "5", "create_ticket", `{"email":"a@b.c","subject":"x","description":"y","group_id":6}`); !strings.Contains(err.Error(), "only its own group") {
		t.Fatalf("other group: %v", err)
	}
	if err := callErr(t, a, "5", "create_ticket", `{"subject":"x","description":"y"}`); !strings.Contains(err.Error(), "email or contact_id") {
		t.Fatalf("no customer: %v", err)
	}
}

func TestUpdateTicket(t *testing.T) {
	f, a := newFake(t)
	call(t, a, "5", "update_ticket", `{"id":1,"status":"resolved","agent_id":0,"tags":[]}`)
	sent := f.sent["PUT /api/v2/tickets/1"]
	if sent["status"] != 4.0 || len(sent["tags"].([]any)) != 0 {
		t.Fatalf("sent = %v", sent)
	}
	if v, ok := sent["responder_id"]; !ok || v != nil {
		t.Fatalf("responder_id = %v, %v", v, ok)
	}
	if err := callErr(t, a, "5", "update_ticket", `{"id":1,"status":"done"}`); !strings.Contains(err.Error(), "not a status") {
		t.Fatalf("bad status: %v", err)
	}
	if err := callErr(t, a, "5", "update_ticket", `{"id":2,"status":"open"}`); !strings.Contains(err.Error(), "not in this connector's group") {
		t.Fatalf("other group: %v", err)
	}
}

func TestMessages(t *testing.T) {
	f, a := newFake(t)
	call(t, a, "5", "reply", `{"id":1,"body":"Done."}`)
	if f.sent["POST /api/v2/tickets/1/reply"]["body"] != "Done." {
		t.Fatalf("reply = %v", f.sent)
	}
	call(t, a, "5", "add_note", `{"id":1,"body":"Checked."}`)
	if f.sent["POST /api/v2/tickets/1/notes"]["private"] != true {
		t.Fatalf("note = %v", f.sent)
	}
	if err := callErr(t, a, "5", "reply", `{"id":2,"body":"x"}`); !strings.Contains(err.Error(), "not in this connector's group") {
		t.Fatalf("other group: %v", err)
	}
}
