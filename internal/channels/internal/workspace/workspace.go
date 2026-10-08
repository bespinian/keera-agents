// Package workspace holds what the Google Workspace API adapters share: the
// service account settings, signing in, as the service account or as each
// person, calling an API as JSON, and finding, checking and creating Docs,
// Sheets and Slides files through Drive.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/google"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// The scopes every file kind adds to its own API's: reading Drive's file
// list to find files, and making files in a folder, which only Drive can do.
// drive.file reaches only files the connector created or was shared.
const (
	DriveMetadata = "https://www.googleapis.com/auth/drive.metadata.readonly"
	DriveFile     = "https://www.googleapis.com/auth/drive.file"
)

// maxBody bounds an API answer. A large presentation is a few MiB of JSON.
const maxBody = 32 << 20

// Fields are the settings of every Google Workspace kind. api names the API
// to enable, such as "Google Docs API".
func Fields(api string) []channels.Field {
	return []channels.Field{
		{
			Name: "service_account_json", Label: "Service account key", Secret: true, Required: true, Multiline: true,
			Help: "The JSON key file of a service account in a project with the " + api + " enabled.",
		},
		{
			Name: "subject", Label: "Act as user",
			Help: "An email in your domain to act as, through domain-wide delegation. " +
				"Leave empty to act as the service account, and share with it what agents may use.",
		},
	}
}

// Settings are a connection's key file and the user it acts as, or, for a
// connector people sign in to, the access token of the person a call is
// for.
type Settings struct {
	Key, Subject   string
	Token, Account string
}

func SettingsOf(conn channels.Connection) (Settings, error) {
	if conn.PerUser {
		if conn.Token == "" {
			return Settings{}, oauth.ErrNoToken
		}
		return Settings{Token: conn.Token, Account: conn.Account}, nil
	}
	s := Settings{Key: web.Value(conn, "service_account_json"), Subject: web.Value(conn, "subject")}
	if s.Key == "" {
		return s, errors.New("the service account key is missing")
	}
	return s, nil
}

// Who says whose access a call has, for errors that ask to share something.
func (s Settings) Who() string {
	if s.Token != "" {
		if s.Account != "" {
			return s.Account
		}
		return "the person the agent works for"
	}
	if s.Subject != "" {
		return s.Subject
	}
	if sa, err := oauth.ParseServiceAccount(s.Key); err == nil {
		return "the service account " + sa.Email
	}
	return "the service account"
}

// Client calls Google APIs with one set of scopes.
type Client struct {
	auth  *google.Auth
	scope string
	drive string
}

// NewClient signs in for scopes. Drive is at the "drive" endpoint, by
// default https://www.googleapis.com.
func NewClient(opts channels.Options, scopes ...string) *Client {
	return &Client{
		auth:  google.NewAuth(opts),
		scope: strings.Join(scopes, " "),
		drive: web.Endpoint(opts, "drive", "https://www.googleapis.com"),
	}
}

// Do sends in as JSON, unless it is nil, and decodes the answer into out,
// unless it is nil.
func (c *Client) Do(ctx context.Context, s Settings, method, u string, in, out any) error {
	req, err := web.NewJSONRequest(ctx, method, u, in)
	if err != nil {
		return err
	}
	var b []byte
	if s.Token != "" {
		b, err = c.auth.DoAs(req, s.Token, maxBody)
	} else {
		b, err = c.auth.Do(req, s.Key, c.scope, s.Subject, maxBody)
	}
	if err != nil || out == nil || len(b) == 0 {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("could not read Google's answer: %w", err)
	}
	return nil
}

// Check signs in and makes one cheap call, probe, to prove the key works and
// the API is enabled. A nil probe asks Drive who the connector is. A
// connector people sign in to has nobody signed in yet, so only its OAuth
// client is checked.
func (c *Client) Check(ctx context.Context, conn channels.Connection, api string, probe func(context.Context, Settings) error) error {
	if conn.PerUser {
		return c.auth.Accounts(c.scope, api).CheckClient(ctx, conn)
	}
	s, err := SettingsOf(conn)
	if err != nil {
		return err
	}
	if err := c.auth.Check(ctx, s.Key, c.scope, s.Subject); err != nil {
		if s.Subject != "" {
			return fmt.Errorf("%w; check that the service account has domain-wide delegation for %s",
				err, strings.ReplaceAll(c.scope, " ", ","))
		}
		return err
	}
	if probe == nil {
		probe = func(ctx context.Context, s Settings) error {
			return c.Do(ctx, s, http.MethodGet, c.drive+"/drive/v3/about?fields=user", nil, &struct{}{})
		}
	}
	err = probe(ctx, s)
	switch web.Status(err) {
	case 0:
		return err
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("signed in, but %w; check that the %s is enabled in the service account's project", err, api)
	}
	return fmt.Errorf("signed in, but %w", err)
}

