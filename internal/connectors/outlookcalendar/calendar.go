package outlookcalendar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/exchange"
	"github.com/bespinian/keera-agents/internal/connectors/internal/mailtext"
	"github.com/bespinian/keera-agents/internal/connectors/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const (
	eventFields = "id,subject,start,end,isAllDay,isCancelled,location,organizer,attendees,onlineMeeting,bodyPreview"
	// maxRange bounds list_events and free_busy, so a year of meetings does
	// not fill the model's context.
	maxRange = 93 * 24 * time.Hour
	// maxBody bounds an event's description, in bytes.
	maxBody   = 20000
	localTime = "2006-01-02T15:04:05"
)

const (
	rangeStart = `"start":{"type":"string","description":"RFC 3339 time or a date, such as 2026-11-05. Default now."}`
	rangeEnd   = `"end":{"type":"string","description":"RFC 3339 time or a date. Default 7 days after start."}`
	zoneShown  = `"time_zone":{"type":"string","description":"The time zone to show times in, such as Europe/Zurich. Default UTC."}`
	timeHelp   = "Local time in time_zone, such as 2026-11-05T14:00, or RFC 3339 with an offset."
)

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name:        "list_events",
			Description: "List calendar events in a time range, earliest first. Default the next 7 days.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + rangeStart + `,` + rangeEnd + `,` + zoneShown + `,
				"limit":{"type":"integer","description":"At most 100. Default 50."}}}`),
			Run: exchange.Run(a.listEvents),
		},
		{
			Name:        "get_event",
			Description: "Read one event, with its description, attendees and their answers.",
			Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"The event's id, from list_events."},` + zoneShown + `}}`),
			Run: exchange.Run(a.getEvent),
		},
		{
			Name:        "free_busy",
			Description: "List when the calendar is busy in a time range, without the events' details.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{` + rangeStart + `,` + rangeEnd + `,` + zoneShown + `}}`),
			Run:         exchange.Run(a.freeBusy),
		},
		{
			Name:        "create_event",
			Description: "Create a calendar event. Attendees get an invitation by email.",
			Parameters: apitool.Schema(`{"type":"object","required":["subject","start","end"],"properties":{
				"subject":{"type":"string"},
				"start":{"type":"string","description":"` + timeHelp + `"},
				"end":{"type":"string","description":"As start."},
				"time_zone":{"type":"string","description":"Such as Europe/Zurich. Default UTC."},
				"attendees":{"type":"array","items":{"type":"string"},"description":"Email addresses to invite."},
				"body":{"type":"string","description":"Plain text."},
				"location":{"type":"string"},
				"online_meeting":{"type":"boolean","description":"Add a Teams meeting."}}}`),
			Write: true,
			Run:   exchange.Run(a.createEvent),
		},
		{
			Name: "update_event",
			Description: "Change an event. Only the fields given change; attendees, if given, replaces the list. " +
				"Attendees get the update by email.",
			Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"The event's id."},
				"subject":{"type":"string"},
				"start":{"type":"string","description":"` + timeHelp + `"},
				"end":{"type":"string","description":"As start."},
				"time_zone":{"type":"string","description":"Such as Europe/Zurich. Default UTC."},
				"attendees":{"type":"array","items":{"type":"string"},"description":"Email addresses."},
				"body":{"type":"string","description":"Plain text."},
				"location":{"type":"string"}}}`),
			Write: true,
			Run:   exchange.Run(a.updateEvent),
		},
		{
			Name:        "delete_event",
			Description: "Delete an event. If the mailbox organised it, attendees get a cancellation.",
			Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"The event's id."}}}`),
			Write: true,
			Run:   exchange.Run(a.deleteEvent),
		},
	}
}

// validZone is a time zone as Graph takes them: IANA, such as
// Europe/Zurich, or Windows, such as W. Europe Standard Time. It goes into
// a header, between quotes.
var validZone = regexp.MustCompile(`^[A-Za-z0-9 _/+:.()-]{1,64}$`)

func zone(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "UTC", nil
	}
	if !validZone.MatchString(s) {
		return "", fmt.Errorf("%q is not a time zone", web.Short(s, 40))
	}
	return s, nil
}

