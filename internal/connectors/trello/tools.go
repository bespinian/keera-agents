package trello

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

// What get_card returns at most, so one card does not fill the model's
// context.
const (
	maxComments    = 20
	maxDescription = 20000
	maxComment     = 4000
)

// cardFields are the fields of a card in a list.
const cardFields = "name,idBoard,idList,labels,idMembers,due,dueComplete,closed,shortUrl,dateLastActivity"

const boardArg = `"board":{"type":"string","description":"The board's address or id; only needed when the connector may use every board"}`

const cardArg = `"card":{"type":"string","description":"The card's id or address"}`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name:        "list_boards",
			Description: "List the boards this connector may use.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{}}`),
			Run:         apitool.Typed(a.listBoards),
		},
		{
			Name:        "get_board",
			Description: "Read a board: its lists, labels and members, with the ids the other tools take.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{` + boardArg + `}}`),
			Run:         apitool.Typed(a.getBoard),
		},
		{
			Name:        "list_cards",
			Description: "List the open cards on a board, or in one of its lists, in the board's order.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + boardArg + `,
				"list":{"type":"string","description":"Only cards in this list, by id"},
				"limit":{"type":"integer","description":"At most this many cards; default 50, up to 200"}}}`),
			Run: apitool.Typed(a.listCards),
		},
		{
			Name: "search_cards",
			Description: "Search cards by text, with Trello's search operators such as label:bug or due:week. " +
				"A connector limited to one board searches only that board.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"query":{"type":"string"},
				"limit":{"type":"integer","description":"At most this many cards; default 25, up to 100"}},"required":["query"]}`),
			Run: apitool.Typed(a.searchCards),
		},
		{
			Name:        "get_card",
			Description: "Read one card: its fields, description, checklists and latest comments.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{` + cardArg + `},"required":["card"]}`),
			Run:         apitool.Typed(a.getCard),
		},
		{
			Name:        "create_card",
			Description: "Create a card in a list. The description is Markdown.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"list":{"type":"string","description":"The list's id, from get_board"},
				"name":{"type":"string"},
				"description":{"type":"string"},
				"due":{"type":"string","description":"When it is due, such as 2026-12-01T17:00:00Z"},
				"labels":{"type":"array","items":{"type":"string"},"description":"Label ids, from get_board"},
				"members":{"type":"array","items":{"type":"string"},"description":"Member ids, from get_board"},
				"top":{"type":"boolean","description":"Put it at the top of the list rather than the bottom"}},"required":["list","name"]}`),
			Write: true,
			Run:   apitool.Typed(a.createCard),
		},
		{
			Name: "update_card",
			Description: "Change a card, move it to another list on its board, or archive it. " +
				"Leave out what stays the same.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + cardArg + `,
				"name":{"type":"string"},
				"description":{"type":"string","description":"Replaces the description; Markdown"},
				"list":{"type":"string","description":"Move it to this list on the same board, by id"},
				"due":{"type":"string","description":"When it is due, or none to clear it"},
				"done":{"type":"boolean","description":"Whether the due date is complete"},
				"labels":{"type":"array","items":{"type":"string"},"description":"Replaces all labels, by id"},
				"members":{"type":"array","items":{"type":"string"},"description":"Replaces all members, by id"},
				"archived":{"type":"boolean"}},"required":["card"]}`),
			Write: true,
			Run:   apitool.Typed(a.updateCard),
		},
		{
			Name:        "comment",
			Description: "Comment on a card, in Markdown.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + cardArg + `,
				"text":{"type":"string"}},"required":["card","text"]}`),
			Write: true,
			Run:   apitool.Typed(a.comment),
		},
	}
}

// Board is a board in a list.
type Board struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func (a *Adapter) listBoards(ctx context.Context, conn connectors.Connection, target string, _ struct{}) (any, error) {
	if target != connectors.AnyTarget {
		b, err := a.board(ctx, conn, target)
		if err != nil {
			return nil, err
		}
		return []Board{{ID: b.ID, Name: b.Name, URL: b.URL}}, nil
	}
	var bs []board
	if err := a.call(ctx, conn, http.MethodGet, "/1/members/me/boards?filter=open&fields=name,url", nil, &bs); err != nil {
		return nil, err
	}
	out := []Board{}
	for _, b := range bs {
		out = append(out, Board{ID: b.ID, Name: b.Name, URL: b.URL})
	}
	return out, nil
}

type label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// title is a label as people see it: its name, or its colour when it has
// none.
func (l label) title() string {
	if l.Name != "" {
		return l.Name
	}
	return l.Color
}

type member struct {
	ID       string `json:"id"`
	FullName string `json:"fullName"`
	Username string `json:"username"`
}

type card struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Desc             string   `json:"desc"`
	IDBoard          string   `json:"idBoard"`
	IDList           string   `json:"idList"`
	IDMembers        []string `json:"idMembers"`
	Labels           []label  `json:"labels"`
	Due              *string  `json:"due"`
	DueComplete      bool     `json:"dueComplete"`
	Closed           bool     `json:"closed"`
	ShortURL         string   `json:"shortUrl"`
	DateLastActivity string   `json:"dateLastActivity"`
	List             *struct {
		Name string `json:"name"`
	} `json:"list"`
	Members    []member `json:"members"`
	Checklists []struct {
		Name       string `json:"name"`
		CheckItems []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"checkItems"`
	} `json:"checklists"`
	Actions []struct {
		Date          string `json:"date"`
		MemberCreator *struct {
			FullName string `json:"fullName"`
		} `json:"memberCreator"`
		Data struct {
			Text string `json:"text"`
		} `json:"data"`
	} `json:"actions"`
}

