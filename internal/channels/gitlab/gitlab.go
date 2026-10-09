// Package gitlab lets agents work in GitLab projects with an access token: a
// project or group access token, or a personal one of a user made for the
// agents, or the token of each person who signs in through an OAuth
// application. It works with gitlab.com and with GitLab run on your own
// servers.
//
// The token reaches the REST API for issues and merge requests, and git over
// HTTPS, through the control plane's git proxy. It never reaches an agent.
//
// Endpoints:
//   - "base": GitLab, default https://gitlab.com, overridden by a
//     connection's base URL. The API is <base>/api/v4.
package gitlab

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// validPath is group/project, with subgroups between. It goes into URLs.
var validPath = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)

// developer is the access level that may push to unprotected branches.
const developer = 30

// maxComments bounds what Issue reads, so a long thread does not fill the
// model's context.
const maxComments = 100

type Adapter struct {
	hc   *http.Client
	base string
	oauth.Accounts
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{hc: web.SameServerClient(opts), base: web.Endpoint(opts, "base", "https://gitlab.com")}
	a.Accounts = oauth.NewAccounts(oauth.AccountsConfig{
		Service: "GitLab",
		HTTP:    a.hc,
		SignIn: channels.SignIn{
			Fields: []channels.Field{
				{Name: "client_id", Label: "Application ID", Required: true},
				{Name: "client_secret", Label: "Secret", Secret: true, Required: true},
				selfManaged,
			},
			Setup: "1. In your group's or instance's settings, under Applications, add an **OAuth application**.\n" +
				"2. Set its redirect URI to {callback}, keep it confidential, and give it the scopes api, openid and email.\n" +
				"3. Enter its application ID and secret here. Then each person connects their own GitLab account under Connectors. " +
				"Agents reach what that person may, and push and open merge requests as them.",
		},
		App: func(conn channels.Connection) (oauth.App, error) {
			base, err := a.baseOf(conn)
			id, secret := web.Value(conn, "client_id"), web.Value(conn, "client_secret")
			switch {
			case err != nil:
				return oauth.App{}, err
			case id == "" || secret == "":
				return oauth.App{}, errors.New("the application ID or secret is missing")
			}
			return oauth.App{
				AuthURL: base + "/oauth/authorize", TokenURL: base + "/oauth/token", ClientID: id, Secret: secret,
				Scopes: []string{"api", "openid", "email"},
			}, nil
		},
	})
	return a
}

// selfManaged is a setting of both kinds of connector.
var selfManaged = channels.Field{
	Name: "base_url", Label: "GitLab URL", Endpoint: true,
	Help: "Only for GitLab on your own servers, such as https://gitlab.example.com. Empty for gitlab.com.",
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.GitLab,
		Label:    "GitLab",
		Vendor:   "GitLab",
		Category: "Code",
		Summary:  "Agents clone projects, push branches, open merge requests and work on issues.",
		Fields: []channels.Field{
			{
				Name: "token", Label: "Access token", Secret: true, Required: true,
				Help: "A project, group or personal access token, starting with glpat-.",
			},
			selfManaged,
		},
		Setup: "1. Create a group or project access token with the role Developer and the scopes api and " +
			"write_repository. A personal access token of a user made for the agents works too.\n" +
			"2. Agents push only to branches starting with keera/, so protect your main branches as you would for a person.",
		TargetLabel: "Project",
		TargetHelp:  "The project's path, such as acme/website or acme/apps/website.",
	}
}

// settings are the token to call with, and where. person is a token of
// someone who signed in.
type settings struct {
	token, base string
	person      bool
}

const badBase = "the GitLab URL must be https://, such as https://gitlab.example.com"

// baseOf is gitlab.com, or the connection's own GitLab.
func (a *Adapter) baseOf(conn channels.Connection) (string, error) {
	base, ok := web.BaseURL(web.Value(conn, "base_url"))
	if !ok {
		return "", errors.New(badBase)
	}
	return cmp.Or(base, a.base), nil
}