// showIn asks Graph to answer with times in tz.
func showIn(tz string) string { return `outlook.timezone="` + tz + `"` }

// location is the zone for times given without an offset, or UTC when Go
// does not know it, such as a Windows name.
func location(tz string) *time.Location {
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

// parseTime reads an RFC 3339 time, or a local time or date in loc.
func parseTime(s string, loc *time.Location) (t time.Time, offset bool, err error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true, nil
	}
	for _, f := range []string{localTime, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t, false, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("%q is not a time; write 2026-11-05T14:00 or 2026-11-05", web.Short(s, 40))
}

// eventTime reads a time for an event in tz. Graph takes local times, so an
// absolute one is turned into the event's zone, which must be one Go knows.
func eventTime(s, tz string) (time.Time, error) {
	loc := location(tz)
	t, offset, err := parseTime(s, loc)
	if err != nil || !offset {
		return t, err
	}
	if loc == time.UTC && tz != "UTC" {
		return t, fmt.Errorf("give times without an offset, or an IANA time zone such as Europe/Zurich instead of %q", web.Short(tz, 40))
	}
	return t.In(loc), nil
}

// timeRange reads a range to look in: by default the next 7 days, and at
// most maxRange.
func timeRange(startText, endText, tz string) (start, end time.Time, err error) {
	loc := location(tz)
	start = time.Now()
	if startText != "" {
		if start, _, err = parseTime(startText, loc); err != nil {
			return start, end, err
		}
	}
	end = start.Add(7 * 24 * time.Hour)
	if endText != "" {
		if end, _, err = parseTime(endText, loc); err != nil {
			return start, end, err
		}
	}
	switch {
	case !end.After(start):
		return start, end, errors.New("end must be after start")
	case end.Sub(start) > maxRange:
		return start, end, errors.New("ask for at most 93 days at a time")
	}
	return start, end, nil
}

// viewQuery asks calendarView for the events between start and end.
func viewQuery(start, end time.Time, fields string, top int) map[string]string {
	return map[string]string{
		"startDateTime": start.UTC().Format(time.RFC3339),
		"endDateTime":   end.UTC().Format(time.RFC3339),
		"$select":       fields,
		"$orderby":      "start/dateTime",
		"$top":          strconv.Itoa(top),
	}
}

type graphTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// short drops the seven digits of fractions Graph writes, which nobody needs.
func (g graphTime) short() string {
	s, _, _ := strings.Cut(g.DateTime, ".")
	return s
}

type event struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	Start       graphTime `json:"start"`
	End         graphTime `json:"end"`
	IsAllDay    bool      `json:"isAllDay"`
	IsCancelled bool      `json:"isCancelled"`
	ShowAs      string    `json:"showAs"`
	Location    struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Organizer *exchange.Recipient `json:"organizer"`
	Attendees []struct {
		exchange.Recipient
		Status struct {
			Response string `json:"response"`
		} `json:"status"`
	} `json:"attendees"`
	OnlineMeeting *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
	BodyPreview string `json:"bodyPreview"`
	Body        struct {
		Content string `json:"content"`
	} `json:"body"`
	WebLink string `json:"webLink"`
}

// Event is a calendar event. Start and end are local times in TimeZone.
type Event struct {
	ID        string   `json:"id"`
	Subject   string   `json:"subject"`
	Start     string   `json:"start"`
	End       string   `json:"end"`
	TimeZone  string   `json:"time_zone"`
	AllDay    bool     `json:"all_day,omitempty"`
	Cancelled bool     `json:"cancelled,omitempty"`
	Location  string   `json:"location,omitempty"`
	Organizer string   `json:"organizer,omitempty"`
	Attendees []string `json:"attendees,omitempty"`
	JoinURL   string   `json:"online_meeting_url,omitempty"`
	Preview   string   `json:"preview,omitempty"`
	Body      string   `json:"body,omitempty"`
	WebLink   string   `json:"web_link,omitempty"`
}

