package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/store"
)

// fakeDocs is a service people sign in to, whose one tool says whom it was
// called as.
type fakeDocs struct{ personal }

func (fakeDocs) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind: "fakedocs", Label: "Fake Docs", Setup: "Share it.", TargetLabel: "Document", AnyTarget: true,
		Fields: []connectors.Field{{Name: "key", Label: "Key", Secret: true, Required: true}},
	}
}

func (fakeDocs) Check(context.Context, connectors.Connection) error { return nil }

func (fakeDocs) Tools() []connectors.Tool {
	return []connectors.Tool{{Name: "whoami", Description: "Whom the call is as."}}
}

func (fakeDocs) Target(_ context.Context, conn connectors.Connection, target string) (string, error) {
	if conn.PerUser && conn.Token == "" {
		return "", errors.New("called as nobody")
	}
	return target, nil
}

func (fakeDocs) Call(_ context.Context, conn connectors.Connection, _, _ string, _ json.RawMessage) (any, error) {
	return conn.Account + " with " + conn.Token, nil
}

// personal signs people in to a fake kind: the code "good" is Ada's
// account, and an access token is its refresh token with a- before it.
type personal struct{}

func (personal) SignIn() connectors.SignIn {
	return connectors.SignIn{Setup: "Register {callback}.", Fields: []connectors.Field{
		{Name: "client_id", Label: "Client ID", Required: true},
		{Name: "client_secret", Label: "Client secret", Secret: true, Required: true},
	}}
}

func (personal) SignInURL(conn connectors.Connection, redirect, state, challenge string) (string, error) {
	return "https://idp.example/auth?" + url.Values{
		"client_id": {conn.Config["client_id"]}, "redirect_uri": {redirect}, "state": {state},
		"code_challenge": {challenge},
	}.Encode(), nil
}

func (personal) Exchange(_ context.Context, _ connectors.Connection, _, code, verifier string) (connectors.Grant, error) {
	if code != "good" || verifier == "" {
		return connectors.Grant{}, errors.New("bad code")
	}
	return connectors.Grant{Refresh: "r1", Access: "a0", Expiry: time.Now().Add(time.Hour), Account: "ada@acme.ch"}, nil
}

func (personal) CheckClient(_ context.Context, conn connectors.Connection) error {
	if conn.Secrets["client_secret"] != "pw" {
		return errors.New("unknown client")
	}
	return nil
}

func (personal) Refresh(_ context.Context, _ connectors.Connection, refresh string) (connectors.Grant, error) {
	return connectors.Grant{Access: "a-" + refresh, Expiry: time.Now().Add(time.Hour)}, nil
}

// fakeFiles is a file store people sign in to. Each file is named after
// whom it was listed as.
type fakeFiles struct{ personal }

func (fakeFiles) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind: "fakefiles", Label: "Fake Files", Setup: "Share it.", TargetLabel: "Folder",
		Fields: []connectors.Field{{Name: "key", Label: "Key", Secret: true, Required: true}},
	}
}

func (fakeFiles) Check(context.Context, connectors.Connection) error { return nil }

func (fakeFiles) List(_ context.Context, conn connectors.Connection, _ string) ([]connectors.File, error) {
	return []connectors.File{{ID: "f1", Name: conn.Account + " with " + conn.Token}}, nil
}

func (fakeFiles) Read(context.Context, connectors.Connection, string) (connectors.File, []byte, error) {
	return connectors.File{}, nil, errors.New("not here")
}

func (fakeFiles) Write(context.Context, connectors.Connection, string, string, string, []byte) (connectors.File, error) {
	return connectors.File{}, errors.New("not here")
}

// Changes lists one new file, after the first call, only for a person.
func (fakeFiles) Changes(_ context.Context, conn connectors.Connection, _, cursor string) ([]connectors.File, string, error) {
	if conn.Token == "" {
		return nil, "", errors.New("called as nobody")
	}
	if cursor == "" {
		return nil, "c1", nil
	}
	return []connectors.File{{ID: "new", Name: "invoice.pdf", Modified: time.Now()}}, "c2", nil
}

// fakeCode is a code host people sign in to. git is given the person's
// token.
type fakeCode struct{ personal }

