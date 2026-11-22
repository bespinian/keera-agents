// Package gdrive lets agents work in Google Drive folders as a service
// account, as a user of the domain through domain-wide delegation, or as
// each person who signs in with their own account.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "drive": the base of the Drive API and its uploads, default
//     https://www.googleapis.com
package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/google"
	"github.com/bespinian/keera-agents/internal/connectors/internal/oauth"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace"
)

const (
	scope      = "https://www.googleapis.com/auth/drive"
	folderType = "application/vnd.google-apps.folder"
	fileFields = "id,name,mimeType,size,modifiedTime,webViewLink"
	// maxPages bounds one List or Changes call. Changes picks up where it
	// stopped on the next call.
	maxPages = 20
)

// exports says what each Google Workspace type is read as.
var exports = map[string]string{
	"application/vnd.google-apps.document":     "text/plain",
	"application/vnd.google-apps.spreadsheet":  "text/csv",
	"application/vnd.google-apps.presentation": "text/plain",
	"application/vnd.google-apps.drawing":      "image/svg+xml",
}

// Adapter is the Google Drive adapter.
type Adapter struct {
	auth *google.Auth
	base string
	oauth.Accounts
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{
		auth: google.NewAuth(opts),
		base: web.Endpoint(opts, "drive", "https://www.googleapis.com"),
	}
	a.Accounts = a.auth.Accounts(scope, "Google Drive API").WithFields(sharedDrive)
	return a
}

// sharedDrive is a setting of both kinds of connector.
var sharedDrive = connectors.Field{
	Name: "shared_drive_id", Label: "Shared drive ID",
	Help: "To work in a shared drive: the ID at the end of its URL. Leave empty for My Drive.",
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.GoogleDrive,
		Label:    "Google Drive",
		Vendor:   "Google",
		Category: "Files",
		Summary:  "Agents watch folders for new files, and read and write files in them.",
		Fields:   append(workspace.Fields("Google Drive API"), sharedDrive),
		Setup: workspace.Setup("Google Drive API", "the folders, or a shared drive,",
			"A service account has no storage of its own, so agents can then add files only in a shared drive.", scope),
		TargetLabel: "Folder",
		TargetHelp: "A folder's ID, the last part of its URL, or root. On a connector people sign in to, " +
			"root is the My Drive of whoever the agent works for.",
	}
}

// settings are what a call is made with, and the drive.
type settings struct {
	google.Creds
	drive string
}

func settingsOf(conn connectors.Connection) (settings, error) {
	c, err := google.CredsOf(conn)
	s := settings{Creds: c, drive: web.Value(conn, "shared_drive_id")}
	if err != nil {
		return s, err
	}
	if s.drive != "" && !workspace.ValidID.MatchString(s.drive) {
		return s, errors.New("the shared drive ID is not valid")
	}
	return s, nil
}

// root is the folder an empty id means.
func (s settings) root(folder string) (string, error) {
	if folder == "" {
		if s.drive != "" {
			return s.drive, nil
		}
		return "root", nil
	}
	if !workspace.ValidID.MatchString(folder) {
		return "", fmt.Errorf("%q is not a Drive id", web.Short(folder, 80))
	}
	return folder, nil
}

type driveFile struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	MimeType     string    `json:"mimeType"`
	Size         string    `json:"size"`
	ModifiedTime time.Time `json:"modifiedTime"`
	WebViewLink  string    `json:"webViewLink"`
	Parents      []string  `json:"parents"`
	Trashed      bool      `json:"trashed"`
}

func (f driveFile) file() connectors.File {
	size, _ := strconv.ParseInt(f.Size, 10, 64)
	return connectors.File{
		ID: f.ID, Name: f.Name, MimeType: f.MimeType, Size: size,
		Modified: f.ModifiedTime, WebURL: f.WebViewLink, Folder: f.MimeType == folderType,
	}
}

// call makes one API request and returns the body, up to limit bytes.
func (a *Adapter) call(ctx context.Context, s settings, method, path string, q url.Values, body io.Reader, contentType string, limit int64) ([]byte, error) {
	u := a.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return a.auth.Do(req, s.Creds, scope, limit)
}

