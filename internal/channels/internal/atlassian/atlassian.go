// Package atlassian holds what the Jira and Confluence adapters share: the
// site and its credentials, calling the REST API, reading its errors, and
// keeping a JQL or CQL query within a binding's project or space.
package atlassian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// Fields are the settings of both kinds. product names the service, such as
// Jira, for the help texts.
func Fields(product string) []channels.Field {
	return []channels.Field{
		{
			Name: "base_url", Label: "Site URL", Required: true, Endpoint: true,
			Help: "Your Atlassian site, such as https://acme.atlassian.net, or the address of your " + product + " Data Center.",
		},
		{
			Name: "email", Label: "Email",
			Help: "Only for Atlassian Cloud: the email of the account the API token belongs to. Empty for a Data Center personal access token.",
		},
		{
			Name: "api_token", Label: "API token", Secret: true, Required: true,
			Help: "An API token on Atlassian Cloud, or a personal access token on Data Center.",
		},
	}
}

// Site is a connection's site and credentials.
type Site struct {
	// Base is the site's address as entered, without a trailing slash.
	Base  string
	Email string
	token string
}

// Cloud is a site on Atlassian Cloud. Only Cloud pairs an email with its
// API token; Data Center takes a personal access token alone.
func (s Site) Cloud() bool { return s.Email != "" }

// Open reads a connection's site and credentials.
func Open(conn channels.Connection) (Site, error) {
	s := Site{Email: web.Value(conn, "email"), token: web.Value(conn, "api_token")}
	if s.token == "" {
		return s, errors.New("the API token is missing")
	}
	base, ok := web.BaseURL(web.Value(conn, "base_url"))
	if !ok || base == "" {
		return s, errors.New("the site URL must be https://, such as https://acme.atlassian.net")
	}
	s.Base = base
	return s, nil
}

// Client calls one Atlassian product.
type Client struct {
	hc *http.Client
	// Service names the product in errors, such as "Jira".
	Service string
}

// New makes a client. The site is the organisation's own setting, so
// redirects are followed only on it.
func New(opts channels.Options, service string) *Client {
	return &Client{hc: web.SameServerClient(opts), Service: service}
}

// Call sends in as JSON to the full URL u and decodes the answer into out,
// unless out is nil.
func (c *Client) Call(ctx context.Context, s Site, method, u string, in, out any) error {
	req, err := web.NewJSONRequest(ctx, method, u, in)
	if err != nil {
		return err
	}
	if s.Cloud() {
		req.SetBasicAuth(s.Email, s.token)
	} else {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	req.Header.Set("Accept", "application/json")
	err = web.DoJSON(c.hc, req, c.Service, 16<<20, c.fail, out)
	if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		// Usually a sign-in page or another server's HTML.
		return fmt.Errorf("%s answered with something other than JSON; check the site URL", c.Service)
	}
	return err
}

// fail reads both products' error bodies: Jira's errorMessages and errors
// by field, and Confluence's message.
func (c *Client) fail(status int, b []byte) error {
	var e struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
		Message       string            `json:"message"`
	}
	_ = json.Unmarshal(b, &e)
	msgs := e.ErrorMessages
	fields := make([]string, 0, len(e.Errors))
	for k := range e.Errors {
		fields = append(fields, k)
	}
	slices.Sort(fields)
	for _, k := range fields {
		msgs = append(msgs, k+": "+e.Errors[k])
	}
	if e.Message != "" {
		msgs = append(msgs, e.Message)
	}
	return &web.APIError{Service: c.Service, Status: status, Message: strings.Join(msgs, "; ")}
}

// CheckError explains why a site refused the "who am I" call made when a
// connector is saved.
func (c *Client) CheckError(s Site, err error) error {
	switch web.Status(err) {
	case http.StatusUnauthorized:
		if s.Cloud() {
			return fmt.Errorf("%s does not accept the email and API token; check both, and that the token has not expired (%w)", c.Service, err)
		}
		return fmt.Errorf("%s does not accept the personal access token; check it has not expired, or enter the email if this is Atlassian Cloud (%w)", c.Service, err)
	case http.StatusForbidden:
		return fmt.Errorf("%s refused the account; check it may use %s, and sign in once in a browser in case it must solve a CAPTCHA (%w)", c.Service, c.Service, err)
	case http.StatusNotFound:
		return fmt.Errorf("found no %s at %s; check the site URL (%w)", c.Service, s.Base, err)
	}
	return err
}

// Run makes a tool's function from one that takes the site and the
// decoded arguments.
func Run[T any](f func(ctx context.Context, s Site, target string, in T) (any, error)) apitool.Run {
	return func(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
		s, err := Open(conn)
		if err != nil {
			return nil, err
		}
		in, err := apitool.Decode[T](args)
		if err != nil {
			return nil, err
		}
		return f(ctx, s, target, in)
	}
}

// Quote makes s a string in JQL or CQL.
func Quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

var orderBy = regexp.MustCompile(`(?i)\border\s+by\b`)

// Scope narrows a JQL or CQL query to clause, such as project = "WEB": the
// query goes in parentheses after "clause AND", and its ORDER BY after
// them. order is the ORDER BY for a query that has none, or empty. An
// empty clause leaves the query as it is.
//
// The query's quotes and parentheses must balance, or "x) OR (y" would
// close the parentheses and reach past clause.
func Scope(clause, query, order string) (string, error) {
	query = strings.TrimSpace(query)
	top, err := topLevel(query)
	if err != nil {
		return "", err
	}
	where := query
	ms := orderBy.FindAllStringIndex(query, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		if top[ms[i][0]] {
			where, order = strings.TrimSpace(query[:ms[i][0]]), query[ms[i][0]:]
			break
		}
	}
	var parts []string
	switch {
	case clause == "":
		parts = append(parts, where)
	case where == "":
		parts = append(parts, clause)
	default:
		parts = append(parts, clause+" AND ("+where+")")
	}
	if order != "" {
		parts = append(parts, order)
	}
	return strings.TrimSpace(strings.Join(parts, " ")), nil
}

// topLevel says, for each byte of q, whether it is outside quotes and
// parentheses.
func topLevel(q string) ([]bool, error) {
	top := make([]bool, len(q))
	var quote byte
	depth := 0
	for i := 0; i < len(q); i++ {
		ch := q[i]
		switch {
		case quote != 0:
			switch ch {
			case '\\':
				i++
			case quote:
				quote = 0
			}
			continue
		case ch == '"' || ch == '\'':
			quote = ch
		case ch == '(':
			depth++
		case ch == ')':
			depth--
			if depth < 0 {
				return nil, errors.New("the query closes a parenthesis it did not open")
			}
		}
		top[i] = quote == 0 && depth == 0
	}
	if quote != 0 {
		return nil, errors.New("the query has a quote that is not closed")
	}
	if depth != 0 {
		return nil, errors.New("the query has a parenthesis that is not closed")
	}
	return top, nil
}

// Cut trims s to n characters, saying so, so a long text does not fill the
// model's context.
func Cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "\n[… cut; the rest is longer than this tool returns]"
}
