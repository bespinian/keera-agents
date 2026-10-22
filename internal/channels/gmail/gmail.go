// Package gmail lets agents search, read, send, answer, draft and label mail
// in one Gmail mailbox.
//
// A service account has no mailbox of its own, so a shared connector acts
// as the binding's mailbox through domain-wide delegation. On a connector
// people sign in to, a binding names "me": the mailbox of the person the
// task is for, the only one Gmail lets their token reach.
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "auth": where people sign in, default https://accounts.google.com/o/oauth2/v2/auth
//   - "gmail": the Gmail API, default https://gmail.googleapis.com
package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/google"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
	"github.com/bespinian/keera-agents/internal/channels/internal/workspace"
)

// scope reads, sends, drafts and labels mail, but never deletes it for
// good.
const scope = "https://www.googleapis.com/auth/gmail.modify"

// me is a binding to the mailbox of the person the task is for.
const me = "me"

// validID is a message's, thread's, draft's or label's id. It goes into
// paths.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

type Adapter struct {
	client *workspace.Client
	api    string
	apitool.Set
	workspace.Accounts
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{
		client: workspace.NewClient(opts, scope),
		api:    web.Endpoint(opts, "gmail", "https://gmail.googleapis.com") + "/gmail/v1/users/me",
	}
	a.Set = a.toolSet()
	a.Accounts = workspace.NewAccounts(a.client, "Gmail API")
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.Gmail,
		Label:    "Gmail",
		Vendor:   "Google",
		Category: "Email and calendar",
		Summary:  "Agents search, read, send, answer, draft and label mail in a Gmail mailbox.",
		Fields: []channels.Field{
			{
				Name: "service_account_json", Label: "Service account key", Secret: true, Required: true, Multiline: true,
				Help: "The JSON key file of a service account with domain-wide delegation, in a project with the Gmail API enabled.",
			},
		},
		Setup: "1. In a Google Cloud project, enable the Gmail API, and create a service account with a JSON key.\n" +
			"2. As a Workspace administrator, under Security, API controls, Domain-wide delegation, grant the service " +
			"account's client ID the scope " + scope + ". It then may act as any mailbox in your domain, " +
			"so keep its key here only.\n" +
			"3. Bind each agent to one mailbox, best one made for it, such as assistant@acme.com.",
		TargetLabel: "Mailbox",
		TargetHelp: "The mailbox's address, such as assistant@acme.com. On a connector people sign in to, " +
			"me: the mailbox of whoever gives the agent work.",
	}
}

// mailbox is how a call reaches the binding's mailbox: as it, through
// domain-wide delegation, or with the person's own token for me.
func mailbox(conn channels.Connection, target string) (workspace.Settings, error) {
	box := strings.ToLower(strings.TrimSpace(target))
	switch {
	case box == me && !conn.PerUser:
		return workspace.Settings{}, errors.New("me is for a connector people sign in to; name the mailbox")
	case box == me:
		return workspace.SettingsOf(conn)
	case conn.PerUser:
		return workspace.Settings{}, errors.New("a person's sign-in reaches only their own mailbox; name me")
	}
	if addr, err := mail.ParseAddress(box); err != nil || addr.Address != box || addr.Name != "" {
		return workspace.Settings{}, fmt.Errorf("%q is not a mailbox's address", web.Short(target, 80))
	}
	s, err := workspace.SettingsOf(conn)
	s.Subject = box
	return s, err
}

// call makes one request under the mailbox, at path with query q.
func (a *Adapter) call(ctx context.Context, s workspace.Settings, method, path string, q url.Values, in, out any) error {
	u := a.api + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return a.client.Do(ctx, s, method, u, in, out)
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	// A service account has no mailbox, so there is nothing to ask Gmail
	// until one is bound; Target checks it.
	return a.client.Check(ctx, conn, "Gmail API", func(context.Context, workspace.Settings) error { return nil })
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	s, err := mailbox(conn, target)
	if err != nil {
		return "", err
	}
	var p struct {
		Email string `json:"emailAddress"`
	}
	err = a.call(ctx, s, http.MethodGet, "/profile", nil, nil, &p)
	// Google refuses to sign in as a mailbox the service account may not
	// act as.
	if _, ok := errors.AsType[*oauth.Error](err); ok && s.Subject != "" {
		return "", google.Delegation(err, scope)
	}
	switch web.Status(err) {
	case 0:
		if err != nil {
			return "", err
		}
	case http.StatusBadRequest, http.StatusNotFound:
		return "", fmt.Errorf("%s has no Gmail mailbox (%w)", s.Who(), err)
	default:
		return "", err
	}
	if s.Subject == "" {
		return me, nil
	}
	return s.Subject, nil
}

// id checks an id from the model before it goes into a path.
func id(what, s string) (string, error) {
	s = strings.TrimSpace(s)
	if !validID.MatchString(s) {
		return "", fmt.Errorf("%q is not a %s id", web.Short(s, 80), what)
	}
	return s, nil
}

// run opens the binding's mailbox for a tool.
func run[T any](f func(ctx context.Context, s workspace.Settings, in T) (any, error)) apitool.Run {
	return func(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
		s, err := mailbox(conn, target)
		if err != nil {
			return nil, err
		}
		in, err := apitool.Decode[T](args)
		if err != nil {
			return nil, err
		}
		return f(ctx, s, in)
	}
}
