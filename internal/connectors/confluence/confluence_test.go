package confluence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
)

type fake struct {
	srv *httptest.Server
	// sent holds each call's query or JSON body, by "METHOD path".
	sent map[string]string
}

const storageBody = `<h2>Goals</h2><p>Ship &amp; learn.</p><ul><li><p>One</p></li><li>Two</li></ul>`

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{sent: map[string]string{}}
	page := func(id, space string, version int) string {
		b, _ := json.Marshal(map[string]any{
			"id": id, "type": "page", "title": "Plan", "space": map[string]string{"key": space},
			"version": map[string]any{"number": version, "when": "2026-01-01T10:00:00.000Z", "by": map[string]string{"displayName": "Ada"}},
			"body":    map[string]any{"storage": map[string]string{"value": storageBody}},
			"_links":  map[string]string{"webui": "/spaces/" + space + "/pages/" + id},
		})
		return string(b)
	}
	routes := map[string]string{
		"GET /wiki/rest/api/user/current": `{"type":"known","displayName":"Keera"}`,
		"GET /rest/api/user/current":      `{"type":"known","displayName":"Keera"}`,
		"GET /wiki/rest/api/space/DOCS":   `{"key":"DOCS","name":"Docs","type":"global","_links":{"webui":"/spaces/DOCS"}}`,
		"GET /wiki/rest/api/space":        `{"results":[{"key":"DOCS","name":"Docs"},{"key":"OTHER","name":"Other"}]}`,
		"GET /wiki/rest/api/search": `{"totalSize":3,"results":[
			{"content":{"id":"1","type":"page","space":{"key":"DOCS"}},"title":"@@@hl@@@Plan@@@endhl@@@","excerpt":"the &amp; plan","url":"/spaces/DOCS/pages/1"},
			{"content":{"id":"2","type":"page","space":{"key":"OTHER"}},"title":"Other","url":"/spaces/OTHER/pages/2"},
			{"title":"Docs","url":"/spaces/DOCS"}]}`,
		"GET /wiki/rest/api/content/1":  page("1", "DOCS", 7),
		"GET /wiki/rest/api/content/2":  page("2", "OTHER", 1),
		"POST /wiki/rest/api/content":   `{"id":"3","type":"page","title":"New","version":{"number":1},"_links":{"webui":"/spaces/DOCS/pages/3"}}`,
		"PUT /wiki/rest/api/content/1":  `{"id":"1","type":"page","title":"Plan","version":{"number":8},"_links":{"webui":"/spaces/DOCS/pages/1"}}`,
		"GET /rest/api/content/1":       page("1", "DOCS", 7),
		"GET /wiki/rest/api/content/99": ``,
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		auth := r.Header.Get("Authorization")
		cloud := user == "ada@acme.test" && pass == "tok"
		if !cloud && auth != "Bearer pat" && auth != "Bearer anon" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := r.Method + " " + r.URL.Path
		body, ok := routes[key]
		if auth == "Bearer anon" {
			body, ok = `{"type":"anonymous"}`, true
		}
		if !ok || body == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"statusCode":404,"message":"No content found with id: 99"}`))
			return
		}
		f.sent[key] = r.URL.RawQuery
		if r.Method != http.MethodGet {
			// Decoded and encoded again, so tests read <p> and not \u003cp\u003e.
			var in any
			_ = json.NewDecoder(r.Body).Decode(&in)
			var b strings.Builder
			e := json.NewEncoder(&b)
			e.SetEscapeHTML(false)
			_ = e.Encode(in)
			f.sent[key] = strings.TrimSpace(b.String())
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f, New(connectors.Options{HTTP: f.srv.Client()})
}

func (f *fake) cloud() connectors.Connection {
	return connectors.Connection{
		Kind:    connectors.Confluence,
		Config:  map[string]string{"base_url": f.srv.URL + "/wiki/", "email": "ada@acme.test"},
		Secrets: map[string]string{"api_token": "tok"},
	}
}

func (f *fake) dataCenter(token string) connectors.Connection {
	return connectors.Connection{
		Kind:    connectors.Confluence,
		Config:  map[string]string{"base_url": f.srv.URL},
		Secrets: map[string]string{"api_token": token},
	}
}

func call(t *testing.T, a *Adapter, conn connectors.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), nil
}

func TestCheck(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, f.cloud()); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, f.dataCenter("pat")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, f.dataCenter("wrong")); err == nil || !strings.Contains(err.Error(), "personal access token") {
		t.Fatalf("wrong token: %v", err)
	}
	if err := a.Check(ctx, f.dataCenter("anon")); err == nil || !strings.Contains(err.Error(), "as nobody") {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestTarget(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if got, err := a.Target(ctx, f.cloud(), "DOCS"); err != nil || got != "DOCS" {
		t.Fatalf("Target = %q, %v", got, err)
	}
	if got, err := a.Target(ctx, f.cloud(), connectors.AnyTarget); err != nil || got != connectors.AnyTarget {
		t.Fatalf("any target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, f.cloud(), "NOPE"); err == nil || !strings.Contains(err.Error(), "cannot see a Confluence space NOPE") {
		t.Fatalf("missing space: %v", err)
	}
	if _, err := a.Target(ctx, f.cloud(), "../x"); err == nil || !strings.Contains(err.Error(), "not a space key") {
		t.Fatalf("bad key: %v", err)
	}
}

func TestSearch(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "DOCS", "search", `{"text":"plan"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct{ Results []Hit }
	_ = json.Unmarshal([]byte(out), &res)
	if len(res.Results) != 1 || res.Results[0].Title != "Plan" || res.Results[0].Excerpt != "the & plan" ||
		res.Results[0].URL != f.srv.URL+"/wiki/spaces/DOCS/pages/1" {
		t.Fatalf("search = %s", out)
	}
	if sent := f.sent["GET /wiki/rest/api/search"]; !strings.Contains(sent,
		"cql=space+%3D+%22DOCS%22+AND+%28type+in+%28page%2C+blogpost%29+AND+siteSearch+~+%22plan%22%29") {
		t.Fatalf("query = %s", sent)
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "search", `{"cql":"title ~ x) OR (space = OTHER"}`); err == nil {
		t.Fatal("a query escaping the space passed")
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "search", `{}`); err == nil {
		t.Fatal("an empty search passed")
	}
}