func (a *Adapter) settingsOf(conn channels.Connection) (settings, error) {
	base, err := a.baseOf(conn)
	s := settings{token: web.Value(conn, "token"), base: base, person: conn.PerUser}
	if s.person {
		s.token = conn.Token
	}
	switch {
	case err != nil:
	case s.token == "" && s.person:
		err = oauth.ErrNoToken
	case s.token == "":
		err = errors.New("the access token is missing")
	}
	return s, err
}

// who names whose access a call has, for errors.
func (s settings) who() string {
	if s.person {
		return "the GitLab account"
	}
	return "the token"
}

// open reads the connection's settings and checks the project's path.
func (a *Adapter) open(conn channels.Connection, path string) (settings, string, error) {
	s, err := a.settingsOf(conn)
	if err != nil {
		return s, "", err
	}
	path, ok := web.RepoPath(path, validPath)
	if !ok {
		return s, "", fmt.Errorf("%q is not a project; write its path, such as group/project", web.Short(path, 80))
	}
	return s, path, nil
}

// project is the API's address of a project: its path, escaped whole.
func project(path string) string { return "/projects/" + url.PathEscape(path) }

func (a *Adapter) call(ctx context.Context, s settings, method, path string, in, out any) error {
	req, err := web.NewJSONRequest(ctx, method, s.base+"/api/v4"+path, in)
	if err != nil {
		return err
	}
	if s.person {
		// An OAuth token goes as a bearer token; PRIVATE-TOKEN is for the
		// access tokens people create.
		req.Header.Set("Authorization", "Bearer "+s.token)
	} else {
		req.Header.Set("PRIVATE-TOKEN", s.token)
	}
	req.Header.Set("Accept", "application/json")
	return web.DoJSON(a.hc, req, "GitLab", 16<<20, func(status int, b []byte) error {
		// GitLab's message is a string, a list or an object of lists.
		var e struct {
			Message json.RawMessage `json:"message"`
			Error   string          `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Error
		if len(e.Message) > 0 {
			var text string
			if json.Unmarshal(e.Message, &text) != nil {
				text = string(e.Message)
			}
			msg = text
		}
		return &web.APIError{Service: "GitLab", Status: status, Message: msg}
	}, out)
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	s, err := a.settingsOf(conn)
	if err != nil {
		return err
	}
	var me struct {
		Username string `json:"username"`
	}
	err = a.call(ctx, s, http.MethodGet, "/user", nil, &me)
	if web.Status(err) == http.StatusUnauthorized {
		return fmt.Errorf("GitLab does not accept the token; check it has not expired or been revoked (%w)", err)
	}
	return err
}

type access struct {
	AccessLevel int `json:"access_level"`
}

func (a *Adapter) Repo(ctx context.Context, conn channels.Connection, path string) (channels.Repo, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Repo{}, err
	}
	var p struct {
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
		Permissions       struct {
			Project *access `json:"project_access"`
			Group   *access `json:"group_access"`
		} `json:"permissions"`
	}
	err = a.call(ctx, s, http.MethodGet, project(path), nil, &p)
	if web.Status(err) == http.StatusNotFound {
		return channels.Repo{}, fmt.Errorf("%s cannot see %s; check the path and its projects", s.who(), path)
	}
	if err != nil {
		return channels.Repo{}, err
	}
	// An administrator's token has neither, and may push anywhere.
	level := 0
	for _, x := range []*access{p.Permissions.Project, p.Permissions.Group} {
		if x != nil {
			level = max(level, x.AccessLevel)
		}
	}
	if (p.Permissions.Project != nil || p.Permissions.Group != nil) && level < developer {
		return channels.Repo{}, fmt.Errorf("%s may read %s but not push to it; it needs the role Developer", s.who(), p.PathWithNamespace)
	}
	return channels.Repo{Path: p.PathWithNamespace, DefaultBranch: p.DefaultBranch}, nil
}

func (a *Adapter) Remote(_ context.Context, conn channels.Connection, path string) (channels.Remote, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Remote{}, err
	}
	// GitLab takes any user name with a token; this one is what it documents.
	return channels.Remote{URL: s.base + "/" + path + ".git", User: "oauth2", Password: s.token}, nil
}

type issue struct {
	IID          int       `json:"iid"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	State        string    `json:"state"`
	WebURL       string    `json:"web_url"`
	UpdatedAt    time.Time `json:"updated_at"`
	Labels       []string  `json:"labels"`
	SourceBranch string    `json:"source_branch"`
	Author       struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (i issue) view(change bool) channels.Issue {
	// GitLab says opened; GitHub and the tools say open.
	st := i.State
	if st == "opened" {
		st = "open"
	}
	return channels.Issue{
		Number: i.IID, Title: i.Title, Body: i.Description, State: st, Author: i.Author.Username,
		Labels: i.Labels, WebURL: i.WebURL, Change: change, Branch: i.SourceBranch, Updated: i.UpdatedAt,
	}
}

func kindOf(change bool) string {
	if change {
		return "/merge_requests"
	}
	return "/issues"
}

func (a *Adapter) Issues(ctx context.Context, conn channels.Connection, path, st string, changes bool) ([]channels.Issue, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return nil, err
	}
	if st == "open" {
		st = "opened"
	}
	q := url.Values{"state": {st}, "per_page": {"50"}, "order_by": {"updated_at"}, "sort": {"desc"}}
	var list []issue
	if err := a.call(ctx, s, http.MethodGet, project(path)+kindOf(changes)+"?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	out := []channels.Issue{}
	for _, i := range list {
		out = append(out, i.view(changes).Brief())
	}
	return out, nil
}

func (a *Adapter) Issue(ctx context.Context, conn channels.Connection, path string, number int, change bool) (channels.Issue, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Issue{}, err
	}
	base := project(path) + kindOf(change) + "/" + strconv.Itoa(number)
	var i issue
	if err := a.call(ctx, s, http.MethodGet, base, nil, &i); err != nil {
		return channels.Issue{}, err
	}
	out := i.view(change)
	var notes []struct {
		Body      string    `json:"body"`
		System    bool      `json:"system"`
		CreatedAt time.Time `json:"created_at"`
		Author    struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	q := url.Values{"sort": {"asc"}, "order_by": {"created_at"}, "per_page": {strconv.Itoa(maxComments)}}
	if err := a.call(ctx, s, http.MethodGet, base+"/notes?"+q.Encode(), nil, &notes); err != nil {
		return channels.Issue{}, err
	}
	for _, n := range notes {
		// System notes are GitLab's own, such as "changed the description".
		if !n.System {
			out.Comments = append(out.Comments, channels.Comment{Author: n.Author.Username, Body: n.Body, At: n.CreatedAt})
		}
	}
	return out, nil
}

func (a *Adapter) Comment(ctx context.Context, conn channels.Connection, path string, number int, change bool, body string) error {
	s, path, err := a.open(conn, path)
	if err != nil {
		return err
	}
	return a.call(ctx, s, http.MethodPost, project(path)+kindOf(change)+"/"+strconv.Itoa(number)+"/notes",
		map[string]string{"body": body}, nil)
}

func (a *Adapter) OpenChange(ctx context.Context, conn channels.Connection, path string, c channels.Change) (channels.Issue, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Issue{}, err
	}
	title := c.Title
	if c.Draft {
		title = "Draft: " + title
	}
	var i issue
	err = a.call(ctx, s, http.MethodPost, project(path)+"/merge_requests", map[string]any{
		"source_branch": c.Branch, "target_branch": c.Base, "title": title, "description": c.Body,
		"remove_source_branch": true,
	}, &i)
	if err != nil {
		return channels.Issue{}, err
	}
	return i.view(true).Brief(), nil
}
