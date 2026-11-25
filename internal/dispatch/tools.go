package dispatch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// Platform tools run here, not in the container, because they need
// credentials the container must never hold. Each checks that the agent may
// do it: it posts only where it is bound, and reads files only in folders it
// is bound to, and works only in repositories it is bound to.

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// WorkspaceTask is what a workspace's tool calls run as. A workspace has no
// task, so it acts for its owner, and remembers what it has seen of its
// folders under its own id.
func WorkspaceTask(a store.Agent) store.Task {
	return store.Task{
		ID: "workspace:" + a.ID, OrgID: a.OrgID, AgentID: a.ID, State: store.TaskRunning,
		RequestedBy: a.OwnerEmail, RequestedByID: a.OwnerID,
	}
}

// Tools lists the platform tools an agent may call, described with what it
// is bound to, so the model knows the ids to use. It also lists what the
// agent is connected to, for its prompt.
func (d *Dispatcher) Tools(ctx context.Context, a store.Agent) ([]protocol.ToolSpec, []protocol.Connected, error) {
	bindings, err := d.st.AgentBindings(ctx, a.ID)
	if err != nil {
		return nil, nil, err
	}
	tools := []protocol.ToolSpec{
		{
			Name: "send_message",
			Description: "Post a message now, before your final answer: a progress update in the " +
				"conversation the task came from (leave channel empty), or a message in one of your " +
				"channels." + bindingList(bindings, "Your channels", isChannel),
			Parameters: schema(`{"type":"object","properties":{
				"text":{"type":"string","description":"The message, in Markdown."},
				"channel":{"type":"string","description":"A channel id from the list, or empty for the task's own conversation."}},
				"required":["text"]}`),
		},
	}
	squads, err := d.st.AgentSquads(ctx, a.ID)
	if err != nil {
		return nil, nil, err
	}
	tools = append(tools, squadTools(squads)...)
	folders := bindingList(bindings, "Your folders", isFolder)
	if folders != "" {
		tools = append(tools,
			protocol.ToolSpec{
				Name:        "files_list",
				Description: "List a folder you have access to, or a folder inside it." + folders,
				Parameters: schema(`{"type":"object","properties":{
					"folder":{"type":"string","description":"A folder id from the list."},
					"subfolder_id":{"type":"string","description":"The id of a folder found by an earlier files_list, or empty."}},
					"required":["folder"]}`),
			},
			protocol.ToolSpec{
				Name:        "files_read",
				Description: "Read a file found with files_list, or named in your task, as text where possible.",
				Parameters: schema(`{"type":"object","properties":{
					"folder":{"type":"string"},"file_id":{"type":"string"}},"required":["folder","file_id"]}`),
			},
			protocol.ToolSpec{
				Name:        "files_write",
				Description: "Create or replace a file in a folder you have access to.",
				Parameters: schema(`{"type":"object","properties":{
					"folder":{"type":"string"},"subfolder_id":{"type":"string"},
					"name":{"type":"string"},"content":{"type":"string"},
					"mime_type":{"type":"string","description":"Defaults to text/plain."}},
					"required":["folder","name","content"]}`),
			},
		)
	}
	tools = append(tools, modelTools(a, folders)...)
	tools = append(tools, codeTools(bindings)...)
	tools = append(tools, apiTools(bindings)...)
	if a.Kind == store.KindWorkspace {
		// A workspace has no chats: there is nowhere to send a message.
		tools = slices.DeleteFunc(tools, func(t protocol.ToolSpec) bool { return t.Name == "send_message" })
	}
	conn := connected(bindings)
	for _, q := range squads {
		name := q.Name
		if q.LeadID != nil && *q.LeadID == a.ID {
			name += ", which you lead"
		}
		conn = append(conn, protocol.Connected{ID: q.ID, What: protocol.ConnectedSquad, Service: "board", Name: name})
	}
	return tools, conn, nil
}

