package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
	"github.com/bespinian/keera-agents/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// These tests run the dispatcher against a real Postgres, a stand-in for the
// gateway's service routes and fake adapters. Adapters are registered by
// kind for the whole binary, so the fakes have kinds of their own.

const serviceKey = "service-key-0123456789abcdef0123"

// fakeGateway answers the gateway's member lookups, and model calls with
// agentKey. down makes it fail.
type fakeGateway struct {
	users map[string]gateway.User // org|email and org|id
	down  atomic.Bool
	calls atomic.Int32
	// models holds the model calls it answered.
	mu     sync.Mutex
	models []modelCall
}

func newFakeGateway() *fakeGateway {
	g := &fakeGateway{users: map[string]gateway.User{}}
	g.add("org_a", gateway.User{ID: "usr_ada", Email: "ada@example.ch", Name: "Ada"})
	g.add("org_a", gateway.User{ID: "usr_bob", Email: "bob@example.ch", Name: "Bob"})
	g.add("org_a", gateway.User{ID: "usr_dis", Email: "dis@example.ch", Name: "Dis", Disabled: true})
	g.add("org_b", gateway.User{ID: "usr_eve", Email: "eve@example.ch", Name: "Eve"})
	return g
}

func (g *fakeGateway) add(org string, u gateway.User) {
	g.users[org+"|"+strings.ToLower(u.Email)] = u
	g.users[org+"|"+u.ID] = u
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.calls.Add(1)
	if strings.HasPrefix(r.URL.Path, gateway.ModelPrefix+"/") {
		g.model(w, r)
		return
	}
	if r.URL.Path != "/control/v1/service/users" || r.Header.Get("Authorization") != "Bearer "+serviceKey {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		return
	}
	if g.down.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"down"}}`))
		return
	}
	q := r.URL.Query()
	key := q.Get("org_id") + "|" + strings.ToLower(q.Get("email"))
	if q.Get("id") != "" {
		key = q.Get("org_id") + "|" + q.Get("id")
	}
	u, ok := g.users[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not a member"}}`))
		return
	}
	_ = json.NewEncoder(w).Encode(u)
}

// fakeChat is a chat service whose webhooks are plain JSON, checked by a
// token in a header.
type fakeChat struct {
	mu   sync.Mutex
	sent []string
	fail error
}

const chatKind = "dispatchchat"

func (c *fakeChat) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{Kind: chatKind, Label: "Test Chat"}
}

func (c *fakeChat) Check(context.Context, connectors.Connection) error { return nil }

// hookBody is one webhook of fakeChat.
type hookBody struct {
	ID, Text, Email, Channel string
	Outside, Direct          bool
	// Challenge is answered in the response, as Slack's URL check is.
	Challenge string
}

func (c *fakeChat) Receive(_ context.Context, conn connectors.Connection, r *http.Request, body []byte) (connectors.Receipt, error) {
	if r.Header.Get("X-Token") != conn.Secrets["token"] {
		return connectors.Receipt{}, connectors.ErrUnverified
	}
	var in hookBody
	if err := json.Unmarshal(body, &in); err != nil {
		return connectors.Receipt{}, err
	}
	if in.Challenge != "" {
		return connectors.Receipt{Response: connectors.Response{
			Status: http.StatusAccepted, ContentType: "text/plain", Body: []byte(in.Challenge),
		}}, nil
	}
	return connectors.Receipt{Messages: []connectors.Inbound{inbound(in)}}, nil
}

func inbound(in hookBody) connectors.Inbound {
	return connectors.Inbound{
		Target:    connectors.Target{Conversation: in.Channel, Thread: in.ID},
		Sender:    connectors.Person{Email: in.Email, Outside: in.Outside},
		Text:      in.Text,
		MessageID: in.ID, Direct: in.Direct, ConversationLabel: "#" + in.Channel,
	}
}

func (c *fakeChat) Send(_ context.Context, _ connectors.Connection, to connectors.Target, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return "", c.fail
	}
	c.sent = append(c.sent, to.Conversation+"/"+to.Thread+": "+text)
	return "m", nil
}

func (c *fakeChat) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

func (c *fakeChat) failWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = err
}

// testEnv is a dispatcher on an empty schema, with its stand-ins.
type testEnv struct {
	ctx    context.Context
	dsn    string
	st     *store.Store
	d      *Dispatcher
	gw     *fakeGateway
	chat   *fakeChat
	queued chan string
}

