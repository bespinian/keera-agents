// Package freshdesk lets agents work in a Freshdesk helpdesk: find and read
// tickets, create and update them, reply to customers and add notes. It
// calls API version 2 with an agent's API key. A binding names a group;
// every ticket call stays in it.
//
// Endpoints:
//   - "api": the helpdesk's address, such as https://acme.freshdesk.com,
//     in place of the connection's domain
package freshdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

var (
	// validSub is the helpdesk's name in acme.freshdesk.com.
	validSub = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// validID is the id of a ticket, group, agent or contact. It goes into
	// URLs.
	validID = regexp.MustCompile(`^[0-9]{1,20}$`)
)

var _ connectors.API = (*Adapter)(nil)

type Adapter struct {
	hc *http.Client
	// api replaces the connection's domain, for tests.
	api string
	apitool.Set
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{hc: web.SameServerClient(opts), api: web.Endpoint(opts, "api", "")}
	a.Set = a.toolSet()
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Freshdesk,
		Label:    "Freshdesk",
		Vendor:   "Freshworks",
		Category: "Support",
		Summary:  "Agents find, read, create and update tickets, reply to customers and add notes.",
		Fields: []connectors.Field{
			{
				Name: "domain", Label: "Domain", Required: true, Endpoint: true,
				Help: "Your helpdesk's address, such as acme.freshdesk.com.",
			},
			{
				Name: "api_key", Label: "API key", Secret: true, Required: true,
				Help: "The API key of the Freshdesk agent account Keera's agents work as.",
			},
		},
		Setup: "1. In Freshdesk, add an agent account for Keera's agents to work as, in the groups they work in. " +
			"Give it a role that may do what they should, such as reply to tickets.\n" +
			"2. Signed in as that account, open **Profile settings** and copy the **API key** here.\n" +
			"3. Enter your helpdesk's address, such as acme.freshdesk.com, not a custom domain.",
		TargetLabel: "Group",
		TargetHelp:  "The group's name or id, such as Billing.",
		AnyTarget:   true,
	}
}

// site is a connection's helpdesk: where its API is, and its key.
type site struct {
	base string
	key  string
}

func (s site) ticketURL(id int64) string {
	return s.base + "/a/tickets/" + strconv.FormatInt(id, 10)
}

// open reads a connection's domain and key. Only a freshdesk.com address
// is taken, so an editor of the connector cannot send its key elsewhere.
func (a *Adapter) open(conn connectors.Connection) (site, error) {
	key := web.Value(conn, "api_key")
	if key == "" {
		return site{}, errors.New("the API key is missing")
	}
	if a.api != "" {
		return site{base: a.api, key: key}, nil
	}
	d := strings.ToLower(web.Value(conn, "domain"))
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimRight(d, "/")
	d = strings.TrimSuffix(d, ".freshdesk.com")
	if !validSub.MatchString(d) {
		return site{}, fmt.Errorf("%q is not a Freshdesk address; give one such as acme.freshdesk.com", web.Short(web.Value(conn, "domain"), 80))
	}
	return site{base: "https://" + d + ".freshdesk.com", key: key}, nil
}

// call calls API version 2. path may have a query; in, if not nil, is
// sent as JSON.
func (a *Adapter) call(ctx context.Context, s site, method, path string, in, out any) error {
	req, err := web.NewJSONRequest(ctx, method, s.base+"/api/v2"+path, in)
	if err != nil {
		return err
	}
	// Freshdesk takes the key as the user, with any password.
	req.SetBasicAuth(s.key, "X")
	req.Header.Set("Accept", "application/json")
	return web.DoJSON(a.hc, req, "Freshdesk", 16<<20, func(status int, b []byte) error {
		var e struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Description string `json:"description"`
			Errors      []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Message
		if msg == "" {
			msg = e.Description
		}
		for _, f := range e.Errors {
			msg += "; " + f.Field + ": " + f.Message
		}
		return &web.APIError{Service: "Freshdesk", Status: status, Code: e.Code, Message: msg}
	}, out)
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	s, err := a.open(conn)
	if err != nil {
		return err
	}
	err = a.call(ctx, s, http.MethodGet, "/agents/me", nil, &struct{}{})
	switch web.Status(err) {
	case http.StatusUnauthorized:
		return fmt.Errorf("the API key does not work with %s; copy it again from the agent's profile (%w)", s.base, err)
	case http.StatusNotFound:
		return fmt.Errorf("there is no Freshdesk at %s; check the domain (%w)", s.base, err)
	}
	return err
}

