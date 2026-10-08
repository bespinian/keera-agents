package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

type fake struct {
	srv *httptest.Server
	// sent holds each call's query or JSON body, by "METHOD path".
	sent map[string]string
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{sent: map[string]string{}}
	var comments []string
	for i := 1; i <= 22; i++ {
		comments = append(comments, fmt.Sprintf(`{"author":{"displayName":"Bob"},"body":"Comment %d","created":"2026-01-01T10:00:00.000+0000"}`, i))
	}
	routes := map[string]string{
		"GET /rest/api/2/myself":         `{"accountId":"1","displayName":"Keera"}`,
		"GET /rest/api/2/project/WEB":    `{"key":"WEB","name":"Website","projectTypeKey":"software"}`,
		"GET /rest/api/2/project/search": `{"values":[{"key":"WEB","name":"Website"},{"key":"OPS","name":"Operations"}],"isLast":true}`,
		"GET /rest/api/2/search/jql": `{"issues":[
			{"key":"WEB-1","fields":{"summary":"Fix login","status":{"name":"To Do"},"issuetype":{"name":"Bug"},"project":{"key":"WEB"},"assignee":{"displayName":"Ada"}}},
			{"key":"OPS-2","fields":{"summary":"Elsewhere","project":{"key":"OPS"}}}],"isLast":false}`,
		"GET /rest/api/2/search": `{"issues":[],"total":0}`,
		"GET /rest/api/2/issue/WEB-1": `{"key":"WEB-1","fields":{"summary":"Fix login","status":{"name":"To Do"},"issuetype":{"name":"Bug"},
			"priority":{"name":"High"},"project":{"key":"WEB"},"labels":["auth"],"description":"It *fails*.",
			"assignee":{"accountId":"a-1","displayName":"Ada"},
			"comment":{"total":22,"comments":[` + strings.Join(comments, ",") + `]}}}`,
		"GET /rest/api/2/issue/OPS-2":              `{"key":"OPS-2","fields":{"project":{"key":"OPS"}}}`,
		"POST /rest/api/2/issue":                   `{"id":"10","key":"WEB-3"}`,
		"PUT /rest/api/2/issue/WEB-1":              ``,
		"PUT /rest/api/2/issue/WEB-1/assignee":     ``,
		"POST /rest/api/2/issue/WEB-1/comment":     `{"id":"100"}`,
		"GET /rest/api/2/issue/WEB-1/transitions":  `{"transitions":[{"id":"21","name":"Start","to":{"name":"In Progress"}},{"id":"31","name":"Finish","to":{"name":"Done"}}]}`,
		"POST /rest/api/2/issue/WEB-1/transitions": ``,
		"GET /rest/api/2/user/assignable/search":   `[{"accountId":"a-1","displayName":"Ada","accountType":"atlassian","active":true},{"accountId":"bot","displayName":"Bot","accountType":"app"}]`,
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		cloud := user == "ada@acme.test" && pass == "tok"
		if !cloud && r.Header.Get("Authorization") != "Bearer pat" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := r.Method + " " + r.URL.Path
		body, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`))
			return
		}
		f.sent[key] = r.URL.RawQuery
		if r.Body != nil && r.Method != http.MethodGet {
			var in json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.sent[key] = string(in)
		}
		if key == "POST /rest/api/2/issue" && strings.Contains(f.sent[key], `"Nope"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errorMessages":[],"errors":{"issuetype":"Specify a valid issue type"}}`))
			return
		}
		if body == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f, New(channels.Options{HTTP: f.srv.Client()})
}

func (f *fake) cloud() channels.Connection {
	return channels.Connection{
		Kind:    channels.Jira,
		Config:  map[string]string{"base_url": f.srv.URL, "email": "ada@acme.test"},
		Secrets: map[string]string{"api_token": "tok"},
	}
}

func (f *fake) dataCenter() channels.Connection {
	return channels.Connection{
		Kind:    channels.Jira,
		Config:  map[string]string{"base_url": f.srv.URL},
		Secrets: map[string]string{"api_token": "pat"},
	}
}

func call(t *testing.T, a *Adapter, conn channels.Connection, target, tool, args string) (string, error) {
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
	if err := a.Check(ctx, f.dataCenter()); err != nil {
		t.Fatal(err)
	}
	wrong := f.cloud()
	wrong.Secrets["api_token"] = "nope"
	if err := a.Check(ctx, wrong); err == nil || !strings.Contains(err.Error(), "does not accept the email and API token") {
		t.Fatalf("wrong token: %v", err)
	}
	plain := f.cloud()
	plain.Config["base_url"] = "http://acme.atlassian.net"
	if err := a.Check(ctx, plain); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain http: %v", err)
	}
}

