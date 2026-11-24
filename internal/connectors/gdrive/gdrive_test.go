package gdrive

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/jwttest"
)

// fake is a small Drive with a root folder "ROOTID" holding a doc, a
// sheet, a text file and a subfolder.
type fake struct {
	srv     *httptest.Server
	key     *jwttest.Key
	mu      sync.Mutex
	files   map[string]map[string]any
	content map[string]string
	queries []string
	uploads []upload
}

type upload struct {
	method, path string
	meta         map[string]any
	contentType  string
	content      string
}

func newFake(t *testing.T) *fake {
	f := &fake{
		key: jwttest.NewKey(t, "sa"),
		files: map[string]map[string]any{
			"DOC":   {"id": "DOC", "name": "Plan", "mimeType": "application/vnd.google-apps.document", "modifiedTime": "2026-10-01T10:00:00Z", "parents": []any{"ROOTID"}},
			"SHEET": {"id": "SHEET", "name": "Budget", "mimeType": "application/vnd.google-apps.spreadsheet", "modifiedTime": "2026-10-01T10:00:00Z", "parents": []any{"ROOTID"}},
			"TXT":   {"id": "TXT", "name": "notes.txt", "mimeType": "text/plain", "size": "5", "modifiedTime": "2026-10-02T10:00:00Z", "parents": []any{"ROOTID"}},
			"BIG":   {"id": "BIG", "name": "video.mp4", "mimeType": "video/mp4", "size": "999999999", "parents": []any{"ROOTID"}},
			"FORM":  {"id": "FORM", "name": "Survey", "mimeType": "application/vnd.google-apps.form", "parents": []any{"ROOTID"}},
			"SUB":   {"id": "SUB", "name": "Archive", "mimeType": "application/vnd.google-apps.folder", "parents": []any{"ROOTID"}},
		},
		content: map[string]string{"DOC": "plan text", "SHEET": "a,b\n1,2\n", "TXT": "hello"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		c := jwttest.Claims(t, r.Form.Get("assertion"), &f.key.Private.PublicKey)
		if c["scope"] != "https://www.googleapis.com/auth/drive" {
			t.Errorf("scope = %v", c["scope"])
		}
		if c["sub"] == "outsider@x.ch" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens using this method."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"drive-token","expires_in":3600}`))
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer drive-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET /drive/v3/about", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"emailAddress":"sa@p.iam.gserviceaccount.com"}}`))
	}))
	mux.HandleFunc("GET /drive/v3/drives/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "SHARED" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Shared drive not found: X","errors":[{"reason":"notFound"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"SHARED","name":"Team"}`))
	}))
	mux.HandleFunc("GET /drive/v3/files", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		f.queries = append(f.queries, q)
		if r.URL.Query().Get("supportsAllDrives") != "true" {
			t.Errorf("supportsAllDrives missing")
		}
		var out []map[string]any
		for _, file := range f.files {
			parent := file["parents"].([]any)[0].(string)
			if !strings.Contains(q, "'"+parent+"' in parents") && (parent != "ROOTID" || !strings.Contains(q, "'root' in parents")) {
				continue
			}
			if strings.HasPrefix(q, "name = ") && !strings.Contains(q, "name = '"+strings.ReplaceAll(file["name"].(string), "'", `\'`)+"'") {
				continue
			}
			out = append(out, file)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": out})
	}))
	mux.HandleFunc("GET /drive/v3/files/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "root" {
			_, _ = w.Write([]byte(`{"id":"ROOTID"}`))
			return
		}
		file, ok := f.files[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"File not found: ` + id + `.","errors":[{"reason":"notFound"}]}}`))
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write([]byte(f.content[id]))
			return
		}
		_ = json.NewEncoder(w).Encode(file)
	}))
	mux.HandleFunc("GET /drive/v3/files/{id}/export", auth(func(w http.ResponseWriter, r *http.Request) {
		want := map[string]string{"DOC": "text/plain", "SHEET": "text/csv"}[r.PathValue("id")]
		if got := r.URL.Query().Get("mimeType"); got != want {
			t.Errorf("export as %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(f.content[r.PathValue("id")]))
	}))
	uploadHandler := auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uploadType") != "multipart" {
			t.Errorf("uploadType = %q", r.URL.Query().Get("uploadType"))
		}
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "multipart/related" {
			t.Errorf("content type = %q", mt)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		up := upload{method: r.Method, path: r.URL.Path}
		p, _ := mr.NextPart()
		_ = json.NewDecoder(p).Decode(&up.meta)
		p, _ = mr.NextPart()
		up.contentType = p.Header.Get("Content-Type")
		b, _ := io.ReadAll(p)
		up.content = string(b)
		f.uploads = append(f.uploads, up)
		id := r.PathValue("id")
		if id == "" {
			id = "NEW"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": "x", "mimeType": up.contentType, "size": "3", "modifiedTime": "2026-11-01T00:00:00Z"})
	})
	mux.HandleFunc("POST /upload/drive/v3/files", uploadHandler)
	mux.HandleFunc("PATCH /upload/drive/v3/files/{id}", uploadHandler)
	mux.HandleFunc("GET /drive/v3/changes/startPageToken", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"startPageToken":"100"}`))
	}))
	mux.HandleFunc("GET /drive/v3/changes", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("pageToken") {
		case "100":
			_, _ = w.Write([]byte(`{"nextPageToken":"101","changes":[
				{"fileId":"TXT","file":{"id":"TXT","name":"notes.txt","mimeType":"text/plain","parents":["ROOTID"],"modifiedTime":"2026-10-02T10:00:00Z"}},
				{"fileId":"SUB","file":{"id":"SUB","name":"Archive","mimeType":"application/vnd.google-apps.folder","parents":["ROOTID"]}},
				{"fileId":"OLD","removed":true},
				{"fileId":"TRASH","file":{"id":"TRASH","name":"t","mimeType":"text/plain","parents":["ROOTID"],"trashed":true}},
				{"fileId":"ELSE","file":{"id":"ELSE","name":"e","mimeType":"text/plain","parents":["SUB"]}}
			]}`))
		case "101":
			_, _ = w.Write([]byte(`{"newStartPageToken":"102","changes":[
				{"fileId":"DOC","file":{"id":"DOC","name":"Plan","mimeType":"application/vnd.google-apps.document","parents":["ROOTID"]}},
				{"fileId":"TXT","file":{"id":"TXT","name":"notes.txt","mimeType":"text/plain","parents":["ROOTID"],"modifiedTime":"2026-10-03T10:00:00Z"}}
			]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid page token"}}`))
		}
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) adapter() *Adapter {
	return New(connectors.Options{HTTP: f.srv.Client(), Endpoints: map[string]string{"token": f.srv.URL + "/token", "drive": f.srv.URL}})
}