// Brief is a card in a list.
type Brief struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	List    string   `json:"list,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Members []string `json:"members,omitempty"`
	Due     string   `json:"due,omitempty"`
	Done    bool     `json:"done,omitempty"`
	// Archived cards show up only in search and get_card.
	Archived bool   `json:"archived,omitempty"`
	Updated  string `json:"updated,omitempty"`
}

// brief makes a card's Brief. lists and members name the ids a card holds,
// where the caller has them.
func (c card) brief(lists, members map[string]string) Brief {
	b := Brief{
		ID: c.ID, Name: c.Name, URL: c.ShortURL, List: lists[c.IDList],
		Done: c.DueComplete, Archived: c.Closed, Updated: c.DateLastActivity,
	}
	if c.List != nil {
		b.List = c.List.Name
	}
	if c.Due != nil {
		b.Due = *c.Due
	}
	for _, l := range c.Labels {
		b.Labels = append(b.Labels, l.title())
	}
	for _, id := range c.IDMembers {
		if m := members[id]; m != "" {
			b.Members = append(b.Members, m)
		}
	}
	return b
}

type boardData struct {
	board
	Lists []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"lists"`
	Labels  []label  `json:"labels"`
	Members []member `json:"members"`
	Cards   []card   `json:"cards"`
}

// boardData reads a board with its open lists, labels and members, and its
// open cards if cards is set, in one call.
func (a *Adapter) boardData(ctx context.Context, conn connectors.Connection, id string, cards bool) (boardData, error) {
	q := url.Values{
		"fields":        {"name,desc,url"},
		"lists":         {"open"},
		"list_fields":   {"name"},
		"labels":        {"all"},
		"label_fields":  {"name,color"},
		"labels_limit":  {"1000"},
		"members":       {"all"},
		"member_fields": {"fullName,username"},
	}
	if cards {
		q.Set("cards", "open")
		q.Set("card_fields", cardFields)
	}
	var d boardData
	err := a.call(ctx, conn, http.MethodGet, "/1/boards/"+id+"?"+q.Encode(), nil, &d)
	if unseen(err) {
		return boardData{}, fmt.Errorf("the account cannot see board %s (%w)", id, err)
	}
	return d, err
}

// BoardDetail is a board with what the other tools take ids of.
type BoardDetail struct {
	Board
	Description string   `json:"description,omitempty"`
	Lists       []Named  `json:"lists"`
	Labels      []Label  `json:"labels"`
	Members     []Person `json:"members"`
}

type Named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Color string `json:"color,omitempty"`
}

type Person struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

func (a *Adapter) getBoard(ctx context.Context, conn connectors.Connection, target string, in struct {
	Board string `json:"board"`
},
) (any, error) {
	id, err := a.boardFor(ctx, conn, target, in.Board)
	if err != nil {
		return nil, err
	}
	d, err := a.boardData(ctx, conn, id, false)
	if err != nil {
		return nil, err
	}
	out := BoardDetail{
		ID: d.ID, Name: d.Name, URL: d.URL,
		Description: web.Cut(d.Desc, maxDescription, "…"),
		Lists:       []Named{}, Labels: []Label{}, Members: []Person{},
	}
	for _, l := range d.Lists {
		out.Lists = append(out.Lists, Named{ID: l.ID, Name: l.Name})
	}
	for _, l := range d.Labels {
		out.Labels = append(out.Labels, Label(l))
	}
	for _, m := range d.Members {
		out.Members = append(out.Members, Person{ID: m.ID, Name: m.FullName, Username: m.Username})
	}
	return out, nil
}

