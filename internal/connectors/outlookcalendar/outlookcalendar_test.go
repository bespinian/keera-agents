package outlookcalendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/exchange"
)

const box = "ada@example.com"

type fake struct {
	last *http.Request
	body map[string]any
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/tenant-1/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "pw" || r.Form.Get("scope") != exchange.Scope {
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
	mux.HandleFunc("GET "+u+"/calendar", reply(200, `{"id":"AAMkCal"}`))
	mux.HandleFunc("GET /graph/users/bob@example.com/calendar", reply(403,
		`{"error":{"code":"ErrorAccessDenied","message":"Access to OData is disabled."}}`))
	mux.HandleFunc("GET "+u+"/calendarView", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("$select"), "showAs") {
			reply(200, `{"value":[
				{"start":{"dateTime":"2026-11-05T09:00:00.0000000"},"end":{"dateTime":"2026-11-05T09:15:00.0000000"},"showAs":"busy"},
				{"start":{"dateTime":"2026-11-05T10:00:00.0000000"},"end":{"dateTime":"2026-11-05T11:00:00.0000000"},"showAs":"free"},
				{"start":{"dateTime":"2026-11-05T12:00:00.0000000"},"end":{"dateTime":"2026-11-05T13:00:00.0000000"},"showAs":"busy","isCancelled":true},
				{"start":{"dateTime":"2026-11-06T00:00:00.0000000"},"end":{"dateTime":"2026-11-07T00:00:00.0000000"},"showAs":"oof"}]}`)(w, r)
			return
		}
		reply(200, `{"value":[{"id":"e1","subject":"Standup",
			"start":{"dateTime":"2026-11-05T09:00:00.0000000","timeZone":"Europe/Zurich"},
			"end":{"dateTime":"2026-11-05T09:15:00.0000000","timeZone":"Europe/Zurich"},
			"organizer":{"emailAddress":{"address":"bob@example.com"}},
			"attendees":[{"emailAddress":{"address":"ada@example.com"},"status":{"response":"accepted"}}]}]}`)(w, r)
	})
	created := `{"id":"e2","subject":"Review",
		"start":{"dateTime":"2026-11-05T14:00:00.0000000","timeZone":"Europe/Zurich"},
		"end":{"dateTime":"2026-11-05T15:00:00.0000000","timeZone":"Europe/Zurich"},
		"onlineMeeting":{"joinUrl":"https://teams.example/join"}}`
	mux.HandleFunc("POST "+u+"/events", reply(201, created))
	mux.HandleFunc("PATCH "+u+"/events/e2", reply(200, created))
	mux.HandleFunc("DELETE "+u+"/events/e2", reply(204, ``))
	mux.HandleFunc("GET "+u+"/events/e2", reply(200, `{"id":"e2","subject":"Review","bodyPreview":"Agenda",
		"start":{"dateTime":"2026-11-05T14:00:00.0000000","timeZone":"UTC"},
		"end":{"dateTime":"2026-11-05T15:00:00.0000000","timeZone":"UTC"},
		"body":{"contentType":"text","content":"Agenda\r\n\r\n\r\n\r\n1. Numbers"},"webLink":"https://outlook.example/e2"}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a := New(connectors.Options{Endpoints: map[string]string{"login": srv.URL + "/login", "graph": srv.URL + "/graph"}})
	return f, a
}

func conn() connectors.Connection {
	return connectors.Connection{
		Kind:    connectors.OutlookCalendar,
		Config:  map[string]string{"tenant_id": "tenant-1", "client_id": "app"},
		Secrets: map[string]string{"client_secret": "pw"},
	}
}

func call[T any](t *testing.T, a *Adapter, tool, args string) T {
	t.Helper()
	out, err := a.Call(context.Background(), conn(), box, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	v, ok := out.(T)
	if !ok {
		t.Fatalf("%s returned %T", tool, out)
	}
	return v
}

func fails(t *testing.T, a *Adapter, tool, args string) {
	t.Helper()
	if _, err := a.Call(context.Background(), conn(), box, tool, json.RawMessage(args)); err == nil {
		t.Errorf("%s %s passed", tool, args)
	}
}

// The calendar is checked, not the inbox, so an app with only
// Calendars.ReadWrite may be bound.
func TestTarget(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if got, err := a.Target(ctx, conn(), " Ada@Example.com "); err != nil || got != box {
		t.Fatalf("Target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, conn(), "bob@example.com"); err == nil || !strings.Contains(err.Error(), "Calendars.ReadWrite") {
		t.Errorf("mailbox out of scope: %v", err)
	}
	for _, bad := range []string{"*", "", "me", "ada@example.com/../bob@example.com"} {
		if _, err := a.Target(ctx, conn(), bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestListEvents(t *testing.T) {
	f, a := newFake(t)
	list := call[[]Event](t, a, "list_events", `{"start":"2026-11-05","time_zone":"Europe/Zurich"}`)
	if len(list) != 1 || list[0].Start != "2026-11-05T09:00:00" || list[0].Attendees[0] != "ada@example.com (accepted)" {
		t.Fatalf("events = %+v", list)
	}
	if q := f.last.URL.Query(); q.Get("endDateTime") == "" || f.last.Header.Get("Prefer") != `outlook.timezone="Europe/Zurich"` {
		t.Errorf("query %s, Prefer %q", f.last.URL.RawQuery, f.last.Header.Get("Prefer"))
	}
	fails(t, a, "list_events", `{"start":"2026-01-01","end":"2026-12-31"}`)
	fails(t, a, "list_events", `{"time_zone":"Europe/Zurich\""}`)
}

func TestGetEvent(t *testing.T) {
	f, a := newFake(t)
	ev := call[Event](t, a, "get_event", `{"id":"e2"}`)
	if ev.Body != "Agenda\n\n1. Numbers" || ev.Preview != "" || ev.WebLink == "" {
		t.Errorf("event = %+v", ev)
	}
	if p := f.last.Header.Values("Prefer"); len(p) != 2 || p[1] != exchange.TextBody {
		t.Errorf("Prefer = %q", p)
	}
}

// Free, cancelled and working-elsewhere events leave the time free.
func TestFreeBusy(t *testing.T) {
	_, a := newFake(t)
	out := call[map[string]any](t, a, "free_busy", `{"start":"2026-11-05","end":"2026-11-08"}`)
	busy := out["busy"].([]Busy)
	if len(busy) != 2 || busy[0] != (Busy{Start: "2026-11-05T09:00:00", End: "2026-11-05T09:15:00", Status: "busy"}) ||
		busy[1].Status != "oof" || out["note"] != nil {
		t.Errorf("free_busy = %+v", out)
	}
}

func TestCreateEvent(t *testing.T) {
	f, a := newFake(t)
	ev := call[Event](t, a, "create_event", `{"subject":"Review","start":"2026-11-05T14:00","end":"2026-11-05T15:00",
		"time_zone":"Europe/Zurich","attendees":["bob@example.com"],"online_meeting":true}`)
	if ev.ID != "e2" || ev.JoinURL == "" {
		t.Errorf("event = %+v", ev)
	}
	start := f.body["start"].(map[string]any)
	if start["dateTime"] != "2026-11-05T14:00:00" || start["timeZone"] != "Europe/Zurich" || f.body["isOnlineMeeting"] != true ||
		len(f.body["attendees"].([]any)) != 1 || f.body["body"] != nil || f.body["location"] != nil {
		t.Errorf("sent %v", f.body)
	}
	// An absolute time lands in the event's zone.
	call[Event](t, a, "create_event", `{"subject":"x","start":"2026-11-05T13:00:00Z","end":"2026-11-05T14:00:00Z","time_zone":"Europe/Zurich"}`)
	if got := f.body["start"].(map[string]any)["dateTime"]; got != "2026-11-05T14:00:00" {
		t.Errorf("start = %v", got)
	}
	fails(t, a, "create_event", `{"subject":"x","start":"2026-11-05T15:00","end":"2026-11-05T14:00"}`)
	fails(t, a, "create_event", `{"subject":"x","start":"2026-11-05T15:00"}`)
	fails(t, a, "create_event", `{"start":"2026-11-05T14:00","end":"2026-11-05T15:00"}`)
}

// Only the fields given are sent, so the rest of the event stays.
func TestUpdateEvent(t *testing.T) {
	f, a := newFake(t)
	call[Event](t, a, "update_event", `{"id":"e2","subject":"Review 2"}`)
	if f.last.Method != http.MethodPatch || len(f.body) != 1 || f.body["subject"] != "Review 2" {
		t.Errorf("sent %s %v", f.last.Method, f.body)
	}
	call[Event](t, a, "update_event", `{"id":"e2","end":"2026-11-05T16:00","time_zone":"Europe/Zurich","attendees":[],"location":""}`)
	if len(f.body) != 3 || f.body["start"] != nil || len(f.body["attendees"].([]any)) != 0 ||
		f.body["location"].(map[string]any)["displayName"] != "" {
		t.Errorf("sent %v", f.body)
	}
	fails(t, a, "update_event", `{"id":"e2"}`)
	fails(t, a, "update_event", `{"id":"e2","start":"2026-11-05T16:00","end":"2026-11-05T15:00"}`)
}

func TestDeleteEvent(t *testing.T) {
	f, a := newFake(t)
	if out := call[map[string]any](t, a, "delete_event", `{"id":"e2"}`); out["deleted"] != true || f.last.Method != http.MethodDelete {
		t.Errorf("delete = %v", out)
	}
}

// The mailbox in every path is the binding's; the model can only pick ids
// within it.
func TestStaysInMailbox(t *testing.T) {
	_, a := newFake(t)
	for _, tool := range []string{"get_event", "update_event", "delete_event"} {
		for _, id := range []string{"../../bob@example.com/events/e2", "e2?x=1", ""} {
			fails(t, a, tool, `{"id":"`+id+`","subject":"x"}`)
		}
	}
	if _, err := a.Call(context.Background(), conn(), connectors.AnyTarget, "list_events", nil); err == nil {
		t.Error("any target passed")
	}
	for _, tool := range a.Tools() {
		write := map[string]bool{"create_event": true, "update_event": true, "delete_event": true}[tool.Name]
		if tool.Write != write {
			t.Errorf("%s: Write = %v", tool.Name, tool.Write)
		}
	}
}

// People who sign in are asked for the calendar only, not their mail.
func TestSignInScopes(t *testing.T) {
	_, a := newFake(t)
	client := conn()
	client.PerUser = true
	u, err := a.SignInURL(client, "https://k/cb", "st", "ch")
	if err != nil || !strings.Contains(u, "Calendars.ReadWrite") || strings.Contains(u, "Mail.") {
		t.Errorf("SignInURL = %s, %v", u, err)
	}
}