func (f *fake) conn(config map[string]string) connectors.Connection {
	return connectors.Connection{
		ID: "c1", Kind: connectors.GoogleDrive, Config: config,
		Secrets: map[string]string{"service_account_json": f.key.ServiceAccountJSON("sa@p.iam.gserviceaccount.com")},
	}
}

func TestList(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	files, err := a.List(context.Background(), f.conn(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 6 {
		t.Errorf("files = %+v", files)
	}
	for _, file := range files {
		if file.ID == "SUB" && !file.Folder {
			t.Error("SUB is not a folder")
		}
		if file.ID == "TXT" && (file.Size != 5 || file.Modified.IsZero()) {
			t.Errorf("TXT = %+v", file)
		}
	}
	if _, err := a.List(context.Background(), f.conn(nil), "x' or 'a"); err == nil {
		t.Error("bad id accepted")
	}
}

func TestRead(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	c := f.conn(nil)
	for id, want := range map[string]string{"DOC": "plan text", "SHEET": "a,b\n1,2\n", "TXT": "hello"} {
		file, b, err := a.Read(context.Background(), c, id, connectors.MaxReadBytes)
		if err != nil || string(b) != want {
			t.Errorf("%s: %q, %v", id, b, err)
		}
		if id == "SHEET" && file.MimeType != "text/csv" {
			t.Errorf("sheet read as %q", file.MimeType)
		}
	}
	for id, want := range map[string]string{"SUB": "is a folder", "BIG": "larger than 25 MiB", "FORM": "cannot be read", "NOPE": "404"} {
		if _, _, err := a.Read(context.Background(), c, id, connectors.MaxReadBytes); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", id, err)
		}
	}
}

func TestWrite(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	c := f.conn(nil)
	file, err := a.Write(context.Background(), c, "", "report.md", "text/markdown", []byte("# r"))
	if err != nil || file.ID != "NEW" {
		t.Fatalf("Write new = %+v, %v", file, err)
	}
	up := f.uploads[0]
	if up.method != "POST" || up.meta["name"] != "report.md" || up.meta["parents"].([]any)[0] != "root" || up.content != "# r" || up.contentType != "text/markdown" {
		t.Errorf("upload = %+v", up)
	}
	// The same name again replaces the file.
	if _, err := a.Write(context.Background(), c, "ROOTID", "notes.txt", "text/plain", []byte("new")); err != nil {
		t.Fatal(err)
	}
	up = f.uploads[1]
	if up.method != "PATCH" || up.path != "/upload/drive/v3/files/TXT" || len(up.meta) != 0 || up.content != "new" {
		t.Errorf("upload = %+v", up)
	}
	// Names are quoted in the query.
	_, _ = a.Write(context.Background(), c, "ROOTID", `it's`, "text/plain", []byte("x"))
	if q := f.queries[len(f.queries)-1]; !strings.Contains(q, `name = 'it\'s'`) {
		t.Errorf("query = %s", q)
	}
}

func TestChanges(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	c := f.conn(nil)
	files, cursor, err := a.Changes(context.Background(), c, "", "")
	if err != nil || len(files) != 0 || cursor != "100" {
		t.Fatalf("start = %v, %q, %v", files, cursor, err)
	}
	files, cursor, err = a.Changes(context.Background(), c, "", cursor)
	if err != nil || cursor != "102" {
		t.Fatalf("changes = %q, %v", cursor, err)
	}
	// Both changes to TXT are listed, across the two pages.
	if len(files) != 3 || files[0].ID != "TXT" || files[1].ID != "DOC" || files[2].ID != "TXT" || files[2].Modified.Day() != 3 {
		t.Errorf("files = %+v", files)
	}
	files, _, err = a.Changes(context.Background(), c, "SUB", "100")
	if err != nil || len(files) != 1 || files[0].ID != "ELSE" {
		t.Errorf("subfolder = %+v, %v", files, err)
	}
	if _, _, err := a.Changes(context.Background(), c, "", "bad"); err == nil || !strings.Contains(err.Error(), "Invalid page token") {
		t.Errorf("err = %v", err)
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	if err := a.Check(context.Background(), f.conn(nil)); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(context.Background(), f.conn(map[string]string{"shared_drive_id": "SHARED"})); err != nil {
		t.Fatal(err)
	}
	err := a.Check(context.Background(), f.conn(map[string]string{"shared_drive_id": "OTHER"}))
	if err == nil || !strings.Contains(err.Error(), "Shared drive not found") {
		t.Errorf("err = %v", err)
	}
	err = a.Check(context.Background(), f.conn(map[string]string{"subject": "outsider@x.ch"}))
	if err == nil || !strings.Contains(err.Error(), "unauthorized_client") || !strings.Contains(err.Error(), "domain-wide delegation") {
		t.Errorf("err = %v", err)
	}
}

// A person's calls carry their token, without signing in as the service
// account.
func TestPerUser(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	user := connectors.Connection{
		Kind: connectors.GoogleDrive, PerUser: true, Token: "drive-token",
		Config: map[string]string{"client_id": "cid"}, Secrets: map[string]string{"client_secret": "pw"},
	}
	if files, err := a.List(context.Background(), user, ""); err != nil || len(files) != 6 {
		t.Fatalf("List = %+v, %v", files, err)
	}
	user.Token = ""
	if _, err := a.List(context.Background(), user, ""); err == nil {
		t.Error("a call for nobody passed")
	}
	if fields := a.SignIn().Fields; fields[len(fields)-1].Name != "shared_drive_id" {
		t.Errorf("sign-in fields = %+v", fields)
	}
}
