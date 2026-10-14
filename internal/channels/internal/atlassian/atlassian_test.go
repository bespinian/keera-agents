package atlassian

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestScope(t *testing.T) {
	const p = `project = "WEB"`
	for _, c := range []struct{ query, order, want string }{
		{"", "ORDER BY updated DESC", p + " ORDER BY updated DESC"},
		{"status = Done", "", p + " AND (status = Done)"},
		{"status = Done order by created", "ORDER BY updated DESC", p + " AND (status = Done) order by created"},
		{"ORDER BY rank", "", p + " ORDER BY rank"},
		{`summary ~ "order by" AND x = 1`, "", p + ` AND (summary ~ "order by" AND x = 1)`},
		{`(a = 1 ORDER BY b) OR c = 2`, "", p + ` AND ((a = 1 ORDER BY b) OR c = 2)`},
		{`summary ~ "say \"hi\" )"`, "", p + ` AND (summary ~ "say \"hi\" )")`},
	} {
		got, err := Scope(p, c.query, c.order)
		if err != nil || got != c.want {
			t.Errorf("Scope(%q) = %q, %v; want %q", c.query, got, err, c.want)
		}
	}
	// Each would close the parentheses and reach other projects.
	for _, bad := range []string{"a = 1) OR (project = OPS", "a = 1) OR project = OPS OR (b = 2", `x = "open`, "(a = 1"} {
		if got, err := Scope(p, bad, ""); err == nil {
			t.Errorf("Scope(%q) = %q, want an error", bad, got)
		}
	}
	if got, _ := Scope("", "status = Done", "ORDER BY updated DESC"); got != "status = Done ORDER BY updated DESC" {
		t.Errorf("no clause: %q", got)
	}
}

func TestOpen(t *testing.T) {
	conn := func(base, email, token string) channels.Connection {
		return channels.Connection{
			Config:  map[string]string{"base_url": base, "email": email},
			Secrets: map[string]string{"api_token": token},
		}
	}
	s, err := open(conn("https://acme.atlassian.net/", "ada@acme.test", "tok"))
	if err != nil || s.Base != "https://acme.atlassian.net" || !s.Cloud() {
		t.Fatalf("Open = %+v, %v", s, err)
	}
	if s, _ := open(conn("https://jira.acme.test", "", "pat")); s.Cloud() {
		t.Fatal("a token without email is Cloud")
	}
	for _, c := range []channels.Connection{
		conn("http://acme.atlassian.net", "", "tok"),
		conn("", "", "tok"),
		conn("https://acme.atlassian.net", "", ""),
	} {
		if _, err := open(c); err == nil {
			t.Errorf("open(%v) passed", c.Config)
		}
	}
}

func TestFail(t *testing.T) {
	c := &Client{Service: "Jira"}
	err := c.fail(400, []byte(`{"errorMessages":["Bad"],"errors":{"summary":"Required","issuetype":"Invalid"}}`))
	if want := "Jira answered 400 Bad Request: Bad; issuetype: Invalid; summary: Required"; err.Error() != want {
		t.Errorf("Jira error = %q", err)
	}
	c.Service = "Confluence"
	if err := c.fail(404, []byte(`{"statusCode":404,"message":"No space with key : X"}`)); !strings.HasSuffix(err.Error(), ": No space with key : X") {
		t.Errorf("Confluence error = %q", err)
	}
}

// gateway fakes Atlassian's sign-in and API gateway for a person's token.
func gateway(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_ = r.ParseForm()
			switch {
			case r.Form.Get("client_secret") != "pw":
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"Unauthorized"}`))
			case r.Form.Get("grant_type") == "authorization_code":
				_, _ = w.Write([]byte(`{"access_token":"user-tok","expires_in":3600,"refresh_token":"r1"}`))
			default:
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Unknown or invalid refresh token."}`))
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer user-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/oauth/token/accessible-resources":
			lookups.Add(1)
			_, _ = w.Write([]byte(`[{"id":"other","url":"https://else.atlassian.net"},{"id":"cloud-1","url":"https://acme.atlassian.net/"}]`))
		case "/me":
			_, _ = w.Write([]byte(`{"email":"ada@acme.ch"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &lookups
}

func TestPerUser(t *testing.T) {
	srv, lookups := gateway(t)
	c := New(channels.Options{HTTP: srv.Client(), Endpoints: map[string]string{"auth": srv.URL, "api": srv.URL}}, "Jira")
	ctx := context.Background()
	conn := channels.Connection{
		Kind: channels.Jira, PerUser: true,
		Config:  map[string]string{"base_url": "https://acme.atlassian.net", "client_id": "cid"},
		Secrets: map[string]string{"client_secret": "pw"},
	}
	accounts := c.Accounts("read:jira-work")

	if err := accounts.CheckClient(ctx, conn); err != nil {
		t.Errorf("CheckClient = %v", err)
	}
	u, err := accounts.SignInURL(conn, "https://k/cb", "st", "ch")
	q, _ := url.Parse(u)
	if err != nil || q.Query().Get("audience") != "api.atlassian.com" ||
		q.Query().Get("scope") != "read:jira-work read:me offline_access" {
		t.Errorf("SignInURL = %s, %v", u, err)
	}
	// Atlassian gives no ID token, so it is asked who signed in.
	g, err := accounts.Exchange(ctx, conn, "https://k/cb", "code", "v")
	if err != nil || g.Account != "ada@acme.ch" || g.Refresh != "r1" {
		t.Fatalf("Exchange = %+v, %v", g, err)
	}

	if _, err := c.Open(ctx, conn); err == nil {
		t.Error("opened without anyone's token")
	}
	conn.Token = "user-tok"
	for range 2 {
		s, err := c.Open(ctx, conn)
		if err != nil || s.APIBase() != srv.URL+"/ex/jira/cloud-1" || s.Base != "https://acme.atlassian.net" || !s.Cloud() {
			t.Fatalf("Open = %+v, %v", s, err)
		}
	}
	if lookups.Load() != 1 {
		t.Errorf("the cloud id was looked up %d times", lookups.Load())
	}
	conn.Config["base_url"] = "https://nope.atlassian.net"
	if _, err := c.Open(ctx, conn); err == nil || !strings.Contains(err.Error(), "not given access to https://nope.atlassian.net") {
		t.Errorf("a site not granted: %v", err)
	}
}
