// Package onedrive lets agents work in a OneDrive or a SharePoint document
// library through Microsoft Graph, signed in as an app with client
// credentials.
//
// Endpoints:
//   - "login": Microsoft Entra, default https://login.microsoftonline.com
//   - "graph": Microsoft Graph, default https://graph.microsoft.com/v1.0
package onedrive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	scope      = "https://graph.microsoft.com/.default"
	itemFields = "id,name,size,lastModifiedDateTime,webUrl,file,folder"
	// deltaFields adds what Changes needs to tell where a file is.
	deltaFields = itemFields + ",deleted,parentReference"
	docxType    = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	// maxPages bounds one List or Changes call. Changes picks up where it
	// stopped on the next call.
	maxPages = 50
)

// validID keeps ids to what Graph uses, since they go into paths. Personal
// OneDrive ids hold a '!'.
var validID = regexp.MustCompile(`^[A-Za-z0-9!_.-]+$`)

// Adapter is the OneDrive and SharePoint adapter.
type Adapter struct {
	auth  *microsoft.Auth
	graph string
}

func New(opts channels.Options) *Adapter {
	return &Adapter{
		auth:  microsoft.NewAuth(opts),
		graph: web.Endpoint(opts, "graph", "https://graph.microsoft.com/v1.0"),
	}
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.OneDrive,
		Label:    "OneDrive and SharePoint",
		Vendor:   "Microsoft",
		Category: "Files",
		Summary:  "Agents watch folders in OneDrive or a SharePoint library, and read and write files in them.",
		Fields: []channels.Field{
			{Name: "tenant_id", Label: "Tenant ID", Required: true, Help: "Your Microsoft Entra tenant ID."},
			{Name: "client_id", Label: "Client ID", Required: true, Help: "The application (client) ID of the app registration."},
			{
				Name: "client_secret", Label: "Client secret", Secret: true, Required: true,
				Help: "A client secret of the app registration, from Certificates & secrets.",
			},
			{
				Name: "drive_id", Label: "Drive ID", Required: true,
				Help: "The drive to work in, starting with b!. In Graph Explorer, a user's OneDrive is " +
					"GET /users/{email}/drive, and a SharePoint site's libraries are GET /sites/{host}:/sites/{site}:/drives.",
			},
		},
		Setup: "1. In Microsoft Entra, register an app and create a client secret.\n" +
			"2. Under API permissions, add the Microsoft Graph application permission Files.ReadWrite.All " +
			"(or Sites.ReadWrite.All, or Sites.Selected with access to one site).\n" +
			"3. Grant admin consent.\n" +
			"4. Look up the ID of the drive to use.",
		TargetLabel: "Folder",
		TargetHelp:  "A folder's item ID, or root.",
	}
}

type settings struct {
	app   microsoft.App
	drive string
}

func settingsOf(conn channels.Connection) (settings, error) {
	s := settings{
		app: microsoft.App{
			Tenant:   web.Value(conn, "tenant_id"),
			ClientID: web.Value(conn, "client_id"),
			Secret:   web.Value(conn, "client_secret"),
			Scope:    scope,
		},
		drive: web.Value(conn, "drive_id"),
	}
	switch {
	case s.app.Tenant == "" || s.app.ClientID == "" || s.app.Secret == "":
		return s, errors.New("the tenant ID, client ID or client secret is missing")
	case s.drive == "":
		return s, errors.New("the drive ID is missing")
	case !validID.MatchString(s.drive):
		return s, errors.New("the drive ID is not valid")
	}
	return s, nil
}

// itemID checks a folder's id; empty is the drive's root.
func itemID(id string) (string, error) {
	if id == "" {
		return "root", nil
	}
	if !validID.MatchString(id) {
		return "", fmt.Errorf("%q is not a drive item id", web.Short(id, 80))
	}
	return id, nil
}

type item struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Size                 int64     `json:"size"`
	LastModifiedDateTime time.Time `json:"lastModifiedDateTime"`
	WebURL               string    `json:"webUrl"`
	File                 *struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
	Folder          *struct{} `json:"folder"`
	Deleted         *struct{} `json:"deleted"`
	ParentReference struct {
		ID string `json:"id"`
	} `json:"parentReference"`
}

func (it item) file() channels.File {
	f := channels.File{
		ID: it.ID, Name: it.Name, Size: it.Size, Modified: it.LastModifiedDateTime,
		WebURL: it.WebURL, Folder: it.Folder != nil,
	}
	if it.File != nil {
		f.MimeType = it.File.MimeType
	}
	return f
}

