package googlechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/jwttest"
)

const endpoint = "https://agents.example.ch/hooks/googlechat/c1"

type fake struct {
	srv       *httptest.Server
	chatKey   *jwttest.Key
	googleKey *jwttest.Key
	saKey     *jwttest.Key
	posted    map[string]any
	query     string
}

func newFake(t *testing.T) *fake {
	f := &fake{chatKey: jwttest.NewKey(t, "chat1"), googleKey: jwttest.NewKey(t, "g1"), saKey: jwttest.NewKey(t, "sa1")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /chatkeys", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(f.chatKey.JWKS()) })
	mux.HandleFunc("GET /googlekeys", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(f.googleKey.JWKS()) })
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		c := jwttest.Claims(t, r.Form.Get("assertion"), &f.saKey.Private.PublicKey)
		if c["scope"] != "https://www.googleapis.com/auth/chat.bot" || c["iss"] != "bot@p.iam.gserviceaccount.com" {
			t.Errorf("assertion claims = %v", c)
		}
		_, _ = w.Write([]byte(`{"access_token":"chat-token","expires_in":3600}`))
	})
	mux.HandleFunc("POST /v1/spaces/{space}/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer chat-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.query = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&f.posted)
		if r.PathValue("space") == "GONE" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Space not found.","status":"NOT_FOUND"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"spaces/` + r.PathValue("space") + `/messages/NEW"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) adapter() *Adapter {
	return New(connectors.Options{HTTP: f.srv.Client(), Endpoints: map[string]string{
		"token": f.srv.URL + "/token", "chat": f.srv.URL,
		"chatkeys": f.srv.URL + "/chatkeys", "googlekeys": f.srv.URL + "/googlekeys",
	}})
}

func (f *fake) conn(audience string) connectors.Connection {
	return connectors.Connection{
		ID:      "c1",
		Kind:    connectors.GoogleChat,
		Config:  map[string]string{"audience": audience},
		Secrets: map[string]string{"service_account_json": f.saKey.ServiceAccountJSON("bot@p.iam.gserviceaccount.com")},
	}
}

