package outlook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/jwttest"
)

const box = "ada@example.com"

type fake struct {
	srv  *httptest.Server
	last *http.Request
	body map[string]any
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/tenant-1/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "pw" || r.Form.Get("scope") != scope {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	})
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.last, f.body = r, nil
			_ = json.NewDecoder(r.Body).Decode(&f.body)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	u := "/graph/users/" + box
	mux.HandleFunc("GET "+u+"/mailFolders/inbox", reply(200, `{"id":"AAMkInbox"}`))
	mux.HandleFunc("GET /graph/users/bob@example.com/mailFolders/inbox", reply(403,
		`{"error":{"code":"ErrorAccessDenied","message":"Access to OData is disabled."}}`))
	mux.HandleFunc("GET /graph/users/nobody@example.com/mailFolders/inbox", reply(404,
		`{"error":{"code":"ErrorInvalidUser","message":"The requested user 'nobody@example.com' is invalid."}}`))
	mux.HandleFunc("GET "+u+"/mailFolders/inbox/messages", reply(200, `{"value":[
		{"id":"m1","subject":"Invoice","from":{"emailAddress":{"name":"Bob","address":"bob@example.com"}},
		 "toRecipients":[{"emailAddress":{"address":"ada@example.com"}}],"receivedDateTime":"2026-11-04T08:00:00Z","isRead":false,"bodyPreview":"Please pay"},
		{"id":"m2","subject":"Lunch","isRead":true}]}`))
	mux.HandleFunc("GET "+u+"/mailFolders", reply(200, `{"value":[{"id":"f1","displayName":"Inbox","unreadItemCount":1,"totalItemCount":2}]}`))
	mux.HandleFunc("GET "+u+"/messages/m1", reply(200, `{"id":"m1","subject":"Invoice",
		"ccRecipients":[{"emailAddress":{"name":"Cy","address":"cy@example.com"}}],
		"body":{"contentType":"text","content":"Hello\r\n\r\n\r\n\r\nPlease pay."},
		"attachments":[{"name":"invoice.pdf","size":1234,"contentType":"application/pdf"}]}`))
	mux.HandleFunc("GET "+u+"/messages/gone", reply(404,
		`{"error":{"code":"ErrorItemNotFound","message":"The specified object was not found in the store."}}`))
	mux.HandleFunc("POST "+u+"/sendMail", reply(202, ``))
	mux.HandleFunc("POST "+u+"/messages/m1/replyAll", reply(202, ``))
	mux.HandleFunc("POST "+u+"/messages/m1/move", reply(201, `{"id":"m1-new"}`))
	mux.HandleFunc("GET "+u+"/calendarView", reply(200, `{"value":[{"id":"e1","subject":"Standup",
		"start":{"dateTime":"2026-11-05T09:00:00.0000000","timeZone":"Europe/Zurich"},
		"end":{"dateTime":"2026-11-05T09:15:00.0000000","timeZone":"Europe/Zurich"},
		"organizer":{"emailAddress":{"address":"bob@example.com"}},
		"attendees":[{"emailAddress":{"address":"ada@example.com"},"status":{"response":"accepted"}}]}]}`))
	mux.HandleFunc("POST "+u+"/events", reply(201, `{"id":"e2","subject":"Review",
		"start":{"dateTime":"2026-11-05T14:00:00.0000000","timeZone":"Europe/Zurich"},
		"end":{"dateTime":"2026-11-05T15:00:00.0000000","timeZone":"Europe/Zurich"},
		"onlineMeeting":{"joinUrl":"https://teams.example/join"}}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	a := New(connectors.Options{Endpoints: map[string]string{"login": f.srv.URL + "/login", "graph": f.srv.URL + "/graph"}})
	return f, a
}

func conn(secret string) connectors.Connection {
	return connectors.Connection{
		Kind:    connectors.Outlook,
		Config:  map[string]string{"tenant_id": "tenant-1", "client_id": "app"},
		Secrets: map[string]string{"client_secret": secret},
	}
}

func call[T any](t *testing.T, a *Adapter, target, tool, args string) T {
	t.Helper()
	out, err := a.Call(context.Background(), conn("pw"), target, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	v, ok := out.(T)
	if !ok {
		t.Fatalf("%s returned %T", tool, out)
	}
	return v
}

func TestCheck(t *testing.T) {
	_, a := newFake(t)
	if err := a.Check(context.Background(), conn("pw")); err != nil {
		t.Fatal(err)
	}
	err := a.Check(context.Background(), conn("wrong"))
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("wrong secret: %v", err)
	}
}

func TestTarget(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if got, err := a.Target(ctx, conn("pw"), " Ada@Example.com "); err != nil || got != box {
		t.Fatalf("Target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, conn("pw"), "bob@example.com"); err == nil || !strings.Contains(err.Error(), "RBAC") {
		t.Errorf("mailbox out of scope: %v", err)
	}
	if _, err := a.Target(ctx, conn("pw"), "nobody@example.com"); err == nil || !strings.Contains(err.Error(), "no Exchange Online mailbox") {
		t.Errorf("unknown mailbox: %v", err)
	}
	for _, bad := range []string{"*", "", "ada", "ada@example.com/../bob@example.com", "a@b.com?x=1"} {
		if _, err := a.Target(ctx, conn("pw"), bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestListMessages(t *testing.T) {
	f, a := newFake(t)
	list := call[[]Brief](t, a, box, "list_messages", `{}`)
	if len(list) != 2 || list[0].From != "Bob <bob@example.com>" || list[0].To[0] != box || list[0].Preview != "Please pay" {
		t.Fatalf("list = %+v", list)
	}
	if q := f.last.URL.Query(); q.Get("$top") != "25" || q.Get("$orderby") != "receivedDateTime desc" {
		t.Errorf("query = %s", f.last.URL.RawQuery)
	}
	list = call[[]Brief](t, a, box, "list_messages", `{"search":"in\"voice","unread_only":true,"limit":500}`)
	if len(list) != 1 || list[0].ID != "m1" {
		t.Fatalf("unread search = %+v", list)
	}
	if q := f.last.URL.Query(); q.Get("$search") != `"in voice"` || q.Get("$top") != "100" || q.Get("$orderby") != "" {
		t.Errorf("query = %s", f.last.URL.RawQuery)
	}
	if strings.Contains(f.last.URL.RawQuery, "+") || strings.Contains(f.last.URL.RawQuery, "%24") {
		t.Errorf("query not as Graph wants it: %s", f.last.URL.RawQuery)
	}
	folders := call[[]Folder](t, a, box, "list_folders", `{}`)
	if len(folders) != 1 || folders[0].Unread != 1 {
		t.Errorf("folders = %+v", folders)
	}
}

func TestReadMessage(t *testing.T) {
	f, a := newFake(t)
	m := call[Message](t, a, box, "read_message", `{"id":"m1"}`)
	if m.Body != "Hello\n\nPlease pay." || m.Cc[0] != "Cy <cy@example.com>" || m.Attachments[0].Name != "invoice.pdf" {
		t.Fatalf("message = %+v", m)
	}
	if p := f.last.Header.Get("Prefer"); p != textBody {
		t.Errorf("Prefer = %q", p)
	}
	_, err := a.Call(context.Background(), conn("pw"), box, "read_message", json.RawMessage(`{"id":"gone"}`))
	if err == nil || !strings.Contains(err.Error(), "Microsoft Graph answered 404 Not Found ErrorItemNotFound") {
		t.Errorf("missing message: %v", err)
	}
}

func TestSendAndReply(t *testing.T) {
	f, a := newFake(t)
	call[map[string]any](t, a, box, "send_message",
		`{"to":["Bob <bob@example.com>"],"cc":["cy@example.com"],"subject":"Hi","body":"Line 1\nLine 2"}`)
	msg := f.body["message"].(map[string]any)
	if msg["subject"] != "Hi" || msg["body"].(map[string]any)["contentType"] != "Text" ||
		msg["toRecipients"].([]any)[0].(map[string]any)["emailAddress"].(map[string]any)["address"] != "bob@example.com" ||
		len(msg["ccRecipients"].([]any)) != 1 {
		t.Errorf("sent %v", f.body)
	}
	if _, err := a.Call(context.Background(), conn("pw"), box, "send_message",
		json.RawMessage(`{"to":["not an address"],"subject":"Hi","body":"x"}`)); err == nil {
		t.Error("bad address passed")
	}

	call[map[string]any](t, a, box, "reply", `{"id":"m1","body":"Done <ok>\nThanks","reply_all":true}`)
	if c := f.body["comment"]; c != "Done &lt;ok&gt;<br>\nThanks" {
		t.Errorf("comment = %q", c)
	}

	moved := call[map[string]any](t, a, box, "move_message", `{"id":"m1","folder":"archive"}`)
	if moved["id"] != "m1-new" || f.body["destinationId"] != "archive" {
		t.Errorf("moved %v, sent %v", moved, f.body)
	}
}

func TestEvents(t *testing.T) {
	f, a := newFake(t)
	list := call[[]Event](t, a, box, "list_events", `{"start":"2026-11-05","time_zone":"Europe/Zurich"}`)
	if len(list) != 1 || list[0].Start != "2026-11-05T09:00:00" || list[0].Attendees[0] != "ada@example.com (accepted)" {
		t.Fatalf("events = %+v", list)
	}
	q := f.last.URL.Query()
	if q.Get("endDateTime") == "" || f.last.Header.Get("Prefer") != `outlook.timezone="Europe/Zurich"` {
		t.Errorf("query %s, Prefer %q", f.last.URL.RawQuery, f.last.Header.Get("Prefer"))
	}
	if _, err := a.Call(context.Background(), conn("pw"), box, "list_events",
		json.RawMessage(`{"start":"2026-01-01","end":"2026-12-31"}`)); err == nil {
		t.Error("a year passed")
	}

	ev := call[Event](t, a, box, "create_event", `{"subject":"Review","start":"2026-11-05T14:00","end":"2026-11-05T15:00",
		"time_zone":"Europe/Zurich","attendees":["bob@example.com"],"online_meeting":true}`)
	if ev.ID != "e2" || ev.JoinURL == "" {
		t.Errorf("event = %+v", ev)
	}
	start := f.body["start"].(map[string]any)
	if start["dateTime"] != "2026-11-05T14:00:00" || start["timeZone"] != "Europe/Zurich" || f.body["isOnlineMeeting"] != true {
		t.Errorf("sent %v", f.body)
	}
	if _, err := a.Call(context.Background(), conn("pw"), box, "create_event",
		json.RawMessage(`{"subject":"x","start":"2026-11-05T15:00","end":"2026-11-05T14:00"}`)); err == nil {
		t.Error("end before start passed")
	}
}

// The mailbox in every path is the binding's; the model can only pick ids
// within it.
func TestStaysInMailbox(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	for _, args := range []string{`{"id":"../../bob@example.com/messages/m1"}`, `{"id":"m1?x=1"}`, `{"id":""}`} {
		if _, err := a.Call(ctx, conn("pw"), box, "read_message", json.RawMessage(args)); err == nil {
			t.Errorf("%s passed", args)
		}
	}
	if _, err := a.Call(ctx, conn("pw"), connectors.AnyTarget, "list_messages", nil); err == nil {
		t.Error("any target passed")
	}
	for _, tool := range a.Tools() {
		write := map[string]bool{"send_message": true, "reply": true, "move_message": true, "create_event": true}[tool.Name]
		if tool.Write != write {
			t.Errorf("%s: Write = %v", tool.Name, tool.Write)
		}
	}
}

// A connector people sign in to works as each of them, in their own mailbox
// or one shared with them.
func TestPerUser(t *testing.T) {
	key := jwttest.NewKey(t, "k")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/tenant-1/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case r.Form.Get("client_secret") != "pw":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
		case r.Form.Get("grant_type") == "authorization_code":
			// Microsoft may name the account only by its user name.
			id := key.Sign(t, map[string]any{"preferred_username": "ada@example.com"})
			_, _ = w.Write([]byte(`{"access_token":"user-tok","expires_in":3600,"refresh_token":"r1","id_token":"` + id + `"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS9002313: Invalid request."}`))
		}
	})
	asUser := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer user-tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("GET /graph/me/mailFolders/inbox", asUser(200, `{"id":"AAMkInbox"}`))
	mux.HandleFunc("GET /graph/me/mailFolders", asUser(200, `{"value":[{"id":"f1","displayName":"Inbox"}]}`))
	mux.HandleFunc("GET /graph/users/boss@example.com/mailFolders/inbox", asUser(403,
		`{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a := New(connectors.Options{Endpoints: map[string]string{"login": srv.URL + "/login", "graph": srv.URL + "/graph"}})
	ctx := context.Background()
	client := conn("pw")
	client.PerUser = true

	if err := a.CheckClient(ctx, client); err != nil {
		t.Errorf("CheckClient = %v", err)
	}
	if err := a.CheckClient(ctx, conn("wrong")); err == nil {
		t.Error("a wrong secret passed")
	}
	u, err := a.SignInURL(client, "https://k/cb", "st", "ch")
	if err != nil || !strings.HasPrefix(u, srv.URL+"/login/tenant-1/oauth2/v2.0/authorize?") ||
		!strings.Contains(u, "Mail.ReadWrite") || !strings.Contains(u, "offline_access") {
		t.Errorf("SignInURL = %s, %v", u, err)
	}
	g, err := a.Exchange(ctx, client, "https://k/cb", "code", "v")
	if err != nil || g.Account != "ada@example.com" {
		t.Fatalf("Exchange = %+v, %v", g, err)
	}

	user := client
	user.Token = "user-tok"
	if got, err := a.Target(ctx, user, "Me"); err != nil || got != me {
		t.Errorf("Target(me) = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, user, "boss@example.com"); err == nil || !strings.Contains(err.Error(), "shared with you") {
		t.Errorf("a mailbox not shared: %v", err)
	}
	if _, err := a.Target(ctx, conn("pw"), "me"); err == nil {
		t.Error("me passed on a shared connector")
	}
	if out, err := a.Call(ctx, user, me, "list_folders", nil); err != nil || out == nil {
		t.Errorf("list_folders = %v, %v", out, err)
	}
	if _, err := a.Call(ctx, client, me, "list_folders", nil); err == nil {
		t.Error("a call for nobody passed")
	}
}