func (a *Adapter) getJSON(ctx context.Context, s settings, path string, q url.Values, v any) error {
	b, err := a.call(ctx, s, http.MethodGet, path, q, nil, "", 8<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	s, err := settingsOf(conn)
	if err != nil {
		return err
	}
	if err := a.auth.Check(ctx, s.Creds, scope); err != nil {
		return err
	}
	var v struct{}
	if s.drive != "" {
		err = a.getJSON(ctx, s, "/drive/v3/drives/"+s.drive, url.Values{"fields": {"id,name"}}, &v)
	} else {
		err = a.getJSON(ctx, s, "/drive/v3/about", url.Values{"fields": {"user"}}, &v)
	}
	if err != nil {
		return fmt.Errorf("signed in, but %w", err)
	}
	return nil
}

func (a *Adapter) List(ctx context.Context, conn connectors.Connection, folder string) ([]connectors.File, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return nil, err
	}
	id, err := s.root(folder)
	if err != nil {
		return nil, err
	}
	q := s.listQuery("'" + id + "' in parents and trashed = false")
	q.Set("fields", "nextPageToken,files("+fileFields+")")
	q.Set("orderBy", "folder,name")
	q.Set("pageSize", "1000")
	var out []connectors.File
	for range maxPages {
		var r struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := a.getJSON(ctx, s, "/drive/v3/files", q, &r); err != nil {
			return nil, fmt.Errorf("gdrive: listing the folder: %w", err)
		}
		for _, f := range r.Files {
			out = append(out, f.file())
		}
		if r.NextPageToken == "" {
			break
		}
		q.Set("pageToken", r.NextPageToken)
	}
	return out, nil
}

// listQuery asks for the files that match query, in the shared drive if
// there is one.
func (s settings) listQuery(query string) url.Values {
	q := url.Values{
		"q":                         {query},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
	}
	if s.drive != "" {
		q.Set("corpora", "drive")
		q.Set("driveId", s.drive)
	}
	return q
}

func (a *Adapter) Read(ctx context.Context, conn connectors.Connection, id string) (connectors.File, []byte, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return connectors.File{}, nil, err
	}
	if !workspace.ValidID.MatchString(id) {
		return connectors.File{}, nil, fmt.Errorf("gdrive: %q is not a Drive id", web.Short(id, 80))
	}
	var meta driveFile
	if err := a.getJSON(ctx, s, "/drive/v3/files/"+id, url.Values{"fields": {fileFields}, "supportsAllDrives": {"true"}}, &meta); err != nil {
		return connectors.File{}, nil, fmt.Errorf("gdrive: reading the file: %w", err)
	}
	f := meta.file()
	var b []byte
	switch export, ok := exports[meta.MimeType]; {
	case f.Folder:
		return f, nil, fmt.Errorf("gdrive: %q is a folder", meta.Name)
	case ok:
		b, err = a.call(ctx, s, http.MethodGet, "/drive/v3/files/"+id+"/export", url.Values{"mimeType": {export}}, nil, "", connectors.MaxFileBytes)
		f.MimeType = export
	case strings.HasPrefix(meta.MimeType, "application/vnd.google-apps."):
		return f, nil, fmt.Errorf("gdrive: %q is a %s, which cannot be read as a file", meta.Name, strings.TrimPrefix(meta.MimeType, "application/vnd.google-apps."))
	case f.Size > connectors.MaxFileBytes:
		return f, nil, web.FileTooLarge("gdrive", meta.Name)
	default:
		b, err = a.call(ctx, s, http.MethodGet, "/drive/v3/files/"+id, url.Values{"alt": {"media"}, "supportsAllDrives": {"true"}}, nil, "", connectors.MaxFileBytes)
	}
	if errors.Is(err, web.ErrTooLarge) {
		return f, nil, web.FileTooLarge("gdrive", meta.Name)
	}
	if err != nil {
		return f, nil, fmt.Errorf("gdrive: downloading the file: %w", err)
	}
	f.Size = int64(len(b))
	return f, b, nil
}

