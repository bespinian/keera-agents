package onedrive

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

const drive = "b!drive"

type fake struct {
	srv        *httptest.Server
	tokenCalls atomic.Int32
	mu         sync.Mutex
	items      map[string]string // id -> JSON
	content    map[string][]byte
	puts       []put
	deltaGone  bool
}

type put struct {
	path, contentType, body string
}

func docx(t *testing.T) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		`<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:tab/><w:t xml:space="preserve"> world</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Second &amp; last</w:t></w:r></w:p></w:body></w:document>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newFake(t *testing.T) *fake {
	f := &fake{
		items: map[string]string{
			"TXT":  `{"id":"TXT","name":"notes.txt","size":5,"lastModifiedDateTime":"2026-10-02T10:00:00Z","webUrl":"https://x/notes","file":{"mimeType":"text/plain"}}`,
			"DOCX": `{"id":"DOCX","name":"Plan.docx","size":100,"file":{"mimeType":"application/vnd.openxmlformats-officedocument.wordprocessingml.document"}}`,
			"XLSX": `{"id":"XLSX","name":"Budget.xlsx","size":4,"file":{"mimeType":"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}`,
			"BIG":  `{"id":"BIG","name":"video.mp4","size":999999999,"file":{"mimeType":"video/mp4"}}`,
			"SUB":  `{"id":"SUB","name":"Archive","folder":{"childCount":1}}`,
		},
		content: map[string][]byte{"TXT": []byte("hello"), "DOCX": docx(t), "XLSX": []byte("PK\x03\x04")},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		_ = r.ParseForm()
		if r.PathValue("tenant") != "tenant-1" || r.Form.Get("scope") != "https://graph.microsoft.com/.default" {
			t.Errorf("tenant %q scope %q", r.PathValue("tenant"), r.Form.Get("scope"))
		}
		if r.Form.Get("client_secret") != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"graph-token","expires_in":3600}`))
	})
	g := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer graph-token" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"InvalidAuthenticationToken","message":"Access token is empty."}}`))
				return
			}
			if r.PathValue("drive") != drive {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"itemNotFound","message":"The resource could not be found."}}`))
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1.0/drives/{drive}", g(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"b!drive","name":"Documents","driveType":"documentLibrary"}`))
	}))
	mux.HandleFunc("GET /v1.0/drives/{drive}/root", g(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ROOTID"}`))
	}))
	mux.HandleFunc("GET /v1.0/drives/{drive}/items/{id}/children", g(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "root" {
			_, _ = w.Write([]byte(`{"value":[]}`))
			return
		}
		if r.URL.Query().Get("$skiptoken") == "" {
			_, _ = w.Write([]byte(`{"value":[` + f.items["TXT"] + `,` + f.items["SUB"] + `],"@odata.nextLink":"http://` + r.Host + `/v1.0/drives/b!drive/items/root/children?$skiptoken=2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":[` + f.items["DOCX"] + `]}`))
	}))
	mux.HandleFunc("GET /v1.0/drives/{drive}/items/{id}", g(func(w http.ResponseWriter, r *http.Request) {
		it, ok := f.items[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"itemNotFound","message":"The resource could not be found."}}`))
			return
		}
		_, _ = w.Write([]byte(it))
	}))
	mux.HandleFunc("GET /v1.0/drives/{drive}/items/{id}/content", g(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/download/"+r.PathValue("id"), http.StatusFound)
	}))
	mux.HandleFunc("GET /download/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = w.Write(f.content[r.PathValue("id")])
	})
	mux.HandleFunc("PUT /v1.0/drives/{drive}/items/{rest...}", g(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.puts = append(f.puts, put{r.URL.EscapedPath(), r.Header.Get("Content-Type"), string(b)})
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"NEW","name":"report.md","size":3,"file":{"mimeType":"text/markdown"}}`))
	}))
	mux.HandleFunc("GET /v1.0/drives/{drive}/root/delta", g(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host + "/v1.0/drives/b!drive/root/delta"
		q := r.URL.Query()
		switch {
		case q.Get("token") == "latest":
			_, _ = w.Write([]byte(`{"value":[],"@odata.deltaLink":"` + base + `?token=T1"}`))
		case q.Get("token") == "T1" && f.deltaGone:
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":{"code":"resyncRequired","message":"Resync required."}}`))
		case q.Get("token") == "T1":
			_, _ = w.Write([]byte(`{"value":[
				{"id":"ROOTID","name":"root","folder":{}},
				{"id":"TXT","name":"notes.txt","file":{"mimeType":"text/plain"},"parentReference":{"id":"ROOTID"},"lastModifiedDateTime":"2026-10-02T10:00:00Z"},
				{"id":"SUB","name":"Archive","folder":{},"parentReference":{"id":"ROOTID"}},
				{"id":"GONE","name":"old.txt","deleted":{"state":"deleted"},"parentReference":{"id":"ROOTID"}},
				{"id":"DEEP","name":"deep.txt","file":{"mimeType":"text/plain"},"parentReference":{"id":"SUB"}}
			],"@odata.nextLink":"` + base + `?token=T1b"}`))
		case q.Get("token") == "T1b":
			_, _ = w.Write([]byte(`{"value":[
				{"id":"TXT","name":"notes.txt","file":{"mimeType":"text/plain"},"parentReference":{"id":"ROOTID"},"lastModifiedDateTime":"2026-10-03T10:00:00Z"}
			],"@odata.deltaLink":"` + base + `?token=T2"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) adapter() *Adapter {
	return New(channels.Options{HTTP: f.srv.Client(), Endpoints: map[string]string{"login": f.srv.URL + "/login", "graph": f.srv.URL + "/v1.0"}})
}

func conn(secret, driveID string) channels.Connection {
	return channels.Connection{
		ID: "c1", Kind: channels.OneDrive,
		Config:  map[string]string{"tenant_id": "tenant-1", "client_id": "client", "drive_id": driveID},
		Secrets: map[string]string{"client_secret": secret},
	}
}

func TestList(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	files, err := a.List(context.Background(), conn("pw", drive), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[0].ID != "TXT" || files[0].WebURL != "https://x/notes" || files[0].MimeType != "text/plain" || !files[1].Folder || files[2].ID != "DOCX" {
		t.Errorf("files = %+v", files)
	}
	if f.tokenCalls.Load() != 1 {
		t.Errorf("token fetched %d times", f.tokenCalls.Load())
	}
	if _, err := a.List(context.Background(), conn("pw", drive), "../x"); err == nil {
		t.Error("bad id accepted")
	}
}

func TestRead(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	c := conn("pw", drive)
	file, b, err := a.Read(context.Background(), c, "TXT")
	if err != nil || string(b) != "hello" || file.Name != "notes.txt" {
		t.Errorf("TXT = %+v %q %v", file, b, err)
	}
	file, b, err = a.Read(context.Background(), c, "DOCX")
	if err != nil || string(b) != "Hello\t world\nSecond & last" || file.MimeType != "text/plain" {
		t.Errorf("DOCX = %+v %q %v", file, b, err)
	}
	// Other Office files come back as stored.
	if _, b, err = a.Read(context.Background(), c, "XLSX"); err != nil || string(b) != "PK\x03\x04" {
		t.Errorf("XLSX = %q %v", b, err)
	}
	for id, want := range map[string]string{"SUB": "is a folder", "BIG": "larger than 4 MiB", "NOPE": "itemNotFound"} {
		if _, _, err := a.Read(context.Background(), c, id); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", id, err)
		}
	}
}

func TestWrite(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	c := conn("pw", drive)
	file, err := a.Write(context.Background(), c, "SUB", "my report.md", "text/markdown", []byte("# r"))
	if err != nil || file.ID != "NEW" {
		t.Fatalf("Write = %+v, %v", file, err)
	}
	p := f.puts[0]
	if p.path != "/v1.0/drives/b%21drive/items/SUB:/my%20report.md:/content" || p.contentType != "text/markdown" || p.body != "# r" {
		t.Errorf("put = %+v", p)
	}
}

func TestChanges(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	c := conn("pw", drive)
	files, cursor, err := a.Changes(context.Background(), c, "", "")
	if err != nil || len(files) != 0 || !strings.HasSuffix(cursor, "token=T1") {
		t.Fatalf("start = %v %q %v", files, cursor, err)
	}
	files, next, err := a.Changes(context.Background(), c, "", cursor)
	if err != nil || !strings.HasSuffix(next, "token=T2") {
		t.Fatalf("changes = %q %v", next, err)
	}
	if len(files) != 1 || files[0].ID != "TXT" || files[0].Modified.Day() != 3 {
		t.Errorf("files = %+v", files)
	}
	files, _, err = a.Changes(context.Background(), c, "SUB", cursor)
	if err != nil || len(files) != 1 || files[0].ID != "DEEP" {
		t.Errorf("subfolder = %+v %v", files, err)
	}
	// A cursor that leaves Graph is refused, so the token stays on Graph.
	if _, _, err := a.Changes(context.Background(), c, "", "https://evil.example/v1.0/x"); err == nil {
		t.Error("foreign cursor accepted")
	}
	// A forgotten cursor starts over from now.
	f.deltaGone = true
	files, next, err = a.Changes(context.Background(), c, "", cursor)
	if err != nil || len(files) != 0 || !strings.HasSuffix(next, "token=T1") {
		t.Errorf("resync = %v %q %v", files, next, err)
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	a := f.adapter()
	if err := a.Check(context.Background(), conn("pw", drive)); err != nil {
		t.Fatal(err)
	}
	err := a.Check(context.Background(), conn("wrong", drive))
	if err == nil || !strings.Contains(err.Error(), "Microsoft refused the credentials (invalid_client") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("err = %v", err)
	}
	err = a.Check(context.Background(), conn("pw", "b!other"))
	if err == nil || !strings.Contains(err.Error(), "itemNotFound") {
		t.Errorf("err = %v", err)
	}
	if err := a.Check(context.Background(), conn("pw", "")); err == nil || !strings.Contains(err.Error(), "drive ID is missing") {
		t.Errorf("err = %v", err)
	}
}

func TestDocxTextRejectsOtherZips(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	_, _ = zw.Create("other.xml")
	_ = zw.Close()
	if _, err := docxText(buf.Bytes()); err == nil {
		t.Error("zip without a document accepted")
	}
	if _, err := docxText([]byte("not a zip")); err == nil {
		t.Error("garbage accepted")
	}
}