func (f *fake) chatToken(t *testing.T, aud string) string {
	return f.chatKey.Sign(t, map[string]any{
		"iss": "chat@system.gserviceaccount.com", "aud": aud,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func (f *fake) idToken(t *testing.T, email string) string {
	return f.googleKey.Sign(t, map[string]any{
		"iss": "https://accounts.google.com", "aud": endpoint, "email": email, "email_verified": true,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func request(token string, ev any) (*http.Request, []byte) {
	body, _ := json.Marshal(ev)
	r := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r, body
}

func dm(text string) map[string]any {
	return map[string]any{
		"type":  "MESSAGE",
		"space": map[string]any{"name": "spaces/DM1", "type": "DM", "spaceType": "DIRECT_MESSAGE"},
		"message": map[string]any{
			"name": "spaces/DM1/messages/M1", "text": text,
			"sender": map[string]any{"name": "users/1", "displayName": "Ana Muster", "email": "ana@example.ch", "type": "HUMAN"},
			"thread": map[string]any{"name": "spaces/DM1/threads/T1"},
		},
		"user": map[string]any{"name": "users/1", "email": "ana@example.ch"},
	}
}

func roomMessage(mention bool) map[string]any {
	msg := map[string]any{
		"name": "spaces/ROOM/messages/M2", "text": "@Keera summarise the week", "argumentText": " summarise the week",
		"sender": map[string]any{"name": "users/1", "displayName": "Ana Muster", "type": "HUMAN"},
		"thread": map[string]any{"name": "spaces/ROOM/threads/T2"},
	}
	if mention {
		msg["annotations"] = []any{map[string]any{"type": "USER_MENTION", "userMention": map[string]any{"user": map[string]any{"name": "users/app", "type": "BOT"}, "type": "MENTION"}}}
	}
	return map[string]any{
		"type":    "MESSAGE",
		"space":   map[string]any{"name": "spaces/ROOM", "type": "ROOM", "spaceType": "SPACE", "displayName": "Sales"},
		"message": msg,
		"user":    map[string]any{"name": "users/1", "displayName": "Ana Muster", "email": "ana@example.ch"},
	}
}

func TestReceiveDirect(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	r, body := request(f.chatToken(t, "123456"), dm(" hello "))
	rc, err := a.Receive(context.Background(), f.conn("123456"), r, body)
	if err != nil || len(rc.Messages) != 1 {
		t.Fatalf("%+v, %v", rc, err)
	}
	m := rc.Messages[0]
	if !m.Direct || m.Text != "hello" || m.Target.Conversation != "spaces/DM1" || m.Target.Thread != "spaces/DM1/threads/T1" ||
		m.Sender.Email != "ana@example.ch" || m.MessageID != "spaces/DM1/messages/M1" {
		t.Errorf("message = %+v", m)
	}
	if string(rc.Response.Body) != "{}" || rc.Response.Status != 200 {
		t.Errorf("response = %+v", rc.Response)
	}
}

func TestReceiveSpace(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	// The endpoint URL as audience: a Google ID token for Chat.
	r, body := request(f.idToken(t, "chat@system.gserviceaccount.com"), roomMessage(true))
	rc, err := a.Receive(context.Background(), f.conn(endpoint), r, body)
	if err != nil || len(rc.Messages) != 1 {
		t.Fatalf("%+v, %v", rc, err)
	}
	m := rc.Messages[0]
	if m.Direct || m.Text != "summarise the week" || m.ConversationLabel != "Sales" || m.Sender.Email != "ana@example.ch" {
		t.Errorf("message = %+v", m)
	}
	r, body = request(f.idToken(t, "chat@system.gserviceaccount.com"), roomMessage(false))
	if rc, err := a.Receive(context.Background(), f.conn(endpoint), r, body); err != nil || len(rc.Messages) != 0 {
		t.Errorf("unmentioned: %+v, %v", rc, err)
	}
}

func TestReceiveAddOn(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	ev := dm("from an add-on")
	addOn := map[string]any{
		"commonEventObject": map[string]any{"hostApp": "CHAT"},
		"chat": map[string]any{
			"user":           ev["user"],
			"messagePayload": map[string]any{"space": ev["space"], "message": ev["message"]},
		},
	}
	r, body := request(f.idToken(t, "service-123456@gcp-sa-gsuiteaddons.iam.gserviceaccount.com"), addOn)
	rc, err := a.Receive(context.Background(), f.conn(endpoint), r, body)
	if err != nil || len(rc.Messages) != 1 || rc.Messages[0].Text != "from an add-on" {
		t.Fatalf("%+v, %v", rc, err)
	}

	added := map[string]any{"chat": map[string]any{"addedToSpacePayload": map[string]any{"space": map[string]any{"name": "spaces/ROOM"}}}}
	r, body = request(f.idToken(t, "chat@system.gserviceaccount.com"), added)
	rc, err = a.Receive(context.Background(), f.conn(endpoint), r, body)
	if err != nil || !strings.Contains(string(rc.Response.Body), `"hostAppDataAction"`) || !strings.Contains(string(rc.Response.Body), "Hello") {
		t.Errorf("added: %s, %v", rc.Response.Body, err)
	}
}

func TestReceiveOtherEvents(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	r, body := request(f.chatToken(t, "123456"), map[string]any{"type": "ADDED_TO_SPACE", "space": map[string]any{"name": "spaces/ROOM"}})
	rc, err := a.Receive(context.Background(), f.conn("123456"), r, body)
	if err != nil || len(rc.Messages) != 0 || !strings.HasPrefix(string(rc.Response.Body), `{"text":"Hello`) {
		t.Errorf("added: %s, %v", rc.Response.Body, err)
	}
	// Mentioning the app adds it, and the message comes with that event.
	added := roomMessage(true)
	added["type"] = "ADDED_TO_SPACE"
	r, body = request(f.chatToken(t, "123456"), added)
	rc, err = a.Receive(context.Background(), f.conn("123456"), r, body)
	if err != nil || len(rc.Messages) != 1 || !strings.HasPrefix(string(rc.Response.Body), `{"text":"Hello`) {
		t.Errorf("added by a mention: %+v, %v", rc, err)
	}
	r, body = request(f.chatToken(t, "123456"), map[string]any{"type": "REMOVED_FROM_SPACE"})
	rc, err = a.Receive(context.Background(), f.conn("123456"), r, body)
	if err != nil || string(rc.Response.Body) != "{}" {
		t.Errorf("removed: %s, %v", rc.Response.Body, err)
	}
}

func TestReceiveRejects(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	cases := map[string]struct {
		audience, token string
	}{
		"no token":             {"123456", ""},
		"other project":        {"123456", f.chatToken(t, "999")},
		"ID token for project": {"123456", f.idToken(t, "chat@system.gserviceaccount.com")},
		"chat token for URL":   {endpoint, f.chatToken(t, endpoint)},
		"other email":          {endpoint, f.idToken(t, "someone@gmail.com")},
		"no audience":          {"", f.chatToken(t, "")},
	}
	for name, c := range cases {
		r, body := request(c.token, dm("x"))
		if _, err := a.Receive(context.Background(), f.conn(c.audience), r, body); !errors.Is(err, connectors.ErrUnverified) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSend(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	c := f.conn("123456")
	id, err := a.Send(context.Background(), c, connectors.Target{Conversation: "spaces/ROOM", Thread: "spaces/ROOM/threads/T2"}, "## Summary\n**done**")
	if err != nil || id != "spaces/ROOM/messages/NEW" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	if f.posted["text"] != "*Summary*\n*done*" || f.query != "messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" {
		t.Errorf("posted %v ? %s", f.posted, f.query)
	}
	if th, _ := f.posted["thread"].(map[string]any); th["name"] != "spaces/ROOM/threads/T2" {
		t.Errorf("thread = %v", f.posted["thread"])
	}
	_, err = a.Send(context.Background(), c, connectors.Target{Conversation: "spaces/GONE"}, "x")
	if err == nil || !strings.Contains(err.Error(), "NOT_FOUND: Space not found.") {
		t.Errorf("err = %v", err)
	}
	for _, to := range []connectors.Target{
		{Conversation: "*"},
		{Conversation: "spaces/../x"},
		{Conversation: "spaces/ROOM", Thread: "spaces/OTHER/threads/T"},
	} {
		if _, err := a.Send(context.Background(), c, to, "x"); err == nil {
			t.Errorf("sent to %+v", to)
		}
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	if err := a.Check(context.Background(), f.conn("123456")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(context.Background(), f.conn(endpoint)); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(context.Background(), f.conn("my-project")); err == nil || !strings.Contains(err.Error(), "project number") {
		t.Errorf("err = %v", err)
	}
	c := f.conn("123456")
	c.Secrets = map[string]string{"service_account_json": "{not json"}
	if err := a.Check(context.Background(), c); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("err = %v", err)
	}
}
