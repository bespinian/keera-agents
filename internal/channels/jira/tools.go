package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/atlassian"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Tool: channels.Tool{
				Name: "search_issues",
				Description: "Search issues with JQL, newest change first unless the query has its own ORDER BY. " +
					"A connector limited to one project searches only that project.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"jql":{"type":"string","description":"A JQL query, such as status = \"In Progress\" AND assignee = currentUser()"},
					"limit":{"type":"integer","description":"At most this many issues; default 25, up to 100"}}}`),
			},
			Run: atlassian.Run(a.c, a.searchIssues),
		},
		{
			Tool: channels.Tool{
				Name:        "get_issue",
				Description: "Read one issue: its fields, description and latest comments.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"key":{"type":"string","description":"The issue's key, such as WEB-12"}},"required":["key"]}`),
			},
			Run: atlassian.Run(a.c, a.getIssue),
		},
		{
			Tool: channels.Tool{
				Name:        "list_projects",
				Description: "List the projects this connector may use.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"query":{"type":"string","description":"Only projects whose name or key contains this"},
					"limit":{"type":"integer","description":"At most this many projects; default 25, up to 100"}}}`),
			},
			Run: atlassian.Run(a.c, a.listProjects),
		},
		{
			Tool: channels.Tool{
				Name:        "find_users",
				Description: "Find people by name or email, to get the id update_issue assigns with. In a project, only people who may be assigned there.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"query":{"type":"string","description":"Part of a name or email"},
					"project":{"type":"string","description":"Only people assignable in this project, by key"},
					"limit":{"type":"integer","description":"At most this many people; default 25, up to 100"}},"required":["query"]}`),
			},
			Run: atlassian.Run(a.c, a.findUsers),
		},
		{
			Tool: channels.Tool{
				Name:        "create_issue",
				Description: "Create an issue. Text is in Jira's wiki markup.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"project":{"type":"string","description":"The project's key; only needed when the connector may use every project"},
					"summary":{"type":"string"},
					"description":{"type":"string"},
					"issue_type":{"type":"string","description":"Such as Task, Bug or Story; default Task"},
					"labels":{"type":"array","items":{"type":"string"}},
					"priority":{"type":"string","description":"Such as High"}},"required":["summary"]}`),
				Write: true,
			},
			Run: atlassian.Run(a.c, a.createIssue),
		},
		{
			Tool: channels.Tool{
				Name:        "update_issue",
				Description: "Change an issue's summary, description, labels or assignee. Leave out what stays the same.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"key":{"type":"string","description":"The issue's key, such as WEB-12"},
					"summary":{"type":"string"},
					"description":{"type":"string","description":"Replaces the description; Jira wiki markup"},
					"labels":{"type":"array","items":{"type":"string"},"description":"Replaces all labels"},
					"assignee":{"type":"string","description":"A person's id from find_users or get_issue, or none to unassign"}},"required":["key"]}`),
				Write: true,
			},
			Run: atlassian.Run(a.c, a.updateIssue),
		},
		{
			Tool: channels.Tool{
				Name:        "comment",
				Description: "Comment on an issue, in Jira's wiki markup.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"key":{"type":"string","description":"The issue's key, such as WEB-12"},
					"body":{"type":"string"}},"required":["key","body"]}`),
				Write: true,
			},
			Run: atlassian.Run(a.c, a.comment),
		},
		{
			Tool: channels.Tool{
				Name:        "transition",
				Description: "Move an issue to another status, such as In Progress or Done.",
				Parameters: apitool.Schema(`{"type":"object","properties":{
					"key":{"type":"string","description":"The issue's key, such as WEB-12"},
					"to":{"type":"string","description":"The status, or the workflow transition's name"}},"required":["key","to"]}`),
				Write: true,
			},
			Run: atlassian.Run(a.c, a.transition),
		},
	}
}

// Brief is an issue in a list.
type Brief struct {
	Key      string `json:"key"`
	Summary  string `json:"summary"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	Priority string `json:"priority,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Updated  string `json:"updated"`
	URL      string `json:"url"`
}

type searchIn struct {
	JQL   string `json:"jql"`
	Limit int    `json:"limit"`
}

func (a *Adapter) searchIssues(ctx context.Context, s atlassian.Site, target string, in searchIn) (any, error) {
	clause := ""
	if target != channels.AnyTarget {
		clause = "project = " + atlassian.Quote(target)
	} else if strings.TrimSpace(in.JQL) == "" {
		return nil, errors.New(`give jql, such as project = WEB AND status = "In Progress"`)
	}
	jql, err := atlassian.Scope(clause, in.JQL, "ORDER BY updated DESC")
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"jql":        {jql},
		"maxResults": {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))},
		"fields":     {"summary,status,issuetype,priority,assignee,updated,project"},
	}
	// Cloud removed /search for /search/jql, which Data Center does not
	// have yet. Try the one the site most likely has first.
	paths := []string{"/search/jql", "/search"}
	if !s.Cloud() {
		paths[0], paths[1] = paths[1], paths[0]
	}
	var res struct {
		Issues        []issue `json:"issues"`
		Total         int     `json:"total"`
		IsLast        *bool   `json:"isLast"`
		NextPageToken string  `json:"nextPageToken"`
	}
	err = a.call(ctx, s, http.MethodGet, paths[0]+"?"+q.Encode(), nil, &res)
	if st := web.Status(err); st == http.StatusNotFound || st == http.StatusGone {
		err = a.call(ctx, s, http.MethodGet, paths[1]+"?"+q.Encode(), nil, &res)
	}
	if err != nil {
		return nil, err
	}
	out := []Brief{}
	for _, i := range res.Issues {
		if target != channels.AnyTarget && !strings.EqualFold(i.Fields.Project.Key, target) {
			continue
		}
		out = append(out, Brief{
			Key: i.Key, Summary: i.Fields.Summary, Status: i.Fields.Status.Name, Type: i.Fields.IssueType.Name,
			Priority: i.priority(), Assignee: i.Fields.Assignee.name(), Updated: i.Fields.Updated, URL: browse(s, i.Key),
		})
	}
	more := res.NextPageToken != "" || (res.IsLast != nil && !*res.IsLast) || res.Total > len(res.Issues)
	return map[string]any{"issues": out, "more": more}, nil
}

// Issue is one issue in full.
type Issue struct {
	Key             string    `json:"key"`
	URL             string    `json:"url"`
	Project         string    `json:"project"`
	Summary         string    `json:"summary"`
	Status          string    `json:"status"`
	Type            string    `json:"type"`
	Priority        string    `json:"priority,omitempty"`
	Parent          string    `json:"parent,omitempty"`
	Assignee        *Person   `json:"assignee,omitempty"`
	Reporter        *Person   `json:"reporter,omitempty"`
	Labels          []string  `json:"labels,omitempty"`
	Created         string    `json:"created"`
	Updated         string    `json:"updated"`
	Description     string    `json:"description,omitempty"`
	Comments        []Comment `json:"comments,omitempty"`
	EarlierComments int       `json:"earlier_comments,omitempty"`
}

type Comment struct {
	Author string `json:"author,omitempty"`
	Body   string `json:"body"`
	At     string `json:"at"`
}

type keyIn struct {
	Key string `json:"key"`
}

func (a *Adapter) getIssue(ctx context.Context, s atlassian.Site, target string, in keyIn) (any, error) {
	key, err := issueKey(in.Key)
	if err != nil {
		return nil, err
	}
	var i issue
	q := url.Values{"fields": {"summary,status,issuetype,priority,assignee,reporter,labels,description,created,updated,project,parent,comment"}}
	if err := a.call(ctx, s, http.MethodGet, "/issue/"+key+"?"+q.Encode(), nil, &i); err != nil {
		return nil, err
	}
	if target != channels.AnyTarget && !strings.EqualFold(i.Fields.Project.Key, target) {
		return nil, outside(key, target)
	}
	f := i.Fields
	out := Issue{
		Key: i.Key, URL: browse(s, i.Key), Project: f.Project.Key, Summary: f.Summary, Status: f.Status.Name,
		Type: f.IssueType.Name, Priority: i.priority(), Assignee: f.Assignee.person(), Reporter: f.Reporter.person(),
		Labels: f.Labels, Created: f.Created, Updated: f.Updated, Description: atlassian.Cut(f.Description, maxDescription),
	}
	if f.Parent != nil {
		out.Parent = f.Parent.Key
	}
	cs := f.Comment.Comments
	if len(cs) > maxComments {
		cs = cs[len(cs)-maxComments:]
	}
	out.EarlierComments = max(f.Comment.Total, len(f.Comment.Comments)) - len(cs)
	for _, c := range cs {
		out.Comments = append(out.Comments, Comment{Author: c.Author.name(), Body: atlassian.Cut(c.Body, maxComment), At: c.Created})
	}
	return out, nil
}

// Project is a project in a list.
type Project struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	URL  string `json:"url"`
}

type listProjectsIn struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (a *Adapter) listProjects(ctx context.Context, s atlassian.Site, target string, in listProjectsIn) (any, error) {
	view := func(p project) Project {
		return Project{Key: p.Key, Name: p.Name, Type: p.ProjectTypeKey, URL: s.Base + "/browse/" + p.Key}
	}
	if target != channels.AnyTarget {
		p, err := a.project(ctx, s, target)
		if err != nil {
			return nil, err
		}
		return []Project{view(p)}, nil
	}
	limit := apitool.Limit(in.Limit, 25, 100)
	var list []project
	if s.Cloud() {
		var page struct {
			Values []project `json:"values"`
		}
		q := url.Values{"maxResults": {strconv.Itoa(limit)}, "query": {in.Query}}
		if err := a.call(ctx, s, http.MethodGet, "/project/search?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		list = page.Values
	} else if err := a.call(ctx, s, http.MethodGet, "/project", nil, &list); err != nil {
		return nil, err
	}
	out := []Project{}
	query := strings.ToLower(strings.TrimSpace(in.Query))
	for _, p := range list {
		if len(out) == limit {
			break
		}
		if query == "" || strings.Contains(strings.ToLower(p.Key+" "+p.Name), query) {
			out = append(out, view(p))
		}
	}
	return out, nil
}

type findUsersIn struct {
	Query   string `json:"query"`
	Project string `json:"project"`
	Limit   int    `json:"limit"`
}

func (a *Adapter) findUsers(ctx context.Context, s atlassian.Site, target string, in findUsersIn) (any, error) {
	if err := apitool.Need("query", in.Query); err != nil {
		return nil, err
	}
	q := url.Values{"maxResults": {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))}}
	// Cloud finds people by query, Data Center by username, which matches
	// names and emails there too.
	if s.Cloud() {
		q.Set("query", in.Query)
	} else {
		q.Set("username", in.Query)
	}
	path := "/user/search"
	if target != channels.AnyTarget || strings.TrimSpace(in.Project) != "" {
		p, err := projectFor(target, in.Project)
		if err != nil {
			return nil, err
		}
		q.Set("project", p)
		path = "/user/assignable/search"
	}
	var users []user
	if err := a.call(ctx, s, http.MethodGet, path+"?"+q.Encode(), nil, &users); err != nil {
		return nil, err
	}
	out := []*Person{}
	for _, u := range users {
		// Apps and integrations are users on Cloud, too.
		if (u.Active != nil && !*u.Active) || (u.AccountType != "" && u.AccountType != "atlassian") {
			continue
		}
		out = append(out, u.person())
	}
	return out, nil
}

type createIn struct {
	Project     string   `json:"project"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	IssueType   string   `json:"issue_type"`
	Labels      []string `json:"labels"`
	Priority    string   `json:"priority"`
}