// Accounts lets people sign in to a kind's connectors with their own
// accounts. An adapter embeds it to implement channels.PerUser.
type Accounts = oauth.Accounts

// NewAccounts signs people in for client's scopes. apis names the APIs to
// enable, as in Setup.
func NewAccounts(client *Client, apis string) Accounts {
	return client.auth.Accounts(client.scope, apis)
}

// File is a Docs, Sheets or Slides file, as tools return it.
type File struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	URL      string    `json:"url,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
}

type driveFile struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	MimeType     string    `json:"mimeType"`
	ModifiedTime time.Time `json:"modifiedTime"`
	WebViewLink  string    `json:"webViewLink"`
	Trashed      bool      `json:"trashed"`
}

func (f driveFile) file() File {
	return File{ID: f.ID, Name: f.Name, URL: f.WebViewLink, Modified: f.ModifiedTime}
}

const fileFields = "id,name,mimeType,modifiedTime,webViewLink,trashed"

// validID is Drive's alphabet. Ids go into URLs and Drive queries.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

// ID reads a file's id, or a URL to it as people paste it, such as
// https://docs.google.com/document/d/<id>/edit, or a folder's.
func ID(s string) (string, error) {
	id := strings.TrimSpace(s)
	if strings.Contains(id, "://") {
		if u, err := url.Parse(id); err == nil {
			id = u.Query().Get("id")
			parts := strings.Split(u.Path, "/")
			for i, p := range parts {
				if (p == "d" || p == "folders") && i+1 < len(parts) {
					id = parts[i+1]
					break
				}
			}
		}
	}
	if !validID.MatchString(id) {
		return "", fmt.Errorf("%q is not a Google file id or link", web.Short(s, 80))
	}
	return id, nil
}

// Files is the Drive side of one file kind: Docs, Sheets or Slides.
type Files struct {
	*Client
	// MimeType is the kind's Drive type. Noun names one file, such as
	// "document", and Arg is the tool argument that holds its id.
	MimeType, Noun, Arg string
}

// Get reads a file's details and checks it is of the kind.
func (f Files) Get(ctx context.Context, s Settings, id string) (File, error) {
	var d driveFile
	err := f.Do(ctx, s, http.MethodGet, f.drive+"/drive/v3/files/"+id+"?"+url.Values{
		"fields": {fileFields}, "supportsAllDrives": {"true"},
	}.Encode(), nil, &d)
	switch {
	case web.Status(err) == http.StatusNotFound:
		return File{}, fmt.Errorf("there is no %s %s, or it is not shared with %s", f.Noun, id, s.Who())
	case err != nil:
		return File{}, err
	case d.Trashed:
		return File{}, fmt.Errorf("%q is in the trash", d.Name)
	case d.MimeType != f.MimeType:
		return File{}, fmt.Errorf("%q is not a Google %s (it is %s)", d.Name, f.Noun, d.MimeType)
	}
	return d.file(), nil
}

// Target checks a binding's target: AnyTarget, or one file's id or link,
// which it returns as the bare id.
func (f Files) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	if strings.TrimSpace(target) == channels.AnyTarget {
		return channels.AnyTarget, nil
	}
	s, err := SettingsOf(conn)
	if err != nil {
		return "", err
	}
	id, err := ID(target)
	if err != nil {
		return "", err
	}
	if _, err := f.Get(ctx, s, id); err != nil {
		return "", err
	}
	return id, nil
}

// Pick returns the file a tool works on: id, which must be target unless
// that is AnyTarget. An empty id is target itself.
func (f Files) Pick(target, id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		if target == channels.AnyTarget {
			return "", apitool.Need(f.Arg, id)
		}
		return target, nil
	}
	id, err := ID(id)
	if err != nil {
		return "", err
	}
	if target != channels.AnyTarget && id != target {
		return "", fmt.Errorf("this connector reaches only the %s %s, not %s", f.Noun, target, id)
	}
	return id, nil
}

// Find lists the files of the kind whose name contains name, newest first.
// With a target of one file, it is that file alone.
func (f Files) Find(ctx context.Context, s Settings, target, name string, limit int) ([]File, error) {
	if target != channels.AnyTarget {
		file, err := f.Get(ctx, s, target)
		if err != nil {
			return nil, err
		}
		return []File{file}, nil
	}
	query := fmt.Sprintf("mimeType = '%s' and trashed = false", f.MimeType)
	if name = strings.TrimSpace(name); name != "" {
		query += fmt.Sprintf(" and name contains '%s'", quote(name))
	}
	q := url.Values{
		"q":                         {query},
		"corpora":                   {"allDrives"},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
		"orderBy":                   {"modifiedTime desc"},
		"pageSize":                  {fmt.Sprint(limit)},
		"fields":                    {"files(" + fileFields + ")"},
	}
	var r struct {
		Files []driveFile `json:"files"`
	}
	if err := f.Do(ctx, s, http.MethodGet, f.drive+"/drive/v3/files?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := []File{}
	for _, d := range r.Files {
		out = append(out, d.file())
	}
	return out, nil
}

// FindTool is the find tool every file kind has.
func (f Files) FindTool() apitool.Tool {
	plural := f.Noun + "s"
	return apitool.Tool{
		Tool: channels.Tool{
			Name: "find",
			Description: "Find Google " + plural + " by a part of their name, newest first. " +
				"A connector bound to one " + f.Noun + " always returns that one.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"name":{"type":"string","description":"Text the name contains. Empty lists the newest."},
				"limit":{"type":"integer","description":"At most this many, 25 by default, up to 100."}}}`),
		},
		Run: func(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
			in, err := apitool.Decode[struct {
				Name  string `json:"name"`
				Limit int    `json:"limit"`
			}](args)
			if err != nil {
				return nil, err
			}
			s, err := SettingsOf(conn)
			if err != nil {
				return nil, err
			}
			files, err := f.Find(ctx, s, target, in.Name, apitool.Limit(in.Limit, 25, 100))
			if err != nil {
				return nil, fmt.Errorf("finding %s: %w", plural, err)
			}
			return files, nil
		},
	}
}

