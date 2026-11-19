// Package exchange reaches one Exchange Online mailbox through Microsoft
// Graph, for the Outlook adapters: as an app with client credentials, or as
// each person who signs in with their own account.
//
// An app's permissions reach every mailbox in the tenant, so each binding
// names one mailbox and every call goes to /users/{mailbox}. On a connector
// people sign in to, a binding may name "me" instead: the mailbox of the
// person the task is for.
package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/mailtext"
	"github.com/bespinian/keera-agents/internal/connectors/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const (
	Scope   = "https://graph.microsoft.com/.default"
	service = "Microsoft Graph"
	// Me is a binding to the mailbox of the person the task is for.
	Me = "me"
)

var (
	// validMailbox is a mailbox's address in lower case. It goes into paths.
	validMailbox = regexp.MustCompile(`^[a-z0-9._+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	// validID is a Graph message, event or folder id, or a well-known
	// folder's name. Older ids hold + and /, which are escaped in paths.
	validID = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{1,1000}$`)
)

// TargetHelp explains a binding's target in the panel.
const TargetHelp = "The mailbox's email address, such as assistant@example.com. On a connector people sign in to, " +
	"me is the mailbox of whoever gives the agent work."

// Client calls Graph under one mailbox.
type Client struct {
	Auth  *microsoft.Auth
	graph string
}

func NewClient(opts connectors.Options) *Client {
	return &Client{
		Auth:  microsoft.NewAuth(opts),
		graph: web.Endpoint(opts, "graph", "https://graph.microsoft.com/v1.0"),
	}
}

// Mailbox checks a binding's target and returns it in lower case.
func Mailbox(conn connectors.Connection, target string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(target))
	if m == connectors.AnyTarget {
		return "", errors.New("an Outlook connector must be bound to one mailbox")
	}
	if m == Me {
		if !conn.PerUser {
			return "", errors.New("me is for a connector people sign in to; name the mailbox")
		}
		return m, nil
	}
	if !validMailbox.MatchString(m) {
		return "", fmt.Errorf("%q is not a mailbox's email address", web.Short(target, 80))
	}
	return m, nil
}

// ID checks an id from the model before it goes into a path.
func ID(what, s string) (string, error) {
	s = strings.TrimSpace(s)
	if !validID.MatchString(s) {
		return "", fmt.Errorf("%q is not a %s id", web.Short(s, 80), what)
	}
	return url.PathEscape(s), nil
}

// Call makes one Graph request under the mailbox and decodes the answer
// into out, unless it is nil.
func (c *Client) Call(ctx context.Context, app microsoft.App, box, method, path string, q map[string]string, in, out any, prefer ...string) error {
	root := c.graph + "/users/" + url.PathEscape(box)
	if box == Me {
		root = c.graph + "/me"
	}
	req, err := web.NewJSONRequest(ctx, method, root+path+microsoft.Query(q), in)
	if err != nil {
		return err
	}
	for _, p := range prefer {
		req.Header.Add("Prefer", p)
	}
	return c.Auth.DoJSON(req, app, service, 16<<20, out)
}

func (c *Client) Check(ctx context.Context, conn connectors.Connection) error {
	app, err := microsoft.AppOf(conn, Scope)
	if err != nil {
		return err
	}
	// The app reaches mailboxes, not a user of its own, so there is nothing
	// to ask Graph until a mailbox is bound; Target checks that one.
	return c.Auth.Check(ctx, app)
}

// Target checks a binding by reading path in the mailbox, which needs the
// permission named, and is refused when RBAC for Applications or an access
// policy leaves the mailbox out.
func (c *Client) Target(ctx context.Context, conn connectors.Connection, target, path, permission string) (string, error) {
	app, err := microsoft.AppOf(conn, Scope)
	if err != nil {
		return "", err
	}
	box, err := Mailbox(conn, target)
	if err != nil {
		return "", err
	}
	err = c.Call(ctx, app, box, http.MethodGet, path, map[string]string{"$select": "id"}, nil, nil)
	switch web.Status(err) {
	case 0:
		if err != nil {
			return "", err
		}
		return box, nil
	case http.StatusNotFound:
		return "", fmt.Errorf("there is no Exchange Online mailbox %s in the tenant (%w)", box, err)
	case http.StatusForbidden, http.StatusUnauthorized:
		if conn.PerUser {
			return "", fmt.Errorf("you may not open %s; it must be shared with you, or name me for your own (%w)", box, err)
		}
		return "", fmt.Errorf("the app may not open %s; give it %s and include the mailbox in its RBAC scope or access policy (%w)", box, permission, err)
	default:
		return "", err
	}
}

// Run opens the connection and the binding's mailbox for a tool.
func Run[T any](f func(ctx context.Context, app microsoft.App, box string, in T) (any, error)) apitool.Run {
	return apitool.Typed(func(ctx context.Context, conn connectors.Connection, target string, in T) (any, error) {
		app, err := microsoft.AppOf(conn, Scope)
		if err != nil {
			return nil, err
		}
		box, err := Mailbox(conn, target)
		if err != nil {
			return nil, err
		}
		return f(ctx, app, box, in)
	})
}

type EmailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// Recipient is a person on a message or an event, as Graph writes them.
type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

func (r Recipient) String() string {
	if r.EmailAddress.Name == "" || r.EmailAddress.Name == r.EmailAddress.Address {
		return r.EmailAddress.Address
	}
	return r.EmailAddress.Name + " <" + r.EmailAddress.Address + ">"
}

func Names(rs []Recipient) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.String()
	}
	return out
}

// Recipients checks addresses the model gives, which may name the person
// too, such as "Ada <ada@example.com>".
func Recipients(field string, list []string) ([]Recipient, error) {
	addrs, err := mailtext.Addresses(field, list)
	out := make([]Recipient, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, Recipient{EmailAddress: EmailAddress{Name: addr.Name, Address: addr.Address}})
	}
	return out, err
}

// Body is a message's or an event's body.
type Body struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// TextBody asks Graph for bodies as plain text.
const TextBody = `outlook.body-content-type="text"`
