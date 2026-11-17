package gcalendar

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace/workspacetest"
)

const cal = "team@group.calendar.google.com"

type sent struct {
	method, path, query string
	body                map[string]any
}

const eventJSON = `{"id":"ev1","status":"confirmed","summary":"Standup","htmlLink":"https://calendar.google.com/e/ev1",
	"start":{"dateTime":"2026-11-05T09:00:00+01:00"},"end":{"dateTime":"2026-11-05T09:15:00+01:00"},
	"organizer":{"email":"ada@acme.com"},"recurringEventId":"ev0",
	"attendees":[{"email":"ada@acme.com","responseStatus":"accepted"},{"email":"bob@acme.com","responseStatus":"needsAction","optional":true}]}`

func setup(t *testing.T) (*workspacetest.Fake, *Adapter, *[]sent) {
	f := workspacetest.New(t)
	var calls []sent
	f.Handle("GET /calendar/v3/users/me/calendarList", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	f.Handle("GET /calendar/v3/calendars/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case cal:
			_, _ = w.Write([]byte(`{"id":"` + cal + `","summary":"Team"}`))
		case "primary":
			_, _ = w.Write([]byte(`{"id":"ada@acme.com"}`))
		default:
			workspacetest.Error(w, http.StatusNotFound, "Not Found")
		}
	})
	record := func(reply string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("cal") != cal {
				workspacetest.Error(w, http.StatusNotFound, "Not Found")
				return
			}
			calls = append(calls, sent{r.Method, r.URL.Path, r.URL.RawQuery, workspacetest.JSON(r)})
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write([]byte(reply))
		}
	}
	f.Handle("GET /calendar/v3/calendars/{cal}/events", record(`{"items":[`+eventJSON+`],"nextPageToken":"x"}`))
	f.Handle("GET /calendar/v3/calendars/{cal}/events/{id}", record(eventJSON))
	f.Handle("POST /calendar/v3/calendars/{cal}/events", record(`{"id":"new","summary":"Lunch","start":{"date":"2026-11-06"},"end":{"date":"2026-11-07"}}`))
	f.Handle("PATCH /calendar/v3/calendars/{cal}/events/{id}", record(eventJSON))
	f.Handle("DELETE /calendar/v3/calendars/{cal}/events/{id}", record(""))
	f.Handle("POST /calendar/v3/freeBusy", func(w http.ResponseWriter, r *http.Request) {
		in := workspacetest.JSON(r)
		calls = append(calls, sent{r.Method, r.URL.Path, "", in})
		_, _ = w.Write([]byte(`{"calendars":{"` + cal + `":{"busy":[{"start":"2026-11-05T08:00:00Z","end":"2026-11-05T09:00:00Z"}]}}}`))
	})
	a := New(f.Options())
	a.now = func() time.Time { return time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) }
	return f, a, &calls
}