// call makes one Graph request to u, a full URL under the Graph base.
func (a *Adapter) call(ctx context.Context, s settings, method, u string, body io.Reader, contentType string, limit int64) ([]byte, error) {
	// Links Graph hands back, and cursors made from them, must stay on
	// Graph: the request carries the app's token.
	if !strings.HasPrefix(u, a.graph+"/") {
		return nil, errors.New("the link does not point to Microsoft Graph")
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return a.auth.Do(req, s.app, "Microsoft Graph", limit)
}

func (a *Adapter) getJSON(ctx context.Context, s settings, u string, v any) error {
	b, err := a.call(ctx, s, http.MethodGet, u, nil, "", 16<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (a *Adapter) driveURL(s settings, path string, q url.Values) string {
	u := a.graph + "/drives/" + url.PathEscape(s.drive) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	s, err := settingsOf(conn)
	if err != nil {
		return err
	}
	// Sign in first and without the cache, so a wrong secret is reported as
	// such and not as a Graph error.
	if err := a.auth.Check(ctx, s.app); err != nil {
		return err
	}
	var d struct{}
	err = a.getJSON(ctx, s, a.driveURL(s, "", url.Values{"$select": {"id,name,driveType"}}), &d)
	if st := web.Status(err); st == http.StatusForbidden || st == http.StatusUnauthorized {
		return fmt.Errorf("signed in, but the app may not open the drive; grant it Files.ReadWrite.All or Sites.ReadWrite.All with admin consent (%w)", err)
	}
	if err != nil {
		return fmt.Errorf("signed in, but %w", err)
	}
	return nil
}

func (a *Adapter) List(ctx context.Context, conn channels.Connection, folder string) ([]channels.File, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return nil, err
	}
	id, err := itemID(folder)
	if err != nil {
		return nil, err
	}
	u := a.driveURL(s, "/items/"+url.PathEscape(id)+"/children", url.Values{"$select": {itemFields}, "$top": {"999"}})
	var out []channels.File
	for range maxPages {
		var r struct {
			Value    []item `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := a.getJSON(ctx, s, u, &r); err != nil {
			return nil, fmt.Errorf("onedrive: listing the folder: %w", err)
		}
		for _, it := range r.Value {
			out = append(out, it.file())
		}
		if r.NextLink == "" {
			break
		}
		u = r.NextLink
	}
	return out, nil
}

func (a *Adapter) Read(ctx context.Context, conn channels.Connection, id string) (channels.File, []byte, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return channels.File{}, nil, err
	}
	if id == "" || !validID.MatchString(id) {
		return channels.File{}, nil, fmt.Errorf("onedrive: %q is not a drive item id", web.Short(id, 80))
	}
	var it item
	if err := a.getJSON(ctx, s, a.driveURL(s, "/items/"+url.PathEscape(id), url.Values{"$select": {itemFields}}), &it); err != nil {
		return channels.File{}, nil, fmt.Errorf("onedrive: reading the file: %w", err)
	}
	f := it.file()
	switch {
	case f.Folder:
		return f, nil, fmt.Errorf("onedrive: %q is a folder", it.Name)
	case f.Size > channels.MaxFileBytes:
		return f, nil, web.FileTooLarge("onedrive", it.Name)
	}
	// /content redirects to a pre-signed download URL; the client drops the
	// Authorization header on the way, as it changes hosts.
	b, err := a.call(ctx, s, http.MethodGet, a.driveURL(s, "/items/"+url.PathEscape(id)+"/content", nil), nil, "", channels.MaxFileBytes)
	if errors.Is(err, web.ErrTooLarge) {
		return f, nil, web.FileTooLarge("onedrive", it.Name)
	}
	if err != nil {
		return f, nil, fmt.Errorf("onedrive: downloading the file: %w", err)
	}
	f.Size = int64(len(b))
	// Word documents are zipped XML. Their text is what an agent wants;
	// other Office files are returned as stored.
	if f.MimeType == docxType || strings.HasSuffix(strings.ToLower(it.Name), ".docx") {
		if text, err := docxText(b); err == nil {
			f.MimeType, f.Size = "text/plain", int64(len(text))
			return f, text, nil
		}
	}
	return f, b, nil
}

// docxText extracts the paragraphs of a Word document.
func docxText(b []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}
	for _, zf := range zr.File {
		if zf.Name != "word/document.xml" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		// The limit guards against a small file that unzips to a huge one.
		dec := xml.NewDecoder(io.LimitReader(rc, 64<<20))
		var out bytes.Buffer
		inText := false
		for out.Len() < channels.MaxFileBytes {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "t":
					inText = true
				case "tab":
					out.WriteByte('\t')
				case "br", "cr":
					out.WriteByte('\n')
				}
			case xml.EndElement:
				switch t.Name.Local {
				case "t":
					inText = false
				case "p":
					out.WriteByte('\n')
				}
			case xml.CharData:
				if inText {
					out.Write(t)
				}
			}
		}
		text := out.Bytes()
		if len(text) > channels.MaxFileBytes {
			text = text[:channels.MaxFileBytes]
		}
		return bytes.TrimSpace(text), nil
	}
	return nil, errors.New("not a Word document")
}

func (a *Adapter) Write(ctx context.Context, conn channels.Connection, folder, name, mimeType string, content []byte) (channels.File, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return channels.File{}, err
	}
	parent, err := itemID(folder)
	if err != nil {
		return channels.File{}, err
	}
	// A simple upload is enough within MaxFileBytes; larger files would
	// need an upload session.
	u := a.driveURL(s, "/items/"+url.PathEscape(parent)+":/"+url.PathEscape(name)+":/content", nil)
	b, err := a.call(ctx, s, http.MethodPut, u, bytes.NewReader(content), mimeType, 1<<20)
	if err != nil {
		return channels.File{}, fmt.Errorf("onedrive: uploading the file: %w", err)
	}
	var it item
	if err := json.Unmarshal(b, &it); err != nil {
		return channels.File{}, fmt.Errorf("onedrive: unreadable answer: %w", err)
	}
	return it.file(), nil
}

// Changes runs a delta query on the drive's root and keeps the files
// directly in folder: OneDrive for Business and SharePoint only support
// delta on the root. The cursor is Graph's deltaLink (or nextLink, when a
// long list was cut short).
func (a *Adapter) Changes(ctx context.Context, conn channels.Connection, folder, cursor string) ([]channels.File, string, error) {
	s, err := settingsOf(conn)
	if err != nil {
		return nil, "", err
	}
	parent, err := itemID(folder)
	if err != nil {
		return nil, "", err
	}
	if cursor == "" {
		next, err := a.startDelta(ctx, s)
		return nil, next, err
	}
	if parent == "root" {
		var r item
		if err := a.getJSON(ctx, s, a.driveURL(s, "/root", url.Values{"$select": {"id"}}), &r); err != nil {
			return nil, "", fmt.Errorf("onedrive: finding the drive's root: %w", err)
		}
		parent = r.ID
	}
	files, next, err := a.delta(ctx, s, cursor, parent)
	if web.Status(err) == http.StatusGone {
		// Graph forgot the cursor. Start again from now; changes in the gap
		// are lost, which beats replaying the whole drive.
		next, err = a.startDelta(ctx, s)
		return nil, next, err
	}
	return files, next, err
}

// startDelta returns a cursor for changes from now on. The cursor keeps the
// fields it was asked with, so every later page has what delta reads.
func (a *Adapter) startDelta(ctx context.Context, s settings) (string, error) {
	q := url.Values{"token": {"latest"}, "$select": {deltaFields}}
	_, next, err := a.delta(ctx, s, a.driveURL(s, "/root/delta", q), "")
	return next, err
}

// delta follows a delta query's pages from u. With parent empty it only
// looks for the link to ask with next.
func (a *Adapter) delta(ctx context.Context, s settings, u, parent string) ([]channels.File, string, error) {
	var out []channels.File
	for range maxPages {
		var r struct {
			Value     []item `json:"value"`
			NextLink  string `json:"@odata.nextLink"`
			DeltaLink string `json:"@odata.deltaLink"`
		}
		if err := a.getJSON(ctx, s, u, &r); err != nil {
			return nil, "", fmt.Errorf("onedrive: listing changes: %w", err)
		}
		for _, it := range r.Value {
			if parent == "" || it.Deleted != nil || it.Folder != nil || it.File == nil || it.ParentReference.ID != parent {
				continue
			}
			out = append(out, it.file())
		}
		switch {
		case r.DeltaLink != "":
			return out, r.DeltaLink, nil
		case r.NextLink == "":
			return out, u, nil
		}
		u = r.NextLink
	}
	return out, u, nil
}
