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
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace"
)

// scope reads, sends, drafts and labels mail, but never deletes it for
// good.
const scope = "https://www.googleapis.com/auth/gmail.modify"

// validID is a message's, thread's, draft's or label's id. It goes into
// paths.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

type Adapter struct {
	client *workspace.Client
	api    string
	apitool.Set
	workspace.Accounts
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{
		client: workspace.NewClient(opts, scope),
		api:    web.Endpoint(opts, "gmail", "https://gmail.googleapis.com") + "/gmail/v1/users/me",
	}
	a.Set = a.toolSet()
	a.Accounts = workspace.NewAccounts(a.client, "Gmail API")
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Gmail,
		Label:    "Gmail",
		Vendor:   "Google",
		Category: "Email and calendar",
		Summary:  "Agents search, read, send, answer, draft and label mail in a Gmail mailbox.",
		Fields:   workspace.DelegationFields("Gmail API"),
		Setup: workspace.DelegationSetup("Gmail API", scope, "act as any mailbox in your domain",
			"Bind each agent to one mailbox, best one made for it, such as assistant@acme.com."),
		TargetLabel: "Mailbox",
		TargetHelp: "The mailbox's address, such as assistant@acme.com. On a connector people sign in to, " +
			"me: the mailbox of whoever gives the agent work.",
		Mailbox: true,
	}
}

// call makes one request under the mailbox, at path with query q.
func (a *Adapter) call(ctx context.Context, s workspace.Settings, method, path string, q url.Values, in, out any) error {
	u := a.api + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return a.client.Do(ctx, s, method, u, in, out)
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	// A service account has no mailbox, so there is nothing to ask Gmail
	// until one is bound; Target checks it.
	return a.client.Check(ctx, conn, "Gmail API", func(context.Context, workspace.Settings) error { return nil })
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	return a.client.PersonTarget(ctx, conn, target, "mailbox", func(ctx context.Context, s workspace.Settings) error {
		err := a.call(ctx, s, http.MethodGet, "/profile", nil, nil, &struct{}{})
		if st := web.Status(err); st == http.StatusBadRequest || st == http.StatusNotFound {
			return fmt.Errorf("%s has no Gmail mailbox (%w)", s.Who(), err)
		}
		return err
	})
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
	return workspace.PersonRun("mailbox", f)
}