func (a *Adapter) createIssue(ctx context.Context, s atlassian.Site, target string, in createIn) (any, error) {
	p, err := projectFor(target, in.Project)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("summary", in.Summary); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.IssueType) == "" {
		in.IssueType = "Task"
	}
	fields := map[string]any{
		"project":   map[string]string{"key": p},
		"summary":   in.Summary,
		"issuetype": map[string]string{"name": in.IssueType},
	}
	if in.Description != "" {
		fields["description"] = in.Description
	}
	if len(in.Labels) > 0 {
		fields["labels"] = in.Labels
	}
	if in.Priority != "" {
		fields["priority"] = map[string]string{"name": in.Priority}
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := a.call(ctx, s, http.MethodPost, "/issue", map[string]any{"fields": fields}, &created); err != nil {
		return nil, err
	}
	return map[string]string{"key": created.Key, "url": browse(s, created.Key)}, nil
}

type updateIn struct {
	Key         string    `json:"key"`
	Summary     *string   `json:"summary"`
	Description *string   `json:"description"`
	Labels      *[]string `json:"labels"`
	Assignee    *string   `json:"assignee"`
}

func (a *Adapter) updateIssue(ctx context.Context, s atlassian.Site, target string, in updateIn) (any, error) {
	key, err := a.inTarget(ctx, s, target, in.Key)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if in.Summary != nil {
		fields["summary"] = *in.Summary
	}
	if in.Description != nil {
		fields["description"] = *in.Description
	}
	if in.Labels != nil {
		fields["labels"] = *in.Labels
	}
	if len(fields) == 0 && in.Assignee == nil {
		return nil, errors.New("give at least one of summary, description, labels or assignee")
	}
	changed := []string{}
	if len(fields) > 0 {
		if err := a.call(ctx, s, http.MethodPut, "/issue/"+key, map[string]any{"fields": fields}, nil); err != nil {
			return nil, err
		}
		for k := range fields {
			changed = append(changed, k)
		}
		slices.Sort(changed)
	}
	if in.Assignee != nil {
		// The assign call needs only Assign issues, not Edit issues, and
		// works whether or not the edit screen shows the assignee.
		var who any
		if id := strings.TrimSpace(*in.Assignee); id != "" && !strings.EqualFold(id, "none") {
			who = id
		}
		body := map[string]any{"name": who}
		if s.Cloud() {
			body = map[string]any{"accountId": who}
		}
		if err := a.call(ctx, s, http.MethodPut, "/issue/"+key+"/assignee", body, nil); err != nil {
			return nil, fmt.Errorf("could not assign %s: %w", key, err)
		}
		changed = append(changed, "assignee")
	}
	return map[string]any{"key": key, "url": browse(s, key), "changed": changed}, nil
}