// Create makes an empty file of the kind called name, in folder if it is not
// empty. A service account has no storage of its own, so without a folder
// it works only when acting as a user.
func (f Files) Create(ctx context.Context, s Settings, target, name, folder string) (File, error) {
	if target != channels.AnyTarget {
		return File{}, fmt.Errorf("this connector reaches only the %s %s, so it cannot create others", f.Noun, target)
	}
	if err := apitool.Need("title", name); err != nil {
		return File{}, err
	}
	meta := map[string]any{"name": strings.TrimSpace(name), "mimeType": f.MimeType}
	if strings.TrimSpace(folder) != "" {
		id, err := ID(folder)
		if err != nil {
			return File{}, err
		}
		meta["parents"] = []string{id}
	}
	var d driveFile
	err := f.Do(ctx, s, http.MethodPost, f.drive+"/drive/v3/files?"+url.Values{
		"fields": {fileFields}, "supportsAllDrives": {"true"},
	}.Encode(), meta, &d)
	if err != nil {
		return File{}, fmt.Errorf("creating the %s: %w", f.Noun, err)
	}
	return d.file(), nil
}

// quote escapes a string for a Drive query.
func quote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// Trim cuts s to at most n bytes, at a rune boundary, and says whether it
// did.
func Trim(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// Setup is the steps on Google's side for a kind that calls apis with
// scopes. share says what to share with the service account.
func Setup(apis, share string, scopes ...string) string {
	return "1. In a Google Cloud project, enable the " + apis + ".\n" +
		"2. Create a service account with a JSON key.\n" +
		"3. Either share " + share + " with the service account's email, or, as a Workspace administrator, " +
		"grant the service account's client ID domain-wide delegation for the scopes " +
		strings.Join(scopes, ",") + " and enter the user to act as."
}
