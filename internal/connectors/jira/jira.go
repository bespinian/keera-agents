// Package jira lets agents find, read, create and change Jira issues, with
// an API token on Jira Cloud or a personal access token on Jira Data Center,
// or, on Jira Cloud, as each person who signs in with their own account.
//
// It calls REST API version 2, which both have, so text fields are plain
// strings in Jira's wiki markup, not Atlassian Document Format. A binding
// names a project; every call stays in it.
//
// It calls the connection's site URL, or for a person, Atlassian's API
// gateway, whose endpoints package atlassian lists.
package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/atlassian"
	"github.com/bespinian/keera-agents/internal/connectors/internal/oauth"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

var (
	// validKey and validProject go into URLs and queries.
	validKey     = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
	validProject = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// What get_issue returns at most, so one issue does not fill the model's
// context.
const (
	maxComments    = 20
	maxDescription = 20000
	maxComment     = 4000
)

var (
	_ connectors.API     = (*Adapter)(nil)
	_ connectors.PerUser = (*Adapter)(nil)
)

type Adapter struct {
	c *atlassian.Client
	apitool.Set
	oauth.Accounts
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{c: atlassian.New(opts, "Jira")}
	a.Set = a.toolSet()
	a.Accounts = a.c.Accounts("read:jira-work", "write:jira-work", "read:jira-user")
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Jira,
		Label:    "Jira",
		Vendor:   "Atlassian",
		Category: "Projects and wikis",
		Summary:  "Agents search, read, create and update issues, comment on them and move them through the workflow.",
		Fields:   atlassian.Fields("Jira"),
		Setup: "1. Create an Atlassian account for your agents and add it to the projects they work in. " +
			"To read, it needs Browse projects; to write, also Create, Edit, Assign and Transition issues and Add comments.\n" +
			"2. On Atlassian Cloud, sign in as that account and create an API token, without scopes, " +
			"at https://id.atlassian.com/manage-profile/security/api-tokens. Enter it with the account's email.\n" +
			"3. On Jira Data Center, sign in as that account and create a personal access token in your profile. " +
			"Leave the email empty.",
		TargetLabel: "Project",
		TargetHelp:  "The project's key, such as WEB.",
		AnyTarget:   true,
	}
}

func (a *Adapter) call(ctx context.Context, s atlassian.Site, method, path string, in, out any) error {
	return a.c.Call(ctx, s, method, s.APIBase()+"/rest/api/2"+path, in, out)
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	s, err := a.c.Open(ctx, conn)
	if err != nil {
		return err
	}
	return a.c.CheckError(s, a.call(ctx, s, http.MethodGet, "/myself", nil, &struct{}{}))
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	if target == connectors.AnyTarget {
		return target, nil
	}
	s, err := a.c.Open(ctx, conn)
	if err != nil {
		return "", err
	}
	p, err := a.project(ctx, s, target)
	if err != nil {
		return "", err
	}
	return p.Key, nil
}

type project struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	ProjectTypeKey string `json:"projectTypeKey"`
}

func (a *Adapter) project(ctx context.Context, s atlassian.Site, key string) (project, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !validProject.MatchString(key) {
		return project{}, fmt.Errorf("%q is not a project key, such as WEB", web.Short(key, 40))
	}
	var p project
	err := a.call(ctx, s, http.MethodGet, "/project/"+key, nil, &p)
	if web.Status(err) == http.StatusNotFound {
		return project{}, fmt.Errorf("the account cannot see a Jira project %s; check the key and the account's access", key)
	}
	return p, err
}

// issueKey checks an issue's key, which goes into URLs.
func issueKey(key string) (string, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !validKey.MatchString(key) {
		return "", fmt.Errorf("%q is not an issue key, such as WEB-12", web.Short(key, 40))
	}
	return key, nil
}

// inTarget refuses an issue outside the binding's project. It asks Jira
// rather than trusting the key, since a moved issue keeps its old key.
func (a *Adapter) inTarget(ctx context.Context, s atlassian.Site, target, key string) (string, error) {
	key, err := issueKey(key)
	if err != nil || target == connectors.AnyTarget {
		return key, err
	}
	var i issue
	if err := a.call(ctx, s, http.MethodGet, "/issue/"+key+"?fields=project", nil, &i); err != nil {
		return "", err
	}
	if !strings.EqualFold(i.Fields.Project.Key, target) {
		return "", outside(key, target)
	}
	return i.Key, nil
}

func outside(key, target string) error {
	return fmt.Errorf("%s is not in project %s, the only project this connector may use", key, target)
}

// projectFor is the project a new issue goes in: the binding's, or the
// one named when the binding is to every project.
func projectFor(target, named string) (string, error) {
	named = strings.ToUpper(strings.TrimSpace(named))
	if target != connectors.AnyTarget {
		if named != "" && named != target {
			return "", fmt.Errorf("this connector may use only project %s", target)
		}
		return target, nil
	}
	if !validProject.MatchString(named) {
		return "", errors.New("give project, the key of the project, such as WEB")
	}
	return named, nil
}

type named struct {
	Name string `json:"name"`
}

type user struct {
	AccountID   string `json:"accountId"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"emailAddress"`
	Active      *bool  `json:"active"`
	AccountType string `json:"accountType"`
}

// Person is a user as tools show them. ID is what update_issue takes to
// assign: the account ID on Cloud, the user name on Data Center.
type Person struct {
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	Email string `json:"email,omitempty"`
}

func (u *user) person() *Person {
	if u == nil {
		return nil
	}
	id := u.AccountID
	if id == "" {
		id = u.Name
	}
	return &Person{Name: u.DisplayName, ID: id, Email: u.Email}
}

func (u *user) name() string {
	if u == nil {
		return ""
	}
	return u.DisplayName
}

type issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string   `json:"summary"`
		Status      named    `json:"status"`
		IssueType   named    `json:"issuetype"`
		Priority    *named   `json:"priority"`
		Assignee    *user    `json:"assignee"`
		Reporter    *user    `json:"reporter"`
		Labels      []string `json:"labels"`
		Description string   `json:"description"`
		Created     string   `json:"created"`
		Updated     string   `json:"updated"`
		Project     struct {
			Key string `json:"key"`
		} `json:"project"`
		Parent *struct {
			Key string `json:"key"`
		} `json:"parent"`
		Comment struct {
			Total    int `json:"total"`
			Comments []struct {
				Author  *user  `json:"author"`
				Body    string `json:"body"`
				Created string `json:"created"`
			} `json:"comments"`
		} `json:"comment"`
	} `json:"fields"`
}

func (i issue) priority() string {
	if i.Fields.Priority == nil {
		return ""
	}
	return i.Fields.Priority.Name
}

func browse(s atlassian.Site, key string) string { return s.Base + "/browse/" + key }