// connected lists the chats, folders, repositories and services an agent
// is bound to, as its prompt shows them.
func connected(bindings []store.Binding) []protocol.Connected {
	var out []protocol.Connected
	for _, b := range bindings {
		c := protocol.Connected{ID: b.ID, Service: serviceName(b.Kind), Name: named(b)}
		switch {
		case isChannel(b):
			c.What = protocol.ConnectedChannel
		case isFolder(b):
			c.What = protocol.ConnectedFolder
		case b.Mode == store.ModeRepo:
			c.What, c.Clone = protocol.ConnectedRepository, cloneCommand(b)
		case isAPI(b):
			if _, ok := connectors.APIFor(connectors.Kind(b.Kind)); !ok {
				continue
			}
			c.What, c.ReadOnly = protocol.ConnectedService, b.Mode == store.ModeRead
		default:
			continue
		}
		out = append(out, c)
	}
	return out
}

// named is how the model sees a binding: its label, if any, and always what
// it stands for, so a labelled repository still shows its owner/name.
func named(b store.Binding) string {
	target := b.Target
	if target == connectors.AnyTarget {
		target = "everything the connector reaches"
	}
	if b.Label != "" && b.Label != b.Target {
		return b.Label + ", " + target
	}
	return target
}

// serviceName is a kind's name as people know it, such as GitHub.
func serviceName(kind string) string {
	if d, ok := connectors.Describe(connectors.Kind(kind)); ok && d.Label != "" {
		return d.Label
	}
	return kind
}

