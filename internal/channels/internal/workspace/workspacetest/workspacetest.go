// Package workspacetest fakes Google's token endpoint and the parts of Drive
// the Google Workspace adapters use, for their tests. Each test adds the
// API it needs to Mux.
package workspacetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/jwttest"
)

// Outsider is a user the fake refuses to sign in as.
const Outsider = "outsider@x.ch"

// The OAuth client people sign in through, and what signing in gives.
const (
	ClientID     = "cid.apps.googleusercontent.com"
	ClientSecret = "client-secret"
	Code         = "the-code"
	Refresh      = "refresh-1"
)

type Fake struct {
	Srv *httptest.Server
	Key *jwttest.Key
	// Files are Drive's files by id, as Drive answers them.
	Files map[string]map[string]any
	// Queries are the q of every files.list; Created every new file's
	// details.
	Queries []string
	Created []map[string]any
	// Scope is the scope of the last token asked for.
	Scope string

	mu  sync.Mutex
	mux *http.ServeMux
}

func New(t *testing.T) *Fake {
	f := &Fake{Key: jwttest.NewKey(t, "sa"), Files: map[string]map[string]any{}, mux: http.NewServeMux()}
	f.mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if grant := r.Form.Get("grant_type"); grant == "authorization_code" || grant == "refresh_token" {
			f.signIn(t, w, r)
			return
		}
		c := jwttest.Claims(t, r.Form.Get("assertion"), &f.Key.Private.PublicKey)
		f.mu.Lock()
		f.Scope, _ = c["scope"].(string)
		f.mu.Unlock()
		if c["sub"] == Outsider {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens using this method."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ws-token","expires_in":3600}`))
	})
	f.Handle("GET /drive/v3/about", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"emailAddress":"sa@p.iam.gserviceaccount.com"}}`))
	})
	f.Handle("GET /drive/v3/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		file, ok := f.Files[r.PathValue("id")]
		if !ok {
			Error(w, http.StatusNotFound, "File not found.")
			return
		}
		_ = json.NewEncoder(w).Encode(file)
	})
	f.Handle("GET /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		f.Queries = append(f.Queries, q)
		var out []map[string]any
		for _, file := range f.Files {
			if strings.Contains(q, "'"+file["mimeType"].(string)+"'") {
				out = append(out, file)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": out})
	})
	f.Handle("POST /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.Created = append(f.Created, in)
		in["id"] = "NEW"
		f.Files["NEW"] = in
		_ = json.NewEncoder(w).Encode(in)
	})
	f.Srv = httptest.NewServer(f.mux)
	t.Cleanup(f.Srv.Close)
	return f
}

// signIn answers a person's sign-in with the code, and refreshes with the
// refresh token, as Google does. Each gives the fake's token.
func (f *Fake) signIn(t *testing.T, w http.ResponseWriter, r *http.Request) {
	refuse := func(code string) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"` + code + `","error_description":"refused"}`))
	}
	if r.Form.Get("client_id") != ClientID || r.Form.Get("client_secret") != ClientSecret {
		refuse("invalid_client")
		return
	}
	if r.Form.Get("grant_type") == "refresh_token" {
		if r.Form.Get("refresh_token") != Refresh {
			refuse("invalid_grant")
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ws-token","expires_in":3600}`))
		return
	}
	if r.Form.Get("code") != Code || r.Form.Get("code_verifier") == "" {
		refuse("invalid_grant")
		return
	}
	idToken := f.Key.Sign(t, map[string]any{"email": "ada@acme.com"})
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "ws-token", "expires_in": 3600, "refresh_token": Refresh, "id_token": idToken,
	})
}

// Handle serves pattern for callers with the fake's token, one at a time.
func (f *Fake) Handle(pattern string, h http.HandlerFunc) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ws-token" {
			Error(w, http.StatusUnauthorized, "Request had invalid authentication credentials.")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		h(w, r)
	})
}

// Options point every Google endpoint at the fake.
func (f *Fake) Options() channels.Options {
	e := map[string]string{}
	for _, name := range []string{"token", "drive", "docs", "sheets", "slides", "calendar", "gmail"} {
		e[name] = f.Srv.URL
	}
	e["token"] = f.Srv.URL + "/token"
	e["auth"] = "https://accounts.example/auth"
	return channels.Options{HTTP: f.Srv.Client(), Endpoints: e}
}

// UserConn is a connection people sign in to, with the fake's client. With
// a token, it is a call for the person it belongs to.
func (f *Fake) UserConn(kind channels.Kind, token string) channels.Connection {
	return channels.Connection{
		Kind: kind, PerUser: true, Token: token, Account: "ada@acme.com",
		Config:  map[string]string{"client_id": ClientID},
		Secrets: map[string]string{"client_secret": ClientSecret},
	}
}

// Conn is a connection with the fake's key, acting as subject if it is not
// empty.
func (f *Fake) Conn(kind channels.Kind, subject string) channels.Connection {
	return channels.Connection{
		Kind:    kind,
		Config:  map[string]string{"subject": subject},
		Secrets: map[string]string{"service_account_json": f.Key.ServiceAccountJSON("sa@p.iam.gserviceaccount.com")},
	}
}

// Error answers as Google APIs do when they refuse a call.
func Error(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": status, "message": msg, "status": strings.ToUpper(strings.ReplaceAll(http.StatusText(status), " ", "_")),
	}})
}

// JSON decodes a request's body.
func JSON(r *http.Request) map[string]any {
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	return in
}