func (fakeCode) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind: "fakecode", Label: "Fake Code", Setup: "Share it.", TargetLabel: "Repository",
		Fields: []connectors.Field{{Name: "key", Label: "Key", Secret: true, Required: true}},
	}
}

func (fakeCode) Check(context.Context, connectors.Connection) error { return nil }

func (fakeCode) Repo(_ context.Context, conn connectors.Connection, path string) (connectors.Repo, error) {
	if conn.Token == "" {
		return connectors.Repo{}, errors.New("called as nobody")
	}
	return connectors.Repo{Path: path, DefaultBranch: "main"}, nil
}

func (fakeCode) Remote(_ context.Context, conn connectors.Connection, path string) (connectors.Remote, error) {
	return connectors.Remote{URL: "https://code.example/" + path + ".git", User: "x", Password: conn.Token}, nil
}

func (fakeCode) Issues(context.Context, connectors.Connection, string, string, bool) ([]connectors.Issue, error) {
	return nil, nil
}

func (fakeCode) Issue(context.Context, connectors.Connection, string, int, bool) (connectors.Issue, error) {
	return connectors.Issue{}, nil
}

func (fakeCode) Comment(context.Context, connectors.Connection, string, int, bool, string) error {
	return nil
}

func (fakeCode) OpenChange(context.Context, connectors.Connection, string, connectors.Change) (connectors.Issue, error) {
	return connectors.Issue{}, nil
}