// setup connects to KEERA_AGENTS_TEST_DATABASE_URL, or skips. Each test
// starts from an empty schema, so it sees only its own rows.
func setup(t *testing.T) *testEnv {
	t.Helper()
	st, dsn := storetest.Open(t)
	ctx := context.Background()

	gw := newFakeGateway()
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	gwc := gateway.New(gwSrv.URL, serviceKey)
	box, _ := secret.New("0123456789abcdef")
	chat := &fakeChat{}
	connectors.Register(chat)
	queued := make(chan string, 64)
	// No driver: waking an agent does nothing.
	fl := fleet.New(st, gwc, nil, box, fleet.Options{Log: slog.New(slog.DiscardHandler)})
	d := New(st, fl, gwc, box, Options{
		PublicURL: "https://agents.example.ch", WatchInterval: time.Minute,
		OnQueued: func(agentID string) { queued <- agentID },
		Log:      slog.New(slog.DiscardHandler),
	})
	return &testEnv{ctx: ctx, dsn: dsn, st: st, d: d, gw: gw, chat: chat, queued: queued}
}

// agent makes an agent in org owned by Ada.
func (e *testEnv) agent(t *testing.T, org, name, access string, allowed ...string) store.Agent {
	t.Helper()
	a, err := e.st.CreateAgent(e.ctx, store.Agent{
		ID: id.New("agt"), OrgID: org, OwnerID: "usr_ada", OwnerEmail: "ada@example.ch",
		Name: name, Model: "keera-speed", MaxSteps: 10, Access: access, AllowedEmails: allowed, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// conn makes a connection of kind in org, its secrets sealed.
func (e *testEnv) conn(t *testing.T, org, kind string, secrets map[string]string) store.Connection {
	t.Helper()
	cid := id.New("con")
	c, err := e.st.CreateConnection(e.ctx, store.Connection{
		ID: cid, OrgID: org, Kind: kind, Name: kind + " " + org, Secrets: e.d.SealSecrets(cid, secrets),
		CreatedBy: "ada@example.ch",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// bind binds an agent to a connection's target, as Ada.
func (e *testEnv) bind(t *testing.T, a store.Agent, c store.Connection, target, mode string) store.Binding {
	t.Helper()
	b, err := e.st.CreateBinding(e.ctx, store.Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID, Target: target, Mode: mode,
		CreatedBy: "ada@example.ch", CreatedByID: "usr_ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tasks lists an agent's tasks, newest first.
func (e *testEnv) tasks(t *testing.T, a store.Agent) []store.Task {
	t.Helper()
	ts, err := e.st.Tasks(e.ctx, store.TaskQuery{OrgID: a.OrgID, AgentID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// finish ends a task as its runtime would.
func (e *testEnv) finish(t *testing.T, taskID, state, result, errText string) store.Task {
	t.Helper()
	if ok, err := e.st.FinishTask(e.ctx, taskID, state, result, errText); err != nil || !ok {
		t.Fatalf("FinishTask = %v, %v", ok, err)
	}
	out, err := e.st.Task(e.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// exec runs SQL the store has no method for, such as a trigger that makes
// a write fail.
func (e *testEnv) exec(t *testing.T, sql string) {
	t.Helper()
	db, err := pgx.Connect(e.ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(e.ctx) }()
	if _, err := db.Exec(e.ctx, sql); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitQueuesAndWakes(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	if _, err := e.d.Submit(e.ctx, store.Task{OrgID: "org_a", AgentID: a.ID, Prompt: "  \n "}); !errors.Is(err, ErrNoPrompt) {
		t.Fatalf("an empty prompt: %v", err)
	}
	if n := len(e.tasks(t, a)); n != 0 {
		t.Fatalf("an empty prompt queued %d tasks", n)
	}
	key := "k1"
	task, err := e.d.Submit(e.ctx, store.Task{
		OrgID: "org_a", AgentID: a.ID, Source: protocol.SourceManual, Prompt: " do it ", DedupeKey: &key,
	})
	if err != nil || task.Prompt != "do it" || task.State != store.TaskQueued {
		t.Fatalf("Submit = %+v, %v", task, err)
	}
	if got := <-e.queued; got != a.ID {
		t.Fatalf("OnQueued got %q", got)
	}
	if _, err := e.d.Submit(e.ctx, store.Task{OrgID: "org_a", AgentID: a.ID, Prompt: "again", DedupeKey: &key}); !errors.Is(err, errDuplicate) {
		t.Fatalf("the same event again: %v", err)
	}
	if len(e.queued) != 0 {
		t.Fatal("a duplicate woke the agent")
	}
}

func TestCancelEndsATaskOnce(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	task, err := e.d.Submit(e.ctx, store.Task{OrgID: "org_a", AgentID: a.ID, Source: protocol.SourceManual, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.Cancel(e.ctx, task, "ada@example.ch"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.Task(e.ctx, task.ID)
	if got.State != store.TaskCancelled || got.Error != "cancelled by ada@example.ch" {
		t.Fatalf("cancelled task = %s %q", got.State, got.Error)
	}
	if err := e.d.Cancel(e.ctx, task, "ada@example.ch"); !errors.Is(err, ErrFinished) {
		t.Fatalf("cancel again: %v", err)
	}
}