func TestTarget(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if got, err := a.Target(ctx, f.cloud(), " web "); err != nil || got != "WEB" {
		t.Fatalf("Target = %q, %v", got, err)
	}
	if got, err := a.Target(ctx, f.cloud(), channels.AnyTarget); err != nil || got != channels.AnyTarget {
		t.Fatalf("any target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, f.cloud(), "NOPE"); err == nil || !strings.Contains(err.Error(), "cannot see a Jira project NOPE") {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := a.Target(ctx, f.cloud(), "../x"); err == nil || !strings.Contains(err.Error(), "not a project key") {
		t.Fatalf("bad key: %v", err)
	}
}

func TestTools(t *testing.T) {
	_, a := newFake(t)
	seen := map[string]bool{}
	for _, tool := range a.Tools() {
		if seen[tool.Name] || tool.Description == "" || len(tool.Parameters) == 0 {
			t.Errorf("tool %+v", tool)
		}
		seen[tool.Name] = true
		if write := tool.Name == "create_issue" || tool.Name == "update_issue" || tool.Name == "comment" || tool.Name == "transition"; write != tool.Write {
			t.Errorf("%s: Write = %v", tool.Name, tool.Write)
		}
	}
}

func TestSearch(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "WEB", "search_issues", `{"jql":"status = Done ORDER BY created","limit":500}`)
	if err != nil {
		t.Fatal(err)
	}
	// OPS-2 cannot come from the scoped query, but is dropped if it does.
	if !strings.Contains(out, `"key":"WEB-1"`) || strings.Contains(out, "OPS-2") || !strings.Contains(out, `"more":true`) {
		t.Fatalf("search = %s", out)
	}
	sent := f.sent["GET /rest/api/2/search/jql"]
	if !strings.Contains(sent, "maxResults=100") || !strings.Contains(sent, "jql=project+%3D+%22WEB%22+AND+%28status+%3D+Done%29+ORDER+BY+created") {
		t.Fatalf("query = %s", sent)
	}
	if _, err := call(t, a, f.cloud(), "WEB", "search_issues", `{"jql":"x = 1) OR (project = OPS"}`); err == nil {
		t.Fatal("a query escaping the project passed")
	}
	if _, err := call(t, a, f.cloud(), channels.AnyTarget, "search_issues", `{}`); err == nil {
		t.Fatal("an unbounded search passed")
	}
	// Data Center has only the old search.
	if _, err := call(t, a, f.dataCenter(), "WEB", "search_issues", `{}`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.sent["GET /rest/api/2/search"], "ORDER+BY+updated+DESC") {
		t.Fatalf("data center query = %s", f.sent["GET /rest/api/2/search"])
	}
}

func TestGetIssue(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "WEB", "get_issue", `{"key":"web-1"}`)
	if err != nil {
		t.Fatal(err)
	}
	var i Issue
	_ = json.Unmarshal([]byte(out), &i)
	if i.Summary != "Fix login" || i.Assignee.ID != "a-1" || i.Description != "It *fails*." || len(i.Comments) != maxComments ||
		i.Comments[0].Body != "Comment 3" || i.EarlierComments != 2 || !strings.HasSuffix(i.URL, "/browse/WEB-1") {
		t.Fatalf("issue = %s", out)
	}
	if _, err := call(t, a, f.cloud(), "WEB", "get_issue", `{"key":"OPS-2"}`); err == nil || !strings.Contains(err.Error(), "not in project WEB") {
		t.Fatalf("other project: %v", err)
	}
	if _, err := call(t, a, f.cloud(), channels.AnyTarget, "get_issue", `{"key":"OPS-2"}`); err != nil {
		t.Fatalf("any project: %v", err)
	}
	if _, err := call(t, a, f.cloud(), "WEB", "get_issue", `{"key":"WEB-1/../x"}`); err == nil {
		t.Fatal("a bad key passed")
	}
	if _, err := call(t, a, f.cloud(), "WEB", "get_issue", `{"key":"WEB-404"}`); err == nil || !strings.Contains(err.Error(), "Jira answered 404") {
		t.Fatalf("missing issue: %v", err)
	}
}

func TestListProjectsAndUsers(t *testing.T) {
	f, a := newFake(t)
	if out, err := call(t, a, f.cloud(), "WEB", "list_projects", `{}`); err != nil || !strings.Contains(out, `"name":"Website"`) || strings.Contains(out, "OPS") {
		t.Fatalf("one project = %s, %v", out, err)
	}
	if out, err := call(t, a, f.cloud(), channels.AnyTarget, "list_projects", `{"query":"oper"}`); err != nil || !strings.Contains(out, "OPS") || strings.Contains(out, "WEB") {
		t.Fatalf("every project = %s, %v", out, err)
	}
	out, err := call(t, a, f.cloud(), "WEB", "find_users", `{"query":"ada"}`)
	if err != nil || out != `[{"name":"Ada","id":"a-1"}]` {
		t.Fatalf("users = %s, %v", out, err)
	}
	if !strings.Contains(f.sent["GET /rest/api/2/user/assignable/search"], "project=WEB") {
		t.Fatalf("user query = %s", f.sent["GET /rest/api/2/user/assignable/search"])
	}
	if _, err := call(t, a, f.cloud(), "WEB", "find_users", `{"query":"ada","project":"OPS"}`); err == nil {
		t.Fatal("another project's people passed")
	}
}