func (a *Adapter) listCards(ctx context.Context, conn connectors.Connection, target string, in struct {
	Board string `json:"board"`
	List  string `json:"list"`
	Limit int    `json:"limit"`
},
) (any, error) {
	var id string
	var err error
	if strings.TrimSpace(in.List) != "" {
		id, err = a.listBoard(ctx, conn, target, in.List)
	} else {
		id, err = a.boardFor(ctx, conn, target, in.Board)
	}
	if err != nil {
		return nil, err
	}
	d, err := a.boardData(ctx, conn, id, true)
	if err != nil {
		return nil, err
	}
	lists, members := map[string]string{}, map[string]string{}
	for _, l := range d.Lists {
		lists[l.ID] = l.Name
	}
	for _, m := range d.Members {
		members[m.ID] = m.FullName
	}
	limit := apitool.Limit(in.Limit, 50, 200)
	out := []Brief{}
	more := false
	for _, c := range d.Cards {
		if in.List != "" && c.IDList != strings.TrimSpace(in.List) {
			continue
		}
		if len(out) == limit {
			more = true
			break
		}
		out = append(out, c.brief(lists, members))
	}
	return map[string]any{"cards": out, "more": more}, nil
}

func (a *Adapter) searchCards(ctx context.Context, conn connectors.Connection, target string, in struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
},
) (any, error) {
	if err := apitool.Need("query", in.Query); err != nil {
		return nil, err
	}
	// Trello narrows the search to the boards named; "mine" is every board
	// the account is on.
	boards := "mine"
	if target != connectors.AnyTarget {
		boards = target
	}
	q := url.Values{
		"query":       {strings.TrimSpace(in.Query)},
		"idBoards":    {boards},
		"modelTypes":  {"cards"},
		"cards_limit": {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))},
		"card_fields": {cardFields},
		"card_list":   {"true"},
		"partial":     {"true"},
	}
	var res struct {
		Cards []card `json:"cards"`
	}
	if err := a.call(ctx, conn, http.MethodGet, "/1/search?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	out := []Brief{}
	for _, c := range res.Cards {
		// The board was already in the query; this holds even if Trello
		// ever reads it differently.
		if target != connectors.AnyTarget && c.IDBoard != target {
			continue
		}
		out = append(out, c.brief(nil, nil))
	}
	return map[string]any{"cards": out}, nil
}

// Card is one card in full.
type Card struct {
	Brief
	Board           string      `json:"board"`
	ListID          string      `json:"list_id"`
	Description     string      `json:"description,omitempty"`
	Checklists      []Checklist `json:"checklists,omitempty"`
	Comments        []Comment   `json:"comments,omitempty"`
	EarlierComments bool        `json:"earlier_comments,omitempty"`
}

type Checklist struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

type Comment struct {
	Author string `json:"author,omitempty"`
	Text   string `json:"text"`
	At     string `json:"at"`
}

// card reads a card and refuses one outside the binding's board. It asks
// Trello rather than trusting the address, since a card can be moved.
func (a *Adapter) card(ctx context.Context, conn connectors.Connection, target, named string, full bool) (card, error) {
	r, ok := ref(named, "c")
	if !ok {
		return card{}, fmt.Errorf("%q is not a card; give its id or address", web.Short(named, 80))
	}
	q := url.Values{"fields": {cardFields}}
	if full {
		q = url.Values{
			"fields":                      {cardFields + ",desc"},
			"list":                        {"true"},
			"list_fields":                 {"name"},
			"members":                     {"true"},
			"member_fields":               {"fullName,username"},
			"checklists":                  {"all"},
			"checklist_fields":            {"name"},
			"actions":                     {"commentCard"},
			"actions_limit":               {strconv.Itoa(maxComments + 1)},
			"action_fields":               {"data,date"},
			"action_memberCreator_fields": {"fullName"},
		}
	}
	var c card
	err := a.call(ctx, conn, http.MethodGet, "/1/cards/"+r+"?"+q.Encode(), nil, &c)
	if unseen(err) {
		return card{}, fmt.Errorf("the account cannot see a card %s (%w)", r, err)
	}
	if err != nil {
		return card{}, err
	}
	if target != connectors.AnyTarget && c.IDBoard != target {
		return card{}, fmt.Errorf("card %s is not on this connector's board, the only board it may use", r)
	}
	return c, nil
}

