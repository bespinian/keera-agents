// Package gateway calls Keera Gateway's control API, and its model API for
// the models agents use as tools.
//
// Three credentials are used. A person's sign-in, redeemed through the
// gateway's sign-in hand-over, acts as that person: who they are, their
// organisations. The service key (KEERA_AGENTS_SERVICE_KEY) works only on
// /control/v1/service/: it mints and revokes agents' keys, looks people up and
// reads what an agent may use. An agent's own key calls models for it, so
// its budget and the gateway's rules apply. None is an operator credential.
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
	"slices"
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
	// observe, if set, hears of every call: what it was, how it went and
	// how long it took.
	observe func(op, outcome string, d time.Duration)
}

// Observe sets what hears of every call, such as the metrics. Set it before
// the client is used.
func (c *Client) Observe(f func(op, outcome string, d time.Duration)) { c.observe = f }

// opName names a call for metrics: its method and path, without ids or the
// query, so the names stay few.
func opName(method, path string) string {
	path, _, _ = strings.Cut(path, "?")
	if strings.HasPrefix(path, "/v1/service/keys/") {
		path = "/v1/service/keys/{id}"
	}
	return method + " " + path
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
	// AllowedTools are the MCP servers the key may call. nil is left out,
	// which allows every one the project allows, and works with a gateway
	// too old to know the field. Empty allows none.
	AllowedTools []string `json:"allowed_tools,omitzero"`
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

// Members lists an organisation's active members. It calls with a person's
// own token, as the service key cannot list people: only administrators get
// an answer.
func (c *Client) Members(ctx context.Context, token, orgID string) ([]User, error) {
	var out struct {
		Data []struct {
			User
			DisabledAt *time.Time `json:"disabled_at"`
		} `json:"data"`
	}
	if err := c.call(ctx, "GET", "/v1/users?"+url.Values{"org_id": {orgID}}.Encode(), token, nil, &out); err != nil {
		return nil, err
	}
	users := []User{}
	for _, u := range out.Data {
		if u.DisabledAt == nil {
			users = append(users, User{ID: u.ID, Email: u.Email, Name: u.Name})
		}
	}
	return users, nil
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
	ProjectID string       `json:"project_id"`
	Projects  []Project    `json:"projects"`
	Models    []CatalogRef `json:"models"`
	// TranscriptionModels turn audio into text. An older gateway sends
	// none.
	TranscriptionModels []CatalogRef `json:"transcription_models"`
	MCPServers          []MCPRef     `json:"mcp_servers"`
}

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type CatalogRef struct {
	Alias string `json:"alias"`
	Kind  string `json:"kind"`
	// Capabilities say what the model can do beyond text, such as "tools".
	// Nil means the gateway is too old to say; it passes on as null, so the
	// panel can tell.
	Capabilities []string `json:"capabilities"`
}

// CallsTools reports whether the model calls tools, which agents and coding
// agents need. A gateway too old to say is taken to mean yes.
func (m CatalogRef) CallsTools() bool {
	return m.Capabilities == nil || slices.Contains(m.Capabilities, "tools")
}

// ReadsImages reports whether the model reads images. A gateway too old to
// say is taken to mean no.
func (m CatalogRef) ReadsImages() bool {
	return slices.Contains(m.Capabilities, "vision")
}

// Model finds a model or router by its alias.
func (c Catalog) Model(alias string) (CatalogRef, bool) {
	i := slices.IndexFunc(c.Models, func(m CatalogRef) bool { return m.Alias == alias })
	if i < 0 {
		return CatalogRef{}, false
	}
	return c.Models[i], true
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

// Credit is whether an organisation may run machines, as the gateway, which
// holds its credit, decides. See docs/billing.md.
type Credit struct {
	// Payments says organisations pay in advance on this gateway.
	Payments      bool  `json:"payments"`
	BalanceMicros int64 `json:"balance_micros"`
	// Refusal is set when the organisation may not run machines now.
	Refusal *Refusal `json:"refusal"`
}

// Refusal says why, in words for the people in the organisation.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Client) Credit(ctx context.Context, orgID string) (Credit, error) {
	var out Credit
	err := c.call(ctx, "GET", "/v1/service/billing?"+url.Values{"org_id": {orgID}}.Encode(),
		c.serviceKey, nil, &out)
	return out, err
}

// BillLine is one organisation's use of one item in one hour. ID makes
// sending it again harmless: the gateway records each id once.
type BillLine struct {
	ID             string    `json:"id"`
	OrgID          string    `json:"org_id"`
	TS             time.Time `json:"ts"`
	Item           string    `json:"item"`
	Unit           string    `json:"unit"`
	QuantityMicros int64     `json:"quantity_micros"`
	Micros         int64     `json:"micros"`
}

// MaxBillLines is the most lines the gateway takes in one call.
const MaxBillLines = 1000

// Bill adds lines to the organisations' bills, and takes them from their
// credit when they pay in advance.
func (c *Client) Bill(ctx context.Context, lines []BillLine) error {
	return c.call(ctx, "POST", "/v1/service/billing/lines", c.serviceKey,
		map[string]any{"lines": lines}, nil)
}

// ------------------------------------------------------------ plumbing

func (c *Client) call(ctx context.Context, method, path, token string, in, out any) error {
	return c.timed(ctx, callTimeout, opName(method, path), func(ctx context.Context) error {
		return c.send(ctx, method, path, token, in, out)
	})
}

// timed runs one call to the gateway within timeout, and observes how it
// went as op.
func (c *Client) timed(ctx context.Context, timeout time.Duration, op string, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	err := f(ctx)
	if c.observe != nil && !errors.Is(err, context.Canceled) {
		c.observe(op, outcome(err), time.Since(start))
	}
	return err
}

// outcome says how a call went: ok, 4xx or 5xx as the gateway answered,
// unreachable, or error for an answer it could not read.
func outcome(err error) string {
	e, ok := errors.AsType[*Error](err)
	switch {
	case err == nil:
		return "ok"
	case !ok:
		return "error"
	case e.Code == "gateway_unreachable":
		return "unreachable"
	default:
		return fmt.Sprintf("%dxx", e.Status/100)
	}
}

func (c *Client) send(ctx context.Context, method, path, token string, in, out any) error {
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
	return c.do(req, token, path, out)
}

// do sends req with token, to the control API or the model API, and reads
// the answer into out. path names the call in errors.
func (c *Client) do(req *http.Request, token, path string, out any) error {
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
		return readError(res, req.Method, path)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("reading Keera Gateway's answer to %s %s: %w", req.Method, path, err)
	}
	return nil
}

// readError turns the gateway's refusal into an *Error with its own message.
// Both its APIs answer in the same shape.
func readError(res *http.Response, method, path string) error {
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