type group struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	AgentIDs []int64 `json:"agent_ids"`
}

// groups lists every group, a page of 100 at a time.
func (a *Adapter) groups(ctx context.Context, s site) ([]group, error) {
	var all []group
	for page := 1; page <= 10; page++ {
		var gs []group
		if err := a.call(ctx, s, http.MethodGet, "/groups?per_page=100&page="+strconv.Itoa(page), nil, &gs); err != nil {
			return nil, err
		}
		all = append(all, gs...)
		if len(gs) < 100 {
			break
		}
	}
	return all, nil
}

func (a *Adapter) group(ctx context.Context, s site, id string) (group, error) {
	var g group
	err := a.call(ctx, s, http.MethodGet, "/groups/"+id, nil, &g)
	if web.Status(err) == http.StatusNotFound {
		return group{}, fmt.Errorf("there is no Freshdesk group %s", id)
	}
	return g, err
}

// Target takes a group's name or id and stores its id.
func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	if target == connectors.AnyTarget {
		return target, nil
	}
	s, err := a.open(conn)
	if err != nil {
		return "", err
	}
	target = strings.TrimSpace(target)
	if validID.MatchString(target) {
		g, err := a.group(ctx, s, target)
		return strconv.FormatInt(g.ID, 10), err
	}
	gs, err := a.groups(ctx, s)
	if err != nil {
		return "", err
	}
	for _, g := range gs {
		if strings.EqualFold(g.Name, target) {
			return strconv.FormatInt(g.ID, 10), nil
		}
	}
	return "", fmt.Errorf("there is no Freshdesk group called %q; give its name or id", web.Short(target, 80))
}

// run is apitool.Typed for a tool that calls the connection's helpdesk.
func run[T any](a *Adapter, f func(ctx context.Context, s site, target string, in T) (any, error)) apitool.Run {
	return apitool.Typed(func(ctx context.Context, conn connectors.Connection, target string, in T) (any, error) {
		s, err := a.open(conn)
		if err != nil {
			return nil, err
		}
		return f(ctx, s, target, in)
	})
}

// idOf checks an id an agent gives, which goes into a URL.
func idOf(what string, v json.Number) (string, error) {
	s := strings.TrimSpace(v.String())
	if !validID.MatchString(s) {
		return "", fmt.Errorf("%q is not a %s id; ids are numbers", web.Short(s, 40), what)
	}
	return s, nil
}

// scope narrows a ticket filter to the binding's group. The filter is
// wrapped in parentheses, so one whose parentheses or quotes do not
// balance is refused: it could reach past them.
func scope(target, query string) (string, error) {
	query = strings.TrimSpace(query)
	if strings.Contains(query, `"`) {
		return "", errors.New(`a filter quotes values with ' not "`)
	}
	// An escaped quote would be read one way here and maybe another there.
	if strings.Contains(query, `\`) {
		return "", errors.New("a filter may not hold a backslash")
	}
	depth, quoted := 0, false
	for _, r := range query {
		switch {
		case r == '\'':
			quoted = !quoted
		case quoted:
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth < 0 {
				return "", errors.New("the filter's parentheses do not balance")
			}
		}
	}
	if depth != 0 || quoted {
		return "", errors.New("the filter's parentheses or quotes do not balance")
	}
	switch {
	case target == connectors.AnyTarget:
		return query, nil
	case query == "":
		return "group_id:" + target, nil
	default:
		return "(" + query + ") AND group_id:" + target, nil
	}
}

// inGroup reports whether a ticket is in the binding's group.
func inGroup(target string, g *int64) bool {
	return target == connectors.AnyTarget || g != nil && strconv.FormatInt(*g, 10) == target
}

func outside(id string) error {
	return fmt.Errorf("ticket %s is not in this connector's group, the only group it may use", id)
}

// query builds a query string.
func query(kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	return q.Encode()
}
