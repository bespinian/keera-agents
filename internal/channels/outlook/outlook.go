// Package outlook lets agents read and send mail and keep a calendar in one
// Exchange Online mailbox, through Microsoft Graph, signed in as an app with
// client credentials.
//
// An app's mail permissions reach every mailbox in the tenant, so each
// binding names one mailbox and every call goes to /users/{mailbox}.
//
// Endpoints:
//   - "login": Microsoft Entra, default https://login.microsoftonline.com
//   - "graph": Microsoft Graph, default https://graph.microsoft.com/v1.0
package outlook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	scope   = "https://graph.microsoft.com/.default"
	service = "Microsoft Graph"
)

var (
	// validMailbox is a mailbox's address in lower case. It goes into paths.
	validMailbox = regexp.MustCompile(`^[a-z0-9._+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	// validID is a Graph message, event or folder id, or a well-known
	// folder's name. Older ids hold + and /, which are escaped in paths.
	validID = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{1,1000}$`)
)

type Adapter struct {
	auth  *microsoft.Auth
	graph string
	set   apitool.Set
}

func New(opts channels.Options) *Adapter {
	a := &Adapter{
		auth:  microsoft.NewAuth(opts),
		graph: web.Endpoint(opts, "graph", "https://graph.microsoft.com/v1.0"),
	}
	a.set = a.tools()
	return a
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.Outlook,
		Label:    "Outlook",
		Vendor:   "Microsoft",
		Category: "Email and calendar",
		Summary:  "Agents read, send, answer and file mail and keep the calendar of an Exchange Online mailbox.",
		Fields: []channels.Field{
			{Name: "tenant_id", Label: "Tenant ID", Required: true, Help: "Your Microsoft Entra tenant ID."},
			{Name: "client_id", Label: "Client ID", Required: true, Help: "The application (client) ID of the app registration."},
			{
				Name: "client_secret", Label: "Client secret", Secret: true, Required: true,
				Help: "A client secret of the app registration, from Certificates & secrets.",
			},
		},
		Setup: "1. In Microsoft Entra, register an app and create a client secret.\n" +
			"2. Give the app the Microsoft Graph application permissions Mail.ReadWrite, Mail.Send and Calendars.ReadWrite. " +
			"Granted in Entra, they reach every mailbox in your tenant, so limit them to the agents' mailboxes.\n" +
			"3. Best, use Exchange Online's RBAC for Applications: create a management scope for the agents' mailboxes and " +
			"assign the app the roles Application Mail.ReadWrite, Application Mail.Send and Application Calendars.ReadWrite " +
			"in that scope, without granting the permissions in Entra.\n" +
			"4. Or grant them in Entra with admin consent, and restrict the app with an application access policy " +
			"(New-ApplicationAccessPolicy) to a mail-enabled security group of the agents' mailboxes.\n" +
			"5. Bind each agent to one mailbox, best one made for it.",
		TargetLabel: "Mailbox",
		TargetHelp:  "The mailbox's email address, such as assistant@example.com.",
	}
}

func appOf(conn channels.Connection) (microsoft.App, error) {
	app := microsoft.App{
		Tenant:   web.Value(conn, "tenant_id"),
		ClientID: web.Value(conn, "client_id"),
		Secret:   web.Value(conn, "client_secret"),
		Scope:    scope,
	}
	if app.Tenant == "" || app.ClientID == "" || app.Secret == "" {
		return app, errors.New("the tenant ID, client ID or client secret is missing")
	}
	return app, nil
}

// mailbox checks a binding's target and returns it in lower case.
func mailbox(target string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(target))
	if m == channels.AnyTarget {
		return "", errors.New("an Outlook connector must be bound to one mailbox")
	}
	if !validMailbox.MatchString(m) {
		return "", fmt.Errorf("%q is not a mailbox's email address", web.Short(target, 80))
	}
	return m, nil
}

// id checks an id from the model before it goes into a path.
func id(what, s string) (string, error) {
	s = strings.TrimSpace(s)
	if !validID.MatchString(s) {
		return "", fmt.Errorf("%q is not a %s id", web.Short(s, 80), what)
	}
	return url.PathEscape(s), nil
}

// query encodes OData options. Graph wants their $ unescaped and spaces as
// %20, not +.
func query(q map[string]string) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strings.ReplaceAll(url.QueryEscape(q[k]), "+", "%20")
	}
	return "?" + strings.Join(parts, "&")
}

// call makes one Graph request under the mailbox and decodes the answer
// into out, unless it is nil.
func (a *Adapter) call(ctx context.Context, app microsoft.App, box, method, path string, q map[string]string, in, out any, prefer ...string) error {
	req, err := web.NewJSONRequest(ctx, method, a.graph+"/users/"+url.PathEscape(box)+path+query(q), in)
	if err != nil {
		return err
	}
	for _, p := range prefer {
		req.Header.Add("Prefer", p)
	}
	b, err := a.auth.Do(req, app, service, 16<<20)
	if err != nil || out == nil || len(b) == 0 {
		return err
	}
	return json.Unmarshal(b, out)
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	app, err := appOf(conn)
	if err != nil {
		return err
	}
	// The app reaches mailboxes, not a user of its own, so there is nothing
	// to ask Graph until a mailbox is bound; Target checks that one.
	return a.auth.Check(ctx, app)
}

func (a *Adapter) Target(ctx context.Context, conn channels.Connection, target string) (string, error) {
	app, err := appOf(conn)
	if err != nil {
		return "", err
	}
	box, err := mailbox(target)
	if err != nil {
		return "", err
	}
	// The inbox needs only the mail permission, and is refused when RBAC
	// for Applications or an access policy leaves the mailbox out.
	err = a.call(ctx, app, box, http.MethodGet, "/mailFolders/inbox", map[string]string{"$select": "id"}, nil, nil)
	switch web.Status(err) {
	case 0:
		if err != nil {
			return "", err
		}
		return box, nil
	case http.StatusNotFound:
		return "", fmt.Errorf("there is no Exchange Online mailbox %s in the tenant (%w)", box, err)
	case http.StatusForbidden, http.StatusUnauthorized:
		return "", fmt.Errorf("the app may not open %s; give it Mail.ReadWrite and include the mailbox in its RBAC scope or access policy (%w)", box, err)
	default:
		return "", err
	}
}

func (a *Adapter) Tools() []channels.Tool {
	return a.set.Tools()
}

func (a *Adapter) Call(ctx context.Context, conn channels.Connection, target, tool string, args json.RawMessage) (any, error) {
	return a.set.Call(ctx, conn, target, tool, args)
}

// run opens the connection and the binding's mailbox for a tool.
func run(f func(ctx context.Context, app microsoft.App, box string, args json.RawMessage) (any, error)) apitool.Run {
	return func(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
		app, err := appOf(conn)
		if err != nil {
			return nil, err
		}
		box, err := mailbox(target)
		if err != nil {
			return nil, err
		}
		return f(ctx, app, box, args)
	}
}