type commentIn struct {
	Key  string `json:"key"`
	Body string `json:"body"`
}

func (a *Adapter) comment(ctx context.Context, s atlassian.Site, target string, in commentIn) (any, error) {
	if err := apitool.Need("body", in.Body); err != nil {
		return nil, err
	}
	key, err := a.inTarget(ctx, s, target, in.Key)
	if err != nil {
		return nil, err
	}
	var c struct {
		ID string `json:"id"`
	}
	if err := a.call(ctx, s, http.MethodPost, "/issue/"+key+"/comment", map[string]string{"body": in.Body}, &c); err != nil {
		return nil, err
	}
	return map[string]string{"key": key, "comment_id": c.ID, "url": browse(s, key) + "?focusedCommentId=" + url.QueryEscape(c.ID)}, nil
}

type transitionIn struct {
	Key string `json:"key"`
	To  string `json:"to"`
}

func (a *Adapter) transition(ctx context.Context, s atlassian.Site, target string, in transitionIn) (any, error) {
	if err := apitool.Need("to", in.To); err != nil {
		return nil, err
	}
	key, err := a.inTarget(ctx, s, target, in.Key)
	if err != nil {
		return nil, err
	}
	var res struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   named  `json:"to"`
		} `json:"transitions"`
	}
	if err := a.call(ctx, s, http.MethodGet, "/issue/"+key+"/transitions", nil, &res); err != nil {
		return nil, err
	}
	to := strings.TrimSpace(in.To)
	var names []string
	for _, t := range res.Transitions {
		if strings.EqualFold(t.To.Name, to) || strings.EqualFold(t.Name, to) {
			body := map[string]any{"transition": map[string]string{"id": t.ID}}
			if err := a.call(ctx, s, http.MethodPost, "/issue/"+key+"/transitions", body, nil); err != nil {
				return nil, err
			}
			return map[string]string{"key": key, "status": t.To.Name}, nil
		}
		names = append(names, fmt.Sprintf("%q (to %s)", t.Name, t.To.Name))
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s cannot move to another status now", key)
	}
	return nil, fmt.Errorf("%s cannot move to %q; it can take %s", key, to, strings.Join(names, ", "))
}
