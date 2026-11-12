package dispatch

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// hook sends a webhook to ServeHook and returns its response.
func (e *testEnv) hook(method, connID, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, HookPath+connID, strings.NewReader(body))
	req.Header.Set("X-Token", token)
	w := httptest.NewRecorder()
	e.d.ServeHook(w, req)
	return w
}

func TestServeHookChecksTheRequest(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "hook-secret"})
	e.bind(t, a, c, store.AnyConversation, store.ModeChat)
	msg := `{"ID":"m1","Text":"hi","Email":"ada@example.ch","Channel":"sales"}`

	// Secrets sealed under another id cannot be opened.
	broken := e.conn(t, "org_a", chatKind, nil)
	broken.Secrets = e.d.SealSecrets("con_other", map[string]string{"token": "hook-secret"})
	if _, err := e.st.UpdateConnection(e.ctx, broken); err != nil {
		t.Fatal(err)
	}
	notChat := e.conn(t, "org_a", "nosuchkind", nil)

	for _, tc := range []struct {
		name, method, conn, token, body string
		want                            int
	}{
		{"a GET", http.MethodGet, c.ID, "hook-secret", msg, http.StatusMethodNotAllowed},
		{"an unknown connection", http.MethodPost, "con_nope", "hook-secret", msg, http.StatusNotFound},
		{"a kind that is not a chat", http.MethodPost, notChat.ID, "hook-secret", msg, http.StatusNotFound},
		{"credentials that cannot be read", http.MethodPost, broken.ID, "hook-secret", msg, http.StatusServiceUnavailable},
		{"a wrong token", http.MethodPost, c.ID, "guess", msg, http.StatusUnauthorized},
		{"no token", http.MethodPost, c.ID, "", msg, http.StatusUnauthorized},
		{"a body that is not JSON", http.MethodPost, c.ID, "hook-secret", "{", http.StatusBadRequest},
	} {
		if w := e.hook(tc.method, tc.conn, tc.token, tc.body); w.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	if n := len(e.tasks(t, a)); n != 0 {
		t.Fatalf("refused webhooks queued %d tasks", n)
	}
	if got := e.chat.messages(); len(got) != 0 {
		t.Fatalf("refused webhooks were answered in the chat: %v", got)
	}
}

