// Package github lets agents work in GitHub repositories with a token: a
// fine-grained personal access token, best of a user made for the agents.
//
// The token reaches the REST API for issues and pull requests, and git over
// HTTPS, through the control plane's git proxy. It never reaches an agent.
//
// Endpoints:
//   - "api": the REST API, default https://api.github.com
//   - "git": where repositories are cloned from, default https://github.com
//
// A connection with a base URL, for GitHub Enterprise Server, uses
// <base>/api/v3 and <base> instead.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// validPath is owner/name, as GitHub allows them. It goes into URLs.
var validPath = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// maxComments bounds what Issue reads, so a long thread does not fill the
// model's context.
const maxComments = 100

type Adapter struct {
	hc  *http.Client
	api string
	git string
}

func New(opts channels.Options) *Adapter {
	return &Adapter{
		hc:  web.SameServerClient(opts),
		api: web.Endpoint(opts, "api", "https://api.github.com"),
		git: web.Endpoint(opts, "git", "https://github.com"),
	}
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.GitHub,
		Label:    "GitHub",
		Vendor:   "GitHub",
		Category: "Code",
		Summary:  "Agents clone repositories, push branches, open pull requests and work on issues.",
		Fields: []channels.Field{
			{
				Name: "token", Label: "Access token", Secret: true, Required: true,
				Help: "A fine-grained personal access token, starting with github_pat_.",
			},
			{
				Name: "base_url", Label: "GitHub Enterprise URL", Endpoint: true,
				Help: "Only for GitHub Enterprise Server, such as https://github.example.com. Empty for github.com.",
			},
		},
		Setup: "1. Create a GitHub user for your agents.\n" +
			"2. Add it to the repositories they work in.\n" +
			"3. As that user, create a fine-grained personal access token for those repositories with the permissions " +
			"Contents, Issues and Pull requests set to Read and write.\n" +
			"4. Agents push only to branches starting with keera/, so protect your main branches as you would for a person.",
		TargetLabel: "Repository",
		TargetHelp:  "owner/name, such as acme/website.",
	}
}

type settings struct {
	token, api, git string
}

func (a *Adapter) settingsOf(conn channels.Connection) (settings, error) {
	token, base, err := web.TokenAndBaseURL(conn, "the GitHub Enterprise URL must be https://, such as https://github.example.com")
	s := settings{token: token, api: a.api, git: a.git}
	if base != "" {
		s.api, s.git = base+"/api/v3", base
	}
	return s, err
}

// open reads the connection's settings and checks the repository's path.
func (a *Adapter) open(conn channels.Connection, path string) (settings, string, error) {
	s, err := a.settingsOf(conn)
	if err != nil {
		return s, "", err
	}
	path, ok := web.RepoPath(path, validPath)
	if !ok {
		return s, "", fmt.Errorf("%q is not a repository; write owner/name", web.Short(path, 80))
	}
	return s, path, nil
}

func (a *Adapter) call(ctx context.Context, s settings, method, path string, in, out any) error {
	req, err := web.NewJSONRequest(ctx, method, s.api+path, in)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return web.DoJSON(a.hc, req, "GitHub", 16<<20, func(status int, b []byte) error {
		var e struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Message
		// The first detail says what was wrong, such as a pull request that
		// already exists.
		if len(e.Errors) > 0 && e.Errors[0].Message != "" {
			msg += " (" + e.Errors[0].Message + ")"
		}
		return &web.APIError{Service: "GitHub", Status: status, Message: msg}
	}, out)
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	s, err := a.settingsOf(conn)
	if err != nil {
		return err
	}
	var me struct {
		Login string `json:"login"`
	}
	err = a.call(ctx, s, http.MethodGet, "/user", nil, &me)
	if web.Status(err) == http.StatusUnauthorized {
		return fmt.Errorf("GitHub does not accept the token; check it has not expired (%w)", err)
	}
	return err
}

type repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