func TestGetPage(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "DOCS", "get_page", `{"id":"1"}`)
	if err != nil {
		t.Fatal(err)
	}
	var p Page
	_ = json.Unmarshal([]byte(out), &p)
	if p.Title != "Plan" || p.Version != 7 || p.Body != "## Goals\n\nShip & learn.\n\n- One\n- Two" || p.URL != f.srv.URL+"/wiki/spaces/DOCS/pages/1" {
		t.Fatalf("page = %+v", p)
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "get_page", `{"id":"2"}`); err == nil || !strings.Contains(err.Error(), "not in space DOCS") {
		t.Fatalf("other space: %v", err)
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "get_page", `{"id":"1/../2"}`); err == nil {
		t.Fatal("a bad id passed")
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "get_page", `{"id":"99"}`); err == nil || err.Error() != "Confluence answered 404 Not Found: No content found with id: 99" {
		t.Fatalf("missing page: %v", err)
	}
	// Data Center has no /wiki.
	if _, err := call(t, a, f.dataCenter("pat"), "DOCS", "get_page", `{"id":"1"}`); err != nil {
		t.Fatal(err)
	}
}

func TestListSpaces(t *testing.T) {
	f, a := newFake(t)
	if out, err := call(t, a, f.cloud(), "DOCS", "list_spaces", `{}`); err != nil || strings.Contains(out, "OTHER") || !strings.Contains(out, `"name":"Docs"`) {
		t.Fatalf("one space = %s, %v", out, err)
	}
	if out, err := call(t, a, f.cloud(), connectors.AnyTarget, "list_spaces", `{}`); err != nil || !strings.Contains(out, "OTHER") {
		t.Fatalf("every space = %s, %v", out, err)
	}
}

func TestCreatePage(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "DOCS", "create_page", `{"title":"New","body":"<p>Hi</p>","parent_id":"1"}`)
	if err != nil || !strings.Contains(out, `"id":"3"`) {
		t.Fatalf("create = %s, %v", out, err)
	}
	want := `{"ancestors":[{"id":"1"}],"body":{"storage":{"representation":"storage","value":"<p>Hi</p>"}},"space":{"key":"DOCS"},"title":"New","type":"page"}`
	if got := f.sent["POST /wiki/rest/api/content"]; got != want {
		t.Fatalf("sent %s", got)
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "create_page", `{"title":"New","body":"x","space":"OTHER"}`); err == nil {
		t.Fatal("a page in another space passed")
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "create_page", `{"title":"New","body":"x","parent_id":"2"}`); err == nil {
		t.Fatal("a parent in another space passed")
	}
	if _, err := call(t, a, f.cloud(), connectors.AnyTarget, "create_page", `{"title":"New","body":"x"}`); err == nil {
		t.Fatal("no space with any target passed")
	}
}

