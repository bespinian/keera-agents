// Package trello lets agents work on Trello boards: find and read cards,
// create, change and move them, and comment on them. It calls Trello's REST
// API with a Power-Up's API key and a token of the agents' account. A binding
// names a board; every call stays in it.
//
// Endpoints:
//   - "api": Trello's REST API, default https://api.trello.com
package trello

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

var (
	// validID is the id of a board, list, card, label or member. It goes
	// into URLs.
	validID = regexp.MustCompile(`^[0-9a-f]{24}$`)
	// validShort is the short link of a board or card, as in its address.
	validShort = regexp.MustCompile(`^[A-Za-z0-9]{8}$`)
	// validCredential is an API key or token. They go into a header.
	validCredential = regexp.MustCompile(`^[A-Za-z0-9]{16,256}$`)
)

var _ connectors.API = (*Adapter)(nil)

type Adapter struct {
	hc  *http.Client
	api string
	apitool.Set
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{hc: web.Client(opts), api: web.Endpoint(opts, "api", "https://api.trello.com")}
	a.Set = a.toolSet()
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Trello,
		Label:    "Trello",
		Vendor:   "Atlassian",
		Category: "Projects and wikis",
		Summary:  "Agents find, read, create and move cards, and comment on them.",
		Fields: []connectors.Field{
			{
				Name: "api_key", Label: "API key", Required: true,
				Help: "The API key of a Power-Up, from the Power-Up admin portal.",
			},
			{
				Name: "token", Label: "Token", Secret: true, Required: true,
				Help: "A token the agents' account made for that key.",
			},
		},
		Setup: "1. Create a Trello account for your agents and add it to the boards they work on.\n" +
			"2. At https://trello.com/power-ups/admin, create a Power-Up in one of your workspaces and generate its API key.\n" +
			"3. Signed in as the agents' account, open " +
			"`https://trello.com/1/authorize?expiration=never&scope=read,write&response_type=token&name=Keera+Agents&key=` " +
			"followed by the API key. Allow it and copy the token. Use `scope=read` if agents should only read.",
		TargetLabel: "Board",
		TargetHelp:  "The board's address, or the code in it, such as AbC123xY in https://trello.com/b/AbC123xY/roadmap.",
		AnyTarget:   true,
	}
}

// authorization is the header Trello takes the key and token in. In the
// query, as Trello's examples have them, they would show in the errors Go
// returns for a failed request, which name its URL.
func authorization(conn connectors.Connection) (string, error) {
	key, token := web.Value(conn, "api_key"), web.Value(conn, "token")
	if key == "" || token == "" {
		return "", errors.New("the API key or the token is missing")
	}
	if !validCredential.MatchString(key) || !validCredential.MatchString(token) {
		return "", errors.New("the API key and the token are letters and digits only; copy them again")
	}
	return `OAuth oauth_consumer_key="` + key + `", oauth_token="` + token + `"`, nil
}

// call calls Trello's API. path starts with /1/ and may have a query; in,
// if not nil, is sent as JSON.
func (a *Adapter) call(ctx context.Context, conn connectors.Connection, method, path string, in, out any) error {
	auth, err := authorization(conn)
	if err != nil {
		return err
	}
	req, err := web.NewJSONRequest(ctx, method, a.api+path, in)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	return web.DoJSON(a.hc, req, "Trello", 16<<20, func(status int, b []byte) error {
		// Trello answers most errors in plain text, and some in JSON.
		var e struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil {
			msg = e.Message
		}
		return &web.APIError{Service: "Trello", Status: status, Code: e.Error, Message: msg}
	}, out)
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	err := a.call(ctx, conn, http.MethodGet, "/1/members/me?fields=username", nil, &struct{}{})
	if web.Status(err) == http.StatusUnauthorized {
		return fmt.Errorf("the API key and token do not work with Trello; copy both again (%w)", err)
	}
	return err
}

// ref reads a board or card as people and models give it: its id, its short
// link, or its address, such as https://trello.com/c/AbC123xY/12-title. kind
// is "b" for a board, "c" for a card.
func ref(s, kind string) (string, bool) {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 || parts[0] != kind {
			return "", false
		}
		s = parts[1]
	}
	if validID.MatchString(s) || validShort.MatchString(s) {
		return s, true
	}
	return "", false
}

// unseen is whether a refusal means the account cannot see what was asked
// for. Trello answers 401 for an object the token may not read, and 400 for
// an id that is not one.
func unseen(err error) bool {
	switch web.Status(err) {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound:
		return true
	}
	return false
}

type board struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	URL    string `json:"url"`
	Closed bool   `json:"closed"`
}

// board looks a board up by its id, short link or address.
func (a *Adapter) board(ctx context.Context, conn connectors.Connection, named string) (board, error) {
	r, ok := ref(named, "b")
	if !ok {
		return board{}, fmt.Errorf("%q is not a board; give its address or the code in it, such as AbC123xY", web.Short(named, 80))
	}
	var b board
	err := a.call(ctx, conn, http.MethodGet, "/1/boards/"+r+"?fields=name,desc,url,closed", nil, &b)
	if unseen(err) {
		return board{}, fmt.Errorf("the account cannot see a Trello board %s; check the address and that the account is on the board (%w)", r, err)
	}
	return b, err
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	if target == connectors.AnyTarget {
		return target, nil
	}
	b, err := a.board(ctx, conn, target)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

// boardFor is the board a tool works on: the binding's, or the one named
// when the binding is to every board.
func (a *Adapter) boardFor(ctx context.Context, conn connectors.Connection, target, named string) (string, error) {
	named = strings.TrimSpace(named)
	if target != connectors.AnyTarget {
		if named == "" || named == target {
			return target, nil
		}
		b, err := a.board(ctx, conn, named)
		if err != nil {
			return "", err
		}
		if b.ID != target {
			return "", errors.New("this connector may use only its own board; leave board out")
		}
		return target, nil
	}
	if named == "" {
		return "", errors.New("give board, its address or id; list_boards lists them")
	}
	b, err := a.board(ctx, conn, named)
	return b.ID, err
}

// listBoard returns the board a list is on, and refuses a list outside the
// binding's board.
func (a *Adapter) listBoard(ctx context.Context, conn connectors.Connection, target, list string) (string, error) {
	list = strings.TrimSpace(list)
	if !validID.MatchString(list) {
		return "", fmt.Errorf("%q is not a list id; get_board lists them", web.Short(list, 40))
	}
	var l struct {
		IDBoard string `json:"idBoard"`
	}
	err := a.call(ctx, conn, http.MethodGet, "/1/lists/"+list+"?fields=idBoard", nil, &l)
	if unseen(err) {
		return "", fmt.Errorf("the account cannot see a list %s (%w)", list, err)
	}
	if err != nil {
		return "", err
	}
	if target != connectors.AnyTarget && l.IDBoard != target {
		return "", fmt.Errorf("list %s is not on this connector's board", list)
	}
	return l.IDBoard, nil
}

// ids checks ids an agent gives, such as labels', which go into a request.
func ids(what string, in []string) ([]string, error) {
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if !validID.MatchString(s) {
			return nil, fmt.Errorf("%q is not a %s id; get_board lists them", web.Short(s, 40), what)
		}
		out = append(out, s)
	}
	return out, nil
}