func (a *Adapter) getCard(ctx context.Context, conn connectors.Connection, target string, in struct {
	Card string `json:"card"`
},
) (any, error) {
	c, err := a.card(ctx, conn, target, in.Card, true)
	if err != nil {
		return nil, err
	}
	members := map[string]string{}
	for _, m := range c.Members {
		members[m.ID] = m.FullName
	}
	out := Card{
		Brief: c.brief(nil, members), Board: c.IDBoard, ListID: c.IDList,
		Description: web.Cut(c.Desc, maxDescription, "…"),
	}
	for _, cl := range c.Checklists {
		list := Checklist{Name: cl.Name, Items: []string{}}
		for _, it := range cl.CheckItems {
			box := "[ ] "
			if it.State == "complete" {
				box = "[x] "
			}
			list.Items = append(list.Items, box+it.Name)
		}
		out.Checklists = append(out.Checklists, list)
	}
	// Trello lists comments newest first; the model reads them in order.
	acts := c.Actions
	if len(acts) > maxComments {
		acts, out.EarlierComments = acts[:maxComments], true
	}
	for _, x := range slices.Backward(acts) {

		cm := Comment{Text: web.Cut(x.Data.Text, maxComment, "…"), At: x.Date}
		if x.MemberCreator != nil {
			cm.Author = x.MemberCreator.FullName
		}
		out.Comments = append(out.Comments, cm)
	}
	return out, nil
}

func (a *Adapter) createCard(ctx context.Context, conn connectors.Connection, target string, in struct {
	List        string   `json:"list"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Due         string   `json:"due"`
	Labels      []string `json:"labels"`
	Members     []string `json:"members"`
	Top         bool     `json:"top"`
},
) (any, error) {
	if err := apitool.Need("name", in.Name); err != nil {
		return nil, err
	}
	if _, err := a.listBoard(ctx, conn, target, in.List); err != nil {
		return nil, err
	}
	labels, err := ids("label", in.Labels)
	if err != nil {
		return nil, err
	}
	members, err := ids("member", in.Members)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"idList": strings.TrimSpace(in.List), "name": strings.TrimSpace(in.Name), "desc": in.Description,
		"idLabels": strings.Join(labels, ","), "idMembers": strings.Join(members, ","), "pos": "bottom",
	}
	if in.Top {
		body["pos"] = "top"
	}
	if d := strings.TrimSpace(in.Due); d != "" {
		body["due"] = d
	}
	var c card
	if err := a.call(ctx, conn, http.MethodPost, "/1/cards", body, &c); err != nil {
		return nil, err
	}
	return c.brief(nil, nil), nil
}

func (a *Adapter) updateCard(ctx context.Context, conn connectors.Connection, target string, in struct {
	Card        string    `json:"card"`
	Name        *string   `json:"name"`
	Description *string   `json:"description"`
	List        string    `json:"list"`
	Due         string    `json:"due"`
	Done        *bool     `json:"done"`
	Labels      *[]string `json:"labels"`
	Members     *[]string `json:"members"`
	Archived    *bool     `json:"archived"`
},
) (any, error) {
	c, err := a.card(ctx, conn, target, in.Card, false)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if in.Name != nil {
		if err := apitool.Need("name", *in.Name); err != nil {
			return nil, err
		}
		body["name"] = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		body["desc"] = *in.Description
	}
	if list := strings.TrimSpace(in.List); list != "" {
		// Moving a card to another board would take it out of what the
		// binding names, and needs more than its list.
		b, err := a.listBoard(ctx, conn, target, list)
		if err != nil {
			return nil, err
		}
		if b != c.IDBoard {
			return nil, errors.New("the list is on another board; a card moves only within its board")
		}
		body["idList"] = list
	}
	switch d := strings.TrimSpace(in.Due); {
	case strings.EqualFold(d, "none"):
		body["due"] = nil
	case d != "":
		body["due"] = d
	}
	if in.Done != nil {
		body["dueComplete"] = *in.Done
	}
	if in.Labels != nil {
		labels, err := ids("label", *in.Labels)
		if err != nil {
			return nil, err
		}
		body["idLabels"] = strings.Join(labels, ",")
	}
	if in.Members != nil {
		members, err := ids("member", *in.Members)
		if err != nil {
			return nil, err
		}
		body["idMembers"] = strings.Join(members, ",")
	}
	if in.Archived != nil {
		body["closed"] = *in.Archived
	}
	if len(body) == 0 {
		return nil, errors.New("give something to change")
	}
	var out card
	if err := a.call(ctx, conn, http.MethodPut, "/1/cards/"+c.ID, body, &out); err != nil {
		return nil, err
	}
	return out.brief(nil, nil), nil
}

func (a *Adapter) comment(ctx context.Context, conn connectors.Connection, target string, in struct {
	Card string `json:"card"`
	Text string `json:"text"`
},
) (any, error) {
	if err := apitool.Need("text", in.Text); err != nil {
		return nil, err
	}
	c, err := a.card(ctx, conn, target, in.Card, false)
	if err != nil {
		return nil, err
	}
	if err := a.call(ctx, conn, http.MethodPost, "/1/cards/"+c.ID+"/actions/comments", map[string]string{"text": in.Text}, nil); err != nil {
		return nil, err
	}
	return map[string]string{"card": c.ID, "url": c.ShortURL}, nil
}