func TestServeHookAnswersTheService(t *testing.T) {
	e := setup(t)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "hook-secret"})
	w := e.hook(http.MethodPost, c.ID, "hook-secret", `{"Challenge":"abc"}`)
	if w.Code != http.StatusAccepted || w.Header().Get("Content-Type") != "text/plain" || w.Body.String() != "abc" {
		t.Fatalf("challenge: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestServeHookQueuesAMessage(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "hook-secret"})
	e.bind(t, a, c, "sales", store.ModeChat)
	w := e.hook(http.MethodPost, c.ID, "hook-secret",
		`{"ID":"m1","Text":"Scout: find the invoice","Email":"Ada@Example.ch","Channel":"sales"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook: %d", w.Code)
	}
	select {
	case got := <-e.queued:
		if got != a.ID {
			t.Fatalf("queued for %s, want %s", got, a.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the message was not queued")
	}
	ts := e.tasks(t, a)
	if len(ts) != 1 {
		t.Fatalf("%d tasks", len(ts))
	}
	got := ts[0]
	if got.Source != protocol.SourceChat || got.Prompt != "find the invoice" ||
		got.RequestedBy != "ada@example.ch" || got.RequestedByID != "usr_ada" {
		t.Fatalf("task = %+v", got)
	}
	o := got.Origin
	if o.ConnectionID != c.ID || o.Conversation != "sales" || o.Thread != "m1" || o.Label != "Test Chat #sales" {
		t.Fatalf("origin = %+v", o)
	}
	if got.Conversation != c.ID+":sales:m1" {
		t.Fatalf("conversation = %q", got.Conversation)
	}
}

func TestReceiveRefusesWhoMayNotGiveWork(t *testing.T) {
	e := setup(t)
	open := e.agent(t, "org_a", "Scout", store.AccessOrg)
	owner := e.agent(t, "org_a", "Private", store.AccessOwner)
	listed := e.agent(t, "org_a", "Listed", store.AccessPeople, "carol@example.ch")
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, open, c, "open", store.ModeChat)
	e.bind(t, owner, c, "owner", store.ModeChat)
	e.bind(t, listed, c, "listed", store.ModeChat)
	adapter, conn := e.chatConn(t, c)

	for _, tc := range []struct {
		name  string
		msg   hookBody
		reply string
	}{
		{"a guest from another workspace", hookBody{ID: "1", Channel: "open", Email: "ada@example.ch", Outside: true}, "own workspace"},
		{"no email", hookBody{ID: "2", Channel: "open"}, "could not tell who you are"},
		{"not in the organisation", hookBody{ID: "3", Channel: "open", Email: "eve@example.ch"}, "eve@example.ch is not one of them"},
		{"a disabled account", hookBody{ID: "4", Channel: "open", Email: "dis@example.ch"}, "disabled"},
		{"not the owner", hookBody{ID: "5", Channel: "owner", Email: "bob@example.ch"}, "only takes work from ada@example.ch"},
		{"not on the list", hookBody{ID: "6", Channel: "listed", Email: "bob@example.ch"}, "not on Listed's list"},
	} {
		tc.msg.Text = "do something"
		before := len(e.chat.messages())
		e.d.receive(e.ctx, adapter, conn, inbound(tc.msg))
		sent := e.chat.messages()[before:]
		if len(sent) != 1 || !strings.Contains(sent[0], tc.reply) {
			t.Errorf("%s: replied %q, want %q", tc.name, sent, tc.reply)
		}
		// The refusal goes to where the message was.
		if len(sent) == 1 && !strings.HasPrefix(sent[0], tc.msg.Channel+"/"+tc.msg.ID+": ") {
			t.Errorf("%s: replied in %q", tc.name, sent[0])
		}
	}
	for _, a := range []store.Agent{open, owner, listed} {
		if n := len(e.tasks(t, a)); n != 0 {
			t.Fatalf("%s got %d tasks from refused messages", a.Name, n)
		}
	}

	// The owner may use their own agent.
	e.d.receive(e.ctx, adapter, conn, inbound(hookBody{ID: "7", Channel: "owner", Email: "ada@example.ch", Text: "go"}))
	if n := len(e.tasks(t, owner)); n != 1 {
		t.Fatalf("the owner's message made %d tasks", n)
	}
}

func TestReceiveAsksAgainWhenTheGatewayIsDown(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, a, c, store.AnyConversation, store.ModeChat)
	adapter, conn := e.chatConn(t, c)
	e.gw.down.Store(true)
	e.d.receive(e.ctx, adapter, conn, inbound(hookBody{ID: "1", Channel: "sales", Email: "ada@example.ch", Text: "go"}))
	// An error that is not "not a member" must not read as one.
	if got := e.chat.messages(); len(got) != 1 || !strings.Contains(got[0], "try again") {
		t.Fatalf("replied %q", got)
	}
	if n := len(e.tasks(t, a)); n != 0 {
		t.Fatalf("%d tasks", n)
	}
}

func TestReceiveQueuesARedeliveredMessageOnce(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, a, c, store.AnyConversation, store.ModeChat)
	adapter, conn := e.chatConn(t, c)
	msg := inbound(hookBody{ID: "m1", Channel: "sales", Email: "bob@example.ch", Text: "go"})
	for range 3 {
		e.d.receive(e.ctx, adapter, conn, msg)
	}
	if n := len(e.tasks(t, a)); n != 1 {
		t.Fatalf("the same message three times made %d tasks", n)
	}
	if got := e.chat.messages(); len(got) != 0 {
		t.Fatalf("a duplicate was answered: %v", got)
	}
	// Another message in the same thread is new work.
	msg.MessageID = "m2"
	e.d.receive(e.ctx, adapter, conn, msg)
	if n := len(e.tasks(t, a)); n != 2 {
		t.Fatalf("a second message made %d tasks in all", n)
	}
}

func TestReceiveRoutesToTheRightAgent(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	closer := e.agent(t, "org_a", "Closer", store.AccessOrg)
	off := e.agent(t, "org_a", "Off", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, scout, c, store.AnyConversation, store.ModeChat)
	e.bind(t, closer, c, "sales", store.ModeChat)
	e.bind(t, off, c, "ops", store.ModeChat)
	if _, err := e.st.SetEnabled(e.ctx, off.ID, false); err != nil {
		t.Fatal(err)
	}
	// Another connection's bindings do not listen here.
	other := e.conn(t, "org_a", chatKind, map[string]string{"token": "y"})
	e.bind(t, closer, other, "support", store.ModeChat)
	adapter, conn := e.chatConn(t, c)

	send := func(id, channel, text string) {
		e.d.receive(e.ctx, adapter, conn, inbound(hookBody{ID: id, Channel: channel, Email: "ada@example.ch", Text: text}))
	}
	send("1", "sales", "how are we doing?")
	send("2", "sales", "Scout, summarise the week")
	send("3", "ops", "restart it")    // the bound agent is off, so the catch-all takes it
	send("4", "support", "a refund?") // nobody on this connection listens there but Scout
	prompts := func(a store.Agent) []string {
		var out []string
		for _, task := range e.tasks(t, a) {
			out = append(out, task.Prompt)
		}
		slices.Sort(out)
		return out
	}
	if got := prompts(closer); !slices.Equal(got, []string{"how are we doing?"}) {
		t.Errorf("Closer got %q", got)
	}
	if got := prompts(scout); !slices.Equal(got, []string{"a refund?", "restart it", "summarise the week"}) {
		t.Errorf("Scout got %q", got)
	}
	if n := len(e.tasks(t, off)); n != 0 {
		t.Errorf("a disabled agent got %d tasks", n)
	}
}

func TestReceiveStaysQuietWhereNobodyListens(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, a, c, "sales", store.ModeChat)
	adapter, conn := e.chatConn(t, c)
	// Even a stranger gets no answer: the bot does not reply to every
	// message in a busy channel.
	e.d.receive(e.ctx, adapter, conn, inbound(hookBody{ID: "1", Channel: "random", Email: "eve@example.ch", Text: "hi"}))
	if got := e.chat.messages(); len(got) != 0 {
		t.Fatalf("answered %v", got)
	}
	if n := e.gw.calls.Load(); n != 0 {
		t.Fatalf("asked the gateway %d times", n)
	}
}

func TestReceiveLabelsADirectMessage(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, a, c, store.AnyConversation, store.ModeChat)
	adapter, conn := e.chatConn(t, c)
	e.d.receive(e.ctx, adapter, conn, inbound(hookBody{ID: "1", Channel: "D1", Email: "bob@example.ch", Text: "hi", Direct: true}))
	ts := e.tasks(t, a)
	if len(ts) != 1 || ts[0].Origin.Label != "Test Chat direct message" ||
		!strings.Contains(ts[0].Context, "Bob <bob@example.ch>") {
		t.Fatalf("tasks = %+v", ts)
	}
}

// chatConn returns the fake chat adapter and c as it takes it.
func (e *testEnv) chatConn(t *testing.T, c store.Connection) (channels.Chat, channels.Connection) {
	t.Helper()
	conn, err := e.d.Conn(c)
	if err != nil {
		t.Fatal(err)
	}
	return e.chat, conn
}

func TestAMessageThatCannotBeQueuedSaysSoPlainly(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, a, c, "sales", store.ModeChat)
	adapter, conn := e.chatConn(t, c)
	send := func(id, text string) {
		e.d.receive(e.ctx, adapter, conn, inbound(hookBody{ID: id, Channel: "sales", Email: "ada@example.ch", Text: text}))
	}

	// Nothing but blanks, as a message with only an image: the person can
	// fix that.
	send("1", "  ")
	// The database refuses: that stays in the log.
	e.exec(t, `CREATE FUNCTION no_tasks() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION 'relation tasks is broken'; END $$;
		CREATE TRIGGER no_tasks BEFORE INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION no_tasks()`)
	send("2", "find the invoice")

	got := e.chat.messages()
	if len(got) != 2 || !strings.Contains(got[0], "a task needs something to do") ||
		!strings.Contains(got[1], "a problem on my side") || strings.Contains(got[1], "broken") {
		t.Fatalf("answered %q", got)
	}
}

func TestASlowChatTaskSaysItIsUnderWay(t *testing.T) {
	e := setup(t)
	e.d.ackAfter = 50 * time.Millisecond
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	e.bind(t, a, c, "sales", store.ModeChat)
	adapter, conn := e.chatConn(t, c)
	send := func(id string) store.Task {
		e.d.receive(e.ctx, adapter, conn, inbound(hookBody{ID: id, Channel: "sales", Email: "ada@example.ch", Text: "go"}))
		return e.tasks(t, a)[0]
	}
	// said waits for the n-th message, then for any that should not come.
	said := func(n int) []string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(e.chat.messages()) < n && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(4 * e.d.ackAfter)
		return e.chat.messages()
	}

	// An answer in time comes alone.
	quick := send("m1")
	e.finish(t, quick.ID, store.TaskSucceeded, "done", "")
	if got := said(0); len(got) != 0 {
		t.Fatalf("a quick task said %q", got)
	}

	// A slow one says it is on it, in the message's thread.
	slow := send("m2")
	if got := said(1); len(got) != 1 || got[0] != "sales/m2: I am working on it and will answer here when I am done." {
		t.Fatalf("a slow task said %q", got)
	}

	// One behind another task says it waits.
	if _, err := e.st.ClaimTask(e.ctx, a.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	send("m3")
	got := said(2)
	if len(got) != 2 || !strings.HasPrefix(got[1], "sales/m3: I am finishing another task first.") {
		t.Fatalf("a task behind %s said %q", slow.ID, got)
	}
}