func (a *Adapter) Write(ctx context.Context, conn connectors.Connection, folder, name, mimeType string, content []byte) (connectors.File, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return connectors.File{}, err
	}
	parent, err := s.root(folder)
	if err != nil {
		return connectors.File{}, err
	}

	// Replace the file of that name if there is one, so writing twice does
	// not leave two files with one name.
	q := s.listQuery(fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false and mimeType != '%s'", workspace.Quote(name), parent, folderType))
	q.Set("fields", "files(id)")
	q.Set("pageSize", "1")
	var found struct {
		Files []driveFile `json:"files"`
	}
	if err := a.getJSON(ctx, s, "/drive/v3/files", q, &found); err != nil {
		return connectors.File{}, fmt.Errorf("gdrive: looking for the file: %w", err)
	}

	method, path := http.MethodPost, "/upload/drive/v3/files"
	meta := map[string]any{"name": name, "parents": []string{parent}, "mimeType": mimeType}
	if len(found.Files) > 0 {
		method, path = http.MethodPatch, "/upload/drive/v3/files/"+found.Files[0].ID
		meta = map[string]any{}
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_ = json.NewEncoder(part).Encode(meta)
	part, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {mimeType}})
	_, _ = part.Write(content)
	_ = mw.Close()

	b, err := a.call(ctx, s, method, path, url.Values{
		"uploadType": {"multipart"}, "supportsAllDrives": {"true"}, "fields": {fileFields},
	}, &buf, "multipart/related; boundary="+mw.Boundary(), 1<<20)
	if err != nil {
		return connectors.File{}, fmt.Errorf("gdrive: uploading the file: %w", err)
	}
	var f driveFile
	if err := json.Unmarshal(b, &f); err != nil {
		return connectors.File{}, fmt.Errorf("gdrive: unreadable answer: %w", err)
	}
	return f.file(), nil
}

func (a *Adapter) Changes(ctx context.Context, conn connectors.Connection, folder, cursor string) ([]connectors.File, string, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return nil, "", err
	}
	parent, err := s.root(folder)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{"supportsAllDrives": {"true"}}
	if s.drive != "" {
		q.Set("driveId", s.drive)
	}
	if cursor == "" {
		var r struct {
			StartPageToken string `json:"startPageToken"`
		}
		if err := a.getJSON(ctx, s, "/drive/v3/changes/startPageToken", q, &r); err != nil {
			return nil, "", fmt.Errorf("gdrive: starting to watch: %w", err)
		}
		return nil, r.StartPageToken, nil
	}
	// Parents hold the real id of My Drive's root, never the alias.
	if parent == "root" {
		var r driveFile
		if err := a.getJSON(ctx, s, "/drive/v3/files/root", url.Values{"fields": {"id"}}, &r); err != nil {
			return nil, "", fmt.Errorf("gdrive: finding My Drive: %w", err)
		}
		parent = r.ID
	}

	q.Set("includeItemsFromAllDrives", "true")
	q.Set("spaces", "drive")
	q.Set("pageSize", "1000")
	q.Set("fields", "nextPageToken,newStartPageToken,changes(fileId,removed,file("+fileFields+",parents,trashed))")
	var out []connectors.File
	for range maxPages {
		q.Set("pageToken", cursor)
		var r struct {
			NextPageToken     string `json:"nextPageToken"`
			NewStartPageToken string `json:"newStartPageToken"`
			Changes           []struct {
				Removed bool       `json:"removed"`
				File    *driveFile `json:"file"`
			} `json:"changes"`
		}
		if err := a.getJSON(ctx, s, "/drive/v3/changes", q, &r); err != nil {
			return nil, "", fmt.Errorf("gdrive: listing changes: %w", err)
		}
		for _, c := range r.Changes {
			f := c.File
			if c.Removed || f == nil || f.Trashed || f.MimeType == folderType || !slices.Contains(f.Parents, parent) {
				continue
			}
			out = append(out, f.file())
		}
		switch {
		case r.NewStartPageToken != "":
			return out, r.NewStartPageToken, nil
		case r.NextPageToken == "":
			return out, cursor, nil
		}
		cursor = r.NextPageToken
	}
	return out, cursor, nil
}
