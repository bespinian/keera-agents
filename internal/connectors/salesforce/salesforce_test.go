package salesforce

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const api = "/services/data/" + apiVersion

type fake struct {
	srv *httptest.Server
	// instance is what the token endpoint answers as instance_url; empty
	// is the fake itself.
	instance string
	// valid is the token the API accepts; changing it expires the others.
	valid  atomic.Value
	tokens atomic.Int32
	// sent is each request's body, and query each request's q, by method
	// and path.
	sent  map[string]map[string]any
	query map[string]string
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{sent: map[string]map[string]any{}, query: map[string]string{}}
	f.valid.Store("tok1")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /services/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "key" || r.Form.Get("client_secret") != "secret" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"invalid client credentials"}`))
			return
		}
		f.tokens.Add(1)
		instance := f.instance
		if instance == "" {
			instance = f.srv.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": f.valid.Load().(string), "instance_url": instance})
	})
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+f.valid.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`[{"message":"Session expired or invalid","errorCode":"INVALID_SESSION_ID"}]`))
				return
			}
			key := r.Method + " " + strings.TrimPrefix(r.URL.Path, api)
			f.query[key] = r.URL.Query().Get("q") + r.URL.Query().Get("fields")
			if b, _ := io.ReadAll(r.Body); len(b) > 0 {
				var in map[string]any
				if err := json.Unmarshal(b, &in); err != nil {
					t.Errorf("%s: %v", key, err)
				}
				f.sent[key] = in
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("GET "+api+"/", reply(200, `{"sobjects":"/services/data/v62.0/sobjects"}`))
	mux.HandleFunc("GET "+api+"/query", reply(200, `{"totalSize":3,"done":true,"records":[
		{"attributes":{"type":"Account","url":"/x"},"Id":"001000000000001AAA","Name":"Acme","Owner":{"attributes":{"type":"User"},"Name":"Bob"},"Phone":null},
		{"attributes":{"type":"Account"},"Id":"001000000000002AAA","Name":"Beta"},
		{"attributes":{"type":"Account"},"Id":"001000000000003AAA","Name":"Gamma"}]}`))
	mux.HandleFunc("GET "+api+"/search", reply(200, `{"searchRecords":[
		{"attributes":{"type":"Contact"},"Id":"003000000000001AAA","Name":"Ada Lovelace","Email":"ada@example.com"}]}`))
	mux.HandleFunc("GET "+api+"/sobjects/Account/001000000000001AAA", reply(200,
		`{"attributes":{"type":"Account"},"Id":"001000000000001AAA","Name":"Acme","Description":"`+strings.Repeat("x", 3000)+`"}`))
	mux.HandleFunc("GET "+api+"/sobjects/Account/001000000000009AAA", reply(404,
		`[{"message":"The requested resource does not exist","errorCode":"NOT_FOUND"}]`))
	mux.HandleFunc("GET "+api+"/sobjects/Lead/describe", reply(200, `{"name":"Lead","label":"Lead","fields":[
		{"name":"Id","label":"Lead ID","type":"id","nillable":false,"createable":false,"updateable":false},
		{"name":"LastName","label":"Last Name","type":"string","nillable":false,"createable":true,"updateable":true},
		{"name":"Status","label":"Status","type":"picklist","nillable":false,"createable":true,"updateable":true,"defaultedOnCreate":true,
			"picklistValues":[{"value":"Open","active":true},{"value":"Old","active":false}]},
		{"name":"OwnerId","label":"Owner ID","type":"reference","nillable":false,"createable":true,"updateable":true,"defaultedOnCreate":true,"referenceTo":["User"]}]}`))
	mux.HandleFunc("POST "+api+"/sobjects/Lead", reply(201, `{"id":"00Q000000000001AAA","success":true,"errors":[]}`))
	mux.HandleFunc("PATCH "+api+"/sobjects/Opportunity/006000000000001AAA", reply(204, ``))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f, New(connectors.Options{HTTP: f.srv.Client()})
}

func (f *fake) conn(secret string) connectors.Connection {
	return connectors.Connection{
		Kind:    connectors.Salesforce,
		Config:  map[string]string{"domain_url": f.srv.URL + "/lightning/page/home", "client_id": "key"},
		Secrets: map[string]string{"client_secret": secret},
	}
}

func TestDescriptor(t *testing.T) {
	a := New(connectors.Options{})
	d := a.Descriptor()
	if d.Kind != connectors.Salesforce || d.Category != "CRM" || d.TargetLabel != "" || strings.Contains(d.Setup, "{webhook}") {
		t.Fatalf("descriptor = %+v", d)
	}
	for _, tool := range a.Tools() {
		if want := tool.Name == "create" || tool.Name == "update"; tool.Write != want {
			t.Errorf("%s: write = %v", tool.Name, tool.Write)
		}
	}
}

func TestCheck(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, f.conn("secret")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, f.conn("wrong")); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("wrong secret: %v", err)
	}
	plain := f.conn("secret")
	plain.Config["domain_url"] = "http://acme.my.salesforce.com"
	if err := a.Check(ctx, plain); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain http: %v", err)
	}
	f.instance = "https://evil.example.com"
	if err := a.Check(ctx, f.conn("secret")); err == nil || !strings.Contains(err.Error(), "evil.example.com") {
		t.Fatalf("forged instance: %v", err)
	}
}

func TestInstance(t *testing.T) {
	for _, c := range []struct {
		instance, want string
	}{
		{"", "https://acme.my.salesforce.com"},
		{"https://acme.my.salesforce.com/", "https://acme.my.salesforce.com"},
		{"https://na1.salesforce.com", "https://na1.salesforce.com"},
		{"https://acme.lightning.force.com", "https://acme.lightning.force.com"},
		{"https://salesforce.com.evil.example", ""},
		{"http://na1.salesforce.com", ""},
	} {
		got, err := instanceOf("https://acme.my.salesforce.com", c.instance)
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("instanceOf(%q) = %q, %v", c.instance, got, err)
		}
	}
}

func call(t *testing.T, f *fake, a *Adapter, tool, args string) string {
	t.Helper()
	out, err := a.Call(context.Background(), f.conn("secret"), connectors.AnyTarget, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func TestQuery(t *testing.T) {
	f, a := newFake(t)
	got := call(t, f, a, "query", `{"soql":"SELECT Id, Name, Owner.Name, Phone FROM Account","limit":2}`)
	want := `{"total_size":3,"done":false,"records":[{"Id":"001000000000001AAA","Name":"Acme","Owner":{"Name":"Bob"}},` +
		`{"Id":"001000000000002AAA","Name":"Beta"}]}`
	if got != want {
		t.Fatalf("query = %s", got)
	}
	if q := f.query["GET /query"]; q != "SELECT Id, Name, Owner.Name, Phone FROM Account" {
		t.Fatalf("q = %s", q)
	}
}

func TestSearch(t *testing.T) {
	f, a := newFake(t)
	got := call(t, f, a, "search", `{"text":"ada@example.com {x}"}`)
	if got != `[{"object":"Contact","record":{"Email":"ada@example.com","Id":"003000000000001AAA","Name":"Ada Lovelace"}}]` {
		t.Fatalf("search = %s", got)
	}
	if q := f.query["GET /search"]; !strings.HasPrefix(q, `FIND {ada@example.com \{x\}} IN ALL FIELDS RETURNING Account(`) || !strings.HasSuffix(q, " LIMIT 25") {
		t.Fatalf("q = %s", q)
	}
	call(t, f, a, "search", `{"sosl":"FIND {Acme} RETURNING Account(Id)"}`)
	if q := f.query["GET /search"]; q != "FIND {Acme} RETURNING Account(Id)" {
		t.Fatalf("raw q = %s", q)
	}
}

func TestGet(t *testing.T) {
	f, a := newFake(t)
	var rec map[string]string
	_ = json.Unmarshal([]byte(call(t, f, a, "get", `{"object":"Account","id":"001000000000001AAA","fields":["Name","Description"]}`)), &rec)
	if rec["Name"] != "Acme" || len([]rune(rec["Description"])) != maxValue+1 || f.query["GET /sobjects/Account/001000000000001AAA"] != "Name,Description" {
		t.Fatalf("get = %v", rec)
	}
	for _, bad := range []string{
		`{"object":"Account","id":"../../limits"}`,
		`{"object":"../Account","id":"001000000000001AAA"}`,
		`{"object":"Account","id":"001000000000001AAA","fields":["Name,Id"]}`,
	} {
		if _, err := a.Call(context.Background(), f.conn("secret"), "*", "get", json.RawMessage(bad)); err == nil {
			t.Errorf("%s passed", bad)
		}
	}
}

func TestDescribe(t *testing.T) {
	f, a := newFake(t)
	got := call(t, f, a, "describe", `{"object":"Lead"}`)
	want := `{"name":"Lead","label":"Lead","fields":[{"name":"Id","label":"Lead ID","type":"id"},` +
		`{"name":"LastName","label":"Last Name","type":"string","required":true,"updateable":true},` +
		`{"name":"Status","label":"Status","type":"picklist","updateable":true,"picklist":["Open"]},` +
		`{"name":"OwnerId","label":"Owner ID","type":"reference","updateable":true,"reference_to":["User"]}]}`
	if got != want {
		t.Fatalf("describe = %s", got)
	}
}

func TestWrite(t *testing.T) {
	f, a := newFake(t)
	if got := call(t, f, a, "create", `{"object":"Lead","fields":{"LastName":"Lovelace","Company":"Acme"}}`); got != `{"id":"00Q000000000001AAA"}` {
		t.Fatalf("create = %s", got)
	}
	if f.sent["POST /sobjects/Lead"]["LastName"] != "Lovelace" {
		t.Fatalf("sent %v", f.sent)
	}
	if got := call(t, f, a, "update", `{"object":"Opportunity","id":"006000000000001AAA","fields":{"StageName":"Closed Won"}}`); got != `{"id":"006000000000001AAA","updated":true}` {
		t.Fatalf("update = %s", got)
	}
	if f.sent["PATCH /sobjects/Opportunity/006000000000001AAA"]["StageName"] != "Closed Won" {
		t.Fatalf("sent %v", f.sent)
	}
}

func TestExpiredToken(t *testing.T) {
	f, a := newFake(t)
	call(t, f, a, "describe", `{"object":"Lead"}`)
	f.valid.Store("tok2")
	call(t, f, a, "describe", `{"object":"Lead"}`)
	if n := f.tokens.Load(); n != 2 {
		t.Fatalf("signed in %d times", n)
	}
}

func TestAPIError(t *testing.T) {
	f, a := newFake(t)
	_, err := a.Call(context.Background(), f.conn("secret"), "*", "get", json.RawMessage(`{"object":"Account","id":"001000000000009AAA"}`))
	if err == nil || err.Error() != "Salesforce answered 404 Not Found NOT_FOUND: The requested resource does not exist" {
		t.Fatalf("error = %v", err)
	}
}

// The domain URL keeps only its server, whatever page was pasted.
func TestDomain(t *testing.T) {
	o, err := orgOf(connectors.Connection{Config: map[string]string{
		"domain_url": "https://acme.my.salesforce.com/lightning/setup", "client_id": "k", "client_secret": "s",
	}})
	if err != nil || o.domain != "https://acme.my.salesforce.com" {
		t.Fatalf("org = %+v, %v", o, err)
	}
}

// A person's token goes to the org's own server, and is never swapped for
// the run-as user's.
func TestPerUser(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	query := func(conn connectors.Connection) error {
		_, err := a.Call(ctx, conn, connectors.AnyTarget, "query", json.RawMessage(`{"soql":"SELECT Id, Name FROM Account"}`))
		return err
	}
	user := f.conn("secret")
	user.PerUser, user.Token = true, "tok1"
	if err := query(user); err != nil {
		t.Fatalf("query = %v", err)
	}
	user.Token = "expired"
	if err := query(user); web.Status(err) != http.StatusUnauthorized {
		t.Errorf("a refused token: %v", err)
	}
	if n := f.tokens.Load(); n != 0 {
		t.Errorf("signed in as the run-as user %d times", n)
	}
	client := f.conn("secret")
	client.PerUser = true
	if err := query(client); err == nil {
		t.Error("a call for nobody passed")
	}
	u, err := a.SignInURL(client, "https://k/cb", "st", "ch")
	if err != nil || !strings.HasPrefix(u, f.srv.URL+"/services/oauth2/authorize?") || !strings.Contains(u, "refresh_token") {
		t.Errorf("SignInURL = %s, %v", u, err)
	}
}