func call(t *testing.T, a *Adapter, conn connectors.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func TestCheckAndTarget(t *testing.T) {
	f, a, _ := setup(t)
	conn := f.Conn(connectors.GoogleCalendar, "")
	ctx := context.Background()
	if err := a.Check(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if f.Scope != scope {
		t.Errorf("scope = %q", f.Scope)
	}
	if err := a.Check(ctx, f.Conn(connectors.GoogleCalendar, workspacetest.Outsider)); err == nil || !strings.Contains(err.Error(), "domain-wide delegation") {
		t.Errorf("outsider: %v", err)
	}
	if got, err := a.Target(ctx, conn, " "+cal+" "); err != nil || got != cal {
		t.Errorf("Target = %q, %v", got, err)
	}
	if got, err := a.Target(ctx, conn, "primary"); err != nil || got != "ada@acme.com" {
		t.Errorf("primary = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, conn, "bob@acme.com"); err == nil || !strings.Contains(err.Error(), "not shared with the service account sa@") {
		t.Errorf("unshared: %v", err)
	}
	for _, bad := range []string{"*", "", "a/b", "a b"} {
		if _, err := a.Target(ctx, conn, bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestReads(t *testing.T) {
	f, a, calls := setup(t)
	conn := f.Conn(connectors.GoogleCalendar, "")
	out, err := call(t, a, conn, cal, "list_events", `{"query":"standup"}`)
	if err != nil || !strings.Contains(out, `"summary":"Standup"`) || !strings.Contains(out, `{"email":"bob@acme.com","response":"needsAction","optional":true}`) ||
		!strings.Contains(out, `"recurring":true`) || !strings.Contains(out, `"more"`) {
		t.Errorf("list_events = %s, %v", out, err)
	}
	q := (*calls)[0].query
	for _, want := range []string{"singleEvents=true", "orderBy=startTime", "timeMin=2026-11-04T10%3A00%3A00Z", "timeMax=2026-11-11T10%3A00%3A00Z", "q=standup", "maxResults=25"} {
		if !strings.Contains(q, want) {
			t.Errorf("list query %s lacks %s", q, want)
		}
	}
	if _, err := call(t, a, conn, cal, "list_events", `{"from":"2026-11-05","to":"2026-11-04"}`); err == nil {
		t.Error("a backwards range passed")
	}
	out, err = call(t, a, conn, cal, "get_event", `{"event_id":"ev1"}`)
	if err != nil || !strings.Contains(out, `"organizer":"ada@acme.com"`) {
		t.Errorf("get_event = %s, %v", out, err)
	}
	out, err = call(t, a, conn, cal, "free_busy", `{"from":"2026-11-05T00:00:00Z","to":"2026-11-06T00:00:00Z"}`)
	if err != nil || !strings.Contains(out, `"busy":[{"start":"2026-11-05T08:00:00Z","end":"2026-11-05T09:00:00Z"}]`) {
		t.Errorf("free_busy = %s, %v", out, err)
	}
	if items := (*calls)[2].body["items"].([]any); items[0].(map[string]any)["id"] != cal {
		t.Errorf("free_busy asked for %v", items)
	}
}

func TestWrites(t *testing.T) {
	f, a, calls := setup(t)
	conn := f.Conn(connectors.GoogleCalendar, "")
	out, err := call(t, a, conn, cal, "create_event",
		`{"summary":"Lunch","start":"2026-11-06","end":"2026-11-07","attendees":["bob@acme.com"],"notify":true}`)
	if err != nil || !strings.Contains(out, `"all_day":true`) {
		t.Errorf("create_event = %s, %v", out, err)
	}
	c := (*calls)[0]
	if c.query != "sendUpdates=all" || c.body["start"].(map[string]any)["date"] != "2026-11-06" ||
		c.body["attendees"].([]any)[0].(map[string]any)["email"] != "bob@acme.com" {
		t.Errorf("create sent %+v", c)
	}
	if _, err := call(t, a, conn, cal, "create_event", `{"summary":"x","start":"tomorrow","end":"later"}`); err == nil {
		t.Error("a bad time passed")
	}

	if _, err := call(t, a, conn, cal, "update_event",
		`{"event_id":"ev1","start":"2026-11-05T10:00:00","end":"2026-11-05T10:15:00","time_zone":"Europe/Zurich"}`); err != nil {
		t.Fatal(err)
	}
	c = (*calls)[1]
	start := c.body["start"].(map[string]any)
	if c.method != http.MethodPatch || c.query != "sendUpdates=none" || start["timeZone"] != "Europe/Zurich" || c.body["summary"] != nil {
		t.Errorf("update sent %+v", c)
	}
	if _, err := call(t, a, conn, cal, "update_event", `{"event_id":"ev1"}`); err == nil {
		t.Error("an empty update passed")
	}

	out, err = call(t, a, conn, cal, "delete_event", `{"event_id":"ev1"}`)
	if err != nil || out != `{"deleted":true,"id":"ev1"}` || (*calls)[2].path != "/calendar/v3/calendars/"+cal+"/events/ev1" {
		t.Errorf("delete_event = %s, %v", out, err)
	}
}

func TestTargetEnforced(t *testing.T) {
	f, a, calls := setup(t)
	conn := f.Conn(connectors.GoogleCalendar, "")
	// A tool has no calendar argument: an extra one is ignored, and the call
	// goes to the target.
	if _, err := call(t, a, conn, cal, "get_event", `{"event_id":"ev1","calendar_id":"bob@acme.com"}`); err != nil {
		t.Fatal(err)
	}
	if p := (*calls)[0].path; p != "/calendar/v3/calendars/"+cal+"/events/ev1" {
		t.Errorf("went to %s", p)
	}
	for _, bad := range []string{"../../users/me/calendarList", "ev1/instances", "ev1?x=1"} {
		if _, err := call(t, a, conn, cal, "delete_event", `{"event_id":"`+bad+`"}`); err == nil {
			t.Errorf("event id %q passed", bad)
		}
	}
	if _, err := call(t, a, conn, "*", "list_events", `{}`); err == nil {
		t.Error("AnyTarget passed")
	}
}

func TestAPIError(t *testing.T) {
	f, a, _ := setup(t)
	_, err := call(t, a, f.Conn(connectors.GoogleCalendar, ""), "bob@acme.com", "list_events", `{}`)
	if err == nil || !strings.Contains(err.Error(), "listing events: Google answered 404 Not Found") {
		t.Errorf("err = %v", err)
	}
}