func (a *Adapter) Repo(ctx context.Context, conn channels.Connection, path string) (channels.Repo, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Repo{}, err
	}
	var r repo
	err = a.call(ctx, s, http.MethodGet, "/repos/"+path, nil, &r)
	if web.Status(err) == http.StatusNotFound {
		return channels.Repo{}, fmt.Errorf("the token cannot see %s; check the name and the repositories the token may use", path)
	}
	if err != nil {
		return channels.Repo{}, err
	}
	if !r.Permissions.Push {
		return channels.Repo{}, fmt.Errorf("the token may read %s but not push to it; give it Contents: Read and write", r.FullName)
	}
	return channels.Repo{Path: r.FullName, DefaultBranch: r.DefaultBranch}, nil
}

func (a *Adapter) Remote(_ context.Context, conn channels.Connection, path string) (channels.Remote, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Remote{}, err
	}
	// GitHub takes any user name with a token; this one is what it documents.
	return channels.Remote{URL: s.git + "/" + path + ".git", User: "x-access-token", Password: s.token}, nil
}

type issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	UpdatedAt   time.Time `json:"updated_at"`
	User        user      `json:"user"`
	Labels      []label   `json:"labels"`
	PullRequest *struct{} `json:"pull_request"`
	Head        *struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

type user struct {
	Login string `json:"login"`
}

type label struct {
	Name string `json:"name"`
}

func (i issue) view(change bool) channels.Issue {
	out := channels.Issue{
		Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, Author: i.User.Login,
		WebURL: i.HTMLURL, Change: change || i.PullRequest != nil, Updated: i.UpdatedAt,
	}
	for _, l := range i.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	if i.Head != nil {
		out.Branch = i.Head.Ref
	}
	return out
}

func kindOf(change bool) string {
	if change {
		return "/pulls"
	}
	return "/issues"
}

func (a *Adapter) Issues(ctx context.Context, conn channels.Connection, path, st string, changes bool) ([]channels.Issue, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return nil, err
	}
	q := url.Values{"state": {st}, "per_page": {"50"}, "sort": {"updated"}, "direction": {"desc"}}
	var list []issue
	if err := a.call(ctx, s, http.MethodGet, "/repos/"+path+kindOf(changes)+"?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	out := []channels.Issue{}
	for _, i := range list {
		// GitHub lists pull requests among issues.
		if !changes && i.PullRequest != nil {
			continue
		}
		out = append(out, i.view(changes).Brief())
	}
	return out, nil
}

func (a *Adapter) Issue(ctx context.Context, conn channels.Connection, path string, number int, change bool) (channels.Issue, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Issue{}, err
	}
	n := strconv.Itoa(number)
	var i issue
	if err := a.call(ctx, s, http.MethodGet, "/repos/"+path+kindOf(change)+"/"+n, nil, &i); err != nil {
		return channels.Issue{}, err
	}
	out := i.view(change)
	// Issues and pull requests share their comments.
	var comments []struct {
		Body      string    `json:"body"`
		User      user      `json:"user"`
		CreatedAt time.Time `json:"created_at"`
	}
	q := url.Values{"per_page": {strconv.Itoa(maxComments)}}
	if err := a.call(ctx, s, http.MethodGet, "/repos/"+path+"/issues/"+n+"/comments?"+q.Encode(), nil, &comments); err != nil {
		return channels.Issue{}, err
	}
	for _, c := range comments {
		out.Comments = append(out.Comments, channels.Comment{Author: c.User.Login, Body: c.Body, At: c.CreatedAt})
	}
	return out, nil
}

func (a *Adapter) Comment(ctx context.Context, conn channels.Connection, path string, number int, _ bool, body string) error {
	s, path, err := a.open(conn, path)
	if err != nil {
		return err
	}
	return a.call(ctx, s, http.MethodPost, "/repos/"+path+"/issues/"+strconv.Itoa(number)+"/comments",
		map[string]string{"body": body}, nil)
}

func (a *Adapter) OpenChange(ctx context.Context, conn channels.Connection, path string, c channels.Change) (channels.Issue, error) {
	s, path, err := a.open(conn, path)
	if err != nil {
		return channels.Issue{}, err
	}
	var i issue
	err = a.call(ctx, s, http.MethodPost, "/repos/"+path+"/pulls", map[string]any{
		"head": c.Branch, "base": c.Base, "title": c.Title, "body": c.Body, "draft": c.Draft,
	}, &i)
	if err != nil {
		return channels.Issue{}, err
	}
	return i.view(true).Brief(), nil
}
