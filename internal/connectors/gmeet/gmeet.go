// Package gmeet lets agents read one person's Google Meet meetings: when
// they took place, who came, their recordings, and their transcripts.
//
// A service account takes part in no meetings, so a shared connector acts
// as the binding's person through domain-wide delegation. On a connector
// people sign in to, a binding names "me": the person the task is for.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "meet": the Meet REST API, default https://meet.googleapis.com
package gmeet

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

// scope reads meetings, their participants, recordings and transcripts, but
// changes nothing.
const scope = "https://www.googleapis.com/auth/meetings.space.readonly"

const (
	// maxTranscript bounds one read_transcript answer, so a long meeting
	// comes in parts and does not fill the model's context.
	maxTranscript = 40 << 10
	// maxParticipants bounds the participants named in a transcript. More
	// are shown as "someone".
	maxParticipants = 1000
	// maxShown bounds the participants get_meeting lists.
	maxShown = 100
)

// validCode is a meeting code, such as abc-mnop-xyz. It goes into a filter,
// so it may hold no quotes.
var validCode = regexp.MustCompile(`^[a-z0-9-]{3,128}$`)

type Adapter struct {
	client *workspace.Client
	api    string
	apitool.Set
	workspace.Accounts
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{
		client: workspace.NewClient(opts, scope),
		api:    web.Endpoint(opts, "meet", "https://meet.googleapis.com") + "/v2",
	}
	a.Set = apitool.Set{
		{
			Name: "list_meetings",
			Description: "List the meetings the person took part in, newest first. A meeting has no title: " +
				"match it to a calendar event by its Meet link or start time.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"from":{"type":"string","description":"Only meetings that started at or after this. RFC 3339, or a date."},
				"to":{"type":"string","description":"Only meetings that started before this. RFC 3339, or a date."},
				"meeting":{"type":"string","description":"Only meetings of this Meet link or code, such as https://meet.google.com/abc-mnop-xyz."},
				"limit":{"type":"integer","description":"At most this many, 10 by default, up to 50."}}}`),
			Run: run(a.listMeetings),
		},
		{
			Name:        "get_meeting",
			Description: "Read a meeting: who came and when, and its recordings and transcripts.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"meeting_id":{"type":"string","description":"From list_meetings."}},
				"required":["meeting_id"]}`),
			Run: run(a.getMeeting),
		},
		{
			Name: "read_transcript",
			Description: "Read a meeting's transcript as text, each line with its time from the meeting's start and who spoke. " +
				"A long one comes in parts: pass next to read on. Google keeps it here for 30 days; after that, read its document.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"meeting_id":{"type":"string"},
				"transcript_id":{"type":"string","description":"Needed only when the meeting has several transcripts."},
				"next":{"type":"string","description":"From the last part, to read on."}},
				"required":["meeting_id"]}`),
			Run: run(a.readTranscript),
		},
	}
	a.Accounts = workspace.NewAccounts(a.client, "Google Meet REST API")
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.GoogleMeet,
		Label:    "Google Meet",
		Vendor:   "Google",
		Category: "Email and calendar",
		Summary:  "Agents read a person's Google Meet meetings: who came, recordings and transcripts.",
		Fields:   workspace.DelegationFields("Google Meet REST API"),
		Setup: workspace.DelegationSetup("Google Meet REST API", scope, "read the meetings of anyone in your domain",
			"Bind each agent to the person whose meetings it reads.",
			"In Meet, turn on transcripts or recordings in the meetings agents should read."),
		TargetLabel: "Person",
		TargetHelp: "The email of the person whose meetings agents read, such as ada@acme.com. On a connector people sign in to, " +
			"me: whoever gives the agent work.",
		Mailbox: true,
	}
}

// run opens the binding's person for a tool.
func run[T any](f func(ctx context.Context, s workspace.Settings, in T) (any, error)) apitool.Run {
	return workspace.PersonRun("meetings", f)
}

// get reads path, with query q, as the person.
func (a *Adapter) get(ctx context.Context, s workspace.Settings, path string, q url.Values, out any) error {
	u := a.api + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return a.client.Do(ctx, s, http.MethodGet, u, nil, out)
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	// A service account takes part in no meetings, so there is nothing to
	// ask Meet until a person is bound; Target checks it.
	return a.client.Check(ctx, conn, "Google Meet REST API", func(context.Context, workspace.Settings) error { return nil })
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	return a.client.PersonTarget(ctx, conn, target, "meetings", func(ctx context.Context, s workspace.Settings) error {
		err := a.get(ctx, s, "/conferenceRecords", url.Values{"pageSize": {"1"}}, &struct{}{})
		if web.Status(err) == http.StatusForbidden {
			return fmt.Errorf("%w; check that the Google Meet REST API is enabled, and that %s may use Meet", err, s.Who())
		}
		return err
	})
}

// lastID is the id a resource name ends with, such as abc in
// conferenceRecords/abc, or "" if it is not one.
func lastID(name string) string {
	name = strings.TrimSpace(name)
	name = name[strings.LastIndexByte(name, '/')+1:]
	if !workspace.ValidID.MatchString(name) {
		return ""
	}
	return name
}

// id reads an id from the model, or the resource name it ends.
func id(what, s string) (string, error) {
	if v := lastID(s); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%q is not a %s id", web.Short(strings.TrimSpace(s), 80), what)
}

// meetingCode reads a meeting code, or a Meet link as people paste it, with
// or without https://.
func meetingCode(s string) (string, error) {
	code, _, _ := strings.Cut(strings.TrimSpace(s), "?")
	code = strings.ToLower(code[strings.LastIndexByte(code, '/')+1:])
	if !validCode.MatchString(code) {
		return "", fmt.Errorf("%q is not a Meet link or meeting code, such as abc-mnop-xyz", web.Short(s, 80))
	}
	return code, nil
}

// record is a meeting as the API returns it: one conference in a space.
type record struct {
	Name      string     `json:"name"`
	StartTime time.Time  `json:"startTime"`
	EndTime   *time.Time `json:"endTime"`
	Space     string     `json:"space"`
}

type space struct {
	MeetingURI  string `json:"meetingUri"`
	MeetingCode string `json:"meetingCode"`
}

// meetingView is a meeting for the model.
type meetingView struct {
	ID      string     `json:"id"`
	Start   time.Time  `json:"start"`
	End     *time.Time `json:"end,omitempty"`
	Ongoing bool       `json:"ongoing,omitempty"`
	Code    string     `json:"meeting_code,omitempty"`
	Link    string     `json:"link,omitempty"`
}

func (r record) view(sp space) meetingView {
	v := meetingView{Start: r.StartTime, End: r.EndTime, Ongoing: r.EndTime == nil, Code: sp.MeetingCode, Link: sp.MeetingURI}
	v.ID = lastID(r.Name)
	return v
}

// space reads where a meeting took place, for its code and link. Not every
// person may read every space, so a space that cannot be read is left out.
func (a *Adapter) space(ctx context.Context, s workspace.Settings, name string) space {
	sid := lastID(name)
	if sid == "" {
		return space{}
	}
	var sp space
	if err := a.get(ctx, s, "/spaces/"+sid, nil, &sp); err != nil {
		return space{}
	}
	return sp
}

func (a *Adapter) listMeetings(ctx context.Context, s workspace.Settings, in struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Meeting string `json:"meeting"`
	Limit   int    `json:"limit"`
},
) (any, error) {
	var filter []string
	if in.Meeting != "" {
		code, err := meetingCode(in.Meeting)
		if err != nil {
			return nil, err
		}
		filter = append(filter, `space.meeting_code = "`+code+`"`)
	}
	for _, b := range []struct{ arg, op string }{{in.From, ">="}, {in.To, "<"}} {
		if b.arg == "" {
			continue
		}
		t, err := workspace.ParseTime(b.arg)
		if err != nil {
			return nil, err
		}
		filter = append(filter, `start_time `+b.op+` "`+t.UTC().Format(time.RFC3339)+`"`)
	}
	q := url.Values{"pageSize": {strconv.Itoa(apitool.Limit(in.Limit, 10, 50))}}
	if len(filter) > 0 {
		q.Set("filter", strings.Join(filter, " AND "))
	}
	var r struct {
		ConferenceRecords []record `json:"conferenceRecords"`
		NextPageToken     string   `json:"nextPageToken"`
	}
	if err := a.get(ctx, s, "/conferenceRecords", q, &r); err != nil {
		return nil, fmt.Errorf("listing meetings: %w", err)
	}
	spaces := map[string]space{}
	meetings := []meetingView{}
	for _, rec := range r.ConferenceRecords {
		sp, ok := spaces[rec.Space]
		if !ok {
			sp = a.space(ctx, s, rec.Space)
			spaces[rec.Space] = sp
		}
		meetings = append(meetings, rec.view(sp))
	}
	out := map[string]any{"meetings": meetings}
	if r.NextPageToken != "" {
		out["more"] = "there are more meetings; ask for a shorter range or a higher limit"
	}
	return out, nil
}

// meeting reads a meeting by the model's id.
func (a *Adapter) meeting(ctx context.Context, s workspace.Settings, meetingID string) (string, record, error) {
	mid, err := id("meeting", meetingID)
	if err != nil {
		return "", record{}, err
	}
	var rec record
	err = a.get(ctx, s, "/conferenceRecords/"+mid, nil, &rec)
	if web.Status(err) == http.StatusNotFound {
		return "", record{}, fmt.Errorf("there is no meeting %s that %s took part in; Google keeps meetings for 30 days", mid, s.Who())
	}
	if err != nil {
		return "", record{}, fmt.Errorf("reading the meeting: %w", err)
	}
	return mid, rec, nil
}

// participant is someone who joined a meeting: signed in, anonymous or by
// phone.
type participant struct {
	Name          string     `json:"name"`
	EarliestTime  time.Time  `json:"earliestStartTime"`
	LatestEnd     *time.Time `json:"latestEndTime"`
	SignedinUser  *meetUser  `json:"signedinUser"`
	AnonymousUser *meetUser  `json:"anonymousUser"`
	PhoneUser     *meetUser  `json:"phoneUser"`
}

type meetUser struct {
	DisplayName string `json:"displayName"`
}

// who is a participant's name and how they joined.
func (p participant) who() (name, kind string) {
	switch {
	case p.SignedinUser != nil:
		return p.SignedinUser.DisplayName, ""
	case p.AnonymousUser != nil:
		return p.AnonymousUser.DisplayName, "guest"
	case p.PhoneUser != nil:
		return p.PhoneUser.DisplayName, "phone"
	}
	return "someone", ""
}

// artifact is a recording or transcript, as the API returns it.
type artifact struct {
	Name             string     `json:"name"`
	State            string     `json:"state"`
	StartTime        time.Time  `json:"startTime"`
	EndTime          *time.Time `json:"endTime"`
	DriveDestination *struct {
		File      string `json:"file"`
		ExportURI string `json:"exportUri"`
	} `json:"driveDestination"`
	DocsDestination *struct {
		Document  string `json:"document"`
		ExportURI string `json:"exportUri"`
	} `json:"docsDestination"`
}

// artifactView is a recording or transcript for the model. File is its
// Drive file, or its Google Doc for a transcript.
type artifactView struct {
	ID    string     `json:"id"`
	State string     `json:"state"`
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
	File  string     `json:"file_id,omitempty"`
	URL   string     `json:"url,omitempty"`
}

// states say in words whether a recording's or transcript's file is ready.
var states = map[string]string{
	"STARTED":        "in progress",
	"ENDED":          "being saved",
	"FILE_GENERATED": "ready",
}

func (r artifact) view() artifactView {
	v := artifactView{State: states[r.State], Start: r.StartTime, End: r.EndTime}
	if v.State == "" {
		v.State = strings.ToLower(r.State)
	}
	v.ID = lastID(r.Name)
	switch {
	case r.DriveDestination != nil:
		v.File, v.URL = r.DriveDestination.File, r.DriveDestination.ExportURI
	case r.DocsDestination != nil:
		v.File, v.URL = r.DocsDestination.Document, r.DocsDestination.ExportURI
	}
	return v
}

// artifacts lists a meeting's recordings or transcripts.
func (a *Adapter) artifacts(ctx context.Context, s workspace.Settings, mid, kind string) ([]artifact, error) {
	var r struct {
		Recordings  []artifact `json:"recordings"`
		Transcripts []artifact `json:"transcripts"`
	}
	if err := a.get(ctx, s, "/conferenceRecords/"+mid+"/"+kind, url.Values{"pageSize": {"100"}}, &r); err != nil {
		return nil, fmt.Errorf("listing the meeting's %s: %w", kind, err)
	}
	return append(r.Recordings, r.Transcripts...), nil
}

func (a *Adapter) getMeeting(ctx context.Context, s workspace.Settings, in struct {
	MeetingID string `json:"meeting_id"`
},
) (any, error) {
	mid, rec, err := a.meeting(ctx, s, in.MeetingID)
	if err != nil {
		return nil, err
	}
	list, more, err := a.participants(ctx, s, mid, maxShown)
	if err != nil {
		return nil, err
	}
	type participantView struct {
		Name   string     `json:"name"`
		How    string     `json:"joined_as,omitempty"`
		Joined time.Time  `json:"joined"`
		Left   *time.Time `json:"left,omitempty"`
	}
	people := []participantView{}
	for _, p := range list {
		name, how := p.who()
		people = append(people, participantView{Name: name, How: how, Joined: p.EarliestTime, Left: p.LatestEnd})
	}
	out := map[string]any{"meeting": rec.view(a.space(ctx, s, rec.Space)), "participants": people}
	for _, kind := range []string{"recordings", "transcripts"} {
		list, err := a.artifacts(ctx, s, mid, kind)
		if err != nil {
			return nil, err
		}
		views := []artifactView{}
		for _, r := range list {
			views = append(views, r.view())
		}
		out[kind] = views
	}
	if more {
		out["more_participants"] = true
	}
	return out, nil
}

// transcript picks the transcript to read: the one given, or else the
// meeting's only one.
func (a *Adapter) transcript(ctx context.Context, s workspace.Settings, mid, given string) (artifact, error) {
	if given != "" {
		tid, err := id("transcript", given)
		if err != nil {
			return artifact{}, err
		}
		var t artifact
		err = a.get(ctx, s, "/conferenceRecords/"+mid+"/transcripts/"+tid, nil, &t)
		if web.Status(err) == http.StatusNotFound {
			return artifact{}, fmt.Errorf("the meeting has no transcript %s; get_meeting lists them", tid)
		}
		return t, err
	}
	list, err := a.artifacts(ctx, s, mid, "transcripts")
	if err != nil {
		return artifact{}, err
	}
	switch len(list) {
	case 0:
		return artifact{}, errors.New("the meeting has no transcript: nobody turned transcripts on in it")
	case 1:
		return list[0], nil
	}
	ids := make([]string, len(list))
	for i, t := range list {
		ids[i] = lastID(t.Name)
	}
	return artifact{}, fmt.Errorf("the meeting has %d transcripts; give transcript_id, one of %s", len(list), strings.Join(ids, ", "))
}

// participants lists who came to a meeting, up to limit. more says there
// were others.
func (a *Adapter) participants(ctx context.Context, s workspace.Settings, mid string, limit int) (list []participant, more bool, err error) {
	q := url.Values{"pageSize": {strconv.Itoa(min(limit, 250))}}
	for {
		var r struct {
			Participants  []participant `json:"participants"`
			NextPageToken string        `json:"nextPageToken"`
		}
		if err := a.get(ctx, s, "/conferenceRecords/"+mid+"/participants", q, &r); err != nil {
			return nil, false, fmt.Errorf("listing who came: %w", err)
		}
		list = append(list, r.Participants...)
		if r.NextPageToken == "" || len(r.Participants) == 0 {
			return list, false, nil
		}
		if len(list) >= limit {
			return list, true, nil
		}
		q.Set("pageToken", r.NextPageToken)
	}
}

func (a *Adapter) readTranscript(ctx context.Context, s workspace.Settings, in struct {
	MeetingID    string `json:"meeting_id"`
	TranscriptID string `json:"transcript_id"`
	Next         string `json:"next"`
},
) (any, error) {
	if len(in.Next) > 4096 {
		return nil, errors.New("next is not one read_transcript gave")
	}
	mid, rec, err := a.meeting(ctx, s, in.MeetingID)
	if err != nil {
		return nil, err
	}
	t, err := a.transcript(ctx, s, mid, in.TranscriptID)
	if err != nil {
		return nil, fmt.Errorf("reading the transcript: %w", err)
	}
	tv := t.view()
	people, _, err := a.participants(ctx, s, mid, maxParticipants)
	if err != nil {
		return nil, err
	}
	// Transcript entries name only the participant's resource.
	names := map[string]string{}
	for _, p := range people {
		names[p.Name], _ = p.who()
	}
	var text strings.Builder
	q := url.Values{"pageSize": {"100"}}
	if in.Next != "" {
		q.Set("pageToken", in.Next)
	}
	next := ""
	for {
		var r struct {
			Entries []struct {
				Participant string    `json:"participant"`
				Text        string    `json:"text"`
				StartTime   time.Time `json:"startTime"`
			} `json:"transcriptEntries"`
			NextPageToken string `json:"nextPageToken"`
		}
		err := a.get(ctx, s, "/conferenceRecords/"+mid+"/transcripts/"+tv.ID+"/entries", q, &r)
		if web.Status(err) == http.StatusNotFound && tv.URL != "" {
			return nil, fmt.Errorf("%w; Google keeps transcripts here for 30 days, so read its document instead: %s", err, tv.URL)
		}
		if err != nil {
			return nil, fmt.Errorf("reading the transcript: %w", err)
		}
		for _, e := range r.Entries {
			name := names[e.Participant]
			if name == "" {
				name = "someone"
			}
			fmt.Fprintf(&text, "[%s] %s: %s\n", offset(e.StartTime.Sub(rec.StartTime)), name, strings.TrimSpace(e.Text))
		}
		next = r.NextPageToken
		if next == "" || len(r.Entries) == 0 || text.Len() >= maxTranscript {
			break
		}
		q.Set("pageToken", next)
	}
	out := map[string]any{
		"meeting_id": mid, "transcript_id": tv.ID, "state": tv.State, "meeting_start": rec.StartTime,
		"text": text.String(),
	}
	if tv.URL != "" {
		out["document"] = tv.URL
	}
	if next != "" {
		out["next"] = next
	}
	if tv.State != "ready" {
		out["note"] = "the transcript is not finished, so it may still grow"
	}
	return out, nil
}

// offset is a time since the meeting began, such as 01:02:03.
func offset(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}