func (e event) compact() Event {
	out := Event{
		ID: e.ID, Subject: e.Subject, Start: e.Start.short(), End: e.End.short(),
		TimeZone: e.Start.TimeZone, AllDay: e.IsAllDay, Cancelled: e.IsCancelled,
		Location: e.Location.DisplayName, Preview: e.BodyPreview,
	}
	if e.Organizer != nil {
		out.Organizer = e.Organizer.String()
	}
	for _, at := range e.Attendees {
		s := at.String()
		if r := at.Status.Response; r != "" && r != "none" {
			s += " (" + r + ")"
		}
		out.Attendees = append(out.Attendees, s)
	}
	if e.OnlineMeeting != nil {
		out.JoinURL = e.OnlineMeeting.JoinURL
	}
	return out
}

func (a *Adapter) listEvents(ctx context.Context, app microsoft.App, box string, in struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	TimeZone string `json:"time_zone"`
	Limit    int    `json:"limit"`
},
) (any, error) {
	tz, err := zone(in.TimeZone)
	if err != nil {
		return nil, err
	}
	start, end, err := timeRange(in.Start, in.End, tz)
	if err != nil {
		return nil, err
	}
	var r struct {
		Value []event `json:"value"`
	}
	q := viewQuery(start, end, eventFields, apitool.Limit(in.Limit, 50, 100))
	if err := a.box.Call(ctx, app, box, http.MethodGet, "/calendarView", q, nil, &r, showIn(tz)); err != nil {
		return nil, err
	}
	out := make([]Event, len(r.Value))
	for i, e := range r.Value {
		out[i] = e.compact()
	}
	return out, nil
}

func (a *Adapter) getEvent(ctx context.Context, app microsoft.App, box string, in struct {
	ID       string `json:"id"`
	TimeZone string `json:"time_zone"`
},
) (any, error) {
	eid, err := exchange.ID("event", in.ID)
	if err != nil {
		return nil, err
	}
	tz, err := zone(in.TimeZone)
	if err != nil {
		return nil, err
	}
	var e event
	q := map[string]string{"$select": eventFields + ",body,webLink"}
	if err := a.box.Call(ctx, app, box, http.MethodGet, "/events/"+eid, q, nil, &e, showIn(tz), exchange.TextBody); err != nil {
		return nil, err
	}
	out := e.compact()
	out.Preview, out.Body, out.WebLink = "", mailtext.Body(e.Body.Content, maxBody), e.WebLink
	return out, nil
}

// Busy is a time the calendar is taken. Status is busy, tentative or oof
// (out of office).
type Busy struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Status string `json:"status"`
}

func (a *Adapter) freeBusy(ctx context.Context, app microsoft.App, box string, in struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	TimeZone string `json:"time_zone"`
},
) (any, error) {
	tz, err := zone(in.TimeZone)
	if err != nil {
		return nil, err
	}
	start, end, err := timeRange(in.Start, in.End, tz)
	if err != nil {
		return nil, err
	}
	var r struct {
		Value []event `json:"value"`
		Next  string  `json:"@odata.nextLink"`
	}
	q := viewQuery(start, end, "start,end,isCancelled,showAs", 500)
	if err := a.box.Call(ctx, app, box, http.MethodGet, "/calendarView", q, nil, &r, showIn(tz)); err != nil {
		return nil, err
	}
	busy := []Busy{}
	for _, e := range r.Value {
		// Working elsewhere is free to be booked, as Outlook shows it.
		if e.IsCancelled || e.ShowAs == "free" || e.ShowAs == "workingElsewhere" {
			continue
		}
		busy = append(busy, Busy{Start: e.Start.short(), End: e.End.short(), Status: e.ShowAs})
	}
	out := map[string]any{"time_zone": tz, "busy": busy}
	if r.Next != "" {
		out["note"] = "Only the first 500 events were looked at; ask for a shorter range."
	}
	return out, nil
}

