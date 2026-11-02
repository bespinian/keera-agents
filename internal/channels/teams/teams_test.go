package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/jwttest"
)

const appID = "app-123"

var conn = channels.Connection{
	ID:      "c1",
	Kind:    channels.Teams,
	Config:  map[string]string{"app_id": appID, "tenant_id": "tenant-1"},
	Secrets: map[string]string{"app_password": "pw"},
}

type fake struct {
	srv        *httptest.Server
	key        *jwttest.Key
	tokenCalls atomic.Int32
	mu         sync.Mutex
	posts      []post
}

type post struct {
	path string
	body map[string]any
}

func newFake(t *testing.T) *fake {
	f := &fake{key: jwttest.NewKey(t, "bf1")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openid", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"https://api.botframework.com","jwks_uri":"http://` + r.Host + `/keys"}`))
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(f.key.JWKS())
	})
	mux.HandleFunc("POST /login/{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		_ = r.ParseForm()
		if r.PathValue("tenant") != "tenant-1" && r.PathValue("tenant") != "botframework.com" {
			t.Errorf("tenant = %q", r.PathValue("tenant"))
		}
		if r.Form.Get("scope") != "https://api.botframework.com/.default" {
			t.Errorf("scope = %q", r.Form.Get("scope"))
		}
		if r.Form.Get("client_id") != appID || r.Form.Get("client_secret") != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"bot-token","expires_in":3600}`))
	})
	mux.HandleFunc("GET /v3/conversations/{conv}/members/{user}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.PathValue("user") != "29:user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"29:user","name":"Ana Muster","userPrincipalName":"ana@example.ch"}`))
	})
	activities := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posts = append(f.posts, post{r.URL.EscapedPath(), body})
		f.mu.Unlock()
		if strings.Contains(r.URL.Path, "gone") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"BotNotInConversationRoster","message":"The bot is not part of the conversation roster."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1700000000001"}`))
	}
	mux.HandleFunc("POST /v3/conversations/{conv}/activities", activities)
	mux.HandleFunc("POST /v3/conversations/{conv}/activities/{reply}", activities)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) adapter() *Adapter {
	return New(channels.Options{HTTP: f.srv.Client(), Endpoints: map[string]string{
		"openid":     f.srv.URL + "/openid",
		"login":      f.srv.URL + "/login",
		"serviceurl": f.srv.URL,
	}})
}

func (f *fake) token(t *testing.T, mod func(map[string]any)) string {
	now := time.Now()
	c := map[string]any{
		"iss": "https://api.botframework.com", "aud": appID,
		"exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix(),
		"serviceurl": f.srv.URL + "/",
	}
	if mod != nil {
		mod(c)
	}
	return f.key.Sign(t, c)
}

func (f *fake) request(t *testing.T, token string, act map[string]any) (*http.Request, []byte) {
	if _, ok := act["serviceUrl"]; !ok {
		act["serviceUrl"] = f.srv.URL + "/"
	}
	body, _ := json.Marshal(act)
	r := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r, body
}

func personal(text string) map[string]any {
	return map[string]any{
		"type": "message", "id": "m1", "text": text,
		"from":         map[string]any{"id": "29:user", "name": "Ana"},
		"recipient":    map[string]any{"id": "28:" + appID, "name": "Keera"},
		"conversation": map[string]any{"id": "a:personal", "conversationType": "personal", "tenantId": "tenant-1"},
	}
}

func channelMessage(text string, mention bool) map[string]any {
	act := map[string]any{
		"type": "message", "id": "1700000000000", "text": text,
		"from":         map[string]any{"id": "29:user", "name": "Ana"},
		"recipient":    map[string]any{"id": "28:" + appID, "name": "Keera"},
		"conversation": map[string]any{"id": "19:abc@thread.tacv2;messageid=1699999999999", "conversationType": "channel", "isGroup": true},
		"channelData": map[string]any{
			"tenant": map[string]any{"id": "tenant-1"}, "team": map[string]any{"name": "Sales"}, "channel": map[string]any{"name": "Leads"},
		},
	}
	if mention {
		act["entities"] = []any{
			map[string]any{"type": "mention", "text": "<at>Keera</at>", "mentioned": map[string]any{"id": "28:" + appID, "name": "Keera"}},
			map[string]any{"type": "mention", "text": "<at>Bob</at>", "mentioned": map[string]any{"id": "29:bob", "name": "Bob"}},
		}
	}
	return act
}

