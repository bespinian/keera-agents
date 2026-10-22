// Package gdocs lets agents find, read and write Google Docs as a service
// account, as a user of the domain through domain-wide delegation, or as
// each person who signs in with their own account.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "drive": the base of the Drive API, default https://www.googleapis.com
//   - "docs": the base of the Docs API, default https://docs.googleapis.com
package gdocs

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
	"github.com/bespinian/keera-agents/internal/channels/internal/workspace"
)

const (
	scope    = "https://www.googleapis.com/auth/documents"
	mimeType = "application/vnd.google-apps.document"
	// maxText bounds what read returns, so a long document does not fill
	// the model's context, and fits in the 64 KiB a tool may answer.
	maxText = 50_000
)

type Adapter struct {
	files workspace.Files
	apitool.Set
	workspace.Accounts
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{
		files: workspace.Files{
			Client:   workspace.NewClient(opts, scope, workspace.DriveMetadata, workspace.DriveFile),
			MimeType: mimeType, Noun: "document", Arg: "document_id",
			API: web.Endpoint(opts, "docs", "https://docs.googleapis.com") + "/v1/documents",
		},
	}
	a.Set = apitool.Set{
		a.files.FindTool(),
		{
			Name: "read",
			Description: "Read a Google Doc as text: headings start with #, list items with - or 1., " +
				"and table rows are written | cell | cell |.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"document_id":{"type":"string","description":"The document's id or link. Optional when the connector is bound to one document."}}}`),
			Run: apitool.Typed(a.read),
		},
		{
			Name:        "append",
			Description: "Add text at the end of a Google Doc, as new paragraphs.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"document_id":{"type":"string","description":"The document's id or link. Optional when the connector is bound to one document."},
				"text":{"type":"string","description":"Plain text. Each line becomes a paragraph."}},
				"required":["text"]}`),
			Write: true,
			Run:   apitool.Typed(a.append),
		},
		{
			Name:        "replace_text",
			Description: "Replace every occurrence of a text in a Google Doc, and say how many were changed.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"document_id":{"type":"string","description":"The document's id or link. Optional when the connector is bound to one document."},
				"find":{"type":"string","description":"The text to find."},
				"replace":{"type":"string","description":"What to put in its place. Empty deletes it."},
				"match_case":{"type":"boolean","description":"Match upper and lower case exactly. True by default."}},
				"required":["find","replace"]}`),
			Write: true,
			Run:   apitool.Typed(a.replaceText),
		},
		{
			Name: "create",
			Description: "Create a Google Doc. Only for a connector bound to every document. " +
				"Give a folder unless the connector acts as a user: a service account has no storage of its own.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"title":{"type":"string"},
				"text":{"type":"string","description":"The document's first text, plain. Optional."},
				"folder_id":{"type":"string","description":"The id or link of the Drive folder to create it in."}},
				"required":["title"]}`),
			Write: true,
			Run:   apitool.Typed(a.create),
		},
	}
	a.Accounts = workspace.NewAccounts(a.files.Client, "Google Docs API and the Google Drive API")
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.GoogleDocs,
		Label:    "Google Docs",
		Vendor:   "Google",
		Category: "Documents",
		Summary:  "Agents find, read and write Google Docs: add text, replace text and create documents.",
		Fields:   workspace.Fields("Google Docs API"),
		Setup: workspace.Setup("Google Docs API and the Google Drive API",
			"the documents, or a folder holding them, as Editor for agents that write,",
			scope, workspace.DriveMetadata, workspace.DriveFile),
		TargetLabel: "Document",
		TargetHelp:  "A document's link or id, or * for every document the connector can see.",
		AnyTarget:   true,
	}
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	return a.files.Check(ctx, conn, "Google Drive API", nil)
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	return a.files.Target(ctx, conn, target)
}

type docView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (a *Adapter) read(ctx context.Context, conn channels.Connection, target string, in struct {
	DocumentID string `json:"document_id"`
},
) (any, error) {
	s, id, err := a.files.Open(conn, target, in.DocumentID)
	if err != nil {
		return nil, err
	}
	var d document
	if err := a.files.Do(ctx, s, http.MethodGet, a.files.API+"/"+id+"?includeTabsContent=true", nil, &d); err != nil {
		return nil, fmt.Errorf("reading the document: %w", err)
	}
	full := d.text()
	text := web.Cut(full, maxText, "")
	return docView{ID: d.DocumentID, Title: d.Title, URL: docURL(d.DocumentID), Text: text, Truncated: len(text) < len(full)}, nil
}

func docURL(id string) string {
	return "https://docs.google.com/document/d/" + id + "/edit"
}

func (a *Adapter) append(ctx context.Context, conn channels.Connection, target string, in struct {
	DocumentID string `json:"document_id"`
	Text       string `json:"text"`
},
) (any, error) {
	if err := apitool.Need("text", in.Text); err != nil {
		return nil, err
	}
	s, id, err := a.files.Open(conn, target, in.DocumentID)
	if err != nil {
		return nil, err
	}
	// The end of the body is inside its last paragraph, so a newline first
	// starts a new one.
	_, err = a.files.BatchUpdate(ctx, s, id, map[string]any{"insertText": map[string]any{
		"text": "\n" + strings.TrimRight(in.Text, "\n"), "endOfSegmentLocation": map[string]any{},
	}})
	if err != nil {
		return nil, fmt.Errorf("adding the text: %w", err)
	}
	return map[string]any{"id": id, "url": docURL(id), "appended": true}, nil
}

func (a *Adapter) replaceText(ctx context.Context, conn channels.Connection, target string, in struct {
	DocumentID string `json:"document_id"`
	Find       string `json:"find"`
	Replace    string `json:"replace"`
	MatchCase  *bool  `json:"match_case"`
},
) (any, error) {
	if in.Find == "" {
		return nil, apitool.Need("find", in.Find)
	}
	s, id, err := a.files.Open(conn, target, in.DocumentID)
	if err != nil {
		return nil, err
	}
	r, err := a.files.BatchUpdate(ctx, s, id, map[string]any{"replaceAllText": map[string]any{
		"containsText": map[string]any{"text": in.Find, "matchCase": in.MatchCase == nil || *in.MatchCase},
		"replaceText":  in.Replace,
	}})
	if err != nil {
		return nil, fmt.Errorf("replacing the text: %w", err)
	}
	n := 0
	if len(r.Replies) > 0 {
		n = r.Replies[0].ReplaceAllText.OccurrencesChanged
	}
	return map[string]any{"id": id, "occurrences_changed": n}, nil
}

func (a *Adapter) create(ctx context.Context, conn channels.Connection, target string, in struct {
	Title    string `json:"title"`
	Text     string `json:"text"`
	FolderID string `json:"folder_id"`
},
) (any, error) {
	s, err := workspace.SettingsOf(conn)
	if err != nil {
		return nil, err
	}
	f, err := a.files.Create(ctx, s, target, in.Title, in.FolderID)
	if err != nil {
		return nil, err
	}
	if in.Text != "" {
		_, err := a.files.BatchUpdate(ctx, s, f.ID, map[string]any{"insertText": map[string]any{
			"text": in.Text, "location": map[string]any{"index": 1},
		}})
		if err != nil {
			return nil, fmt.Errorf("created %s, but adding its text failed: %w", f.ID, err)
		}
	}
	return f, nil
}
