package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
)

const secret = "s3cret"

var conn = connectors.Connection{
	ID:      "c1",
	Kind:    connectors.Slack,
	Secrets: map[string]string{"bot_token": "xoxb-good", "signing_secret": secret},
}

// fake is a Slack Web API that knows one user and one channel.
type fake struct {
	srv        *httptest.Server
	userCalls  atomic.Int32
	posted     map[string]any
	slowUsers  bool
	authResult string
}

func newFake(t *testing.T) *fake {
	f := &fake{authResult: `{"ok":true,"team":"T","user":"bot"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-good" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			return
		}
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(f.authResult))
		case "/users.info":
			f.userCalls.Add(1)
			if f.slowUsers {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			switch r.URL.Query().Get("user") {
			case "U1":
				_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","team_id":"T1","name":"ana","profile":{"email":"ana@example.ch","real_name":"Ana Muster"}}}`))
			case "UGRID":
				_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UGRID","team_id":"T2","enterprise_user":{"enterprise_id":"E1"},"profile":{"email":"gina@example.ch"}}}`))
			case "UEXT":
				// A Slack Connect guest whose own admins set their email.
				_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UEXT","team_id":"T9","profile":{"email":"ana@example.ch"}}}`))
			default:
				_, _ = w.Write([]byte(`{"ok":false,"error":"user_not_found"}`))
			}
		case "/conversations.info":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"name":"general"}}`))
		case "/chat.postMessage":
			if err := json.NewDecoder(r.Body).Decode(&f.posted); err != nil {
				t.Error(err)
			}
			if f.posted["channel"] == "CGONE" {
				_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000200"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) adapter() *Adapter {
	return New(connectors.Options{HTTP: f.srv.Client(), Endpoints: map[string]string{"api": f.srv.URL}})
}

func signed(t *testing.T, body string, at time.Time) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":" + body))
	r := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func callback(ev string) string {
	return `{"type":"event_callback","event_id":"Ev1","team_id":"T1",` +
		`"authorizations":[{"user_id":"UBOT","is_bot":true,"team_id":"T1","enterprise_id":"E1"}],"event":` + ev + `}`
}

func TestReceiveVerification(t *testing.T) {
	a := newFake(t).adapter()
	body := `{"type":"url_verification","challenge":"abc"}`
	now := time.Now()

	rc, err := a.Receive(context.Background(), conn, signed(t, body, now), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Response.Status != 200 || string(rc.Response.Body) != `{"challenge":"abc"}` {
		t.Errorf("response = %+v", rc.Response)
	}

	bad := map[string]*http.Request{
		"old":       signed(t, body, now.Add(-6*time.Minute)),
		"future":    signed(t, body, now.Add(6*time.Minute)),
		"unsigned":  httptest.NewRequest(http.MethodPost, "/hook", nil),
		"other key": signed(t, body+" ", now),
	}
	tampered := signed(t, body, now)
	tampered.Header.Set("X-Slack-Signature", "v0=zz")
	bad["not hex"] = tampered
	for name, r := range bad {
		if _, err := a.Receive(context.Background(), conn, r, []byte(body)); !errors.Is(err, connectors.ErrUnverified) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	noSecret := connectors.Connection{Secrets: map[string]string{"bot_token": "xoxb-good"}}
	if _, err := a.Receive(context.Background(), noSecret, signed(t, body, now), []byte(body)); !errors.Is(err, connectors.ErrUnverified) {
		t.Errorf("no secret: err = %v", err)
	}
}

func TestReceiveMention(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	body := callback(`{"type":"app_mention","user":"U1","text":"<@UBOT> what is 1 &lt; 2 for <@U2>?","channel":"C1","ts":"111.222"}`)
	r := signed(t, body, time.Now())
	r.Header.Set("X-Slack-Retry-Num", "1") // retries are parsed like the first delivery
	rc, err := a.Receive(context.Background(), conn, r, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Messages) != 1 {
		t.Fatalf("messages = %+v", rc.Messages)
	}
	m := rc.Messages[0]
	want := connectors.Inbound{
		Target:            connectors.Target{Conversation: "C1", Thread: "111.222"},
		Sender:            connectors.Person{Email: "ana@example.ch"},
		Text:              "what is 1 < 2 for <@U2>?",
		MessageID:         "C1/111.222",
		ConversationLabel: "#general",
	}
	if m.Target.Conversation != want.Target.Conversation || m.Target.Thread != want.Target.Thread ||
		m.Sender != want.Sender || m.Text != want.Text || m.MessageID != want.MessageID ||
		m.Direct || m.ConversationLabel != want.ConversationLabel {
		t.Errorf("got  %+v\nwant %+v", m, want)
	}

	// The sender is cached.
	_, _ = a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body))
	if f.userCalls.Load() != 1 {
		t.Errorf("users.info called %d times", f.userCalls.Load())
	}

	// A reply in a thread stays in that thread.
	body = callback(`{"type":"app_mention","user":"U1","text":"<@UBOT|keera> more","channel":"C1","ts":"333.444","thread_ts":"111.222"}`)
	rc, _ = a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body))
	if len(rc.Messages) != 1 || rc.Messages[0].Target.Thread != "111.222" || rc.Messages[0].Text != "more" {
		t.Errorf("thread reply = %+v", rc.Messages)
	}
}

func TestReceiveDirect(t *testing.T) {
	a := newFake(t).adapter()
	body := callback(`{"type":"message","channel_type":"im","user":"U1","text":"hello","channel":"D1","ts":"5.6"}`)
	rc, err := a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body))
	if err != nil || len(rc.Messages) != 1 {
		t.Fatalf("messages = %+v, %v", rc.Messages, err)
	}
	m := rc.Messages[0]
	if !m.Direct || m.Target.Thread != "" || m.ConversationLabel != "" || m.Sender.Email != "ana@example.ch" {
		t.Errorf("direct = %+v", m)
	}
}

func TestReceiveSenderWorkspace(t *testing.T) {
	a := newFake(t).adapter()
	for user, want := range map[string]connectors.Person{
		"UGRID": {Email: "gina@example.ch"},
		"UEXT":  {Outside: true},
	} {
		body := callback(`{"type":"message","channel_type":"im","user":"` + user + `","text":"hello","channel":"D1","ts":"5.6"}`)
		rc, err := a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body))
		if err != nil || len(rc.Messages) != 1 {
			t.Fatalf("%s: %+v, %v", user, rc, err)
		}
		if got := rc.Messages[0].Sender; got != want {
			t.Errorf("%s: sender = %+v, want %+v", user, got, want)
		}
	}
}

func TestReceiveIgnores(t *testing.T) {
	a := newFake(t).adapter()
	for name, ev := range map[string]string{
		"channel message": `{"type":"message","channel_type":"channel","user":"U1","text":"hi","channel":"C1","ts":"1"}`,
		"own message":     `{"type":"message","channel_type":"im","user":"UBOT","text":"hi","channel":"D1","ts":"1"}`,
		"bot message":     `{"type":"message","channel_type":"im","bot_id":"B1","user":"U9","text":"hi","channel":"D1","ts":"1"}`,
		"edit":            `{"type":"message","subtype":"message_changed","channel_type":"im","channel":"D1","ts":"1"}`,
		"bare mention":    `{"type":"app_mention","user":"U1","text":"<@UBOT>","channel":"C1","ts":"1"}`,
		"reaction":        `{"type":"reaction_added","user":"U1"}`,
	} {
		body := callback(ev)
		rc, err := a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body))
		if err != nil || len(rc.Messages) != 0 || rc.Response.Status != 0 {
			t.Errorf("%s: %+v, %v", name, rc, err)
		}
	}
	body := `{"type":"app_rate_limited"}`
	if rc, err := a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body)); err != nil || len(rc.Messages) != 0 {
		t.Errorf("other envelope: %+v, %v", rc, err)
	}
	body = `not json`
	if _, err := a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body)); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestReceiveSlowLookup(t *testing.T) {
	f := newFake(t)
	f.slowUsers = true
	a := f.adapter()
	body := callback(`{"type":"message","channel_type":"im","user":"U1","text":"hello","channel":"D1","ts":"5.6"}`)
	start := time.Now()
	rc, err := a.Receive(context.Background(), conn, signed(t, body, time.Now()), []byte(body))
	if err != nil || len(rc.Messages) != 1 {
		t.Fatalf("%+v, %v", rc, err)
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Errorf("Receive took %v", d)
	}
	if rc.Messages[0].Sender.Email != "" {
		t.Errorf("sender = %+v", rc.Messages[0].Sender)
	}
}

func TestSend(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	id, err := a.Send(context.Background(), conn, connectors.Target{Conversation: "C1", Thread: "111.222"}, "**Done** see [x](https://x.ch)")
	if err != nil || id != "1700000000.000200" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	if f.posted["text"] != "*Done* see <https://x.ch|x>" || f.posted["thread_ts"] != "111.222" || f.posted["channel"] != "C1" {
		t.Errorf("posted %v", f.posted)
	}
	if _, err := a.Send(context.Background(), conn, connectors.Target{Conversation: "CGONE"}, "x"); err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("err = %v", err)
	}
	if _, err := a.Send(context.Background(), conn, connectors.Target{Conversation: "*"}, "x"); err == nil {
		t.Error("posted to *")
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	if err := a.Check(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		secrets map[string]string
		want    string
	}{
		"wrong token":  {map[string]string{"bot_token": "xoxb-bad", "signing_secret": "x"}, "Slack says the bot token is invalid (invalid_auth)"},
		"user token":   {map[string]string{"bot_token": "xoxp-1", "signing_secret": "x"}, "xoxb-"},
		"no secret":    {map[string]string{"bot_token": "xoxb-good"}, "signing secret is missing"},
		"no bot token": {map[string]string{"signing_secret": "x"}, "bot token is missing"},
	} {
		err := a.Check(context.Background(), connectors.Connection{Secrets: c.secrets})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "xoxb-bad") {
			t.Errorf("%s: error leaks the token", name)
		}
	}
	f.authResult = `{"ok":false,"error":"account_inactive"}`
	if err := a.Check(context.Background(), conn); err == nil || !strings.Contains(err.Error(), "account_inactive") {
		t.Errorf("err = %v", err)
	}
}

func TestDescriptor(t *testing.T) {
	d := New(connectors.Options{}).Descriptor()
	if d.Kind != connectors.Slack || !strings.Contains(d.Setup, "{webhook}") || len(d.Fields) != 2 {
		t.Errorf("descriptor = %+v", d)
	}
	for _, f := range d.Fields {
		if !f.Secret || !f.Required {
			t.Errorf("field %s should be a required secret", f.Name)
		}
	}
}
