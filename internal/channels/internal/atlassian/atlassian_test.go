package atlassian

import (
	"strings"
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
	s, err := Open(conn("https://acme.atlassian.net/", "ada@acme.test", "tok"))
	if err != nil || s.Base != "https://acme.atlassian.net" || !s.Cloud() {
		t.Fatalf("Open = %+v, %v", s, err)
	}
	if s, _ := Open(conn("https://jira.acme.test", "", "pat")); s.Cloud() {
		t.Fatal("a token without email is Cloud")
	}
	for _, c := range []channels.Connection{
		conn("http://acme.atlassian.net", "", "tok"),
		conn("", "", "tok"),
		conn("https://acme.atlassian.net", "", ""),
	} {
		if _, err := Open(c); err == nil {
			t.Errorf("Open(%v) passed", c.Config)
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

func TestCut(t *testing.T) {
	if Cut("short", 10) != "short" {
		t.Fatal("short text was cut")
	}
	if got := Cut("ääääää", 3); !strings.HasPrefix(got, "äää\n[") {
		t.Fatalf("Cut = %q", got)
	}
}
