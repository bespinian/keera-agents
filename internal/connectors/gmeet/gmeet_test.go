package gmeet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace/workspacetest"
)

const records = "/v2/conferenceRecords"

// setup fakes one meeting, rec1, with two people, a recording and a
// transcript of three entries over two pages. It records every list
// query's filter.
func setup(t *testing.T) (*workspacetest.Fake, *Adapter, *[]string) {
	f := workspacetest.New(t)
	var filters []string
	rec := `{"name":"conferenceRecords/rec1","startTime":"2026-11-03T09:00:00Z","endTime":"2026-11-03T09:30:00Z","space":"spaces/sp1"}`
	f.Handle("GET "+records, func(w http.ResponseWriter, r *http.Request) {
		filters = append(filters, r.URL.Query().Get("filter"))
		_, _ = w.Write([]byte(`{"conferenceRecords":[` + rec + `,
			{"name":"conferenceRecords/rec2","startTime":"2026-11-02T09:00:00Z","space":"spaces/sp1"}],"nextPageToken":"p2"}`))
	})
	f.Handle("GET "+records+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "rec1" {
			workspacetest.Error(w, http.StatusNotFound, "Requested entity was not found.")
			return
		}
		_, _ = w.Write([]byte(rec))
	})
	f.Handle("GET /v2/spaces/sp1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"spaces/sp1","meetingUri":"https://meet.google.com/abc-mnop-xyz","meetingCode":"abc-mnop-xyz"}`))
	})
	f.Handle("GET "+records+"/rec1/participants", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"participants":[
			{"name":"conferenceRecords/rec1/participants/p1","earliestStartTime":"2026-11-03T09:00:00Z","latestEndTime":"2026-11-03T09:30:00Z",
			 "signedinUser":{"user":"users/1","displayName":"Ada Lovelace"}},
			{"name":"conferenceRecords/rec1/participants/p2","earliestStartTime":"2026-11-03T09:05:00Z",
			 "phoneUser":{"displayName":"+41 79 *** ** 12"}}]}`))
	})
	f.Handle("GET "+records+"/rec1/recordings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"recordings":[{"name":"conferenceRecords/rec1/recordings/r1","state":"FILE_GENERATED",
			"startTime":"2026-11-03T09:00:00Z","endTime":"2026-11-03T09:30:00Z",
			"driveDestination":{"file":"vid1","exportUri":"https://drive.google.com/file/d/vid1/view"}}]}`))
	})
	transcript := `{"name":"conferenceRecords/rec1/transcripts/t1","state":"FILE_GENERATED","startTime":"2026-11-03T09:00:00Z",
		"docsDestination":{"document":"doc1","exportUri":"https://docs.google.com/document/d/doc1/edit"}}`
	f.Handle("GET "+records+"/rec1/transcripts", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"transcripts":[` + transcript + `]}`))
	})
	f.Handle("GET "+records+"/rec1/transcripts/t1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(transcript))
	})
	f.Handle("GET "+records+"/rec1/transcripts/t1/entries", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"transcriptEntries":[
				{"participant":"conferenceRecords/rec1/participants/p1","text":"Good morning. ","startTime":"2026-11-03T09:00:05Z"},
				{"participant":"conferenceRecords/rec1/participants/p2","text":"Hi Ada.","startTime":"2026-11-03T09:05:10Z"}],
				"nextPageToken":"e2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"transcriptEntries":[
			{"participant":"conferenceRecords/rec1/participants/p9","text":"Bye.","startTime":"2026-11-03T10:01:02Z"}]}`))
	})
	return f, New(f.Options()), &filters
}

