package server

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/store"
)

// These tests call the panel's API as people who may not do something, and
// as those who may. Ada administers org_a, Bob is a plain member of it, and
// Eve administers org_b (see people).

// caller sends a request as one person and answers the status and the body.
type caller func(method, path string, in any) (int, []byte)

// as calls the API with a person's gateway sign-in as a bearer token, as
// Keera Chat does, so no CSRF token is needed.
func (ta *testApp) as(t *testing.T, token string) caller {
	return func(method, path string, in any) (int, []byte) {
		t.Helper()
		var body io.Reader
		if in != nil {
			b, _ := json.Marshal(in)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ta.srv.URL+path, body)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, raw
	}
}

// must calls and fails the test unless the answer is code. With out, it
// decodes the body.
func must(t *testing.T, c caller, method, path string, in any, code int, out any) {
	t.Helper()
	got, raw := c(method, path, in)
	if got != code {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, got, code, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

// sshKey is an ed25519 public key line, made the way ssh-keygen makes one.
func sshKey(seed byte) string {
	typ := "ssh-ed25519"
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(typ)))
	blob = append(blob, typ...)
	blob = binary.BigEndian.AppendUint32(blob, 32)
	blob = append(blob, bytes.Repeat([]byte{seed}, 32)...)
	return typ + " " + base64.StdEncoding.EncodeToString(blob)
}

// fixture is org_a's rows, made through the API.
type fixture struct {
	adaAgent, bobAgent         string // Scout takes work from Ada only; Helper from anyone
	adaWorkspace, bobWorkspace string // called dev and lab
	conn                       string // a chat connector
	adaBinding, wksBinding     string // Scout in #sales; dev's folder
	schedule, board, card      string // Scout's; the card is Scout's, in To do
	task, key                  string // Ada's task for Scout; Ada's SSH key
}

func (f fixture) ids() []string {
	return []string{
		f.adaAgent, f.bobAgent, f.adaWorkspace, f.bobWorkspace, f.conn, f.adaBinding, f.wksBinding,
		f.schedule, f.board, f.card, f.task, f.key,
	}
}

