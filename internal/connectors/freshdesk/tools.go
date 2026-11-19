package freshdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

// What get_ticket returns at most, so one ticket does not fill the model's
// context.
const (
	maxMessages    = 20
	maxDescription = 20000
	maxMessage     = 4000
)

// statuses and priorities are Freshdesk's own numbers for them. A helpdesk
// may add statuses; those show as their number.
var (
	statuses   = []string{2: "open", 3: "pending", 4: "resolved", 5: "closed"}
	priorities = []string{1: "low", 2: "medium", 3: "high", 4: "urgent"}
)

func name(names []string, n int) string {
	if n > 0 && n < len(names) && names[n] != "" {
		return names[n]
	}
	return strconv.Itoa(n)
}

// number is a status's or priority's number by its name, or 0.
func number(names []string, s string) int {
	if i := slices.Index(names, strings.ToLower(strings.TrimSpace(s))); i > 0 {
		return i
	}
	return 0
}

const ticketArg = `"id":{"type":"integer","description":"The ticket's number"}`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name: "search_tickets",
			Description: "Find tickets with a Freshdesk filter, such as status:2 AND priority:4 or tag:'refund'. " +
				"Statuses are 2 open, 3 pending, 4 resolved, 5 closed; priorities 1 low to 4 urgent. " +
				"Without a filter, lists the latest tickets. A connector limited to one group searches only that group.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"filter":{"type":"string","description":"Fields and values; text values in single quotes"},
				"page":{"type":"integer","description":"The page, of 30 tickets; default 1, up to 10"}}}`),
			Run: run(a, a.searchTickets),
		},
		{
			Name:        "get_ticket",
			Description: "Read one ticket: its fields, requester, description and latest replies and notes.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{` + ticketArg + `},"required":["id"]}`),
			Run:         run(a, a.getTicket),
		},
		{
			Name:        "find_contacts",
			Description: "Find customers by email or name, to get the id create_ticket takes.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"query":{"type":"string","description":"An email address, or the start of a name"}},"required":["query"]}`),
			Run: run(a, a.findContacts),
		},
		{
			Name:        "list_groups",
			Description: "List the groups this connector may use.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{}}`),
			Run:         run(a, a.listGroups),
		},
		{
			Name:        "list_agents",
			Description: "List the Freshdesk agents tickets can be assigned to, in this connector's group if it has one.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"email":{"type":"string","description":"Only the agent with this email address"}}}`),
			Run: run(a, a.listAgents),
		},
		{
			Name:        "create_ticket",
			Description: "Create a ticket for a customer, by their email or contact id.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"email":{"type":"string","description":"The customer's email; a new one becomes a contact"},
				"contact_id":{"type":"integer"},
				"subject":{"type":"string"},
				"description":{"type":"string","description":"Plain text"},
				"priority":{"type":"string","enum":["low","medium","high","urgent"],"description":"Default low"},
				"group_id":{"type":"integer","description":"Only when the connector may use every group"},
				"tags":{"type":"array","items":{"type":"string"}}},"required":["subject","description"]}`),
			Write: true,
			Run:   run(a, a.createTicket),
		},
		{
			Name:        "update_ticket",
			Description: "Change a ticket's status, priority, agent, tags or subject. Leave out what stays the same.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + ticketArg + `,
				"status":{"type":"string","enum":["open","pending","resolved","closed"]},
				"priority":{"type":"string","enum":["low","medium","high","urgent"]},
				"agent_id":{"type":"integer","description":"The agent to assign, from list_agents, or 0 to unassign"},
				"tags":{"type":"array","items":{"type":"string"},"description":"Replaces all tags"},
				"subject":{"type":"string"}},"required":["id"]}`),
			Write: true,
			Run:   run(a, a.updateTicket),
		},
		{
			Name:        "reply",
			Description: "Reply to a ticket. The customer gets the reply by email.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + ticketArg + `,
				"body":{"type":"string","description":"Plain text"}},"required":["id","body"]}`),
			Write: true,
			Run:   run(a, a.reply),
		},
		{
			Name:        "add_note",
			Description: "Add a note to a ticket, for the team only unless public is set.",
			Parameters: apitool.Schema(`{"type":"object","properties":{` + ticketArg + `,
				"body":{"type":"string","description":"Plain text"},
				"public":{"type":"boolean","description":"Show the note to the customer too"}},"required":["id","body"]}`),
			Write: true,
			Run:   run(a, a.addNote),
		},
	}
}

// htmlOf turns the plain text the model writes into the HTML Freshdesk
// shows.
func htmlOf(s string) string {
	return strings.ReplaceAll(html.EscapeString(strings.TrimSpace(s)), "\n", "<br>")
}

type ticket struct {
	ID              int64    `json:"id"`
	Subject         string   `json:"subject"`
	Status          int      `json:"status"`
	Priority        int      `json:"priority"`
	Type            *string  `json:"type"`
	GroupID         *int64   `json:"group_id"`
	ResponderID     *int64   `json:"responder_id"`
	RequesterID     int64    `json:"requester_id"`
	Tags            []string `json:"tags"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	DueBy           string   `json:"due_by"`
	DescriptionText string   `json:"description_text"`
	Requester       *struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"requester"`
}

// Brief is a ticket in a list.
type Brief struct {
	ID          int64    `json:"id"`
	Subject     string   `json:"subject"`
	Status      string   `json:"status"`
	Priority    string   `json:"priority"`
	Type        string   `json:"type,omitempty"`
	GroupID     *int64   `json:"group_id,omitempty"`
	AgentID     *int64   `json:"agent_id,omitempty"`
	RequesterID int64    `json:"requester_id"`
	Tags        []string `json:"tags,omitempty"`
	Updated     string   `json:"updated"`
	URL         string   `json:"url"`
}

func (t ticket) brief(s site) Brief {
	b := Brief{
		ID: t.ID, Subject: t.Subject, Status: name(statuses, t.Status), Priority: name(priorities, t.Priority),
		GroupID: t.GroupID, AgentID: t.ResponderID, RequesterID: t.RequesterID, Tags: t.Tags,
		Updated: t.UpdatedAt, URL: s.ticketURL(t.ID),
	}
	if t.Type != nil {
		b.Type = *t.Type
	}
	return b
}

func (a *Adapter) searchTickets(ctx context.Context, s site, target string, in struct {
	Filter string `json:"filter"`
	Page   int    `json:"page"`
},
) (any, error) {
	page := strconv.Itoa(apitool.Limit(in.Page, 1, 10))
	q, err := scope(target, in.Filter)
	if err != nil {
		return nil, err
	}
	var ts []ticket
	total := 0
	if q == "" {
		// The filter API needs a filter; every group's latest tickets come
		// from the list instead.
		err = a.call(ctx, s, http.MethodGet, "/tickets?"+query(
			"order_by", "updated_at", "order_type", "desc", "per_page", "30", "page", page), nil, &ts)
	} else {
		var res struct {
			Results []ticket `json:"results"`
			Total   int      `json:"total"`
		}
		err = a.call(ctx, s, http.MethodGet, "/search/tickets?"+query("query", `"`+q+`"`, "page", page), nil, &res)
		ts, total = res.Results, res.Total
	}
	if err != nil {
		return nil, err
	}
	out := []Brief{}
	for _, t := range ts {
		if inGroup(target, t.GroupID) {
			out = append(out, t.brief(s))
		}
	}
	res := map[string]any{"tickets": out}
	if total > 0 {
		res["total"] = total
	}
	return res, nil
}

// ticket reads a ticket and refuses one outside the binding's group.
func (a *Adapter) ticket(ctx context.Context, s site, target string, n json.Number, include string) (ticket, error) {
	id, err := idOf("ticket", n)
	if err != nil {
		return ticket{}, err
	}
	path := "/tickets/" + id
	if include != "" {
		path += "?include=" + include
	}
	var t ticket
	err = a.call(ctx, s, http.MethodGet, path, nil, &t)
	if web.Status(err) == http.StatusNotFound {
		return ticket{}, fmt.Errorf("there is no ticket %s", id)
	}
	if err != nil {
		return ticket{}, err
	}
	if !inGroup(target, t.GroupID) {
		return ticket{}, outside(id)
	}
	return t, nil
}

// Ticket is one ticket in full.
type Ticket struct {
	Brief
	Requester       string    `json:"requester,omitempty"`
	Created         string    `json:"created"`
	Due             string    `json:"due,omitempty"`
	Description     string    `json:"description,omitempty"`
	Messages        []Message `json:"messages,omitempty"`
	EarlierMessages int       `json:"earlier_messages,omitempty"`
}

// Message is a reply or note on a ticket.
type Message struct {
	// Kind is reply, note or private note.
	Kind     string `json:"kind"`
	From     string `json:"from,omitempty"`
	Incoming bool   `json:"incoming,omitempty"`
	Text     string `json:"text"`
	At       string `json:"at"`
}

type conversation struct {
	BodyText  string `json:"body_text"`
	Incoming  bool   `json:"incoming"`
	Private   bool   `json:"private"`
	Source    int    `json:"source"`
	FromEmail string `json:"from_email"`
	CreatedAt string `json:"created_at"`
}

func (a *Adapter) getTicket(ctx context.Context, s site, target string, in struct {
	ID json.Number `json:"id"`
},
) (any, error) {
	t, err := a.ticket(ctx, s, target, in.ID, "requester")
	if err != nil {
		return nil, err
	}
	out := Ticket{
		Brief: t.brief(s), Created: t.CreatedAt, Due: t.DueBy,
		Description: web.Cut(t.DescriptionText, maxDescription, "…"),
	}
	if t.Requester != nil {
		out.Requester = strings.TrimSpace(t.Requester.Name + " <" + t.Requester.Email + ">")
	}
	// Replies and notes come oldest first; the latest are on the last page.
	var all []conversation
	for page := 1; page <= 10; page++ {
		var cs []conversation
		path := "/tickets/" + strconv.FormatInt(t.ID, 10) + "/conversations?per_page=100&page=" + strconv.Itoa(page)
		if err := a.call(ctx, s, http.MethodGet, path, nil, &cs); err != nil {
			return nil, err
		}
		all = append(all, cs...)
		if len(cs) < 100 {
			break
		}
	}
	if len(all) > maxMessages {
		out.EarlierMessages = len(all) - maxMessages
		all = all[len(all)-maxMessages:]
	}
	for _, c := range all {
		kind := "reply"
		// Source 2 is a note.
		if c.Source == 2 {
			kind = "note"
			if c.Private {
				kind = "private note"
			}
		}
		out.Messages = append(out.Messages, Message{
			Kind: kind, From: c.FromEmail, Incoming: c.Incoming, Text: web.Cut(c.BodyText, maxMessage, "…"), At: c.CreatedAt,
		})
	}
	return out, nil
}

// Contact is a customer.
type Contact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email,omitempty"`
	Company *int64 `json:"company_id,omitempty"`
}