func TestCreateIssue(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "WEB", "create_issue", `{"summary":"New","labels":["x"]}`)
	if err != nil || !strings.Contains(out, `"key":"WEB-3"`) {
		t.Fatalf("create = %s, %v", out, err)
	}
	if sent := f.sent["POST /rest/api/2/issue"]; sent != `{"fields":{"issuetype":{"name":"Task"},"labels":["x"],"project":{"key":"WEB"},"summary":"New"}}` {
		t.Fatalf("sent %s", sent)
	}
	if _, err := call(t, a, f.cloud(), "WEB", "create_issue", `{"project":"OPS","summary":"New"}`); err == nil {
		t.Fatal("an issue in another project passed")
	}
	if _, err := call(t, a, f.cloud(), channels.AnyTarget, "create_issue", `{"summary":"New"}`); err == nil {
		t.Fatal("no project with any target passed")
	}
	_, err = call(t, a, f.cloud(), "WEB", "create_issue", `{"summary":"New","issue_type":"Nope"}`)
	if err == nil || err.Error() != "Jira answered 400 Bad Request: issuetype: Specify a valid issue type" {
		t.Fatalf("bad type: %v", err)
	}
}

func TestUpdateIssue(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.cloud(), "WEB", "update_issue", `{"key":"WEB-1","summary":"Better","assignee":"none"}`)
	if err != nil || !strings.Contains(out, `"changed":["summary","assignee"]`) {
		t.Fatalf("update = %s, %v", out, err)
	}
	if f.sent["PUT /rest/api/2/issue/WEB-1"] != `{"fields":{"summary":"Better"}}` || f.sent["PUT /rest/api/2/issue/WEB-1/assignee"] != `{"accountId":null}` {
		t.Fatalf("sent %v", f.sent)
	}
	if _, err := call(t, a, f.dataCenter(), "WEB", "update_issue", `{"key":"WEB-1","assignee":"ada"}`); err != nil {
		t.Fatal(err)
	}
	if got := f.sent["PUT /rest/api/2/issue/WEB-1/assignee"]; got != `{"name":"ada"}` {
		t.Fatalf("data center assignee = %s", got)
	}
	if _, err := call(t, a, f.cloud(), "WEB", "update_issue", `{"key":"OPS-2","summary":"x"}`); err == nil {
		t.Fatal("another project's issue passed")
	}
	if _, err := call(t, a, f.cloud(), "WEB", "update_issue", `{"key":"WEB-1"}`); err == nil {
		t.Fatal("an empty update passed")
	}
}

func TestCommentAndTransition(t *testing.T) {
	f, a := newFake(t)
	if _, err := call(t, a, f.cloud(), "WEB", "comment", `{"key":"WEB-1","body":"Done."}`); err != nil {
		t.Fatal(err)
	}
	if got := f.sent["POST /rest/api/2/issue/WEB-1/comment"]; got != `{"body":"Done."}` {
		t.Fatalf("comment = %s", got)
	}
	if _, err := call(t, a, f.cloud(), "WEB", "comment", `{"key":"OPS-2","body":"x"}`); err == nil {
		t.Fatal("a comment in another project passed")
	}
	out, err := call(t, a, f.cloud(), "WEB", "transition", `{"key":"WEB-1","to":"in progress"}`)
	if err != nil || !strings.Contains(out, `"status":"In Progress"`) {
		t.Fatalf("transition = %s, %v", out, err)
	}
	if got := f.sent["POST /rest/api/2/issue/WEB-1/transitions"]; got != `{"transition":{"id":"21"}}` {
		t.Fatalf("sent %s", got)
	}
	_, err = call(t, a, f.cloud(), "WEB", "transition", `{"key":"WEB-1","to":"Closed"}`)
	if err == nil || !strings.Contains(err.Error(), `"Finish" (to Done)`) {
		t.Fatalf("unknown status: %v", err)
	}
	if _, err := call(t, a, f.cloud(), "WEB", "transition", `{"key":"OPS-2","to":"Done"}`); err == nil {
		t.Fatal("a transition in another project passed")
	}
}

// A person's calls go through Atlassian's gateway, with their token, and
// links still point at the site.
func TestPerUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/oauth/token/accessible-resources":
			_, _ = w.Write([]byte(`[{"id":"cloud-1","url":"https://acme.atlassian.net"}]`))
		case "/ex/jira/cloud-1/rest/api/2/project/WEB":
			_, _ = w.Write([]byte(`{"key":"WEB","name":"Website","projectTypeKey":"software"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := New(channels.Options{HTTP: srv.Client(), Endpoints: map[string]string{"api": srv.URL}})
	conn := channels.Connection{
		Kind: channels.Jira, PerUser: true, Token: "user-tok",
		Config:  map[string]string{"base_url": "https://acme.atlassian.net", "client_id": "cid"},
		Secrets: map[string]string{"client_secret": "pw"},
	}
	if got, err := a.Target(context.Background(), conn, "WEB"); err != nil || got != "WEB" {
		t.Fatalf("Target = %q, %v", got, err)
	}
	out, err := call(t, a, conn, "WEB", "list_projects", `{}`)
	if err != nil || !strings.Contains(out, "https://acme.atlassian.net/browse/WEB") {
		t.Errorf("list_projects = %s, %v", out, err)
	}
}
