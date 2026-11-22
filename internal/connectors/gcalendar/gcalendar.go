// Package gcalendar lets agents read and change one Google Calendar as a
// service account, as a user of the domain through domain-wide delegation,
// or as each person who signs in with their own account.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "calendar": the base of the Calendar API, default https://www.googleapis.com
package gcalendar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace"
)

const (
	scope = "https://www.googleapis.com/auth/calendar"
	// maxAttendees bounds an event's guest list in a result, so an
	// all-hands meeting does not fill the model's context.
	maxAttendees = 50
)

var (
	// validCalendar is a calendar's id, such as ada@acme.com, a group
	// calendar's …@group.calendar.google.com or a holiday calendar's, which
	// has a #. It is escaped into URLs; slashes and spaces are never valid.
	validCalendar = regexp.MustCompile(`^[^\s/]{1,255}$`)
	// validEvent is an event's id: base32hex, and for one occurrence of a
	// recurring event, _ and its start time.
	validEvent = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

type Adapter struct {
	client *workspace.Client
	api    string
	now    func() time.Time
	apitool.Set
	workspace.Accounts
}

const timeHelp = "RFC 3339, such as 2026-11-04T09:00:00+01:00, or a date such as 2026-11-04 for an all-day event."

func New(opts connectors.Options) *Adapter {
	a := &Adapter{
		client: workspace.NewClient(opts, scope),
		api:    web.Endpoint(opts, "calendar", "https://www.googleapis.com") + "/calendar/v3",
		now:    time.Now,
	}
	a.Set = apitool.Set{
		{
			Name:        "list_events",
			Description: "List the calendar's events in a time range, by start time. Recurring events are listed one occurrence at a time.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"from":{"type":"string","description":"RFC 3339. Now by default. A date alone is midnight UTC."},
				"to":{"type":"string","description":"RFC 3339. Seven days after from by default."},
				"query":{"type":"string","description":"Only events with this text, such as in their title, place or guests."},
				"limit":{"type":"integer","description":"At most this many, 25 by default, up to 100."}}}`),
			Run: run(a.listEvents),
		},
		{
			Name:        "get_event",
			Description: "Read one event of the calendar, with its description and guests.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"event_id":{"type":"string"}},"required":["event_id"]}`),
			Run: run(a.getEvent),
		},
		{
			Name:        "free_busy",
			Description: "List when the calendar is busy in a time range, without the events' details.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"from":{"type":"string","description":"RFC 3339. Now by default. A date alone is midnight UTC."},
				"to":{"type":"string","description":"RFC 3339. Seven days after from by default."}}}`),
			Run: run(a.freeBusy),
		},
		{
			Name:        "create_event",
			Description: "Create an event in the calendar. Guests get an invitation only when notify is true.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"summary":{"type":"string","description":"The event's title."},
				"start":{"type":"string","description":"` + timeHelp + `"},
				"end":{"type":"string","description":"Like start. For an all-day event, the day after the last day."},
				"time_zone":{"type":"string","description":"An IANA time zone, such as Europe/Zurich. Needed for start and end without an offset, and for recurring events."},
				"description":{"type":"string"},
				"location":{"type":"string"},
				"attendees":{"type":"array","items":{"type":"string"},"description":"The guests' emails."},
				"notify":{"type":"boolean","description":"Email the guests. False by default."}},
				"required":["summary","start","end"]}`),
			Write: true,
			Run:   run(a.createEvent),
		},
		{
			Name:        "update_event",
			Description: "Change an event of the calendar. Only the fields given change; attendees, if given, replaces the guest list.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"event_id":{"type":"string"},
				"summary":{"type":"string"},
				"start":{"type":"string","description":"` + timeHelp + `"},
				"end":{"type":"string"},
				"time_zone":{"type":"string"},
				"description":{"type":"string"},
				"location":{"type":"string"},
				"attendees":{"type":"array","items":{"type":"string"}},
				"notify":{"type":"boolean","description":"Email the guests about the change. False by default."}},
				"required":["event_id"]}`),
			Write: true,
			Run:   run(a.updateEvent),
		},
		{
			Name:        "delete_event",
			Description: "Delete an event of the calendar.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"event_id":{"type":"string"},
				"notify":{"type":"boolean","description":"Email the guests that it was cancelled. False by default."}},
				"required":["event_id"]}`),
			Write: true,
			Run:   run(a.deleteEvent),
		},
	}
	a.Accounts = workspace.NewAccounts(a.client, "Google Calendar API")
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.GoogleCalendar,
		Label:    "Google Calendar",
		Vendor:   "Google",
		Category: "Email and calendar",
		Summary:  "Agents read a Google Calendar's events and free time, and create, change and delete events.",
		Fields:   workspace.Fields("Google Calendar API"),
		Setup: workspace.Setup("Google Calendar API",
			"each calendar (with **Make changes to events** for agents that write)",
			"Agents can then invite no guests: Google lets a service account invite people only through domain-wide delegation.",
			scope),
		TargetLabel: "Calendar",
		TargetHelp:  "The calendar's ID, from its settings: a person's email, such as ada@acme.com, or a shared calendar's …@group.calendar.google.com.",
	}
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	return a.client.Check(ctx, conn, "Google Calendar API", func(ctx context.Context, s workspace.Settings) error {
		return a.client.Do(ctx, s, http.MethodGet, a.api+"/users/me/calendarList?maxResults=1", nil, &struct{}{})
	})
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	s, err := workspace.SettingsOf(conn)
	if err != nil {
		return "", err
	}
	id, err := calendarID(target)
	if err != nil {
		return "", err
	}
	var c struct {
		ID string `json:"id"`
	}
	err = a.client.Do(ctx, s, http.MethodGet, a.api+calendarPath(id), nil, &c)
	if web.Status(err) == http.StatusNotFound {
		return "", fmt.Errorf("there is no calendar %s, or it is not shared with %s", id, s.Who())
	}
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func calendarID(target string) (string, error) {
	id := strings.TrimSpace(target)
	if id == connectors.AnyTarget || !validCalendar.MatchString(id) {
		return "", fmt.Errorf("%q is not a calendar ID; it is in the calendar's settings, such as ada@acme.com", web.Short(id, 80))
	}
	return id, nil
}