func TestUpdatePageAndComment(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "DOCS", "update_page", `{"id":"1","title":"Plan 2"}`)
	if err != nil || !strings.Contains(out, `"version":8`) {
		t.Fatalf("update = %s, %v", out, err)
	}
	sent := f.sent["PUT /wiki/rest/api/content/1"]
	if !strings.Contains(sent, `"version":{"number":8}`) || !strings.Contains(sent, `"title":"Plan 2"`) || !strings.Contains(sent, `<h2>Goals</h2>`) {
		t.Fatalf("sent %s", sent)
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "update_page", `{"id":"2","body":"x"}`); err == nil {
		t.Fatal("an update in another space passed")
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "comment", `{"page_id":"1","body":"Looks <good> to me"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "comment", `{"page_id":"1","body":"Fine & done\n\nThanks"}`); err != nil {
		t.Fatal(err)
	}
	want := `{"body":{"storage":{"representation":"storage","value":"<p>Fine &amp; done</p><p>Thanks</p>"}},"container":{"id":"1","type":"page"},"type":"comment"}`
	if got := f.sent["POST /wiki/rest/api/content"]; got != want {
		t.Fatalf("comment = %s", got)
	}
	if _, err := call(t, a, f.cloud(), "DOCS", "comment", `{"page_id":"2","body":"x"}`); err == nil {
		t.Fatal("a comment in another space passed")
	}
}

func TestPlainText(t *testing.T) {
	in := `<h1>Title</h1><p>Intro&nbsp;text with <a href="https://x.test">a link</a> and ` +
		`<ac:link><ri:page ri:content-title="Other page"/></ac:link>.</p>` +
		`<ol><li>First<ul><li>Nested</li></ul></li><li>Second</li></ol>` +
		`<table><tbody><tr><th><p>A</p></th><th>B</th></tr><tr><td>1</td><td>2</td></tr></tbody></table>` +
		`<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">go</ac:parameter>` +
		`<ac:plain-text-body><![CDATA[x := 1
y := 2]]></ac:plain-text-body></ac:structured-macro>` +
		`<ac:structured-macro ac:name="info"><ac:parameter ac:name="title">Note</ac:parameter><ac:rich-text-body><p>Careful</p></ac:rich-text-body></ac:structured-macro>` +
		`<p><ac:image><ri:attachment ri:filename="chart.png"/></ac:image></p>`
	want := "# Title\n\nIntro text with a link (https://x.test) and Other page.\n\n" +
		"1. First\n  - Nested\n2. Second\n\n" +
		"| A | B |\n| 1 | 2 |\n\n" +
		"```\nx := 1\ny := 2\n```\n\n" +
		"Careful\n\n[image: chart.png]"
	if got := plainText(in); got != want {
		t.Fatalf("plainText =\n%s\nwant\n%s", got, want)
	}
	// What does not parse is kept as text.
	if got := plainText(`<p>Fine</p><p a=">broken`); !strings.HasPrefix(got, "Fine") {
		t.Fatalf("broken = %q", got)
	}
}

// A person's calls go through Atlassian's gateway, under /wiki, and links
// still point at the site.
func TestPerUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/oauth/token/accessible-resources":
			_, _ = w.Write([]byte(`[{"id":"cloud-1","url":"https://acme.atlassian.net"}]`))
		case "/ex/confluence/cloud-1/wiki/rest/api/space/DOCS":
			_, _ = w.Write([]byte(`{"key":"DOCS","name":"Docs","type":"global","_links":{"webui":"/spaces/DOCS"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := New(connectors.Options{HTTP: srv.Client(), Endpoints: map[string]string{"api": srv.URL}})
	conn := connectors.Connection{
		Kind: connectors.Confluence, PerUser: true, Token: "user-tok",
		Config:  map[string]string{"base_url": "https://acme.atlassian.net", "client_id": "cid"},
		Secrets: map[string]string{"client_secret": "pw"},
	}
	if got, err := a.Target(context.Background(), conn, "DOCS"); err != nil || got != "DOCS" {
		t.Fatalf("Target = %q, %v", got, err)
	}
	out, err := a.Call(context.Background(), conn, "DOCS", "list_spaces", json.RawMessage(`{}`))
	b, _ := json.Marshal(out)
	if err != nil || !strings.Contains(string(b), "https://acme.atlassian.net/wiki/spaces/DOCS") {
		t.Errorf("list_spaces = %s, %v", b, err)
	}
}