func TestReceivePersonal(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	r, body := f.request(t, f.token(t, nil), personal("hello &amp; welcome"))
	rc, err := a.Receive(context.Background(), conn, r, body)
	if err != nil || len(rc.Messages) != 1 {
		t.Fatalf("%+v, %v", rc, err)
	}
	m := rc.Messages[0]
	if !m.Direct || m.Text != "hello & welcome" || m.Target.Conversation != "a:personal" || m.Target.Thread != "" ||
		m.Target.Extra["serviceUrl"] != f.srv.URL+"/" || m.Sender.Email != "ana@example.ch" || m.MessageID != "a:personal/m1" {
		t.Errorf("message = %+v", m)
	}
	// The sender and the token are cached.
	r, body = f.request(t, f.token(t, nil), personal("again"))
	_, _ = a.Receive(context.Background(), conn, r, body)
	if f.tokenCalls.Load() != 1 {
		t.Errorf("token fetched %d times", f.tokenCalls.Load())
	}
}

func TestReceiveChannel(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	r, body := f.request(t, f.token(t, nil), channelMessage("<at>Keera</at> ask <at>Bob</at> about it\n", true))
	rc, err := a.Receive(context.Background(), conn, r, body)
	if err != nil || len(rc.Messages) != 1 {
		t.Fatalf("%+v, %v", rc, err)
	}
	m := rc.Messages[0]
	if m.Direct || m.Text != "ask Bob about it" || m.Target.Conversation != "19:abc@thread.tacv2" ||
		m.Target.Thread != "1699999999999" || m.ConversationLabel != "Sales / Leads" {
		t.Errorf("message = %+v", m)
	}

	// Without a mention of the bot, a channel message is not for it.
	r, body = f.request(t, f.token(t, nil), channelMessage("just chatting", false))
	if rc, err := a.Receive(context.Background(), conn, r, body); err != nil || len(rc.Messages) != 0 {
		t.Errorf("unmentioned: %+v, %v", rc, err)
	}
}

func TestReceiveIgnoresOtherActivities(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	r, body := f.request(t, f.token(t, nil), map[string]any{"type": "conversationUpdate", "membersAdded": []any{}})
	rc, err := a.Receive(context.Background(), conn, r, body)
	if err != nil || len(rc.Messages) != 0 || rc.Response.Status != 0 {
		t.Errorf("%+v, %v", rc, err)
	}
}