// run makes a tool's function from one that takes its decoded arguments,
// the settings and the target calendar's ID, the only one a call reaches.
func run[T any](f func(ctx context.Context, s workspace.Settings, cal string, in T) (any, error)) apitool.Run {
	return apitool.Typed(func(ctx context.Context, conn connectors.Connection, target string, in T) (any, error) {
		s, err := workspace.SettingsOf(conn)
		if err != nil {
			return nil, err
		}
		cal, err := calendarID(target)
		if err != nil {
			return nil, err
		}
		return f(ctx, s, cal, in)
	})
}

// calendarPath is the API path of a calendar.
func calendarPath(cal string) string {
	return "/calendars/" + url.PathEscape(cal)
}

func eventPath(cal, id string) (string, error) {
	if len(id) > 1024 || !validEvent.MatchString(id) {
		return "", fmt.Errorf("%q is not an event id; list_events shows them", web.Short(id, 80))
	}
	return calendarPath(cal) + "/events/" + id, nil
}

// sendUpdates says whether Google emails the guests.
func sendUpdates(notify bool) string {
	if notify {
		return "?sendUpdates=all"
	}
	return "?sendUpdates=none"
}

type rangeArgs struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// span reads a range, by default the next seven days.
func (a *Adapter) span(in rangeArgs) (from, to time.Time, err error) {
	from = a.now().UTC().Truncate(time.Second)
	if in.From != "" {
		if from, err = parseTime(in.From); err != nil {
			return
		}
	}
	to = from.AddDate(0, 0, 7)
	if in.To != "" {
		if to, err = parseTime(in.To); err != nil {
			return
		}
	}
	if !to.After(from) {
		err = errors.New("the range ends before it starts")
	}
	return
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time; write RFC 3339, such as 2026-11-04T09:00:00+01:00", web.Short(s, 60))
}

func (a *Adapter) listEvents(ctx context.Context, s workspace.Settings, cal string, in rangeArgs) (any, error) {
	from, to, err := a.span(in)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"timeMin":      {from.Format(time.RFC3339)},
		"timeMax":      {to.Format(time.RFC3339)},
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"maxResults":   {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))},
	}
	if in.Query != "" {
		q.Set("q", in.Query)
	}
	var r struct {
		Items         []event `json:"items"`
		NextPageToken string  `json:"nextPageToken"`
	}
	if err := a.client.Do(ctx, s, http.MethodGet, a.api+calendarPath(cal)+"/events?"+q.Encode(), nil, &r); err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	events := []eventView{}
	for _, e := range r.Items {
		events = append(events, e.view(300))
	}
	out := map[string]any{"from": from, "to": to, "events": events}
	if r.NextPageToken != "" {
		out["more"] = "there are more events; ask for a shorter range or a higher limit"
	}
	return out, nil
}