func (a *Adapter) createEvent(ctx context.Context, app microsoft.App, box string, in struct {
	Subject       string   `json:"subject"`
	Start         string   `json:"start"`
	End           string   `json:"end"`
	TimeZone      string   `json:"time_zone"`
	Attendees     []string `json:"attendees"`
	Body          string   `json:"body"`
	Location      string   `json:"location"`
	OnlineMeeting bool     `json:"online_meeting"`
},
) (any, error) {
	if err := apitool.Need("subject", in.Subject); err != nil {
		return nil, err
	}
	attendees := &in.Attendees
	if len(in.Attendees) == 0 {
		attendees = nil
	}
	ev, tz, err := changes(eventChange{
		Subject: &in.Subject, Start: in.Start, End: in.End, TimeZone: in.TimeZone,
		Attendees: attendees, Body: &in.Body, Location: &in.Location,
	}, true)
	if err != nil {
		return nil, err
	}
	if in.OnlineMeeting {
		ev["isOnlineMeeting"] = true
		ev["onlineMeetingProvider"] = "teamsForBusiness"
	}
	var created event
	if err := a.box.Call(ctx, app, box, http.MethodPost, "/events", nil, ev, &created, showIn(tz)); err != nil {
		return nil, err
	}
	return created.compact(), nil
}

func (a *Adapter) updateEvent(ctx context.Context, app microsoft.App, box string, in struct {
	ID        string    `json:"id"`
	Subject   *string   `json:"subject"`
	Start     string    `json:"start"`
	End       string    `json:"end"`
	TimeZone  string    `json:"time_zone"`
	Attendees *[]string `json:"attendees"`
	Body      *string   `json:"body"`
	Location  *string   `json:"location"`
},
) (any, error) {
	eid, err := exchange.ID("event", in.ID)
	if err != nil {
		return nil, err
	}
	ev, tz, err := changes(eventChange{
		Subject: in.Subject, Start: in.Start, End: in.End, TimeZone: in.TimeZone,
		Attendees: in.Attendees, Body: in.Body, Location: in.Location,
	}, false)
	if err != nil {
		return nil, err
	}
	if len(ev) == 0 {
		return nil, errors.New("give at least one field to change")
	}
	var updated event
	if err := a.box.Call(ctx, app, box, http.MethodPatch, "/events/"+eid, nil, ev, &updated, showIn(tz)); err != nil {
		return nil, err
	}
	return updated.compact(), nil
}

func (a *Adapter) deleteEvent(ctx context.Context, app microsoft.App, box string, in struct {
	ID string `json:"id"`
},
) (any, error) {
	eid, err := exchange.ID("event", in.ID)
	if err != nil {
		return nil, err
	}
	if err := a.box.Call(ctx, app, box, http.MethodDelete, "/events/"+eid, nil, nil, nil); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "id": in.ID}, nil
}

// eventChange is what create_event and update_event may set. A nil field
// is left as it is.
type eventChange struct {
	Subject, Body, Location *string
	Start, End, TimeZone    string
	Attendees               *[]string
}

// changes turns fields into Graph's shape, and returns the time zone to
// answer in. A new event needs both its times.
func changes(f eventChange, create bool) (map[string]any, string, error) {
	tz, err := zone(f.TimeZone)
	if err != nil {
		return nil, "", err
	}
	ev := map[string]any{}
	var times [2]time.Time
	for i, s := range []string{f.Start, f.End} {
		if s == "" && !create {
			continue
		}
		if times[i], err = eventTime(s, tz); err != nil {
			return nil, "", err
		}
		ev[[]string{"start", "end"}[i]] = graphTime{DateTime: times[i].Format(localTime), TimeZone: tz}
	}
	if !times[0].IsZero() && !times[1].IsZero() && !times[1].After(times[0]) {
		return nil, "", errors.New("end must be after start")
	}
	if f.Subject != nil {
		ev["subject"] = *f.Subject
	}
	if f.Body != nil && (*f.Body != "" || !create) {
		ev["body"] = exchange.Body{ContentType: "Text", Content: *f.Body}
	}
	if f.Location != nil && (*f.Location != "" || !create) {
		ev["location"] = map[string]string{"displayName": *f.Location}
	}
	if f.Attendees != nil {
		list, err := exchange.Recipients("attendees", *f.Attendees)
		if err != nil {
			return nil, "", err
		}
		type attendee struct {
			exchange.Recipient
			Type string `json:"type"`
		}
		attendees := make([]attendee, len(list))
		for i, r := range list {
			attendees[i] = attendee{Recipient: r, Type: "required"}
		}
		ev["attendees"] = attendees
	}
	return ev, tz, nil
}
