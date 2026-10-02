package outlook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/microsoft"
)

const (
	eventFields = "id,subject,start,end,isAllDay,isCancelled,location,organizer,attendees,onlineMeeting,bodyPreview"
	// maxRange bounds list_events, so a year of meetings does not fill the
	// model's context.
	maxRange  = 93 * 24 * time.Hour
	localTime = "2006-01-02T15:04:05"
)

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
		return "", fmt.Errorf("%q is not a time zone", s)
	}
	return s, nil
}

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
	return time.Time{}, false, fmt.Errorf("%q is not a time; write 2026-11-05T14:00 or 2026-11-05", s)
}

type graphTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type event struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	Start       graphTime `json:"start"`
	End         graphTime `json:"end"`
	IsAllDay    bool      `json:"isAllDay"`
	IsCancelled bool      `json:"isCancelled"`
	Location    struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Organizer *recipient `json:"organizer"`
	Attendees []struct {
		recipient
		Status struct {
			Response string `json:"response"`
		} `json:"status"`
	} `json:"attendees"`
	OnlineMeeting *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
	BodyPreview string `json:"bodyPreview"`
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
}

func (e event) compact() Event {
	// Graph writes seven digits of fractions nobody needs.
	short := func(s string) string { s, _, _ = strings.Cut(s, "."); return s }
	out := Event{
		ID: e.ID, Subject: e.Subject, Start: short(e.Start.DateTime), End: short(e.End.DateTime),
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

func (a *Adapter) listEvents(ctx context.Context, app microsoft.App, box string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Start    string `json:"start"`
		End      string `json:"end"`
		TimeZone string `json:"time_zone"`
		Limit    int    `json:"limit"`
	}](args)
	if err != nil {
		return nil, err
	}
	tz, err := zone(in.TimeZone)
	if err != nil {
		return nil, err
	}
	loc := location(tz)
	start := time.Now()
	if in.Start != "" {
		if start, _, err = parseTime(in.Start, loc); err != nil {
			return nil, err
		}
	}
	end := start.Add(7 * 24 * time.Hour)
	if in.End != "" {
		if end, _, err = parseTime(in.End, loc); err != nil {
			return nil, err
		}
	}
	switch {
	case !end.After(start):
		return nil, errors.New("end must be after start")
	case end.Sub(start) > maxRange:
		return nil, errors.New("ask for at most 93 days at a time")
	}
	q := map[string]string{
		"startDateTime": start.UTC().Format(time.RFC3339),
		"endDateTime":   end.UTC().Format(time.RFC3339),
		"$select":       eventFields,
		"$orderby":      "start/dateTime",
		"$top":          strconv.Itoa(apitool.Limit(in.Limit, 50, 100)),
	}
	var r struct {
		Value []event `json:"value"`
	}
	if err := a.call(ctx, app, box, http.MethodGet, "/calendarView", q, nil, &r, `outlook.timezone="`+tz+`"`); err != nil {
		return nil, err
	}
	out := make([]Event, len(r.Value))
	for i, e := range r.Value {
		out[i] = e.compact()
	}
	return out, nil
}

func (a *Adapter) createEvent(ctx context.Context, app microsoft.App, box string, args json.RawMessage) (any, error) {
	in, err := apitool.Decode[struct {
		Subject       string   `json:"subject"`
		Start         string   `json:"start"`
		End           string   `json:"end"`
		TimeZone      string   `json:"time_zone"`
		Attendees     []string `json:"attendees"`
		Body          string   `json:"body"`
		Location      string   `json:"location"`
		OnlineMeeting bool     `json:"online_meeting"`
	}](args)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("subject", in.Subject); err != nil {
		return nil, err
	}
	tz, err := zone(in.TimeZone)
	if err != nil {
		return nil, err
	}
	loc := location(tz)
	var times [2]time.Time
	for i, s := range []string{in.Start, in.End} {
		t, offset, err := parseTime(s, loc)
		if err != nil {
			return nil, err
		}
		if offset {
			// Graph takes local times, so an absolute one is turned into
			// the event's zone, which must be one Go knows.
			if loc == time.UTC && tz != "UTC" {
				return nil, fmt.Errorf("give times without an offset, or an IANA time zone such as Europe/Zurich instead of %q", tz)
			}
			t = t.In(loc)
		}
		times[i] = t
	}
	if !times[1].After(times[0]) {
		return nil, errors.New("end must be after start")
	}
	attendees, err := recipients("attendees", in.Attendees)
	if err != nil {
		return nil, err
	}
	type attendee struct {
		recipient
		Type string `json:"type"`
	}
	ev := map[string]any{
		"subject": in.Subject,
		"start":   graphTime{DateTime: times[0].Format(localTime), TimeZone: tz},
		"end":     graphTime{DateTime: times[1].Format(localTime), TimeZone: tz},
	}
	if len(attendees) > 0 {
		list := make([]attendee, len(attendees))
		for i, r := range attendees {
			list[i] = attendee{recipient: r, Type: "required"}
		}
		ev["attendees"] = list
	}
	if in.Body != "" {
		ev["body"] = body{ContentType: "Text", Content: in.Body}
	}
	if in.Location != "" {
		ev["location"] = map[string]string{"displayName": in.Location}
	}
	if in.OnlineMeeting {
		ev["isOnlineMeeting"] = true
		ev["onlineMeetingProvider"] = "teamsForBusiness"
	}
	var created event
	if err := a.call(ctx, app, box, http.MethodPost, "/events", nil, ev, &created, `outlook.timezone="`+tz+`"`); err != nil {
		return nil, err
	}
	return created.compact(), nil
}