func shared(f *workspacetest.Fake) connectors.Connection {
	return f.Conn(connectors.GoogleMeet, "")
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

func TestTarget(t *testing.T) {
	f, a, _ := setup(t)
	ctx := context.Background()
	if err := a.Check(ctx, shared(f)); err != nil {
		t.Fatal(err)
	}
	if got, err := a.Target(ctx, shared(f), " Ada@Acme.com "); err != nil || got != "ada@acme.com" {
		t.Errorf("Target = %q, %v", got, err)
	}
	if f.Scope != scope {
		t.Errorf("scope = %q", f.Scope)
	}
	if _, err := a.Target(ctx, shared(f), workspacetest.Outsider); err == nil || !strings.Contains(err.Error(), "domain-wide delegation") {
		t.Errorf("a person without delegation: %v", err)
	}
	user := f.UserConn(connectors.GoogleMeet, "ws-token")
	if got, err := a.Target(ctx, user, "me"); err != nil || got != "me" {
		t.Errorf("Target(me) = %q, %v", got, err)
	}
}

func TestListMeetings(t *testing.T) {
	f, a, filters := setup(t)
	out, err := call(t, a, shared(f), "ada@acme.com", "list_meetings", `{}`)
	if err != nil || !strings.Contains(out, `"id":"rec1"`) || !strings.Contains(out, `"meeting_code":"abc-mnop-xyz"`) ||
		!strings.Contains(out, `"id":"rec2","start":"2026-11-02T09:00:00Z","ongoing":true`) || !strings.Contains(out, `"more"`) {
		t.Errorf("list_meetings = %s, %v", out, err)
	}
	if _, err := call(t, a, shared(f), "ada@acme.com", "list_meetings",
		`{"meeting":"https://meet.google.com/ABC-mnop-xyz?authuser=0","from":"2026-11-01","to":"2026-11-04T10:00:00+01:00"}`); err != nil {
		t.Fatal(err)
	}
	want := `space.meeting_code = "abc-mnop-xyz" AND start_time >= "2026-11-01T00:00:00Z" AND start_time < "2026-11-04T09:00:00Z"`
	if got := (*filters)[1]; got != want {
		t.Errorf("filter = %s", got)
	}
	for _, in := range []string{"meet.google.com/abc-mnop-xyz", " abc-mnop-xyz "} {
		if got, err := meetingCode(in); err != nil || got != "abc-mnop-xyz" {
			t.Errorf("meetingCode(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{`{"meeting":"abc\" OR start_time > \"x"}`, `{"from":"yesterday"}`} {
		if _, err := call(t, a, shared(f), "ada@acme.com", "list_meetings", bad); err == nil {
			t.Errorf("%s passed", bad)
		}
	}
}

func TestGetMeeting(t *testing.T) {
	f, a, _ := setup(t)
	out, err := call(t, a, shared(f), "ada@acme.com", "get_meeting", `{"meeting_id":"conferenceRecords/rec1"}`)
	for _, want := range []string{
		`"name":"Ada Lovelace","joined":"2026-11-03T09:00:00Z","left":"2026-11-03T09:30:00Z"`,
		`"joined_as":"phone"`,
		`"recordings":[{"id":"r1","state":"ready"`, `"file_id":"vid1"`,
		`"transcripts":[{"id":"t1","state":"ready"`, `"file_id":"doc1"`,
		`"link":"https://meet.google.com/abc-mnop-xyz"`,
	} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("get_meeting lacks %s: %s, %v", want, out, err)
		}
	}
	if _, err := call(t, a, shared(f), "ada@acme.com", "get_meeting", `{"meeting_id":"rec9"}`); err == nil ||
		!strings.Contains(err.Error(), "30 days") {
		t.Errorf("a missing meeting: %v", err)
	}
	if _, err := call(t, a, shared(f), "ada@acme.com", "get_meeting", `{"meeting_id":"rec1?x=1"}`); err == nil {
		t.Error("a bad id passed")
	}
}

func TestReadTranscript(t *testing.T) {
	f, a, _ := setup(t)
	out, err := call(t, a, shared(f), "ada@acme.com", "read_transcript", `{"meeting_id":"rec1"}`)
	var r struct {
		Text, Next, Document string
	}
	_ = json.Unmarshal([]byte(out), &r)
	want := "[00:00:05] Ada Lovelace: Good morning.\n[00:05:10] +41 79 *** ** 12: Hi Ada.\n[01:01:02] someone: Bye.\n"
	if err != nil || r.Text != want || r.Next != "" || r.Document != "https://docs.google.com/document/d/doc1/edit" {
		t.Errorf("read_transcript = %s, %v", out, err)
	}
	if _, err := call(t, a, shared(f), "ada@acme.com", "read_transcript", `{"meeting_id":"rec1","transcript_id":"t1","next":"e2"}`); err != nil {
		t.Error(err)
	}
}

// A long transcript comes in parts.
func TestReadTranscriptInParts(t *testing.T) {
	f := workspacetest.New(t)
	f.Handle("GET "+records+"/rec1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"conferenceRecords/rec1","startTime":"2026-11-03T09:00:00Z"}`))
	})
	f.Handle("GET "+records+"/rec1/participants", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	f.Handle("GET "+records+"/rec1/transcripts", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"transcripts":[{"name":"conferenceRecords/rec1/transcripts/t1","state":"STARTED"}]}`))
	})
	pages := 0
	f.Handle("GET "+records+"/rec1/transcripts/t1/entries", func(w http.ResponseWriter, r *http.Request) {
		pages++
		line := fmt.Sprintf(`{"text":%q,"startTime":"2026-11-03T09:00:00Z"}`, strings.Repeat("x", maxTranscript/2))
		_, _ = w.Write([]byte(`{"transcriptEntries":[` + line + `],"nextPageToken":"more"}`))
	})
	a := New(f.Options())
	out, err := call(t, a, f.Conn(connectors.GoogleMeet, ""), "ada@acme.com", "read_transcript", `{"meeting_id":"rec1"}`)
	if err != nil || pages != 2 || !strings.Contains(out, `"next":"more"`) || !strings.Contains(out, `"note":"the transcript is not finished`) {
		t.Errorf("read_transcript read %d pages: %.200s, %v", pages, out, err)
	}
}

func TestPerUser(t *testing.T) {
	f, a, _ := setup(t)
	user := f.UserConn(connectors.GoogleMeet, "ws-token")
	if out, err := call(t, a, user, "me", "get_meeting", `{"meeting_id":"rec1"}`); err != nil || !strings.Contains(out, "Ada Lovelace") {
		t.Errorf("get_meeting = %s, %v", out, err)
	}
}