func TestReceiveRejects(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	other := jwttest.NewKey(t, "bf1")
	cases := map[string]func() (*http.Request, []byte){
		"no token": func() (*http.Request, []byte) { return f.request(t, "", personal("x")) },
		"other audience": func() (*http.Request, []byte) {
			return f.request(t, f.token(t, func(c map[string]any) { c["aud"] = "another-app" }), personal("x"))
		},
		"other issuer": func() (*http.Request, []byte) {
			return f.request(t, f.token(t, func(c map[string]any) { c["iss"] = "https://sts.windows.net/x/" }), personal("x"))
		},
		"expired": func() (*http.Request, []byte) {
			return f.request(t, f.token(t, func(c map[string]any) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() }), personal("x"))
		},
		"forged": func() (*http.Request, []byte) {
			return f.request(t, other.Sign(t, map[string]any{"iss": issuer, "aud": appID, "exp": time.Now().Add(time.Hour).Unix()}), personal("x"))
		},
		"service URL mismatch": func() (*http.Request, []byte) {
			act := personal("x")
			act["serviceUrl"] = "https://smba.trafficmanager.net/emea/"
			return f.request(t, f.token(t, nil), act)
		},
		"other tenant": func() (*http.Request, []byte) {
			act := personal("x")
			act["conversation"].(map[string]any)["tenantId"] = "attacker"
			return f.request(t, f.token(t, nil), act)
		},
		"tenants disagree": func() (*http.Request, []byte) {
			act := channelMessage("<at>Keera</at> x", true)
			act["conversation"].(map[string]any)["tenantId"] = "attacker"
			return f.request(t, f.token(t, nil), act)
		},
		"no tenant": func() (*http.Request, []byte) {
			act := personal("x")
			delete(act["conversation"].(map[string]any), "tenantId")
			return f.request(t, f.token(t, nil), act)
		},
		"service URL not Microsoft": func() (*http.Request, []byte) {
			act := personal("x")
			act["serviceUrl"] = "https://evil.example/"
			return f.request(t, f.token(t, func(c map[string]any) { delete(c, "serviceurl") }), act)
		},
	}
	for name, mk := range cases {
		r, body := mk()
		if _, err := a.Receive(context.Background(), conn, r, body); !errors.Is(err, channels.ErrUnverified) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestReceiveMultiTenant(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	multi := channels.Connection{
		ID: conn.ID, Kind: conn.Kind, Secrets: conn.Secrets,
		Config: map[string]string{"app_id": appID, "allowed_tenants": "tenant-0, TENANT-1"},
	}
	r, body := f.request(t, f.token(t, nil), personal("hello"))
	if rc, err := a.Receive(context.Background(), multi, r, body); err != nil || len(rc.Messages) != 1 {
		t.Errorf("allowed tenant: %+v, %v", rc, err)
	}
	// With no tenants named, nobody's word for who they are is trusted.
	delete(multi.Config, "allowed_tenants")
	r, body = f.request(t, f.token(t, nil), personal("hello"))
	if _, err := a.Receive(context.Background(), multi, r, body); !errors.Is(err, channels.ErrUnverified) {
		t.Errorf("no tenants: err = %v", err)
	}
	if err := a.Check(context.Background(), multi); err == nil || !strings.Contains(err.Error(), "Other tenants") {
		t.Errorf("check without tenants: %v", err)
	}
}

func TestSend(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	extra := map[string]string{"serviceUrl": f.srv.URL + "/"}

	id, err := a.Send(context.Background(), conn, channels.Target{Conversation: "a:personal", Extra: extra}, "**hi**")
	if err != nil || id != "1700000000001" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	id, err = a.Send(context.Background(), conn, channels.Target{Conversation: "19:abc@thread.tacv2", Thread: "1699999999999", Extra: extra}, "in thread")
	if err != nil || id == "" {
		t.Fatalf("Send thread = %q, %v", id, err)
	}
	if len(f.posts) != 2 {
		t.Fatalf("posts = %+v", f.posts)
	}
	if f.posts[0].path != "/v3/conversations/a%3Apersonal/activities" || f.posts[0].body["textFormat"] != "markdown" || f.posts[0].body["text"] != "**hi**" {
		t.Errorf("first post = %+v", f.posts[0])
	}
	if f.posts[1].path != "/v3/conversations/19%3Aabc%40thread.tacv2%3Bmessageid%3D1699999999999/activities/1699999999999" || f.posts[1].body["replyToId"] != "1699999999999" {
		t.Errorf("thread post = %+v", f.posts[1])
	}

	_, err = a.Send(context.Background(), conn, channels.Target{Conversation: "gone", Extra: extra}, "x")
	if err == nil || !strings.Contains(err.Error(), "BotNotInConversationRoster") {
		t.Errorf("err = %v", err)
	}
	// A schedule's result answers no activity, so it has no service URL.
	f.posts = nil
	if _, err := a.Send(context.Background(), conn, channels.Target{Conversation: "19:abc@thread.tacv2"}, "report"); err != nil {
		t.Fatalf("Send without a service URL: %v", err)
	}
	if len(f.posts) != 1 || f.posts[0].path != "/v3/conversations/19%3Aabc%40thread.tacv2/activities" {
		t.Errorf("posts = %+v", f.posts)
	}
	if New(channels.Options{}).global != "https://smba.trafficmanager.net/teams/" {
		t.Error("messages no one asked for do not go to Microsoft's global service URL")
	}

	for _, su := range []string{"https://evil.example/", "http://smba.trafficmanager.net/", "https://smba.trafficmanager.net.evil.example/"} {
		if _, err := a.Send(context.Background(), conn, channels.Target{Conversation: "x", Extra: map[string]string{"serviceUrl": su}}, "x"); err == nil {
			t.Errorf("sent to %q", su)
		}
	}
}

func TestAllowed(t *testing.T) {
	a := New(channels.Options{})
	for s, want := range map[string]bool{
		"https://smba.trafficmanager.net/emea/":             true,
		"https://smba.infra.gcc.teams.microsoft.com/teams/": true,
		"https://webchat.botframework.com/":                 true,
		"https://smba.trafficmanager.net:8443/":             false,
		"https://user@smba.trafficmanager.net/":             false,
		"https://other.trafficmanager.net/":                 false,
		"https://botframework.com.evil.example/":            false,
	} {
		if a.allowed(s) != want {
			t.Errorf("allowed(%q) = %v", s, !want)
		}
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	if err := a.Check(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	bad := channels.Connection{Config: map[string]string{"app_id": appID, "tenant_id": "tenant-1"}, Secrets: map[string]string{"app_password": "wrong"}}
	err := a.Check(context.Background(), bad)
	if err == nil || !strings.Contains(err.Error(), "Microsoft refused the credentials (invalid_client") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("err = %v", err)
	}
	if err := a.Check(context.Background(), channels.Connection{}); err == nil {
		t.Error("empty connection passed")
	}
}