func (a *Adapter) getEvent(ctx context.Context, s workspace.Settings, cal string, in struct {
	EventID string `json:"event_id"`
},
) (any, error) {
	path, err := eventPath(cal, in.EventID)
	if err != nil {
		return nil, err
	}
	var e event
	if err := a.client.Do(ctx, s, http.MethodGet, a.api+path, nil, &e); err != nil {
		return nil, fmt.Errorf("reading the event: %w", err)
	}
	return e.view(5000), nil
}

func (a *Adapter) freeBusy(ctx context.Context, s workspace.Settings, cal string, in rangeArgs) (any, error) {
	from, to, err := a.span(in)
	if err != nil {
		return nil, err
	}
	var r struct {
		Calendars map[string]struct {
			Busy []struct {
				Start time.Time `json:"start"`
				End   time.Time `json:"end"`
			} `json:"busy"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"calendars"`
	}
	err = a.client.Do(ctx, s, http.MethodPost, a.api+"/freeBusy", map[string]any{
		"timeMin": from.Format(time.RFC3339), "timeMax": to.Format(time.RFC3339),
		"items": []any{map[string]string{"id": cal}},
	}, &r)
	if err != nil {
		return nil, fmt.Errorf("reading free and busy times: %w", err)
	}
	c, ok := r.Calendars[cal]
	if !ok {
		return nil, errors.New("the answer had no free and busy times for the calendar")
	}
	if len(c.Errors) > 0 {
		return nil, fmt.Errorf("the calendar's free and busy times cannot be read: %s", c.Errors[0].Reason)
	}
	type block struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	}
	busy := []block{}
	for _, b := range c.Busy {
		busy = append(busy, block{b.Start, b.End})
	}
	return map[string]any{"from": from, "to": to, "busy": busy}, nil
}

type writeArgs struct {
	EventID     string    `json:"event_id"`
	Summary     *string   `json:"summary"`
	Start       string    `json:"start"`
	End         string    `json:"end"`
	TimeZone    string    `json:"time_zone"`
	Description *string   `json:"description"`
	Location    *string   `json:"location"`
	Attendees   *[]string `json:"attendees"`
	Notify      bool      `json:"notify"`
}

// body is the event's fields the arguments give, as the API takes them.
func (in writeArgs) body() (map[string]any, error) {
	b := map[string]any{}
	for name, v := range map[string]*string{"summary": in.Summary, "description": in.Description, "location": in.Location} {
		if v != nil {
			b[name] = *v
		}
	}
	for name, v := range map[string]string{"start": in.Start, "end": in.End} {
		if v == "" {
			continue
		}
		t, err := eventTimeOf(v, in.TimeZone)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		b[name] = t
	}
	if in.Attendees != nil {
		list := []map[string]string{}
		for _, e := range *in.Attendees {
			e = strings.TrimSpace(e)
			if !strings.Contains(e, "@") {
				return nil, fmt.Errorf("%q is not an email", web.Short(e, 80))
			}
			list = append(list, map[string]string{"email": e})
		}
		b["attendees"] = list
	}
	return b, nil
}

// eventTimeOf is a time as the API takes it: a date for an all-day event, a
// time otherwise, with an offset or a time zone.
func eventTimeOf(s, zone string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if _, err := time.Parse(time.DateOnly, s); err == nil {
		return map[string]string{"date": s}, nil
	}
	_, err := time.Parse(time.RFC3339, s)
	if err != nil && zone != "" {
		// A local time is fine with a zone to read it in.
		_, err = time.Parse("2006-01-02T15:04:05", s)
	}
	if err != nil {
		return nil, fmt.Errorf("%q is not a time; %s", web.Short(s, 60), strings.ToLower(timeHelp[:1])+timeHelp[1:])
	}
	t := map[string]string{"dateTime": s}
	if zone != "" {
		t["timeZone"] = zone
	}
	return t, nil
}

