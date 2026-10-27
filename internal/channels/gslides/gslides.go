// Package gslides lets agents find, read and change Google Slides
// presentations as a service account, as a user of the domain through
// domain-wide delegation, or as each person who signs in with their own
// account.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "drive": the base of the Drive API, default https://www.googleapis.com
//   - "slides": the base of the Slides API, default https://slides.googleapis.com
package gslides

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
	"github.com/bespinian/keera-agents/internal/channels/internal/workspace"
)

const (
	scope    = "https://www.googleapis.com/auth/presentations"
	mimeType = "application/vnd.google-apps.presentation"
	// maxText bounds what read returns, so a long deck does not fill the
	// model's context, and fits in the 64 KiB a tool may answer.
	maxText = 50_000
)

// validObjectID is the Slides API's alphabet for a page or shape.
var validObjectID = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_:-]{0,49}$`)

type Adapter struct {
	files workspace.Files
	apitool.Set
	workspace.Accounts
}

const idParam = `"presentation_id":{"type":"string","description":"The presentation's id or link. Optional when the connector is bound to one presentation."}`

func New(opts channels.Options) *Adapter {
	a := &Adapter{
		files: workspace.Files{
			Client:   workspace.NewClient(opts, scope, workspace.DriveMetadata, workspace.DriveFile),
			MimeType: mimeType, Noun: "presentation", Arg: "presentation_id",
			API: web.Endpoint(opts, "slides", "https://slides.googleapis.com") + "/v1/presentations",
		},
	}
	a.Set = apitool.Set{
		a.files.FindTool(),
		{
			Name:        "read",
			Description: "Read a Google Slides presentation: each slide's number, id, title, text and speaker notes.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{` + idParam + `}}`),
			Run:         apitool.Typed(a.read),
		},
		{
			Name:        "replace_text",
			Description: "Replace every occurrence of a text in a presentation, or only on some slides, and say how many were changed.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + idParam + `,
				"find":{"type":"string","description":"The text to find."},
				"replace":{"type":"string","description":"What to put in its place. Empty deletes it."},
				"match_case":{"type":"boolean","description":"Match upper and lower case exactly. True by default."},
				"slide_ids":{"type":"array","items":{"type":"string"},"description":"Only on these slides, by the ids read returns. Empty is every slide."}},
				"required":["find","replace"]}`),
			Write: true,
			Run:   apitool.Typed(a.replaceText),
		},
		{
			Name:        "add_slide",
			Description: "Add a slide with a title and a body, in the presentation's title and body layout.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + idParam + `,
				"title":{"type":"string"},
				"body":{"type":"string","description":"Plain text. Each line becomes a paragraph."},
				"position":{"type":"integer","description":"The slide's number once added, 1 for the first. Empty adds it at the end."}}}`),
			Write: true,
			Run:   apitool.Typed(a.addSlide),
		},
	}
	a.Accounts = workspace.NewAccounts(a.files.Client, "Google Slides API and the Google Drive API")
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.GoogleSlides,
		Label:    "Google Slides",
		Vendor:   "Google",
		Category: "Documents",
		Summary:  "Agents find and read Google Slides presentations, replace their text and add slides.",
		Fields:   workspace.Fields("Google Slides API"),
		Setup: workspace.Setup("Google Slides API and the Google Drive API",
			"the presentations, or a folder holding them, as Editor for agents that write,",
			scope, workspace.DriveMetadata, workspace.DriveFile),
		TargetLabel: "Presentation",
		TargetHelp:  "A presentation's link or id, or * for every presentation the connector can see.",
		AnyTarget:   true,
	}
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	return a.files.Check(ctx, conn)
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	return a.files.Target(ctx, conn, target)
}

type presentation struct {
	PresentationID string `json:"presentationId"`
	Title          string `json:"title"`
	Slides         []page `json:"slides"`
}

type page struct {
	ObjectID        string    `json:"objectId"`
	PageElements    []element `json:"pageElements"`
	SlideProperties struct {
		NotesPage *page `json:"notesPage"`
	} `json:"slideProperties"`
	NotesProperties struct {
		SpeakerNotesObjectID string `json:"speakerNotesObjectId"`
	} `json:"notesProperties"`
}

type element struct {
	ObjectID string `json:"objectId"`
	Shape    *struct {
		Text        *text `json:"text"`
		Placeholder *struct {
			Type string `json:"type"`
		} `json:"placeholder"`
	} `json:"shape"`
	Table *struct {
		TableRows []struct {
			TableCells []struct {
				Text *text `json:"text"`
			} `json:"tableCells"`
		} `json:"tableRows"`
	} `json:"table"`
	ElementGroup *struct {
		Children []element `json:"children"`
	} `json:"elementGroup"`
}

type text struct {
	TextElements []struct {
		TextRun *struct {
			Content string `json:"content"`
		} `json:"textRun"`
	} `json:"textElements"`
}

func (t *text) String() string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	for _, e := range t.TextElements {
		if e.TextRun != nil {
			b.WriteString(e.TextRun.Content)
		}
	}
	// A vertical tab is a line break within a paragraph.
	return strings.TrimSpace(strings.ReplaceAll(b.String(), "\v", "\n"))
}

// collect adds the text of e and what it holds to lines, and a title
// placeholder's to title.
func collect(e element, title *string, lines *[]string) {
	add := func(s string) {
		if s != "" {
			*lines = append(*lines, s)
		}
	}
	switch {
	case e.Shape != nil:
		s := e.Shape.Text.String()
		if p := e.Shape.Placeholder; p != nil && (p.Type == "TITLE" || p.Type == "CENTERED_TITLE") && *title == "" {
			*title = s
			return
		}
		add(s)
	case e.Table != nil:
		for _, row := range e.Table.TableRows {
			var cells []string
			for _, c := range row.TableCells {
				cells = append(cells, strings.Join(strings.Fields(c.Text.String()), " "))
			}
			add("| " + strings.Join(cells, " | ") + " |")
		}
	case e.ElementGroup != nil:
		for _, c := range e.ElementGroup.Children {
			collect(c, title, lines)
		}
	}
}

// notes are a slide's speaker notes: the text of the shape its notes page
// names.
func (p page) notes() string {
	n := p.SlideProperties.NotesPage
	if n == nil {
		return ""
	}
	for _, e := range n.PageElements {
		if e.ObjectID == n.NotesProperties.SpeakerNotesObjectID && e.Shape != nil {
			return e.Shape.Text.String()
		}
	}
	return ""
}

type slideView struct {
	Number int    `json:"number"`
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Text   string `json:"text,omitempty"`
	Notes  string `json:"notes,omitempty"`
}

func (a *Adapter) read(ctx context.Context, conn channels.Connection, target string, in struct {
	PresentationID string `json:"presentation_id"`
},
) (any, error) {
	s, id, err := a.files.Open(conn, target, in.PresentationID)
	if err != nil {
		return nil, err
	}
	var p presentation
	if err := a.files.Do(ctx, s, http.MethodGet, a.files.API+"/"+id, nil, &p); err != nil {
		return nil, fmt.Errorf("reading the presentation: %w", err)
	}
	slides, size := []slideView{}, 0
	out := map[string]any{"id": p.PresentationID, "title": p.Title, "url": slidesURL(p.PresentationID)}
	for i, sl := range p.Slides {
		v := slideView{Number: i + 1, ID: sl.ObjectID, Notes: sl.notes()}
		var lines []string
		for _, e := range sl.PageElements {
			collect(e, &v.Title, &lines)
		}
		v.Text = strings.Join(lines, "\n")
		if size += len(v.Title) + len(v.Text) + len(v.Notes); size > maxText {
			out["truncated"] = fmt.Sprintf("only the first %d of %d slides fit", i, len(p.Slides))
			break
		}
		slides = append(slides, v)
	}
	out["slides"] = slides
	return out, nil
}

func slidesURL(id string) string {
	return "https://docs.google.com/presentation/d/" + id + "/edit"
}

func (a *Adapter) replaceText(ctx context.Context, conn channels.Connection, target string, in struct {
	PresentationID string   `json:"presentation_id"`
	Find           string   `json:"find"`
	Replace        string   `json:"replace"`
	MatchCase      *bool    `json:"match_case"`
	SlideIDs       []string `json:"slide_ids"`
},
) (any, error) {
	if in.Find == "" {
		return nil, apitool.Need("find", in.Find)
	}
	for _, p := range in.SlideIDs {
		if !validObjectID.MatchString(p) {
			return nil, fmt.Errorf("%q is not a slide id; read the presentation for them", web.Short(p, 60))
		}
	}
	s, id, err := a.files.Open(conn, target, in.PresentationID)
	if err != nil {
		return nil, err
	}
	req := map[string]any{
		"containsText": map[string]any{"text": in.Find, "matchCase": in.MatchCase == nil || *in.MatchCase},
		"replaceText":  in.Replace,
	}
	if len(in.SlideIDs) > 0 {
		req["pageObjectIds"] = in.SlideIDs
	}
	r, err := a.files.BatchUpdate(ctx, s, id, map[string]any{"replaceAllText": req})
	if err != nil {
		return nil, fmt.Errorf("replacing the text: %w", err)
	}
	n := 0
	if len(r.Replies) > 0 {
		n = r.Replies[0].ReplaceAllText.OccurrencesChanged
	}
	return map[string]any{"id": id, "occurrences_changed": n}, nil
}

func (a *Adapter) addSlide(ctx context.Context, conn channels.Connection, target string, in struct {
	PresentationID string `json:"presentation_id"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Position       int    `json:"position"`
},
) (any, error) {
	s, id, err := a.files.Open(conn, target, in.PresentationID)
	if err != nil {
		return nil, err
	}
	// Choosing the ids lets the same call fill the layout's placeholders.
	slide, title, body := newObjectID(), newObjectID(), newObjectID()
	create := map[string]any{
		"objectId":             slide,
		"slideLayoutReference": map[string]any{"predefinedLayout": "TITLE_AND_BODY"},
		"placeholderIdMappings": []any{
			map[string]any{"layoutPlaceholder": map[string]any{"type": "TITLE", "index": 0}, "objectId": title},
			map[string]any{"layoutPlaceholder": map[string]any{"type": "BODY", "index": 0}, "objectId": body},
		},
	}
	if in.Position > 0 {
		create["insertionIndex"] = in.Position - 1
	}
	requests := []any{map[string]any{"createSlide": create}}
	// The API refuses to insert empty text.
	for _, f := range [][2]string{{title, in.Title}, {body, in.Body}} {
		if t := strings.TrimSpace(f[1]); t != "" {
			requests = append(requests, map[string]any{"insertText": map[string]any{"objectId": f[0], "text": t, "insertionIndex": 0}})
		}
	}
	if _, err := a.files.BatchUpdate(ctx, s, id, requests...); err != nil {
		return nil, fmt.Errorf("adding the slide: %w", err)
	}
	return map[string]any{"id": id, "slide_id": slide, "url": slidesURL(id) + "#slide=id." + slide}, nil
}

// newObjectID is a fresh id for a page or shape, in the API's alphabet.
func newObjectID() string {
	return "keera_" + strings.ToLower(rand.Text())
}