// bindingList lists the bindings keep picks, one per line, for a tool's
// description. A repository also says where to clone it from.
func bindingList(bindings []store.Binding, header string, keep func(store.Binding) bool) string {
	var lines []string
	for _, b := range bindings {
		if !keep(b) {
			continue
		}
		line := fmt.Sprintf("- %s: %s (%s)", b.ID, named(b), b.Kind)
		if b.Mode == store.ModeRepo {
			line += ", clone with: " + cloneCommand(b)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return " " + header + ":\n" + strings.Join(lines, "\n")
}

// squadTools are the tools of the squads' boards. Only a member of a squad
// has a board to work on.
func squadTools(squads []store.Squad) []protocol.ToolSpec {
	if len(squads) == 0 {
		return nil
	}
	mine := " Your squads: " + squadNames(squads) + "."
	squad := `"squad":{"type":"string","description":"A squad's name. Leave it empty for the squad of the card you work on, or your only squad."}`
	enum, _ := json.Marshal(store.CardStatuses)
	statuses := `{"type":"string","enum":` + string(enum) + `}`
	return []protocol.ToolSpec{
		{
			Name: "squad_list",
			Description: "List your squads: what each is for, its lead, and the members you may hand work to, " +
				"with what each does. Hand one work with board_create_card.",
			Parameters: schema(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "board_list_cards",
			Description: "List cards on your squads' boards, newest change first." + mine,
			Parameters: schema(`{"type":"object","properties":{` + squad + `,
				"status":` + statuses + `,
				"mine":{"type":"boolean","description":"Only cards assigned to you."}}}`),
		},
		{
			Name:        "board_read_card",
			Description: "Read a card: its whole description and every comment, such as the results of cards you handed out.",
			Parameters:  schema(`{"type":"object","properties":{"card_id":{"type":"string"}},"required":["card_id"]}`),
		},
		{
			Name: "board_create_card",
			Description: "Create a card on a squad's board. Assign it to a member by name to have them work on it " +
				"once it is in todo; in todo without an agent, it goes to the squad's lead. A card you create " +
				"while working on a card is handed out from it: when you finish, your card waits until the cards " +
				"you handed out are done, then comes back to you with their results." + mine,
			Parameters: schema(`{"type":"object","properties":{` + squad + `,
				"title":{"type":"string"},"body":{"type":"string"},
				"status":` + statuses + `,
				"agent":{"type":"string","description":"A member's name, or empty."},
				"labels":{"type":"array","items":{"type":"string"}}},
				"required":["title"]}`),
		},
		{
			Name:        "board_update_card",
			Description: "Change a card's title, description, status or agent.",
			Parameters: schema(`{"type":"object","properties":{
				"card_id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},
				"status":` + statuses + `,
				"agent":{"type":"string","description":"A member's name; \"none\" unassigns."}},
				"required":["card_id"]}`),
		},
		{
			Name:        "board_comment",
			Description: "Add a comment to a card.",
			Parameters: schema(`{"type":"object","properties":{
				"card_id":{"type":"string"},"body":{"type":"string"}},"required":["card_id","body"]}`),
		},
	}
}

func isChannel(b store.Binding) bool {
	return b.Mode == store.ModeChat && b.Target != store.AnyConversation
}

func isFolder(b store.Binding) bool { return b.Mode == store.ModeWatch || b.Mode == store.ModeFiles }

// toolArgs holds the arguments of every platform tool; each reads its own.
type toolArgs struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
	Status  string `json:"status"`
	Mine    bool   `json:"mine"`
	CardID  string `json:"card_id"`
	Squad   string `json:"squad"`
	// Title, Body and Agent are nil when not given: board_update_card
	// changes only what is.
	Title     *string  `json:"title"`
	Body      *string  `json:"body"`
	Agent     *string  `json:"agent"`
	Labels    []string `json:"labels"`
	Folder    string   `json:"folder"`
	Subfolder string   `json:"subfolder_id"`
	FileID    string   `json:"file_id"`
	Name      string   `json:"name"`
	Content   string   `json:"content"`
	MimeType  string   `json:"mime_type"`
	Repo      string   `json:"repository"`
	Number    int      `json:"number"`
	Kind      string   `json:"kind"`
	State     string   `json:"state"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Draft     bool     `json:"draft"`
	Model     string   `json:"model"`
	Prompt    string   `json:"prompt"`
}

// decodeArgs reads a tool's arguments and trims every string but a file's
// content.
func decodeArgs(raw json.RawMessage) (toolArgs, error) {
	var in toolArgs
	if len(raw) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, err
	}
	for _, s := range []*string{
		&in.Channel, &in.Text, &in.Status, &in.CardID, &in.Squad, in.Title, in.Body, in.Agent, &in.Folder,
		&in.Subfolder, &in.FileID, &in.Name, &in.MimeType, &in.Repo, &in.Kind, &in.State, &in.Branch, &in.Base, &in.Model,
	} {
		if s != nil {
			*s = strings.TrimSpace(*s)
		}
	}
	return in, nil
}

// cut shortens s to n bytes, on a character boundary, and ends it with more.
func cut(s string, n int, more string) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + more
}

// value is s, or "" when it was not given.
func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CallTool runs one platform tool for a task.
func (d *Dispatcher) CallTool(ctx context.Context, a store.Agent, t store.Task, name string, args json.RawMessage) protocol.ToolResult {
	began := time.Now()
	res := d.callTool(ctx, a, t, name, args)
	res.Content = protocol.CutResult(res.Content)
	outcome := "ok"
	if res.IsError {
		outcome = "error"
	}
	d.opts.Metrics.ToolCall(toolKind(name), outcome, time.Since(began))
	return res
}

// toolKind groups a tool for /metrics: by its connector's kind, such as jira,
// or by its group, such as board. The model names the tool, so a name nobody
// offered is "unknown", and the series stay few.
func toolKind(name string) string {
	if kind, ok := apiKindOf(name); ok {
		return string(kind)
	}
	if name == "send_message" {
		return "chat"
	}
	group, _, _ := strings.Cut(name, "_")
	if slices.Contains([]string{"board", "squad", "files", "repo", "model"}, group) {
		return group
	}
	return "unknown"
}

func (d *Dispatcher) callTool(ctx context.Context, a store.Agent, t store.Task, name string, args json.RawMessage) protocol.ToolResult {
	// An API connector's tools have arguments of their own, which may not
	// fit toolArgs.
	if _, ok := apiKindOf(name); ok {
		out, err := d.toolAPI(ctx, a, t, name, args)
		if err != nil {
			return failed("%v", err)
		}
		return protocol.ToolResult{Content: asText(out)}
	}
	in, err := decodeArgs(args)
	if err != nil {
		return failed("the arguments do not fit the tool's schema: %v", err)
	}
	var out any
	switch name {
	case "send_message":
		err = d.toolSend(ctx, a, t, in)
		out = "sent"
	case "board_list_cards":
		out, err = d.toolListCards(ctx, a, in)
	case "board_read_card":
		out, err = d.toolReadCard(ctx, a, in)
	case "board_create_card":
		out, err = d.toolCreateCard(ctx, a, t, in)
	case "board_update_card":
		out, err = d.toolUpdateCard(ctx, a, t, in)
	case "board_comment":
		out, err = d.toolComment(ctx, a, t, in)
	case "squad_list":
		out, err = d.toolListSquads(ctx, a, t)
	case "files_list":
		out, err = d.toolFilesList(ctx, a, t, in)
	case "files_read":
		out, err = d.toolFilesRead(ctx, a, t, in)
	case "files_write":
		out, err = d.toolFilesWrite(ctx, a, t, in)
	case "model_transcribe":
		out, err = d.toolTranscribe(ctx, a, t, in)
	case "model_ask":
		out, err = d.toolAsk(ctx, a, t, in)
	case "repo_list_issues":
		out, err = d.toolRepoList(ctx, a, t, in)
	case "repo_read_issue":
		out, err = d.toolRepoRead(ctx, a, t, in)
	case "repo_comment":
		out, err = d.toolRepoComment(ctx, a, t, in)
	case "repo_open_change":
		out, err = d.toolRepoOpenChange(ctx, a, t, in)
	default:
		return failed("there is no tool called %s", name)
	}
	if err != nil {
		return failed("%v", err)
	}
	return protocol.ToolResult{Content: asText(out)}
}

// asText is a tool's result as the model reads it: a string as it is,
// anything else as JSON.
func asText(out any) string {
	if s, ok := out.(string); ok {
		return s
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func failed(format string, args ...any) protocol.ToolResult {
	return protocol.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

func (d *Dispatcher) toolSend(ctx context.Context, a store.Agent, t store.Task, in toolArgs) error {
	if in.Text == "" {
		return errors.New("the message is empty")
	}
	if in.Channel == "" {
		if t.Source != protocol.SourceChat {
			return errors.New("this task did not come from a chat; name one of your channels")
		}
		if t.Origin.ConnectionID == "" {
			return errors.New("this task came from Keera Chat, which shows only your answer; " +
				"answer there, or name one of your channels")
		}
		return d.send(ctx, t.Origin.ConnectionID, replyTarget(t.Origin), in.Text)
	}
	b, ok := d.binding(ctx, a, in.Channel, store.ModeChat)
	if !ok || !isChannel(b) {
		return fmt.Errorf("%s is not one of your channels", in.Channel)
	}
	return d.send(ctx, b.ConnectionID, connectors.Target{Conversation: b.Target}, in.Text)
}

type cardView struct {
	ID     string   `json:"id"`
	Squad  string   `json:"squad"`
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"`
	Status string   `json:"status"`
	Agent  string   `json:"agent,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Parent string   `json:"handed_out_from,omitempty"`
}

// viewCard shows a card to an agent, its description cut unless whole.
func viewCard(c store.Card, whole bool) cardView {
	body := c.Body
	if !whole {
		body = cut(body, 500, "…")
	}
	return cardView{
		ID: c.ID, Squad: c.SquadName, Title: c.Title, Body: body, Status: c.Status,
		Agent: c.AgentName, Labels: c.Labels, Parent: value(c.ParentID),
	}
}

// squadNames lists the squads' names, for the agent to choose from.
func squadNames(squads []store.Squad) string {
	names := make([]string, len(squads))
	for i, q := range squads {
		names[i] = q.Name
	}
	return strings.Join(names, ", ")
}

func (d *Dispatcher) toolListCards(ctx context.Context, a store.Agent, in toolArgs) (any, error) {
	if in.Status != "" && !slices.Contains(store.CardStatuses, in.Status) {
		return nil, fmt.Errorf("unknown status %q", in.Status)
	}
	squads, err := d.st.AgentSquads(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	f := store.CardFilter{Status: in.Status, Limit: 50}
	// Without a name, every squad's cards: listing is how an agent finds
	// work in any of them.
	if in.Squad != "" {
		q, err := squadOf(squads, nil, in.Squad)
		if err != nil {
			return nil, err
		}
		f.Squad = q.ID
	}
	if in.Mine {
		f.Agent = a.ID
	}
	cards, err := d.st.MemberCards(ctx, a.ID, f)
	if err != nil {
		return nil, err
	}
	out := make([]cardView, 0, len(cards))
	for _, c := range cards {
		out = append(out, viewCard(c, false))
	}
	return out, nil
}

type commentView struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

func (d *Dispatcher) toolReadCard(ctx context.Context, a store.Agent, in toolArgs) (any, error) {
	c, _, err := d.memberCard(ctx, a, in.CardID)
	if err != nil {
		return nil, err
	}
	comments, err := d.st.Comments(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := struct {
		cardView
		Comments []commentView `json:"comments"`
	}{viewCard(c, true), []commentView{}}
	for _, cm := range comments {
		out.Comments = append(out.Comments, commentView{Author: cm.Author, Body: cm.Body})
	}
	return out, nil
}

type agentView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type squadView struct {
	Name    string      `json:"name"`
	Goal    string      `json:"goal,omitempty"`
	Lead    string      `json:"lead,omitempty"`
	Members []agentView `json:"members"`
}

// toolListSquads lists only the members a card from this task could go
// to: those the person it is for may give work, and that are on, since a
// card for one that is off would not be picked up.
func (d *Dispatcher) toolListSquads(ctx context.Context, a store.Agent, t store.Task) (any, error) {
	squads, err := d.st.AgentSquads(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	agents, err := d.st.Agents(ctx, a.OrgID)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.Agent{}
	for _, x := range agents {
		byID[x.ID] = x
	}
	u, gone, err := d.requester(ctx, t.OrgID, t.RequestedByID)
	if err != nil {
		return nil, fmt.Errorf("could not check who this task is for: %w", err)
	}
	out := []squadView{}
	for _, q := range squads {
		v := squadView{Name: q.Name, Goal: q.Goal, Members: []agentView{}}
		for _, m := range q.Members {
			if q.LeadID != nil && *q.LeadID == m.ID {
				v.Lead = m.Name
			}
			other, ok := byID[m.ID]
			if gone || !ok || other.ID == a.ID || !other.Enabled {
				continue
			}
			if may, _ := May(other, u); may {
				v.Members = append(v.Members, agentView{Name: other.Name, Description: other.Description})
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// squadOf picks the squad, of the agent's squads, that a tool acts in: the
// one named, else the squad of origin, the card the task came from, else
// the agent's only squad.
func squadOf(squads []store.Squad, origin *store.Card, name string) (store.Squad, error) {
	for _, q := range squads {
		if name != "" && (q.ID == name || strings.EqualFold(q.Name, name)) {
			return q, nil
		}
	}
	switch {
	case len(squads) == 0:
		return store.Squad{}, errors.New("you are in no squad")
	case name != "":
		return store.Squad{}, fmt.Errorf("you are not in a squad called %q; yours: %s", name, squadNames(squads))
	}
	if origin != nil {
		for _, q := range squads {
			if q.ID == origin.SquadID {
				return q, nil
			}
		}
	}
	if len(squads) == 1 {
		return squads[0], nil
	}
	return store.Squad{}, fmt.Errorf("you are in several squads; name one: %s", squadNames(squads))
}

// memberCard reads a card on the board of one of the agent's squads.
func (d *Dispatcher) memberCard(ctx context.Context, a store.Agent, cardID string) (store.Card, store.Squad, error) {
	c, err := d.st.Card(ctx, cardID)
	if err == nil && c.OrgID == a.OrgID {
		if q, err := d.st.Squad(ctx, c.SquadID); err == nil && q.Has(a.ID) {
			return c, q, nil
		}
	}
	return store.Card{}, store.Squad{}, fmt.Errorf("there is no card %s in your squads", cardID)
}

// memberRef resolves a member's name in a squad; "" and "none" are nobody.
func memberRef(q store.Squad, name string) (*string, error) {
	if name == "" || strings.EqualFold(name, "none") {
		return nil, nil
	}
	names := make([]string, len(q.Members))
	for i, m := range q.Members {
		if strings.EqualFold(m.Name, name) {
			return &m.ID, nil
		}
		names[i] = m.Name
	}
	return nil, fmt.Errorf("%s is not in the squad %s; its members: %s", name, q.Name, strings.Join(names, ", "))
}

// ToLead gives a card put in To do without an agent to its squad's lead.
func ToLead(q store.Squad, c *store.Card) {
	if c.Status == store.CardTodo && c.AgentID == nil && q.LeadID != nil {
		lead := *q.LeadID
		c.AgentID = &lead
	}
}

// mayGiveFor applies the access rule of the agent a card gives work to, for
// the person the task acts for: an agent may not pass on work that person
// could not give.
func (d *Dispatcher) mayGiveFor(ctx context.Context, t store.Task, agentID string) error {
	other, err := d.st.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	ok, why, err := d.mayGive(ctx, other, t.OrgID, t.RequestedByID)
	if err != nil {
		return fmt.Errorf("could not check who this task is for: %w", err)
	}
	if !ok {
		return errors.New(why)
	}
	return nil
}

// mayGive applies a's access rule to the person with userID, or to nobody
// when it is empty.
func (d *Dispatcher) mayGive(ctx context.Context, a store.Agent, orgID, userID string) (bool, string, error) {
	u, gone, err := d.requester(ctx, orgID, userID)
	if err != nil {
		return false, "", err
	}
	if gone {
		return false, "they are no longer in the organisation", nil
	}
	ok, why := May(a, u)
	return ok, why, nil
}

// requester looks up the person with userID, or nobody when it is empty.
// gone means they left the organisation, so they give no work any more.
func (d *Dispatcher) requester(ctx context.Context, orgID, userID string) (u gateway.User, gone bool, err error) {
	if userID == "" {
		return gateway.User{}, false, nil
	}
	u, err = d.gw.UserByID(ctx, orgID, userID)
	if gateway.IsNotFound(err) {
		return gateway.User{}, true, nil
	}
	return u, false, err
}

// originCard is the card task t came from, or nil.
func (d *Dispatcher) originCard(ctx context.Context, t store.Task) (*store.Card, error) {
	if t.Source != protocol.SourceBoard || t.Origin.CardID == "" {
		return nil, nil
	}
	c, err := d.st.Card(ctx, t.Origin.CardID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// handOut places a card an agent creates while working on parent under
// it, and holds it to the limits on handing out work.
func (d *Dispatcher) handOut(ctx context.Context, q store.Squad, parent *store.Card, c *store.Card) error {
	if q.Open >= store.MaxOpenCards {
		return fmt.Errorf("the squad %s has %d cards in todo and in progress already; wait for some to finish", q.Name, q.Open)
	}
	if parent == nil {
		return nil
	}
	if parent.Depth >= store.MaxCardDepth {
		return fmt.Errorf("this card was handed on %d times already; do the work yourself", parent.Depth)
	}
	n, err := d.st.CountChildCards(ctx, parent.ID, time.Time{})
	if err != nil {
		return err
	}
	if n >= store.MaxChildCards {
		return fmt.Errorf("you handed out %d cards from this card already", n)
	}
	c.ParentID, c.Depth = &parent.ID, parent.Depth+1
	return nil
}

func (d *Dispatcher) toolCreateCard(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	title := value(in.Title)
	if title == "" {
		return nil, errors.New("a card needs a title")
	}
	if err := checkCard(title, value(in.Body)); err != nil {
		return nil, err
	}
	status := cmp.Or(in.Status, store.CardBacklog)
	if !slices.Contains(store.CardStatuses, status) {
		return nil, fmt.Errorf("unknown status %q", status)
	}
	squads, err := d.st.AgentSquads(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	parent, err := d.originCard(ctx, t)
	if err != nil {
		return nil, err
	}
	q, err := squadOf(squads, parent, in.Squad)
	if err != nil {
		return nil, err
	}
	assignee, err := memberRef(q, value(in.Agent))
	if err != nil {
		return nil, err
	}
	c := store.Card{
		ID: id.New("card"), SquadID: q.ID, Title: title, Body: value(in.Body), Status: status,
		AgentID: assignee, Labels: store.CleanLabels(in.Labels), CreatedBy: a.Name,
		RequestedBy: cmp.Or(t.RequestedBy, a.Name), RequestedByID: t.RequestedByID,
	}
	ToLead(q, &c)
	if c.AgentID != nil {
		if err := d.mayGiveFor(ctx, t, *c.AgentID); err != nil {
			return nil, err
		}
	}
	if err := d.handOut(ctx, q, parent, &c); err != nil {
		return nil, err
	}
	out, err := d.st.CreateCard(ctx, c)
	if err != nil {
		return nil, err
	}
	return viewCard(out, false), nil
}

// checkCard holds a card to the limits the panel has.
func checkCard(title, body string) error {
	if len(title) > store.MaxCardTitle {
		return fmt.Errorf("the title is longer than %d bytes", store.MaxCardTitle)
	}
	if len(body) > store.MaxCardBody {
		return fmt.Errorf("the description is longer than %d KiB", store.MaxCardBody>>10)
	}
	return nil
}

func (d *Dispatcher) toolUpdateCard(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	c, q, err := d.memberCard(ctx, a, in.CardID)
	if err != nil {
		return nil, err
	}
	before := c
	if title := value(in.Title); title != "" {
		c.Title = title
	}
	if in.Body != nil {
		c.Body = *in.Body
	}
	// Only what is given: a card from before the limits can still move.
	if err := checkCard(value(in.Title), value(in.Body)); err != nil {
		return nil, err
	}
	if s := in.Status; s != "" && !slices.Contains(store.CardStatuses, s) {
		return nil, fmt.Errorf("unknown status %q", s)
	}
	if in.Agent != nil {
		if c.AgentID, err = memberRef(q, *in.Agent); err != nil {
			return nil, err
		}
	}
	// Checked before anything is written, as the status moves first.
	after := c
	after.Status = cmp.Or(in.Status, c.Status)
	ToLead(q, &after)
	c.AgentID = after.AgentID
	if GivesWork(before, after) {
		if err := d.mayGiveFor(ctx, t, *after.AgentID); err != nil {
			return nil, err
		}
		// The new task runs for the person this one is for, not the card's
		// creator.
		c.RequestedBy, c.RequestedByID = cmp.Or(t.RequestedBy, a.Name), t.RequestedByID
	}
	// The move goes first; c keeps the status it had, so the update below
	// leaves status and position be.
	if after.Status != c.Status {
		if err := d.st.MoveCard(ctx, c.ID, after.Status, nil); err != nil {
			return nil, err
		}
	}
	out, err := d.st.UpdateCard(ctx, before, c)
	if err != nil {
		return nil, err
	}
	return viewCard(out, false), nil
}

func (d *Dispatcher) toolComment(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	body := value(in.Body)
	if body == "" {
		return nil, errors.New("the comment is empty")
	}
	if len(body) > store.MaxComment {
		return nil, fmt.Errorf("the comment is longer than %d KiB", store.MaxComment>>10)
	}
	c, _, err := d.memberCard(ctx, a, in.CardID)
	if err != nil {
		return nil, err
	}
	agentID := a.ID
	cm := store.Comment{ID: id.New("cmt"), CardID: c.ID, Author: a.Name, AgentID: &agentID, Body: body}
	if CommentGivesWork(c) {
		if err := d.mayGiveFor(ctx, t, *c.AgentID); err != nil {
			return nil, err
		}
		// As for an edit: the card's task then runs for the person this
		// one is for.
		cm.RequestedBy, cm.RequestedByID = cmp.Or(t.RequestedBy, a.Name), t.RequestedByID
	}
	if _, err := d.st.AddComment(ctx, cm); err != nil {
		return nil, err
	}
	return "commented", nil
}

// ------------------------------------------------------------ files

// grants remembers the file and folder ids a task has seen in its folders,
// so it can open those and nothing else the connection could reach. It is
// per replica; a task whose calls land on another replica lists again.
type grants struct {
	mu     sync.Mutex
	tasks  map[string]*taskGrants
	pruned time.Time
}

// taskGrants are one task's grants, and when it was last given one.
type taskGrants struct {
	last time.Time
	ids  map[string]struct{} // binding|id
}

func newGrants() *grants { return &grants{tasks: map[string]*taskGrants{}} }

// pruneEvery is how often grants of tasks quiet for a day are forgotten.
// Tasks end within hours. Not on every add: a busy replica would scan
// every grant for every file listed.
const pruneEvery = time.Hour

func (g *grants) add(taskID, bindingID, id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if now.Sub(g.pruned) > pruneEvery {
		for k, t := range g.tasks {
			if now.Sub(t.last) > 24*time.Hour {
				delete(g.tasks, k)
			}
		}
		g.pruned = now
	}
	t := g.tasks[taskID]
	if t == nil {
		t = &taskGrants{ids: map[string]struct{}{}}
		g.tasks[taskID] = t
	}
	t.last = now
	t.ids[bindingID+"|"+id] = struct{}{}
}

func (g *grants) has(taskID, bindingID, id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.tasks[taskID]
	if t == nil {
		return false
	}
	_, ok := t.ids[bindingID+"|"+id]
	return ok
}

// folder returns the agent's file binding, its adapter and its connection,
// for task t.
func (d *Dispatcher) folder(ctx context.Context, a store.Agent, t store.Task, bindingID string) (store.Binding, connectors.Files, connectors.Connection, error) {
	return bound(ctx, d, a, t, bindingID, "folders", connectors.FilesFor, store.ModeWatch, store.ModeFiles)
}

// within resolves a subfolder the task has seen, or the bound folder.
func (d *Dispatcher) within(t store.Task, b store.Binding, sub string) (string, error) {
	if sub == "" || sub == b.Target {
		return b.Target, nil
	}
	if !d.files.has(t.ID, b.ID, sub) {
		return "", fmt.Errorf("list the folder first; %s is not one you have seen in it", sub)
	}
	return sub, nil
}

func (d *Dispatcher) toolFilesList(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	b, adapter, conn, err := d.folder(ctx, a, t, in.Folder)
	if err != nil {
		return nil, err
	}
	folder, err := d.within(t, b, in.Subfolder)
	if err != nil {
		return nil, err
	}
	files, err := adapter.List(ctx, conn, folder)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		d.files.add(t.ID, b.ID, f.ID)
	}
	return files, nil
}

func (d *Dispatcher) toolFilesRead(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	f, content, err := d.readFile(ctx, a, t, in, connectors.MaxFileBytes)
	// Checked before the error: most recordings are too large to read, and
	// their bytes would only fill the model's context anyway.
	if isAudio(f) {
		how := "you cannot read it as text"
		if a.TranscriptionModel != "" {
			how = "read it with model_transcribe"
		}
		return fmt.Sprintf("%s (%s) is audio; %s.", f.Name, f.MimeType, how), nil
	}
	if err != nil {
		return nil, err
	}
	return fmt.Sprintf("%s (%s, %d bytes)\n\n%s", f.Name, f.MimeType, len(content), content), nil
}

// readFile reads up to limit bytes of a file of a connected folder that the
// task has listed, or that its watch is about.
func (d *Dispatcher) readFile(ctx context.Context, a store.Agent, t store.Task, in toolArgs, limit int64) (connectors.File, []byte, error) {
	b, adapter, conn, err := d.folder(ctx, a, t, in.Folder)
	if err != nil {
		return connectors.File{}, nil, err
	}
	watched := t.Origin.BindingID == b.ID && t.Origin.FileID == in.FileID
	if !watched && !d.files.has(t.ID, b.ID, in.FileID) {
		return connectors.File{}, nil, fmt.Errorf("list the folder first; %s is not a file you have seen in it", in.FileID)
	}
	return adapter.Read(ctx, conn, in.FileID, limit)
}

func (d *Dispatcher) toolFilesWrite(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	if in.Name == "" || strings.ContainsAny(in.Name, "/\\") {
		return nil, errors.New("give the file a name without slashes")
	}
	if len(in.Content) > connectors.MaxFileBytes {
		return nil, fmt.Errorf("the file is larger than %d MiB", connectors.MaxFileBytes>>20)
	}
	b, adapter, conn, err := d.folder(ctx, a, t, in.Folder)
	if err != nil {
		return nil, err
	}
	folder, err := d.within(t, b, in.Subfolder)
	if err != nil {
		return nil, err
	}
	mime := cmp.Or(in.MimeType, "text/plain")
	f, err := adapter.Write(ctx, conn, folder, in.Name, mime, []byte(in.Content))
	if err != nil {
		return nil, err
	}
	if err := d.st.RecordWrite(ctx, a.ID, conn.ID, f.ID, f.Modified); err != nil {
		d.log.Warn("could not note a file an agent wrote", "agent", a.ID, "error", err)
	}
	d.files.add(t.ID, b.ID, f.ID)
	return f, nil
}