func (a *Adapter) findContacts(ctx context.Context, s site, _ string, in struct {
	Query string `json:"query"`
},
) (any, error) {
	if err := apitool.Need("query", in.Query); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(in.Query)
	out := []Contact{}
	if strings.Contains(q, "@") {
		if err := a.call(ctx, s, http.MethodGet, "/contacts?"+query("email", q), nil, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	if err := a.call(ctx, s, http.MethodGet, "/contacts/autocomplete?"+query("term", q), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Group is a group of agents.
type Group struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (a *Adapter) listGroups(ctx context.Context, s site, target string, _ struct{}) (any, error) {
	var gs []group
	if target != connectors.AnyTarget {
		g, err := a.group(ctx, s, target)
		if err != nil {
			return nil, err
		}
		gs = []group{g}
	} else {
		var err error
		if gs, err = a.groups(ctx, s); err != nil {
			return nil, err
		}
	}
	out := []Group{}
	for _, g := range gs {
		out = append(out, Group{ID: g.ID, Name: g.Name})
	}
	return out, nil
}

// Agent is a Freshdesk agent, whom tickets are assigned to.
type Agent struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (a *Adapter) listAgents(ctx context.Context, s site, target string, in struct {
	Email string `json:"email"`
},
) (any, error) {
	var members []int64
	if target != connectors.AnyTarget {
		g, err := a.group(ctx, s, target)
		if err != nil {
			return nil, err
		}
		members = g.AgentIDs
	}
	out := []Agent{}
	for page := 1; page <= 10; page++ {
		var as []struct {
			ID      int64 `json:"id"`
			Contact struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"contact"`
		}
		path := "/agents?" + query("per_page", "100", "page", strconv.Itoa(page), "email", strings.TrimSpace(in.Email))
		if err := a.call(ctx, s, http.MethodGet, path, nil, &as); err != nil {
			return nil, err
		}
		for _, x := range as {
			if members == nil || slices.Contains(members, x.ID) {
				out = append(out, Agent{ID: x.ID, Name: x.Contact.Name, Email: x.Contact.Email})
			}
		}
		if len(as) < 100 {
			break
		}
	}
	return out, nil
}

func tagsOf(in []string) []string {
	out := []string{}
	for _, t := range in {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (a *Adapter) createTicket(ctx context.Context, s site, target string, in struct {
	Email       string      `json:"email"`
	ContactID   json.Number `json:"contact_id"`
	Subject     string      `json:"subject"`
	Description string      `json:"description"`
	Priority    string      `json:"priority"`
	GroupID     json.Number `json:"group_id"`
	Tags        []string    `json:"tags"`
},
) (any, error) {
	if err := apitool.Need("subject", in.Subject); err != nil {
		return nil, err
	}
	if err := apitool.Need("description", in.Description); err != nil {
		return nil, err
	}
	body := map[string]any{
		"subject": strings.TrimSpace(in.Subject), "description": htmlOf(in.Description),
		"status": 2, "priority": 1, "tags": tagsOf(in.Tags),
	}
	switch {
	case in.ContactID != "":
		id, err := idOf("contact", in.ContactID)
		if err != nil {
			return nil, err
		}
		body["requester_id"], _ = strconv.ParseInt(id, 10, 64)
	case strings.TrimSpace(in.Email) != "":
		body["email"] = strings.TrimSpace(in.Email)
	default:
		return nil, errors.New("give the customer's email or contact_id")
	}
	if in.Priority != "" {
		p := number(priorities, in.Priority)
		if p == 0 {
			return nil, fmt.Errorf("%q is not a priority; use low, medium, high or urgent", web.Short(in.Priority, 40))
		}
		body["priority"] = p
	}
	group := target
	if in.GroupID != "" {
		g, err := idOf("group", in.GroupID)
		if err != nil {
			return nil, err
		}
		if target != connectors.AnyTarget && g != target {
			return nil, errors.New("this connector may use only its own group; leave group_id out")
		}
		group = g
	}
	if group != connectors.AnyTarget {
		body["group_id"], _ = strconv.ParseInt(group, 10, 64)
	}
	var t ticket
	if err := a.call(ctx, s, http.MethodPost, "/tickets", body, &t); err != nil {
		return nil, err
	}
	return t.brief(s), nil
}

func (a *Adapter) updateTicket(ctx context.Context, s site, target string, in struct {
	ID       json.Number  `json:"id"`
	Status   string       `json:"status"`
	Priority string       `json:"priority"`
	AgentID  *json.Number `json:"agent_id"`
	Tags     *[]string    `json:"tags"`
	Subject  *string      `json:"subject"`
},
) (any, error) {
	t, err := a.ticket(ctx, s, target, in.ID, "")
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if in.Status != "" {
		n := number(statuses, in.Status)
		if n == 0 {
			return nil, fmt.Errorf("%q is not a status; use open, pending, resolved or closed", web.Short(in.Status, 40))
		}
		body["status"] = n
	}
	if in.Priority != "" {
		n := number(priorities, in.Priority)
		if n == 0 {
			return nil, fmt.Errorf("%q is not a priority; use low, medium, high or urgent", web.Short(in.Priority, 40))
		}
		body["priority"] = n
	}
	if in.AgentID != nil {
		id, err := idOf("agent", *in.AgentID)
		if err != nil {
			return nil, err
		}
		n, _ := strconv.ParseInt(id, 10, 64)
		if n == 0 {
			body["responder_id"] = nil
		} else {
			body["responder_id"] = n
		}
	}
	if in.Tags != nil {
		body["tags"] = tagsOf(*in.Tags)
	}
	if in.Subject != nil {
		if err := apitool.Need("subject", *in.Subject); err != nil {
			return nil, err
		}
		body["subject"] = strings.TrimSpace(*in.Subject)
	}
	if len(body) == 0 {
		return nil, errors.New("give something to change")
	}
	var out ticket
	if err := a.call(ctx, s, http.MethodPut, "/tickets/"+strconv.FormatInt(t.ID, 10), body, &out); err != nil {
		return nil, err
	}
	return out.brief(s), nil
}

type messageIn struct {
	ID     json.Number `json:"id"`
	Body   string      `json:"body"`
	Public bool        `json:"public"`
}

func (a *Adapter) reply(ctx context.Context, s site, target string, in messageIn) (any, error) {
	return a.post(ctx, s, target, in, "reply", map[string]any{"body": htmlOf(in.Body)})
}

func (a *Adapter) addNote(ctx context.Context, s site, target string, in messageIn) (any, error) {
	return a.post(ctx, s, target, in, "notes", map[string]any{"body": htmlOf(in.Body), "private": !in.Public})
}

// post adds a reply or note to a ticket in the binding's group.
func (a *Adapter) post(ctx context.Context, s site, target string, in messageIn, what string, body map[string]any) (any, error) {
	if err := apitool.Need("body", in.Body); err != nil {
		return nil, err
	}
	t, err := a.ticket(ctx, s, target, in.ID, "")
	if err != nil {
		return nil, err
	}
	if err := a.call(ctx, s, http.MethodPost, "/tickets/"+strconv.FormatInt(t.ID, 10)+"/"+what, body, nil); err != nil {
		return nil, err
	}
	return map[string]any{"ticket": t.ID, "url": s.ticketURL(t.ID)}, nil
}
