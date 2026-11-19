package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/agent"
	"github.com/bespinian/keera-agents/internal/config"
	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
	"github.com/bespinian/keera-agents/internal/store/storetest"
)

// This test runs the control plane against a real Postgres, a stand-in
// gateway, a stand-in chat service and a driver that runs each agent's
// runtime in the test binary. A message in a chat must come back as a reply
// in its thread, after the agent called a platform tool through the gateway.

// fakeGateway answers the control API and the inference API.
type fakeGateway struct {
	mu      sync.Mutex
	revoked []string
	session string
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	service := r.Header.Get("Authorization") == "Bearer "+strings.Repeat("s", 32)
	switch {
	case r.URL.Path == "/control/v1/me" && r.Method == http.MethodGet:
		me, ok := people[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			reply(map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "sign in"}})
			return
		}
		reply(me)
	case r.URL.Path == "/control/v1/me/org" && r.Method == http.MethodPost:
		// Only an organisation the person is in.
		var in struct {
			OrgID string `json:"org_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if me := people[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; me.OrgID != in.OrgID {
			w.WriteHeader(http.StatusForbidden)
			reply(map[string]any{"error": map[string]string{"code": "forbidden", "message": "not a member"}})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/control/v1/service/keys" && service && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusCreated)
		reply(map[string]string{"id": "key_1", "project_id": "prj_1", "key": "sk-agent"})
	case strings.HasPrefix(r.URL.Path, "/control/v1/service/keys/") && service && r.Method == http.MethodDelete:
		if r.URL.Query().Get("org_id") != "org_a" {
			http.Error(w, "org", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.revoked = append(g.revoked, strings.TrimPrefix(r.URL.Path, "/control/v1/service/keys/"))
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/control/v1/users" && r.Method == http.MethodGet:
		// Only an administrator lists their own organisation's people.
		me := people[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !me.CanAdminOrg || r.URL.Query().Get("org_id") != me.OrgID {
			w.WriteHeader(http.StatusForbidden)
			reply(map[string]any{"error": map[string]string{"code": "forbidden", "message": "administrators only"}})
			return
		}
		var users []gateway.User
		for _, p := range people {
			if p.OrgID == me.OrgID {
				users = append(users, gateway.User{ID: p.UserID, Email: p.Email, Name: p.Name})
			}
		}
		reply(map[string]any{"data": users})
	case r.URL.Path == "/control/v1/service/users" && service:
		q := r.URL.Query()
		if strings.EqualFold(q.Get("email"), "ada@example.ch") || q.Get("id") == "usr_ada" {
			reply(gateway.User{ID: "usr_ada", Email: "ada@example.ch", Name: "Ada"})
			return
		}
		if q.Get("id") == "usr_bob" && q.Get("org_id") == "org_a" {
			reply(gateway.User{ID: "usr_bob", Email: "bob@example.ch", Name: "Bob"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		reply(map[string]any{"error": map[string]string{"code": "not_found", "message": "not a member"}})
	case r.URL.Path == "/control/v1/service/catalog" && service:
		reply(gateway.Catalog{ProjectID: "prj_1", Models: []gateway.CatalogRef{{Alias: "keera-speed", Kind: "model"}}})
	case r.URL.Path == "/api/v1/chat/completions" && r.Header.Get("Authorization") == "Bearer sk-agent":
		g.mu.Lock()
		g.session = r.Header.Get("X-Keera-Session")
		g.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		// First a tool call, then the answer once the tool's result is in.
		if !bytes.Contains(body, []byte(`"role":"tool"`)) {
			reply(map[string]any{"choices": []any{map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]string{"name": "board_create_card", "arguments": `{"title":"Follow up with Globex"}`},
				}}},
			}}})
			return
		}
		reply(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": "All done: I put a card on the board."},
		}}})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

// people are whom the stand-in gateway knows, by sign-in token: Ada
// administers org_a, Bob is a plain member of it, and Eve administers org_b.
var people = map[string]gateway.Me{
	"tok-ada": {
		UserID: "usr_ada", Email: "ada@example.ch", Name: "Ada", Role: "admin",
		OrgID: "org_a", OrgName: "Acme", CanAdminOrg: true, ChatURL: "https://chat.example.ch",
	},
	"tok-bob": {UserID: "usr_bob", Email: "bob@example.ch", Name: "Bob", Role: "member", OrgID: "org_a", OrgName: "Acme"},
	"tok-eve": {
		UserID: "usr_eve", Email: "eve@example.ch", Name: "Eve", Role: "admin",
		OrgID: "org_b", OrgName: "Bravo", CanAdminOrg: true,
	},
}

// fakeChat is a chat service whose webhooks are plain JSON.
type fakeChat struct {
	mu   sync.Mutex
	sent []string
}

func (c *fakeChat) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind: "fakechat", Label: "Fake Chat", Chat: true, TargetLabel: "Channel",
		Fields: []connectors.Field{{Name: "token", Label: "Token", Secret: true, Required: true}},
	}
}

func (c *fakeChat) Check(_ context.Context, conn connectors.Connection) error { return nil }

func (c *fakeChat) Receive(_ context.Context, conn connectors.Connection, r *http.Request, body []byte) (connectors.Receipt, error) {
	if r.Header.Get("X-Token") != conn.Secrets["token"] {
		return connectors.Receipt{}, connectors.ErrUnverified
	}
	var in struct{ ID, Text, Email, Channel string }
	_ = json.Unmarshal(body, &in)
	return connectors.Receipt{Messages: []connectors.Inbound{{
		Target: connectors.Target{Conversation: in.Channel, Thread: in.ID}, Sender: connectors.Person{Email: in.Email},
		Text: in.Text, MessageID: in.ID, ConversationLabel: "#" + in.Channel,
	}}}, nil
}

func (c *fakeChat) Send(_ context.Context, _ connectors.Connection, to connectors.Target, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, to.Conversation+"/"+to.Thread+": "+text)
	return "m", nil
}

func (c *fakeChat) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

// inProcess runs each agent's runtime as a goroutine.
type inProcess struct {
	mu         sync.Mutex
	runs       map[string]context.CancelFunc
	tokens     map[string]string
	terminated []string
	workspace  string
	// idle starts no runtime: the machine only says it runs.
	idle bool
}

func (d *inProcess) Name() string { return "test" }
func (d *inProcess) Tiers() []runtime.Isolation {
	return []runtime.Isolation{runtime.Standard, runtime.Isolated}
}

func (d *inProcess) Create(_ context.Context, spec runtime.Spec) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cancel, ok := d.runs[spec.AgentID]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.runs[spec.AgentID] = cancel
	d.tokens[spec.AgentID] = spec.Env[protocol.EnvRuntimeToken]
	env := map[string]string{}
	maps.Copy(env, spec.Env)
	env[protocol.EnvWorkspace] = d.workspace
	if !d.idle {
		go func() { _ = agent.Serve(ctx, slog.New(slog.DiscardHandler), func(k string) string { return env[k] }) }()
	}
	return runtime.Status{State: runtime.Running}, nil
}

func (d *inProcess) Status(_ context.Context, id string) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.runs[id]; ok {
		return runtime.Status{State: runtime.Running, Token: d.tokens[id]}, nil
	}
	return runtime.Status{State: runtime.Gone}, nil
}
func (d *inProcess) Suspend(context.Context, string) error { return nil }
func (d *inProcess) Resume(context.Context, string) error  { return nil }

func (d *inProcess) Terminate(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cancel, ok := d.runs[id]; ok {
		cancel()
		delete(d.runs, id)
	}
	d.terminated = append(d.terminated, id)
	return nil
}

func (d *inProcess) Logs(context.Context, string, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (d *inProcess) Address(context.Context, string) (string, error) { return "", runtime.ErrNotFound }

// testApp is the control plane, started for a test with its stand-ins, and
// a signed-in administrator, Ada, to call its API as.
type testApp struct {
	ctx    context.Context
	st     *store.Store
	app    *App
	srv    *httptest.Server
	gw     *fakeGateway
	driver *inProcess
	box    *secret.Box
	cookie *http.Cookie
	csrf   string
	// call sends Ada's cookie and CSRF token; chatCall her bearer token, as
	// Keera Chat does. Each decodes a 2xx answer into out.
	call, chatCall func(method, path string, in any, out any) int
	waitFor        func(what string, cond func() bool)
}

// startApp starts the control plane against KEERA_AGENTS_TEST_DATABASE_URL,
// on an empty schema, or skips.
func startApp(t *testing.T) *testApp {
	t.Helper()
	st, _ := storetest.Open(t)
	ctx := context.Background()

	gw := &fakeGateway{}
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	driver := &inProcess{runs: map[string]context.CancelFunc{}, tokens: map[string]string{}, workspace: t.TempDir()}
	box, _ := secret.New("0123456789abcdef")

	// The app needs its own address before it is built, for the runtime URL.
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	cfg := config.Config{
		PublicURL: srv.URL, GatewayURL: gwSrv.URL, GatewayInternalURL: gwSrv.URL,
		TaskTimeout: time.Minute, WatchInterval: time.Minute, MetricsToken: "metrics-0123456789",
		Runtime: config.RuntimeConfig{Driver: "test", Image: "img", URL: srv.URL, GatewayURL: gwSrv.URL},
	}
	app, err := New(cfg, st, gateway.New(gwSrv.URL, strings.Repeat("s", 32)), driver, box,
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	handler = app.Handler

	// A signed-in session, sealed as the API seals it.
	sess, _ := json.Marshal(map[string]any{"t": "tok-ada", "x": time.Now().Add(time.Hour).Unix()})
	cookie := &http.Cookie{Name: "keera_agents", Value: base64.RawURLEncoding.EncodeToString(box.Seal("cookie:keera_agents", sess))}
	sum := sha256.Sum256([]byte("keera-agents-csrf|tok-ada"))
	csrf := hex.EncodeToString(sum[:16])
	client := func(auth func(*http.Request)) func(method, path string, in any, out any) int {
		return func(method, path string, in any, out any) int {
			t.Helper()
			var body io.Reader
			if in != nil {
				b, _ := json.Marshal(in)
				body = bytes.NewReader(b)
			}
			req, _ := http.NewRequest(method, srv.URL+path, body)
			auth(req)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			if out != nil && res.StatusCode < 300 {
				if err := json.Unmarshal(raw, out); err != nil {
					t.Fatalf("%s %s: %v: %s", method, path, err, raw)
				}
			}
			if res.StatusCode >= 300 {
				t.Logf("%s %s: %d %s", method, path, res.StatusCode, raw)
			}
			return res.StatusCode
		}
	}
	call := client(func(req *http.Request) {
		req.AddCookie(cookie)
		req.Header.Set("X-CSRF-Token", csrf)
	})
	// Keera Chat sends the gateway sign-in as a bearer token, with no cookie
	// and no CSRF token.
	chatCall := client(func(req *http.Request) { req.Header.Set("Authorization", "Bearer tok-ada") })
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	return &testApp{
		ctx: ctx, st: st, app: app, srv: srv, gw: gw, driver: driver, box: box, cookie: cookie, csrf: csrf,
		call: call, chatCall: chatCall, waitFor: waitFor,
	}
}

func TestEndToEnd(t *testing.T) {
	ta := startApp(t)
	ctx, st, app, srv, gw, driver := ta.ctx, ta.st, ta.app, ta.srv, ta.gw, ta.driver
	cookie, call, chatCall, waitFor := ta.cookie, ta.call, ta.chatCall, ta.waitFor
	chat := &fakeChat{}
	connectors.Register(chat)

	var me map[string]any
	if call("GET", "/api/v1/me", nil, &me) != 200 || me["org_id"] != "org_a" || me["can_admin"] != true ||
		me["chat_url"] != "https://chat.example.ch" {
		t.Fatalf("me = %v", me)
	}
	// Without the CSRF token, nothing changes.
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/boards", strings.NewReader(`{"name":"x"}`))
	req.AddCookie(cookie)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF token: %d", res.StatusCode)
	}

	var a struct {
		ID string `json:"id"`
	}
	if code := call("POST", "/api/v1/agents", map[string]any{
		"name": "Scout", "model": "keera-speed", "instructions": "Be brief.",
	}, &a); code != http.StatusCreated {
		t.Fatalf("create agent: %d", code)
	}
	if code := call("POST", "/api/v1/agents", map[string]any{"name": "Scout", "model": "keera-speed"}, nil); code != http.StatusConflict {
		t.Fatalf("same name again: %d", code)
	}
	if code := call("POST", "/api/v1/agents", map[string]any{"name": "Other", "model": "gpt-9"}, nil); code != http.StatusBadRequest {
		t.Fatalf("a model the project does not allow: %d", code)
	}
	waitFor("the agent to start", func() bool {
		ag, err := st.Agent(ctx, a.ID)
		return err == nil && ag.GatewayKeyID == "key_1" && ag.State == store.AgentRunning
	})

	var conn struct {
		ID         string `json:"id"`
		WebhookURL string `json:"webhook_url"`
	}
	if code := call("POST", "/api/v1/connections", map[string]any{
		"kind": "fakechat", "name": "Team chat", "secrets": map[string]string{"token": "hook-secret"},
	}, &conn); code != http.StatusCreated || !strings.HasSuffix(conn.WebhookURL, "/hooks/"+conn.ID) {
		t.Fatalf("create connection: %d %+v", code, conn)
	}
	if code := call("POST", "/api/v1/agents/"+a.ID+"/bindings", map[string]any{
		"connection_id": conn.ID, "target": "sales", "mode": "chat",
	}, nil); code != http.StatusCreated {
		t.Fatalf("bind: %d", code)
	}

	hook := func(token, body string) int {
		req, _ := http.NewRequest("POST", conn.WebhookURL, strings.NewReader(body))
		req.Header.Set("X-Token", token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := hook("wrong", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("unverified webhook: %d", code)
	}
	if code := hook("hook-secret", `{"ID":"m1","Text":"Scout: follow up with Globex","Email":"ada@example.ch","Channel":"sales"}`); code != 200 {
		t.Fatalf("webhook: %d", code)
	}
	// The same message again does nothing.
	hook("hook-secret", `{"ID":"m1","Text":"Scout: follow up with Globex","Email":"ada@example.ch","Channel":"sales"}`)

	waitFor("the reply in the thread", func() bool {
		return slices.Contains(chat.messages(), "sales/m1: All done: I put a card on the board.")
	})

	// The task, its tool call, its webhooks and its delivery are counted.
	scrape := func() string {
		req, _ := http.NewRequest("GET", srv.URL+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer metrics-0123456789")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("metrics: %d %s", res.StatusCode, raw)
		}
		return string(raw)
	}
	var metricsOut string
	waitFor("the task in /metrics", func() bool {
		metricsOut = scrape()
		return strings.Contains(metricsOut, `keera_agents_tasks_total{source="chat",state="succeeded"} 1`+"\n")
	})
	for _, want := range []string{
		`keera_agents_deliveries_total{source="chat",outcome="ok"} 1`,
		`keera_agents_tool_calls_total{kind="board",outcome="ok"} 1`,
		`keera_agents_webhooks_total{kind="fakechat",outcome="unverified"} 1`,
		`keera_agents_webhooks_total{kind="fakechat",outcome="accepted"} 2`,
		`keera_agents_machine_starts_total{kind="agent",outcome="ok"} 1`,
		`keera_agents_machines{kind="agent",state="running"} 1`,
		"keera_agents_tasks_queued 0",
		"keera_agents_results_undelivered 0",
	} {
		if !strings.Contains(metricsOut, want+"\n") {
			t.Errorf("/metrics lacks %s:\n%s", want, metricsOut)
		}
	}

	var tasks struct {
		Data []store.Task `json:"data"`
	}
	call("GET", "/api/v1/tasks?agent_id="+a.ID, nil, &tasks)
	if len(tasks.Data) != 1 || tasks.Data[0].State != store.TaskSucceeded || tasks.Data[0].OriginLabel != "Fake Chat #sales" {
		t.Fatalf("tasks = %+v", tasks.Data)
	}
	gw.mu.Lock()
	session := gw.session
	gw.mu.Unlock()
	if session != tasks.Data[0].ID {
		t.Fatalf("the gateway saw session %q, want the task's id", session)
	}
	var detail struct {
		Events []store.Event `json:"events"`
	}
	call("GET", "/api/v1/tasks/"+tasks.Data[0].ID, nil, &detail)
	var sawTool bool
	for _, e := range detail.Events {
		sawTool = sawTool || (e.Kind == protocol.EventToolCall && strings.Contains(e.Text, "board_create_card"))
	}
	if !sawTool {
		t.Fatalf("no tool call among the events: %+v", detail.Events)
	}

	var boards struct {
		Data []store.Board `json:"data"`
	}
	call("GET", "/api/v1/boards", nil, &boards)
	var board struct {
		Cards []store.Card `json:"cards"`
	}
	call("GET", "/api/v1/boards/"+boards.Data[0].ID, nil, &board)
	if len(board.Cards) != 1 || board.Cards[0].Title != "Follow up with Globex" || board.Cards[0].CreatedBy != "Scout" {
		t.Fatalf("cards = %+v", board.Cards)
	}

	// Keera Chat offers the agent and reads its answer from the task. Nothing
	// is posted to a chat service.
	var offered struct {
		Data []struct {
			ID           string `json:"id"`
			Instructions string `json:"instructions"`
		} `json:"data"`
	}
	var detail0 struct {
		CanGive bool `json:"can_give"`
	}
	if call("GET", "/api/v1/agents/"+a.ID, nil, &detail0) != 200 || !detail0.CanGive {
		t.Fatalf("the owner may not give the agent work: %+v", detail0)
	}
	if chatCall("GET", "/api/v1/chat/agents", nil, &offered) != 200 || len(offered.Data) != 1 ||
		offered.Data[0].ID != a.ID || offered.Data[0].Instructions != "" {
		t.Fatalf("agents offered to Keera Chat = %+v", offered.Data)
	}
	if code := chatCall("POST", "/api/v1/chat/agents/"+a.ID+"/messages",
		map[string]string{"conversation": "../c1", "text": "hi"}, nil); code != http.StatusBadRequest {
		t.Fatalf("a conversation id with a path in it: %d", code)
	}
	sentBefore := len(chat.messages())
	var asked store.Task
	if code := chatCall("POST", "/api/v1/chat/agents/"+a.ID+"/messages",
		map[string]string{"conversation": "c0123", "text": "Follow up with Initech"}, &asked); code != http.StatusCreated ||
		asked.OriginLabel != store.KeeraChat {
		t.Fatalf("a message from Keera Chat: %d %+v", code, asked)
	}
	waitFor("the answer for Keera Chat", func() bool {
		var ev struct {
			Task store.Task `json:"task"`
		}
		chatCall("GET", "/api/v1/tasks/"+asked.ID+"/events", nil, &ev)
		return ev.Task.State == store.TaskSucceeded && ev.Task.Result == "All done: I put a card on the board." &&
			ev.Task.Delivered
	})
	if n := len(chat.messages()); n != sentBefore {
		t.Fatalf("a Keera Chat answer was posted to a chat service: %v", chat.messages())
	}

	// Somebody outside the organisation is told no, and nothing is queued.
	hook("hook-secret", `{"ID":"m2","Text":"do my homework","Email":"eve@example.ch","Channel":"sales"}`)
	waitFor("the refusal", func() bool {
		for _, m := range chat.messages() {
			if strings.HasPrefix(m, "sales/m2: ") && strings.Contains(m, "eve@example.ch is not one of them") {
				return true
			}
		}
		return false
	})

	var preview struct {
		Description string      `json:"description"`
		Next        []time.Time `json:"next"`
	}
	if call("POST", "/api/v1/schedules/preview", map[string]string{"cron": "30 7 * * 1-5", "timezone": "Europe/Zurich"}, &preview) != 200 ||
		preview.Description != "Every weekday at 07:30" || len(preview.Next) != 5 {
		t.Fatalf("preview = %+v", preview)
	}

	// An agent whose key was revoked, as when its owner left and came back,
	// gets a new one at its next bootstrap.
	keyed, err := st.Agent(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAgentKey(ctx, a.ID, keyed.GatewayKeyID, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRuntimeToken(ctx, a.ID, fleet.TokenHash("kat_test")); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest("GET", srv.URL+protocol.BootstrapPath, nil)
	req.Header.Set("Authorization", "Bearer kat_test")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var boot protocol.Bootstrap
	_ = json.NewDecoder(res.Body).Decode(&boot)
	res.Body.Close()
	if res.StatusCode != 200 || boot.Gateway.APIKey != "sk-agent" || boot.Agent.Instructions != "Be brief." ||
		boot.Version == "" || len(boot.Tools) == 0 {
		t.Fatalf("bootstrap after the key was revoked: %d %+v", res.StatusCode, boot.Agent)
	}

	// The running runtime's token is no longer the stored one, so it could
	// never fetch a task again. Reconcile starts it with a new token.
	app.fleet.Reconcile(ctx)
	driver.mu.Lock()
	token := driver.tokens[a.ID]
	driver.mu.Unlock()
	if after, err := st.Agent(ctx, a.ID); err != nil || token == "kat_test" ||
		!bytes.Equal(after.RuntimeTokenHash, fleet.TokenHash(token)) {
		t.Fatalf("after reconcile: the container's token does not match the stored one (%v)", err)
	}

	// A stopped agent's token gets nothing, not even a bootstrap.
	if code := call("POST", "/api/v1/agents/"+a.ID+"/stop", nil, nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	req, _ = http.NewRequest("GET", srv.URL+protocol.BootstrapPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if res, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("bootstrap of a stopped agent: %d", res.StatusCode)
	}

	if code := call("DELETE", "/api/v1/agents/"+a.ID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	gw.mu.Lock()
	revoked := gw.revoked
	gw.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "key_1" || len(driver.terminated) != 1 {
		t.Fatalf("after delete: revoked %v, terminated %v", revoked, driver.terminated)
	}
}
