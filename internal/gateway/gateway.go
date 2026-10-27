// Package gateway calls Keera Gateway's control API.
//
// Two credentials are used. A person's sign-in, redeemed through the
// gateway's sign-in hand-over, acts as that person: who they are, their
// organisations. The service key (KEERA_AGENTS_SERVICE_KEY) works only on
// /control/v1/service/: it mints and revokes agents' keys, looks people up and
// reads what an agent may use. Neither is an operator credential.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ControlPrefix = "/control"
	// clientName names keera-agents' requests in the gateway's log.
	clientName = "keera-agents"
	// noAnswer is shown when the gateway is unreachable or its answer
	// unreadable.
	noAnswer = "Keera Gateway did not answer; try again in a moment"

	callTimeout = 20 * time.Second
)

type Client struct {
	base       string
	serviceKey string
	http       *http.Client
}

// New returns a client for the gateway at base, such as http://gateway:8080.
func New(base, serviceKey string) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 16
	return &Client{
		base: strings.TrimRight(base, "/"), serviceKey: serviceKey,
		http: &http.Client{
			Transport: t,
			// A redirect means a proxy's sign-in page. Following it would drop
			// the token and parse HTML as JSON.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Error is a refusal from the gateway, in its own words.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsNotFound reports whether the gateway itself said that what was asked for
// does not exist. A 404 without its error code came from something in
// between, such as a proxy during a redeploy, and proves nothing.
func IsNotFound(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Status == http.StatusNotFound && e.Code == "not_found"
}

// IsUnauthorized reports whether a person's sign-in is no longer valid.
func IsUnauthorized(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Status == http.StatusUnauthorized && e.Code == "unauthenticated"
}

// ------------------------------------------------------------ sign-in

type AuthConfig struct {
	SSO       bool       `json:"sso"`
	Providers []Provider `json:"providers"`
	Passkeys  bool       `json:"passkeys"`
}

type Provider struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Token is a sign-in redeemed from a one-time code.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Me struct {
	UserID      string       `json:"user_id"`
	Email       string       `json:"email"`
	Name        string       `json:"name"`
	Role        string       `json:"role"`
	OrgID       string       `json:"org_id"`
	OrgName     string       `json:"org_name"`
	CanAdminOrg bool         `json:"can_admin_org"`
	Orgs        []Membership `json:"orgs"`
	// ChatURL is Keera Chat's address, when the gateway has one connected.
	ChatURL string `json:"chat_url"`
}

type Membership struct {
	ID      string `json:"org_id"`
	Name    string `json:"org_name"`
	Invited bool   `json:"invited"`
}

func (c *Client) AuthConfig(ctx context.Context) (AuthConfig, error) {
	var out AuthConfig
	err := c.call(ctx, "GET", "/auth/config", "", nil, &out)
	return out, err
}

// Redeem trades a sign-in's one-time code for a token. The verifier proves
// keera-agents started that sign-in.
func (c *Client) Redeem(ctx context.Context, code, verifier string) (Token, error) {
	var out Token
	err := c.call(ctx, "POST", "/auth/cli/token", "", map[string]string{
		"code": code, "verifier": verifier, "label": "Keera Agents",
	}, &out)
	return out, err
}

func (c *Client) Logout(ctx context.Context, token string) error {
	return c.call(ctx, "POST", "/auth/logout", token, nil, nil)
}

func (c *Client) Me(ctx context.Context, token string) (Me, error) {
	var out Me
	err := c.call(ctx, "GET", "/v1/me", token, nil, &out)
	return out, err
}

func (c *Client) SwitchOrg(ctx context.Context, token, orgID string) error {
	return c.call(ctx, "POST", "/v1/me/org", token, map[string]string{"org_id": orgID}, nil)
}

// ------------------------------------------------------------ service

// Key is a key minted for an agent. Secret is only set when it was minted.
type Key struct {
	ID     string `json:"id"`
	Secret string `json:"key"`
}

type KeyRequest struct {
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	// BudgetMicros is a monthly budget on the key itself; 0 is none.
	BudgetMicros int64 `json:"budget_micros,omitempty"`
}

func (c *Client) MintKey(ctx context.Context, in KeyRequest) (Key, error) {
	var out Key
	err := c.call(ctx, "POST", "/v1/service/keys", c.serviceKey, in, &out)
	return out, err
}

// RevokeKey treats a key that is already gone as revoked. The organisation
// is named so a mix-up between tenants is refused, not carried out.
func (c *Client) RevokeKey(ctx context.Context, orgID, id string) error {
	err := c.call(ctx, "DELETE", "/v1/service/keys/"+url.PathEscape(id)+"?"+url.Values{"org_id": {orgID}}.Encode(),
		c.serviceKey, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// User is a person's membership in one organisation.
type User struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

// UserByEmail finds a member of an organisation. A person who is not one
// answers an *Error with status 404.
func (c *Client) UserByEmail(ctx context.Context, orgID, email string) (User, error) {
	return c.user(ctx, orgID, "email", email)
}

func (c *Client) UserByID(ctx context.Context, orgID, id string) (User, error) {
	return c.user(ctx, orgID, "id", id)
}

// user looks a member up by one field, id or email.
func (c *Client) user(ctx context.Context, orgID, field, value string) (User, error) {
	var out User
	err := c.call(ctx, "GET", "/v1/service/users?"+url.Values{"org_id": {orgID}, field: {value}}.Encode(),
		c.serviceKey, nil, &out)
	return out, err
}

// Catalog is what a key in one project may use.
type Catalog struct {
	ProjectID  string       `json:"project_id"`
	Projects   []Project    `json:"projects"`
	Models     []CatalogRef `json:"models"`
	MCPServers []MCPRef     `json:"mcp_servers"`
}

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type CatalogRef struct {
	Alias string `json:"alias"`
	Kind  string `json:"kind"`
}

type MCPRef struct {
	Alias string `json:"alias"`
}

func (c *Client) Catalog(ctx context.Context, orgID, projectID string) (Catalog, error) {
	q := url.Values{"org_id": {orgID}}
	if projectID != "" {
		q.Set("project_id", projectID)
	}
	var out Catalog
	err := c.call(ctx, "GET", "/v1/service/catalog?"+q.Encode(), c.serviceKey, nil, &out)
	return out, err
}

// ------------------------------------------------------------ plumbing

func (c *Client) call(ctx context.Context, method, path, token string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+ControlPrefix+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Keera-Client", clientName)
	req.Header.Set("User-Agent", clientName)

	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return &Error{Status: http.StatusBadGateway, Code: "gateway_unreachable", Message: noAnswer}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode/100 != 2 {
		var env struct {
			Error struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&env)
		e := &Error{Status: res.StatusCode, Message: env.Error.Message}
		if s, ok := env.Error.Code.(string); ok {
			e.Code = s
		}
		if e.Message == "" {
			e.Message = fmt.Sprintf("Keera Gateway answered %s to %s %s", res.Status, method, path)
		}
		return e
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("reading Keera Gateway's answer to %s %s: %w", method, path, err)
	}
	return nil
}