func (a *Adapter) createEvent(ctx context.Context, s workspace.Settings, cal string, in writeArgs) (any, error) {
	if in.Summary == nil || strings.TrimSpace(*in.Summary) == "" {
		return nil, apitool.Need("summary", "")
	}
	if in.Start == "" || in.End == "" {
		return nil, errors.New("give start and end")
	}
	b, err := in.body()
	if err != nil {
		return nil, err
	}
	var e event
	if err := a.client.Do(ctx, s, http.MethodPost, a.api+calendarPath(cal)+"/events"+sendUpdates(in.Notify), b, &e); err != nil {
		return nil, fmt.Errorf("creating the event: %w", err)
	}
	return e.view(500), nil
}

func (a *Adapter) updateEvent(ctx context.Context, s workspace.Settings, cal string, in writeArgs) (any, error) {
	path, err := eventPath(cal, in.EventID)
	if err != nil {
		return nil, err
	}
	b, err := in.body()
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("give at least one field to change")
	}
	var e event
	if err := a.client.Do(ctx, s, http.MethodPatch, a.api+path+sendUpdates(in.Notify), b, &e); err != nil {
		return nil, fmt.Errorf("changing the event: %w", err)
	}
	return e.view(500), nil
}

func (a *Adapter) deleteEvent(ctx context.Context, s workspace.Settings, cal string, in writeArgs) (any, error) {
	path, err := eventPath(cal, in.EventID)
	if err != nil {
		return nil, err
	}
	if err := a.client.Do(ctx, s, http.MethodDelete, a.api+path+sendUpdates(in.Notify), nil, nil); err != nil {
		return nil, fmt.Errorf("deleting the event: %w", err)
	}
	return map[string]any{"id": in.EventID, "deleted": true}, nil
}

// event is an event as the API returns it.
type event struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	HTMLLink    string    `json:"htmlLink"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"`
	Location    string    `json:"location"`
	Start       eventTime `json:"start"`
	End         eventTime `json:"end"`
	Organizer   struct {
		Email string `json:"email"`
	} `json:"organizer"`
	Attendees []struct {
		Email          string `json:"email"`
		ResponseStatus string `json:"responseStatus"`
		Optional       bool   `json:"optional"`
		Resource       bool   `json:"resource"`
	} `json:"attendees"`
	HangoutLink      string `json:"hangoutLink"`
	RecurringEventID string `json:"recurringEventId"`
}

type eventTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
}

func (t eventTime) String() string {
	if t.DateTime != "" {
		return t.DateTime
	}
	return t.Date
}

// eventView is an event for the model.
type eventView struct {
	ID          string         `json:"id"`
	Summary     string         `json:"summary"`
	Start       string         `json:"start"`
	End         string         `json:"end"`
	AllDay      bool           `json:"all_day,omitempty"`
	Status      string         `json:"status,omitempty"`
	Location    string         `json:"location,omitempty"`
	Description string         `json:"description,omitempty"`
	Organizer   string         `json:"organizer,omitempty"`
	Attendees   []attendeeView `json:"attendees,omitempty"`
	More        int            `json:"more_attendees,omitempty"`
	Meet        string         `json:"meet,omitempty"`
	Recurring   bool           `json:"recurring,omitempty"`
	URL         string         `json:"url,omitempty"`
}

type attendeeView struct {
	Email    string `json:"email"`
	Response string `json:"response,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Room     bool   `json:"room,omitempty"`
}

// view trims the description to n bytes.
func (e event) view(n int) eventView {
	desc := web.Cut(e.Description, n, "…")
	v := eventView{
		ID: e.ID, Summary: e.Summary, Start: e.Start.String(), End: e.End.String(), AllDay: e.Start.Date != "",
		Location: e.Location, Description: desc, Organizer: e.Organizer.Email,
		Meet: e.HangoutLink, Recurring: e.RecurringEventID != "", URL: e.HTMLLink,
	}
	if e.Status != "confirmed" {
		v.Status = e.Status
	}
	for i, at := range e.Attendees {
		if i == maxAttendees {
			v.More = len(e.Attendees) - i
			break
		}
		v.Attendees = append(v.Attendees, attendeeView{Email: at.Email, Response: at.ResponseStatus, Optional: at.Optional, Room: at.Resource})
	}
	return v
}