// A connector people sign in to acts as whoever a task is for.
func TestAccounts(t *testing.T) {
	ta := startApp(t)
	ctx, st, call := ta.ctx, ta.st, ta.call
	connectors.Register(fakeDocs{})
	connectors.Register(fakeFiles{})
	connectors.Register(fakeCode{})

	// raw sends Ada's sign-in with more cookies, and does not follow
	// redirects, so the test sees what each answer sets.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	raw := func(method, path string, cookies ...*http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, ta.srv.URL+path, bytes.NewReader(nil))
		req.AddCookie(ta.cookie)
		req.Header.Set("X-CSRF-Token", ta.csrf)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}
	cookieOf := func(res *http.Response, name string) *http.Cookie {
		t.Helper()
		for _, c := range res.Cookies() {
			if c.Name == name && c.Value != "" {
				return c
			}
		}
		t.Fatalf("%s %s set no %s", res.Request.Method, res.Request.URL.Path, name)
		return nil
	}

	var conn struct {
		ID      string `json:"id"`
		PerUser bool   `json:"per_user"`
	}
	if code := call("POST", "/api/v1/connections", map[string]any{
		"kind": "fakedocs", "name": "Docs", "per_user": true,
		"config": map[string]string{"client_id": "cid"}, "secrets": map[string]string{"client_secret": "wrong"},
	}, nil); code != http.StatusBadRequest {
		t.Fatalf("an unknown OAuth client: %d", code)
	}
	if code := call("POST", "/api/v1/connections", map[string]any{
		"kind": "fakedocs", "name": "Docs", "per_user": true,
		"config": map[string]string{"client_id": "cid"}, "secrets": map[string]string{"client_secret": "pw"},
	}, &conn); code != http.StatusCreated || !conn.PerUser {
		t.Fatalf("create: %d %+v", code, conn)
	}
	if code := call("PATCH", "/api/v1/connections/"+conn.ID, map[string]any{"per_user": false}, nil); code != http.StatusBadRequest {
		t.Fatalf("a connector changed whom it acts as: %d", code)
	}

	// Binding a named target checks it as the person binding, who must
	// have signed in.
	a, err := st.CreateAgent(ctx, store.Agent{
		ID: id.New("agt"), OrgID: "org_a", OwnerID: "usr_ada", OwnerEmail: "ada@example.ch",
		Name: "Writer", Model: "keera-speed", MaxSteps: 10, Access: store.AccessOrg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := call("POST", "/api/v1/agents/"+a.ID+"/bindings", map[string]any{
		"connection_id": conn.ID, "target": "*", "mode": "read",
	}, nil); code != http.StatusBadRequest {
		t.Fatalf("bound before signing in: %d", code)
	}

	signIn := func(code string) *http.Response {
		t.Helper()
		res := raw("POST", "/api/v1/connections/"+conn.ID+"/account")
		var start struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(res.Body).Decode(&start)
		u, err := url.Parse(start.URL)
		if res.StatusCode != http.StatusOK || err != nil || u.Query().Get("redirect_uri") != ta.srv.URL+"/auth/connector/callback" {
			t.Fatalf("start: %d %q", res.StatusCode, start.URL)
		}
		pending := cookieOf(res, "keera_agents_account")
		return raw("GET", "/auth/connector/callback?"+url.Values{
			"state": {u.Query().Get("state")}, "code": {code},
		}.Encode(), pending)
	}
	var list struct {
		Data []struct {
			ID      string         `json:"id"`
			Account *store.Account `json:"account"`
		} `json:"data"`
		Result *struct {
			ConnectionID string `json:"connection_id"`
			Error        string `json:"error"`
		} `json:"sign_in_result"`
	}
	listWith := func(res *http.Response) {
		t.Helper()
		if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/connectors" {
			t.Fatalf("callback: %d %s", res.StatusCode, res.Header.Get("Location"))
		}
		list.Result = nil
		got := raw("GET", "/api/v1/connections", cookieOf(res, "keera_agents_account_result"))
		if err := json.NewDecoder(got.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
	}

	listWith(signIn("bad"))
	if list.Result == nil || !strings.Contains(list.Result.Error, "bad code") || list.Data[0].Account != nil {
		t.Fatalf("a refused code: %+v %+v", list.Result, list.Data)
	}
	listWith(signIn("good"))
	if list.Result == nil || list.Result.Error != "" || list.Result.ConnectionID != conn.ID ||
		list.Data[0].Account == nil || list.Data[0].Account.Account != "ada@acme.ch" {
		t.Fatalf("signed in: %+v %+v", list.Result, list.Data)
	}
	// A state from another browser's sign-in connects nothing.
	start := raw("POST", "/api/v1/connections/"+conn.ID+"/account")
	listWith(raw("GET", "/auth/connector/callback?state=forged&code=good", cookieOf(start, "keera_agents_account")))
	if list.Result == nil || !strings.Contains(list.Result.Error, "not the one this browser started") {
		t.Fatalf("a forged state: %+v", list.Result)
	}

	var b store.Binding
	if code := call("POST", "/api/v1/agents/"+a.ID+"/bindings", map[string]any{
		"connection_id": conn.ID, "target": "*", "mode": "read",
	}, &b); code != http.StatusCreated {
		t.Fatalf("bind: %d", code)
	}

	// Tools act as the person the task is for, never as another.
	tool := func(userID, email string) (string, bool) {
		res := ta.app.disp.CallTool(ctx, a, store.Task{
			ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, RequestedBy: email, RequestedByID: userID,
		}, "fakedocs_whoami", json.RawMessage(`{"connector":"`+b.ID+`"}`))
		return res.Content, res.IsError
	}
	if out, failed := tool("usr_ada", "ada@example.ch"); failed || out != "ada@acme.ch with a0" {
		t.Errorf("as Ada: %q", out)
	}
	if out, failed := tool("usr_bob", "bob@example.ch"); !failed ||
		!strings.Contains(out, "bob@example.ch has not connected their account to Docs") {
		t.Errorf("as Bob: %q", out)
	}
	if out, failed := tool("", ""); !failed || !strings.Contains(out, "nobody asked") {
		t.Errorf("as nobody: %q", out)
	}

	// Someone who left the organisation is signed out.
	if err := st.SaveAccount(ctx, store.Account{ConnectionID: conn.ID, UserID: "usr_gone", Token: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	ta.app.disp.CheckAccounts(ctx)
	if _, err := st.Account(ctx, conn.ID, "usr_gone"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the account of someone who left: %v", err)
	}
	if _, err := st.Account(ctx, conn.ID, "usr_ada"); err != nil {
		t.Errorf("a member was signed out: %v", err)
	}

	if code := call("DELETE", "/api/v1/connections/"+conn.ID+"/account", nil, nil); code != http.StatusNoContent {
		t.Fatalf("disconnect: %d", code)
	}
	if code := call("DELETE", "/api/v1/connections/"+conn.ID+"/account", nil, nil); code != http.StatusNotFound {
		t.Fatalf("disconnect again: %d", code)
	}
	if out, failed := tool("usr_ada", "ada@example.ch"); !failed || !strings.Contains(out, "has not connected") {
		t.Errorf("after disconnecting: %q", out)
	}
}

// Folders and repositories act as the person a task is for too, and a
// folder watch as whoever added it.
func TestPersonalFilesAndCode(t *testing.T) {
	ta := startApp(t)
	ctx, st, call := ta.ctx, ta.st, ta.call
	connectors.Register(fakeFiles{})
	connectors.Register(fakeCode{})

	a, err := st.CreateAgent(ctx, store.Agent{
		ID: id.New("agt"), OrgID: "org_a", OwnerID: "usr_ada", OwnerEmail: "ada@example.ch",
		Name: "Clerk", Model: "keera-speed", MaxSteps: 10, Access: store.AccessOrg, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	connect := func(kind string) store.Connection {
		t.Helper()
		var c store.Connection
		if code := call("POST", "/api/v1/connections", map[string]any{
			"kind": kind, "per_user": true,
			"config": map[string]string{"client_id": "cid"}, "secrets": map[string]string{"client_secret": "pw"},
		}, &c); code != http.StatusCreated {
			t.Fatalf("create %s: %d", kind, code)
		}
		return c
	}
	bind := func(c store.Connection, target, mode string) (store.Binding, int) {
		var b store.Binding
		code := call("POST", "/api/v1/agents/"+a.ID+"/bindings", map[string]any{
			"connection_id": c.ID, "target": target, "mode": mode, "prompt": "File it.",
		}, &b)
		return b, code
	}
	files, code := connect("fakefiles"), connect("fakecode")

	// Nobody signed in yet, so nothing can be added as anyone.
	if _, status := bind(files, "F1", "watch"); status != http.StatusBadRequest {
		t.Fatalf("a watch before signing in: %d", status)
	}
	for _, c := range []store.Connection{files, code} {
		if _, err := ta.app.disp.Connect(ctx, c, "usr_ada", "good", "v"); err != nil {
			t.Fatal(err)
		}
	}
	folder, status := bind(files, "F1", "files")
	if status != http.StatusCreated {
		t.Fatalf("bind folder: %d", status)
	}
	repo, status := bind(code, "acme/web", "repo")
	if status != http.StatusCreated {
		t.Fatalf("bind repository: %d", status)
	}

	task := func(userID, email string) store.Task {
		return store.Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, RequestedBy: email, RequestedByID: userID}
	}
	list := func(t store.Task) (string, bool) {
		res := ta.app.disp.CallTool(ctx, a, t, "files_list", json.RawMessage(`{"folder":"`+folder.ID+`"}`))
		return res.Content, res.IsError
	}
	if out, failed := list(task("usr_ada", "ada@example.ch")); failed || !strings.Contains(out, "ada@acme.ch with a0") {
		t.Errorf("files_list as Ada: %s", out)
	}
	if out, failed := list(task("usr_bob", "bob@example.ch")); !failed || !strings.Contains(out, "bob@example.ch has not connected") {
		t.Errorf("files_list as Bob: %s", out)
	}

	// git gets the person's token, never another's.
	if r, err := ta.app.disp.GitRemote(ctx, a, task("usr_ada", "ada@example.ch"), repo.ID); err != nil || r.Password != "a0" {
		t.Errorf("GitRemote as Ada = %+v, %v", r, err)
	}
	if _, err := ta.app.disp.GitRemote(ctx, a, task("usr_bob", "bob@example.ch"), repo.ID); err == nil {
		t.Error("git for Bob, who has not signed in")
	}

	// A watch reads the folder as whoever added it, and its tasks are theirs.
	if _, status := bind(files, "F2", "watch"); status != http.StatusCreated {
		t.Fatalf("bind watch: %d", status)
	}
	ta.app.disp.CheckWatches(ctx, time.Now())
	ta.app.disp.CheckWatches(ctx, time.Now().Add(time.Hour))
	var tasks struct {
		Data []store.Task `json:"data"`
	}
	call("GET", "/api/v1/tasks?agent_id="+a.ID, nil, &tasks)
	if len(tasks.Data) != 1 || tasks.Data[0].RequestedBy != "ada@example.ch" {
		t.Fatalf("tasks from the watch = %+v", tasks.Data)
	}
}