func seed(t *testing.T, ta *testApp) fixture {
	t.Helper()
	// Machines only say they run: these tests are about the API.
	ta.driver.mu.Lock()
	ta.driver.idle = true
	ta.driver.mu.Unlock()
	connectors.Register(&fakeChat{})
	ada, bob := ta.as(t, "tok-ada"), ta.as(t, "tok-bob")
	var f fixture
	var out struct {
		ID string `json:"id"`
	}
	mk := func(c caller, path string, in any) string {
		t.Helper()
		out.ID = ""
		must(t, c, "POST", path, in, http.StatusCreated, &out)
		return out.ID
	}
	f.adaAgent = mk(ada, "/api/v1/agents", map[string]any{"name": "Scout", "model": "keera-speed", "access": "owner"})
	f.bobAgent = mk(bob, "/api/v1/agents", map[string]any{"name": "Helper", "model": "keera-speed"})
	f.adaWorkspace = mk(ada, "/api/v1/workspaces", map[string]any{"name": "dev", "experience": "terminal"})
	f.bobWorkspace = mk(bob, "/api/v1/workspaces", map[string]any{"name": "lab", "experience": "vscode"})
	f.conn = mk(ada, "/api/v1/connections", map[string]any{
		"kind": "fakechat", "name": "Team chat", "secrets": map[string]string{"token": "hook-secret"},
	})
	f.adaBinding = mk(ada, "/api/v1/agents/"+f.adaAgent+"/bindings",
		map[string]any{"connection_id": f.conn, "target": "sales", "mode": "chat"})
	// The chat kind holds no files, so this binding is made in the store.
	b, err := ta.st.CreateBinding(ta.ctx, store.Binding{
		ID: id.New("bnd"), AgentID: f.adaWorkspace, ConnectionID: f.conn, Target: "docs", Mode: store.ModeFiles,
		CreatedBy: "ada@example.ch", CreatedByID: "usr_ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.wksBinding = b.ID
	f.schedule = mk(ada, "/api/v1/schedules", map[string]any{
		"agent_id": f.adaAgent, "name": "Morning", "cron": "0 8 * * *", "prompt": "Report.",
	})
	f.board = mk(ada, "/api/v1/boards", map[string]any{"name": "Sales"})
	f.card = mk(ada, "/api/v1/boards/"+f.board+"/cards",
		map[string]any{"title": "Call Globex", "status": "todo", "agent_id": f.adaAgent})
	f.task = mk(ada, "/api/v1/agents/"+f.adaAgent+"/tasks", map[string]any{"prompt": "Summarise the week."})
	f.key = mk(ada, "/api/v1/me/ssh-keys", map[string]any{"name": "laptop", "public_key": sshKey(1)})

	ta.waitFor("every machine to run", func() bool {
		for _, id := range []string{f.adaAgent, f.bobAgent, f.adaWorkspace, f.bobWorkspace} {
			if a, err := ta.st.Agent(ta.ctx, id); err != nil || a.State != store.AgentRunning {
				return false
			}
		}
		return true
	})
	return f
}

// orgState describes what people can change in org_a, without what the
// machines change on their own, such as their state.
func orgState(t *testing.T, ta *testApp) string {
	t.Helper()
	ctx, st := ta.ctx, ta.st
	var b strings.Builder
	machines, err := st.Workspaces(ctx, "org_a", "")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := st.Agents(ctx, "org_a")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range append(machines, agents...) {
		fmt.Fprintf(&b, "machine %s %s %q %s %v %s %s %s %s %+v %s\n", a.ID, a.Name, a.Description, a.OwnerID,
			a.Enabled, a.Image, a.Access, a.Model, a.ProjectID, a.Class, a.Experience)
		bindings, err := st.AgentBindings(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range bindings {
			fmt.Fprintf(&b, "  binding %s %s %s\n", x.ID, x.Target, x.Mode)
		}
	}
	schedules, err := st.Schedules(ctx, "org_a", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schedules {
		fmt.Fprintf(&b, "schedule %s %s %s %q %v %s\n", s.ID, s.Name, s.Cron, s.Prompt, s.Enabled, s.RequestedBy)
	}
	boards, err := st.Boards(ctx, "org_a")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range boards {
		fmt.Fprintf(&b, "board %s %s\n", x.ID, x.Name)
		cards, err := st.BoardCards(ctx, x.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cards {
			comments, _ := st.Comments(ctx, c.ID)
			fmt.Fprintf(&b, "  card %s %q %q %s %v %d comments\n", c.ID, c.Title, c.Body, c.Status,
				c.AgentID != nil, len(comments))
		}
	}
	conns, err := st.Connections(ctx, "org_a")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		fmt.Fprintf(&b, "connection %s %s %v %x %s\n", c.ID, c.Name, c.Config, c.Secrets, c.UpdatedAt)
	}
	tasks, err := st.Tasks(ctx, store.TaskQuery{OrgID: "org_a", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range tasks {
		fmt.Fprintf(&b, "task %s %s\n", x.ID, x.State)
	}
	keys, err := st.SSHKeys(ctx, "org_a", "usr_ada")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		fmt.Fprintf(&b, "ssh key %s\n", k.ID)
	}
	audit, err := st.AuditLog(ctx, "org_a", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "%d audit entries\n", len(audit))
	return b.String()
}

// check is one call in the permission table.
type check struct {
	route string // as server.go registers it; {id} is replaced with id
	id    string
	query string
	body  any
	// other is the answer to a member of another organisation, and member
	// to a plain member of org_a on rows they do not own. 0 skips the call:
	// it changes things, and TestOwnersAndAdmins covers it.
	other, member int
}

func (c check) request() (method, path string) {
	method, path, _ = strings.Cut(c.route, " ")
	path = strings.Replace(path, "{id}", c.id, 1)
	if c.query != "" {
		path += "?" + c.query
	}
	return method, path
}

func permissionTable(f fixture) []check {
	const (
		ok, created, none, bad, forbidden, notFound = 200, 201, 204, 400, 403, 404
	)
	sneakyCard := map[string]any{"title": "Sneak", "status": "todo", "agent_id": f.adaAgent}
	return []check{
		{route: "GET /api/v1/me", other: ok, member: ok},
		// The gateway decides which organisations someone is in.
		{route: "POST /api/v1/me/org", body: map[string]string{"org_id": "org_a"}, other: forbidden, member: ok},
		{route: "GET /api/v1/catalog", other: ok, member: ok},
		{route: "GET /api/v1/members", other: ok, member: forbidden},
		{route: "GET /api/v1/overview", other: ok, member: ok},
		{route: "GET /api/v1/setup", other: ok, member: ok},
		{route: "GET /api/v1/audit", other: ok, member: forbidden},
		// Each reads their own organisation's. This deployment bills no
		// machine time, so an administrator finds nothing.
		{route: "GET /api/v1/billing", other: notFound, member: forbidden},

		{route: "GET /api/v1/agents", other: ok, member: ok},
		{route: "POST /api/v1/agents", body: map[string]any{"name": "Mole", "model": "keera-speed"}, other: created},
		{route: "GET /api/v1/agents/{id}", id: f.adaAgent, other: notFound, member: ok},
		{route: "PATCH /api/v1/agents/{id}", id: f.adaAgent, body: map[string]any{"description": "mine now"}, other: notFound, member: forbidden},
		{route: "DELETE /api/v1/agents/{id}", id: f.adaAgent, other: notFound, member: forbidden},
		{route: "POST /api/v1/agents/{id}/start", id: f.adaAgent, other: notFound, member: forbidden},
		{route: "POST /api/v1/agents/{id}/stop", id: f.adaAgent, other: notFound, member: forbidden},
		{route: "POST /api/v1/agents/{id}/restart", id: f.adaAgent, other: notFound, member: forbidden},
		{route: "POST /api/v1/agents/{id}/take-over", id: f.adaAgent, other: notFound, member: forbidden},
		{route: "GET /api/v1/agents/{id}/activity", id: f.adaAgent, other: notFound, member: ok},
		{route: "GET /api/v1/agents/{id}/cost", id: f.adaAgent, other: notFound, member: forbidden},
		{route: "GET /api/v1/agents/{id}/logs", id: f.adaAgent, other: notFound, member: forbidden},
		{route: "POST /api/v1/agents/{id}/tasks", id: f.adaAgent, body: map[string]string{"prompt": "hi"}, other: notFound, member: forbidden},
		{
			route: "POST /api/v1/agents/{id}/bindings", id: f.adaAgent,
			body: map[string]string{"connection_id": f.conn, "target": "random", "mode": "chat"}, other: notFound, member: forbidden,
		},
		{route: "DELETE /api/v1/bindings/{id}", id: f.adaBinding, other: notFound, member: forbidden},
		// Another member's workspace is not found, as on its own routes.
		{route: "DELETE /api/v1/bindings/{id}", id: f.wksBinding, other: notFound, member: notFound},

		{route: "GET /api/v1/workspaces", other: ok, member: ok},
		{route: "GET /api/v1/workspaces", query: "all=true", other: ok, member: forbidden},
		{route: "POST /api/v1/workspaces", body: map[string]string{"name": "burrow", "experience": "terminal"}, other: created},
		{route: "GET /api/v1/workspaces/{id}", id: f.adaWorkspace, other: notFound, member: notFound},
		// By name, the CLI finds only the caller's own.
		{route: "GET /api/v1/workspaces/{id}", id: "dev", other: notFound, member: notFound},
		{route: "PATCH /api/v1/workspaces/{id}", id: f.adaWorkspace, body: map[string]string{"description": "x"}, other: notFound, member: notFound},
		{route: "DELETE /api/v1/workspaces/{id}", id: f.adaWorkspace, other: notFound, member: notFound},
		{route: "POST /api/v1/workspaces/{id}/start", id: f.adaWorkspace, other: notFound, member: notFound},
		{route: "POST /api/v1/workspaces/{id}/stop", id: f.adaWorkspace, other: notFound, member: notFound},
		{route: "GET /api/v1/workspaces/{id}/logs", id: f.adaWorkspace, other: notFound, member: notFound},
		{route: "GET /api/v1/workspaces/{id}/cost", id: f.adaWorkspace, other: notFound, member: notFound},
		{
			route: "POST /api/v1/workspaces/{id}/bindings", id: f.adaWorkspace,
			body: map[string]string{"connection_id": f.conn, "target": "x", "mode": "files"}, other: notFound, member: notFound,
		},
		{route: "POST /api/v1/workspaces/{id}/connect", id: f.adaWorkspace, query: "port=web", other: notFound, member: notFound},

		{route: "GET /api/v1/me/ssh-keys", other: ok, member: ok},
		{route: "POST /api/v1/me/ssh-keys", body: map[string]string{"public_key": sshKey(9)}, other: created},
		{route: "DELETE /api/v1/me/ssh-keys/{id}", id: f.key, other: notFound, member: notFound},

		{route: "GET /api/v1/schedules", other: ok, member: ok},
		{
			route: "POST /api/v1/schedules", body: map[string]string{"agent_id": f.adaAgent, "name": "x", "cron": "0 9 * * *", "prompt": "x"},
			other: notFound, member: forbidden,
		},
		{route: "POST /api/v1/schedules/preview", body: map[string]string{"cron": "0 9 * * 1"}, other: ok, member: ok},
		{route: "PATCH /api/v1/schedules/{id}", id: f.schedule, body: map[string]string{"prompt": "Leak it."}, other: notFound, member: forbidden},
		{route: "DELETE /api/v1/schedules/{id}", id: f.schedule, other: notFound, member: forbidden},
		{route: "POST /api/v1/schedules/{id}/run", id: f.schedule, other: notFound, member: forbidden},

		// Boards are the organisation's: a member reads and changes them, but
		// gives an agent work only as its access rule allows.
		{route: "GET /api/v1/boards", other: ok, member: ok},
		{route: "POST /api/v1/boards", body: map[string]string{"name": "Den"}, other: created},
		{route: "GET /api/v1/boards/{id}", id: f.board, other: notFound, member: ok},
		{route: "PATCH /api/v1/boards/{id}", id: f.board, body: map[string]string{"name": "Sales"}, other: notFound, member: ok},
		{route: "DELETE /api/v1/boards/{id}", id: f.board, other: notFound},
		{route: "POST /api/v1/boards/{id}/cards", id: f.board, body: sneakyCard, other: notFound, member: bad},
		{route: "GET /api/v1/cards/{id}", id: f.card, other: notFound, member: ok},
		{route: "PATCH /api/v1/cards/{id}", id: f.card, body: map[string]string{"title": "Call Initech"}, other: notFound, member: bad},
		{route: "DELETE /api/v1/cards/{id}", id: f.card, other: notFound},
		{route: "POST /api/v1/cards/{id}/comments", id: f.card, body: map[string]string{"body": "do it"}, other: notFound, member: forbidden},

		{route: "GET /api/v1/chat/agents", other: ok, member: ok},
		{
			route: "POST /api/v1/chat/agents/{id}/messages", id: f.adaAgent,
			body: map[string]string{"conversation": "c1", "text": "hi"}, other: notFound, member: forbidden,
		},

		// A member sees the tasks they asked for and those of their agents.
		{route: "GET /api/v1/tasks", other: ok, member: ok},
		{route: "GET /api/v1/tasks/facets", other: ok, member: ok},
		{route: "GET /api/v1/tasks/{id}", id: f.task, other: notFound, member: notFound},
		{route: "GET /api/v1/tasks/{id}/events", id: f.task, other: notFound, member: notFound},
		{route: "POST /api/v1/tasks/{id}/cancel", id: f.task, other: notFound, member: notFound},

		{route: "GET /api/v1/connection-kinds", other: ok, member: ok},
		{route: "GET /api/v1/connections", other: ok, member: ok},
		{
			route: "POST /api/v1/connections", body: map[string]any{"kind": "fakechat", "secrets": map[string]string{"token": "t"}},
			other: created, member: forbidden,
		},
		{route: "GET /api/v1/connections/{id}", id: f.conn, other: notFound, member: ok},
		{route: "PATCH /api/v1/connections/{id}", id: f.conn, body: map[string]string{"name": "Mine"}, other: notFound, member: forbidden},
		{route: "DELETE /api/v1/connections/{id}", id: f.conn, other: notFound, member: forbidden},
		{route: "POST /api/v1/connections/{id}/check", id: f.conn, other: notFound, member: forbidden},
		// Members may list folders and sign in; this kind has neither.
		{route: "GET /api/v1/connections/{id}/folders", id: f.conn, other: notFound, member: bad},
		{route: "POST /api/v1/connections/{id}/account", id: f.conn, other: notFound, member: bad},
		{route: "DELETE /api/v1/connections/{id}/account", id: f.conn, other: notFound, member: notFound},
	}
}

// registeredRoutes reads the routes Handler gives a signed-in person.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../api/server.go")
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, m := range regexp.MustCompile(`route\("([A-Z]+ /api/[^"]+)"`).FindAllSubmatch(src, -1) {
		routes = append(routes, string(m[1]))
	}
	if len(routes) < 50 {
		t.Fatalf("found only %d routes in server.go; has route() changed?", len(routes))
	}
	return routes
}

func TestPermissionTableCoversEveryRoute(t *testing.T) {
	var covered []string
	for _, c := range permissionTable(fixture{}) {
		covered = append(covered, c.route)
	}
	for _, r := range registeredRoutes(t) {
		if !slices.Contains(covered, r) {
			t.Errorf("%s is not in the permission table", r)
		}
	}
}

// Every route answers a member of another organisation as if org_a's rows
// did not exist, and a plain member as the code documents, and neither
// changes anything of org_a's.
func TestPermissions(t *testing.T) {
	ta := startApp(t)
	f := seed(t, ta)
	before := orgState(t, ta)

	run := func(t *testing.T, c caller, want func(check) int, secret []string) {
		for _, ch := range permissionTable(f) {
			code := want(ch)
			if code == 0 {
				continue
			}
			method, path := ch.request()
			got, raw := c(method, path, ch.body)
			if got != code {
				t.Errorf("%s %s: %d, want %d: %s", method, path, got, code, raw)
				continue
			}
			if code == http.StatusNotFound && bytes.Contains(raw, []byte("no such API route")) {
				t.Errorf("%s %s: the route does not exist", method, path)
			}
			for _, s := range secret {
				if bytes.Contains(raw, []byte(s)) {
					t.Errorf("%s %s: the answer shows %s: %s", method, path, s, raw)
				}
			}
		}
	}

	t.Run("a member of another organisation", func(t *testing.T) {
		run(t, ta.as(t, "tok-eve"), func(c check) int { return c.other },
			append(f.ids(), "Scout", "Helper", "ada@example.ch", "bob@example.ch"))
		if after := orgState(t, ta); after != before {
			t.Errorf("org_a changed:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		// What Eve made is in her own organisation.
		if a, err := ta.st.Agents(ta.ctx, "org_b"); err != nil || len(a) != 1 || a[0].OwnerID != "usr_eve" {
			t.Errorf("org_b's agents = %+v (%v)", a, err)
		}
	})

	t.Run("a member on rows they do not own", func(t *testing.T) {
		run(t, ta.as(t, "tok-bob"), func(c check) int { return c.member },
			[]string{f.adaWorkspace, f.wksBinding, f.task, f.key})
		if after := orgState(t, ta); after != before {
			t.Errorf("org_a changed:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("a member sees what others' agents may do for them", func(t *testing.T) {
		bob := ta.as(t, "tok-bob")
		var scout struct {
			CanEdit bool `json:"can_edit"`
			CanGive bool `json:"can_give"`
		}
		must(t, bob, "GET", "/api/v1/agents/"+f.adaAgent, nil, http.StatusOK, &scout)
		if scout.CanEdit || scout.CanGive {
			t.Errorf("Bob may change or use Ada's private agent: %+v", scout)
		}
		var offered struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		must(t, bob, "GET", "/api/v1/chat/agents", nil, http.StatusOK, &offered)
		if len(offered.Data) != 1 || offered.Data[0].ID != f.bobAgent {
			t.Errorf("Keera Chat offers Bob %+v, want only Helper", offered.Data)
		}
	})

	t.Run("nobody signed in", func(t *testing.T) {
		for name, token := range map[string]string{"no sign-in": "", "a sign-in the gateway does not know": "tok-mallory"} {
			if got, raw := ta.as(t, token)("GET", "/api/v1/agents", nil); got != http.StatusUnauthorized {
				t.Errorf("%s: %d: %s", name, got, raw)
			}
		}
	})
}

// The owner and administrators can do what the table refuses others.
func TestOwnersAndAdmins(t *testing.T) {
	ta := startApp(t)
	f := seed(t, ta)
	ada, bob := ta.as(t, "tok-ada"), ta.as(t, "tok-bob")
	type view struct {
		ID         string `json:"id"`
		OwnerID    string `json:"owner_id"`
		Enabled    bool   `json:"enabled"`
		State      string `json:"state"`
		CanEdit    bool   `json:"can_edit"`
		CanConnect bool   `json:"can_connect"`
	}

	t.Run("a member runs their own agent", func(t *testing.T) {
		helper := "/api/v1/agents/" + f.bobAgent
		var v view
		must(t, bob, "PATCH", helper, map[string]string{"description": "Answers support"}, http.StatusOK, &v)
		if !v.CanEdit {
			t.Errorf("Bob may not edit his own agent: %+v", v)
		}
		// Both decide what the agent may reach.
		must(t, bob, "PATCH", helper, map[string]string{"image": "evil/image:1"}, http.StatusBadRequest, nil)
		must(t, bob, "PATCH", helper, map[string]string{"project_id": "prj_2"}, http.StatusBadRequest, nil)
		must(t, bob, "POST", helper+"/take-over", nil, http.StatusForbidden, nil)

		must(t, bob, "POST", helper+"/stop", nil, http.StatusOK, &v)
		if v.Enabled || v.State != store.AgentStopped {
			t.Errorf("after stop: %+v", v)
		}
		must(t, bob, "POST", helper+"/tasks", map[string]string{"prompt": "Hello"}, http.StatusConflict, nil)
		must(t, bob, "POST", helper+"/start", nil, http.StatusOK, &v)
		must(t, bob, "POST", helper+"/restart", nil, http.StatusOK, &v)
		if !v.Enabled || v.State != store.AgentRunning {
			t.Errorf("after start and restart: %+v", v)
		}
		must(t, bob, "GET", helper+"/logs", nil, http.StatusOK, nil)

		var task store.Task
		must(t, bob, "POST", helper+"/tasks", map[string]string{"prompt": "Hello"}, http.StatusCreated, &task)
		must(t, bob, "GET", "/api/v1/tasks/"+task.ID, nil, http.StatusOK, nil)
		must(t, ada, "GET", "/api/v1/tasks/"+task.ID, nil, http.StatusOK, nil)
		must(t, bob, "POST", "/api/v1/tasks/"+task.ID+"/cancel", nil, http.StatusOK, &task)
		if task.State != store.TaskCancelled {
			t.Errorf("after cancel: %s", task.State)
		}
		must(t, bob, "POST", "/api/v1/tasks/"+task.ID+"/cancel", nil, http.StatusConflict, nil)

		// Ada's agent listens in #sales, and only an administrator lets
		// another person's agent join it.
		bind := map[string]string{"connection_id": f.conn, "target": "sales", "mode": "chat"}
		must(t, bob, "POST", helper+"/bindings", bind, http.StatusForbidden, nil)
		var b struct {
			ID string `json:"id"`
		}
		bind["target"] = "support"
		must(t, bob, "POST", helper+"/bindings", bind, http.StatusCreated, &b)
		must(t, bob, "DELETE", "/api/v1/bindings/"+b.ID, nil, http.StatusNoContent, nil)

		var sc struct {
			ID          string `json:"id"`
			Enabled     bool   `json:"enabled"`
			Description string `json:"description"`
		}
		must(t, bob, "POST", "/api/v1/schedules", map[string]string{
			"agent_id": f.bobAgent, "name": "Weekly", "cron": "0 9 * * 1", "prompt": "Plan the week.",
		}, http.StatusCreated, &sc)
		if sc.Description != "Every Monday at 09:00" {
			t.Errorf("description = %q", sc.Description)
		}
		must(t, bob, "PATCH", "/api/v1/schedules/"+sc.ID, map[string]bool{"enabled": false}, http.StatusOK, &sc)
		if sc.Enabled {
			t.Error("the schedule is still on")
		}
		must(t, bob, "PATCH", "/api/v1/schedules/"+sc.ID, map[string]string{"agent_id": f.adaAgent}, http.StatusBadRequest, nil)
		must(t, bob, "POST", "/api/v1/schedules/"+sc.ID+"/run", nil, http.StatusCreated, nil)
		must(t, bob, "DELETE", "/api/v1/schedules/"+sc.ID, nil, http.StatusNoContent, nil)
	})

	t.Run("an administrator changes a member's agent", func(t *testing.T) {
		helper := "/api/v1/agents/" + f.bobAgent
		must(t, ada, "PATCH", helper, map[string]string{"description": "Set by Ada"}, http.StatusOK, nil)
		must(t, ada, "POST", helper+"/bindings",
			map[string]string{"connection_id": f.conn, "target": "sales", "mode": "chat"}, http.StatusCreated, nil)
		must(t, ada, "POST", helper+"/stop", nil, http.StatusOK, nil)
		must(t, ada, "POST", helper+"/start", nil, http.StatusOK, nil)
		must(t, ada, "GET", helper+"/logs", nil, http.StatusOK, nil)

		var v view
		must(t, ada, "POST", helper+"/take-over", nil, http.StatusOK, &v)
		if v.OwnerID != "usr_ada" {
			t.Fatalf("after take-over the owner is %q", v.OwnerID)
		}
		must(t, bob, "PATCH", helper, map[string]string{"description": "Mine again"}, http.StatusForbidden, nil)
		must(t, ada, "DELETE", helper, nil, http.StatusNoContent, nil)
		must(t, ada, "GET", helper, nil, http.StatusNotFound, nil)
	})

	t.Run("a workspace is its owner's", func(t *testing.T) {
		lab := "/api/v1/workspaces/" + f.bobWorkspace
		var v view
		must(t, bob, "GET", "/api/v1/workspaces/lab", nil, http.StatusOK, &v)
		if v.ID != f.bobWorkspace || !v.CanConnect || !v.CanEdit {
			t.Errorf("Bob's own workspace by name: %+v", v)
		}
		// The permission check passes; only the upgrade is missing.
		must(t, bob, "POST", lab+"/connect?port=web", nil, http.StatusUpgradeRequired, nil)
		must(t, bob, "POST", lab+"/connect?port=telnet", nil, http.StatusBadRequest, nil)
		must(t, bob, "PATCH", lab, map[string]string{"description": "Mine"}, http.StatusOK, nil)
		must(t, bob, "GET", lab+"/logs", nil, http.StatusOK, nil)
		// Only an administrator chooses an image, and only for their own.
		must(t, bob, "PATCH", lab, map[string]string{"image": "bob/image:1"}, http.StatusBadRequest, nil)
		must(t, ada, "PATCH", "/api/v1/workspaces/dev", map[string]string{"image": "acme/workspace:2"}, http.StatusOK, nil)

		var list struct {
			Data []view `json:"data"`
		}
		must(t, bob, "GET", "/api/v1/workspaces", nil, http.StatusOK, &list)
		if len(list.Data) != 1 || list.Data[0].ID != f.bobWorkspace {
			t.Errorf("Bob's workspaces: %+v", list.Data)
		}
		must(t, ada, "GET", "/api/v1/workspaces?all=true", nil, http.StatusOK, &list)
		if len(list.Data) != 2 {
			t.Errorf("every workspace, for Ada: %+v", list.Data)
		}

		// An administrator sees and changes it, and reads its logs, but
		// cannot get in.
		must(t, ada, "GET", lab, nil, http.StatusOK, &v)
		if !v.CanEdit || v.CanConnect {
			t.Errorf("Bob's workspace, for Ada: %+v", v)
		}
		must(t, ada, "GET", "/api/v1/workspaces/lab", nil, http.StatusNotFound, nil)
		must(t, ada, "PATCH", lab, map[string]string{"description": "Checked"}, http.StatusOK, nil)
		must(t, ada, "GET", lab+"/logs", nil, http.StatusOK, nil)
		must(t, ada, "PATCH", lab, map[string]string{"image": "evil/image:1"}, http.StatusBadRequest, nil)
		must(t, ada, "POST", lab+"/connect?port=web", nil, http.StatusForbidden, nil)
		if a, err := ta.st.Agent(ta.ctx, f.bobWorkspace); err != nil || a.Image != "" || a.Description != "Checked" {
			t.Errorf("Bob's workspace after Ada's changes: image %q, description %q (%v)", a.Image, a.Description, err)
		}
		must(t, ada, "POST", lab+"/stop", nil, http.StatusOK, &v)
		if v.Enabled {
			t.Error("Ada could not stop Bob's workspace")
		}
		must(t, ada, "DELETE", lab, nil, http.StatusNoContent, nil)
		must(t, bob, "GET", "/api/v1/workspaces/lab", nil, http.StatusNotFound, nil)
	})

	t.Run("an administrator creates a workspace for someone else", func(t *testing.T) {
		mine := map[string]string{"name": "pit", "experience": "terminal", "owner_id": "usr_ada"}
		must(t, bob, "POST", "/api/v1/workspaces", mine, http.StatusForbidden, nil)
		// The image would run with Bob's keys.
		must(t, ada, "POST", "/api/v1/workspaces",
			map[string]string{"name": "pit", "experience": "terminal", "owner_id": "usr_bob", "image": "evil/image:1"},
			http.StatusBadRequest, nil)
		must(t, ada, "POST", "/api/v1/workspaces",
			map[string]string{"name": "pit", "experience": "terminal", "owner_id": "usr_eve"}, http.StatusBadRequest, nil)

		var v view
		must(t, ada, "POST", "/api/v1/workspaces",
			map[string]string{"name": "pit", "experience": "terminal", "owner_id": "usr_bob"}, http.StatusCreated, &v)
		if v.OwnerID != "usr_bob" || v.CanConnect {
			t.Errorf("the workspace Ada made for Bob: %+v", v)
		}
		pit := "/api/v1/workspaces/" + v.ID
		must(t, bob, "GET", "/api/v1/workspaces/pit", nil, http.StatusOK, &v)
		if !v.CanConnect {
			t.Errorf("Bob cannot get into his workspace: %+v", v)
		}
		must(t, ada, "POST", pit+"/connect?port=web", nil, http.StatusForbidden, nil)
		must(t, ada, "PATCH", pit, map[string]string{"owner_id": "usr_ada"}, http.StatusBadRequest, nil)
		must(t, ada, "DELETE", pit, nil, http.StatusNoContent, nil)
	})

	t.Run("boards are the organisation's", func(t *testing.T) {
		var card struct {
			ID string `json:"id"`
		}
		must(t, bob, "PATCH", "/api/v1/boards/"+f.board, map[string]string{"name": "Pipeline"}, http.StatusOK, nil)
		must(t, bob, "POST", "/api/v1/boards/"+f.board+"/cards", map[string]string{"title": "Draft offer"}, http.StatusCreated, &card)
		must(t, bob, "PATCH", "/api/v1/cards/"+card.ID, map[string]string{"status": "in_progress"}, http.StatusOK, nil)
		must(t, bob, "POST", "/api/v1/cards/"+card.ID+"/comments", map[string]string{"body": "Started"}, http.StatusCreated, nil)
		must(t, bob, "DELETE", "/api/v1/cards/"+card.ID, nil, http.StatusNoContent, nil)
		// Scout takes work from Ada, so she may rewrite its card in To do.
		must(t, ada, "PATCH", "/api/v1/cards/"+f.card, map[string]string{"title": "Call Initech"}, http.StatusOK, nil)
		must(t, ada, "POST", "/api/v1/cards/"+f.card+"/comments", map[string]string{"body": "Today"}, http.StatusCreated, nil)

		var board struct {
			ID string `json:"id"`
		}
		must(t, ada, "POST", "/api/v1/boards", map[string]string{"name": "Scratch"}, http.StatusCreated, &board)
		must(t, bob, "DELETE", "/api/v1/boards/"+board.ID, nil, http.StatusNoContent, nil)
		must(t, ada, "GET", "/api/v1/boards/"+board.ID, nil, http.StatusNotFound, nil)
	})

	t.Run("a connector lists only the workspaces the reader may see", func(t *testing.T) {
		conn := "/api/v1/connections/" + f.conn
		var c struct {
			Bindings int             `json:"bindings"`
			Uses     []store.Binding `json:"uses"`
		}
		ids := func() []string {
			out := []string{}
			for _, b := range c.Uses {
				out = append(out, b.ID)
			}
			return out
		}
		must(t, bob, "GET", conn, nil, http.StatusOK, &c)
		if !slices.Equal(ids(), []string{f.adaBinding}) || c.Bindings != 2 {
			t.Errorf("Bob sees %v of %d, want only Scout's", ids(), c.Bindings)
		}
		must(t, ada, "GET", conn, nil, http.StatusOK, &c)
		if !slices.Equal(ids(), []string{f.adaBinding, f.wksBinding}) {
			t.Errorf("Ada sees %v, want Scout's and her workspace's", ids())
		}
	})

	t.Run("an administrator changes connectors", func(t *testing.T) {
		conn := "/api/v1/connections/" + f.conn
		var c struct {
			Name       string   `json:"name"`
			SecretsSet []string `json:"secrets_set"`
		}
		must(t, ada, "PATCH", conn, map[string]string{"name": "Sales chat"}, http.StatusOK, &c)
		if c.Name != "Sales chat" || !slices.Equal(c.SecretsSet, []string{"token"}) {
			t.Errorf("after rename: %+v", c)
		}
		must(t, ada, "POST", conn+"/check", nil, http.StatusOK, nil)
		must(t, ada, "PATCH", conn, map[string]string{"kind": "other"}, http.StatusBadRequest, nil)
		var audit struct {
			Data []store.AuditEntry `json:"data"`
		}
		must(t, ada, "GET", "/api/v1/audit", nil, http.StatusOK, &audit)
		if !slices.ContainsFunc(audit.Data, func(e store.AuditEntry) bool {
			return e.Action == "connection.update" && e.Target == f.conn && e.Actor == "ada@example.ch"
		}) {
			t.Errorf("the audit log lacks the change: %+v", audit.Data)
		}
		must(t, ada, "DELETE", conn, nil, http.StatusNoContent, nil)
		must(t, bob, "GET", conn+"/folders", nil, http.StatusNotFound, nil)
	})

	t.Run("SSH keys are each person's own", func(t *testing.T) {
		var k struct {
			ID string `json:"id"`
		}
		must(t, bob, "POST", "/api/v1/me/ssh-keys", map[string]string{"public_key": sshKey(2) + " bob@laptop"}, http.StatusCreated, &k)
		must(t, bob, "POST", "/api/v1/me/ssh-keys", map[string]string{"public_key": sshKey(2)}, http.StatusConflict, nil)
		var keys struct {
			Data []store.SSHKey `json:"data"`
		}
		must(t, bob, "GET", "/api/v1/me/ssh-keys", nil, http.StatusOK, &keys)
		if len(keys.Data) != 1 || keys.Data[0].ID != k.ID || keys.Data[0].Name != "bob@laptop" {
			t.Errorf("Bob's keys: %+v", keys.Data)
		}
		must(t, ada, "DELETE", "/api/v1/me/ssh-keys/"+k.ID, nil, http.StatusNotFound, nil)
		must(t, bob, "DELETE", "/api/v1/me/ssh-keys/"+k.ID, nil, http.StatusNoContent, nil)
		must(t, bob, "DELETE", "/api/v1/me/ssh-keys/"+k.ID, nil, http.StatusNotFound, nil)
	})

	t.Run("deleting an agent cancels its work", func(t *testing.T) {
		must(t, ada, "DELETE", "/api/v1/agents/"+f.adaAgent, nil, http.StatusNoContent, nil)
		if task, err := ta.st.Task(ta.ctx, f.task); err != nil || task.State != store.TaskCancelled {
			t.Errorf("Scout's queued task: %s (%v)", task.State, err)
		}
		if s, err := ta.st.Schedules(ta.ctx, "org_a", f.adaAgent); err != nil || len(s) != 0 {
			t.Errorf("Scout's schedules: %+v (%v)", s, err)
		}
	})
}
