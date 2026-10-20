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
	"unicode/utf8"

	"github.com/bespinian/keera-agents/internal/channels"
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
// is bound to, so the model knows the ids to use.
func (d *Dispatcher) Tools(ctx context.Context, a store.Agent) ([]protocol.ToolSpec, error) {
	bindings, err := d.st.AgentBindings(ctx, a.ID)
	if err != nil {
		return nil, err
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
		{
			Name:        "board_list_cards",
			Description: "List cards on the organisation's boards, newest change first.",
			Parameters: schema(`{"type":"object","properties":{
				"status":{"type":"string","enum":["backlog","todo","in_progress","review","done"]},
				"mine":{"type":"boolean","description":"Only cards assigned to you."}}}`),
		},
		{
			Name: "board_create_card",
			Description: "Create a card. Assign it to an agent by name to have that agent work on it " +
				"once it is in todo.",
			Parameters: schema(`{"type":"object","properties":{
				"title":{"type":"string"},"body":{"type":"string"},
				"status":{"type":"string","enum":["backlog","todo","in_progress","review","done"]},
				"agent":{"type":"string","description":"An agent's name, or empty."},
				"labels":{"type":"array","items":{"type":"string"}}},
				"required":["title"]}`),
		},
		{
			Name:        "board_update_card",
			Description: "Change a card's title, description, status or agent.",
			Parameters: schema(`{"type":"object","properties":{
				"card_id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},
				"status":{"type":"string","enum":["backlog","todo","in_progress","review","done"]},
				"agent":{"type":"string","description":"An agent's name; \"none\" unassigns."}},
				"required":["card_id"]}`),
		},
		{
			Name:        "board_comment",
			Description: "Add a comment to a card.",
			Parameters: schema(`{"type":"object","properties":{
				"card_id":{"type":"string"},"body":{"type":"string"}},"required":["card_id","body"]}`),
		},
	}
	if folders := bindingList(bindings, "Your folders", isFolder); folders != "" {
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
	tools = append(tools, codeTools(bindings)...)
	tools = append(tools, apiTools(bindings)...)
	if a.Kind == store.KindWorkspace {
		// A workspace has no chats: there is nowhere to send a message.
		tools = slices.DeleteFunc(tools, func(t protocol.ToolSpec) bool { return t.Name == "send_message" })
	}
	return tools, nil
}

// bindingList lists the bindings keep picks, one per line, for a tool's
// description. A repository also says where to clone it from.
func bindingList(bindings []store.Binding, header string, keep func(store.Binding) bool) string {
	var lines []string
	for _, b := range bindings {
		if !keep(b) {
			continue
		}
		line := fmt.Sprintf("- %s: %s (%s)", b.ID, cmp.Or(b.Label, b.Target), b.Kind)
		if b.Mode == store.ModeRepo {
			line += ", clone with: git clone " + cloneURL(b)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return " " + header + ":\n" + strings.Join(lines, "\n")
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
		&in.Channel, &in.Text, &in.Status, &in.CardID, in.Title, in.Body, in.Agent, &in.Folder,
		&in.Subfolder, &in.FileID, &in.Name, &in.MimeType, &in.Repo, &in.Kind, &in.State, &in.Branch, &in.Base,
	} {
		if s != nil {
			*s = strings.TrimSpace(*s)
		}
	}
	return in, nil
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
	res.Content = cutResult(res.Content)
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
	if slices.Contains([]string{"board", "files", "repo"}, group) {
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
	case "board_create_card":
		out, err = d.toolCreateCard(ctx, a, t, in)
	case "board_update_card":
		out, err = d.toolUpdateCard(ctx, a, t, in)
	case "board_comment":
		out, err = d.toolComment(ctx, a, t, in)
	case "files_list":
		out, err = d.toolFilesList(ctx, a, t, in)
	case "files_read":
		out, err = d.toolFilesRead(ctx, a, t, in)
	case "files_write":
		out, err = d.toolFilesWrite(ctx, a, t, in)
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

// cutResult keeps a tool's result within protocol.MaxToolResult and says
// when it was cut, so the model asks for less instead of trusting a partial
// answer. The note fits within the bound too.
func cutResult(s string) string {
	if len(s) <= protocol.MaxToolResult {
		return s
	}
	note := fmt.Sprintf("\n[Cut at %d KiB of %d KiB. Ask for less, such as with a narrower query or a smaller limit.]",
		protocol.MaxToolResult>>10, len(s)>>10)
	// Not in the middle of a character.
	n := protocol.MaxToolResult - len(note)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + note
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
	return d.send(ctx, b.ConnectionID, channels.Target{Conversation: b.Target}, in.Text)
}

type cardView struct {
	ID     string   `json:"id"`
	Board  string   `json:"board_id"`
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"`
	Status string   `json:"status"`
	Agent  string   `json:"agent,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

func viewCard(c store.Card) cardView {
	body := c.Body
	if len(body) > 500 {
		body = strings.ToValidUTF8(body[:500], "") + "…"
	}
	return cardView{
		ID: c.ID, Board: c.BoardID, Title: c.Title, Body: body, Status: c.Status,
		Agent: c.AgentName, Labels: c.Labels,
	}
}

func (d *Dispatcher) toolListCards(ctx context.Context, a store.Agent, in toolArgs) (any, error) {
	if in.Status != "" && !slices.Contains(store.CardStatuses, in.Status) {
		return nil, fmt.Errorf("unknown status %q", in.Status)
	}
	agentID := ""
	if in.Mine {
		agentID = a.ID
	}
	cards, err := d.st.OrgCards(ctx, a.OrgID, in.Status, agentID, 50)
	if err != nil {
		return nil, err
	}
	out := make([]cardView, 0, len(cards))
	for _, c := range cards {
		out = append(out, viewCard(c))
	}
	return out, nil
}

// agentRef resolves an agent's name in the organisation; "" and "none" are
// nobody.
func (d *Dispatcher) agentRef(ctx context.Context, orgID, name string) (*string, error) {
	if name == "" || strings.EqualFold(name, "none") {
		return nil, nil
	}
	other, err := d.st.AgentByName(ctx, orgID, name)
	if err != nil {
		return nil, fmt.Errorf("there is no agent called %q", name)
	}
	return &other.ID, nil
}

// mayGiveFor applies the access rule of the agent a card gives work to, for
// the person the task acts for: an agent may not pass on work that person
// could not give.
func (d *Dispatcher) mayGiveFor(ctx context.Context, t store.Task, agentID string) error {
	other, err := d.st.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	var u gateway.User
	if t.RequestedByID != "" {
		if u, err = d.gw.UserByID(ctx, t.OrgID, t.RequestedByID); err != nil {
			return fmt.Errorf("could not check who this task is for: %w", err)
		}
	}
	if ok, why := May(other, u); !ok {
		return errors.New(why)
	}
	return nil
}

// EnsureBoard returns the organisation's first board, creating it.
func (d *Dispatcher) EnsureBoard(ctx context.Context, orgID, by string) (store.Board, error) {
	boards, err := d.st.Boards(ctx, orgID)
	if err != nil {
		return store.Board{}, err
	}
	if len(boards) > 0 {
		return boards[0], nil
	}
	return d.st.CreateBoard(ctx, store.Board{ID: id.New("brd"), OrgID: orgID, Name: "Tasks", CreatedBy: by})
}

func (d *Dispatcher) toolCreateCard(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	title := value(in.Title)
	if title == "" {
		return nil, errors.New("a card needs a title")
	}
	status := cmp.Or(in.Status, store.CardBacklog)
	if !slices.Contains(store.CardStatuses, status) {
		return nil, fmt.Errorf("unknown status %q", status)
	}
	assignee, err := d.agentRef(ctx, a.OrgID, value(in.Agent))
	if err != nil {
		return nil, err
	}
	if assignee != nil {
		if err := d.mayGiveFor(ctx, t, *assignee); err != nil {
			return nil, err
		}
	}
	board, err := d.EnsureBoard(ctx, a.OrgID, a.Name)
	if err != nil {
		return nil, err
	}
	c, err := d.st.CreateCard(ctx, store.Card{
		ID: id.New("card"), BoardID: board.ID, Title: title, Body: value(in.Body), Status: status,
		AgentID: assignee, Labels: in.Labels, CreatedBy: a.Name,
		RequestedBy: cmp.Or(t.RequestedBy, a.Name), RequestedByID: t.RequestedByID,
	})
	if err != nil {
		return nil, err
	}
	return viewCard(c), nil
}

func (d *Dispatcher) orgCard(ctx context.Context, a store.Agent, cardID string) (store.Card, error) {
	c, err := d.st.Card(ctx, cardID)
	if err != nil || c.OrgID != a.OrgID {
		return store.Card{}, fmt.Errorf("there is no card %s", cardID)
	}
	return c, nil
}

func (d *Dispatcher) toolUpdateCard(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	c, err := d.orgCard(ctx, a, in.CardID)
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
	if s := in.Status; s != "" && !slices.Contains(store.CardStatuses, s) {
		return nil, fmt.Errorf("unknown status %q", s)
	}
	if in.Agent != nil {
		if c.AgentID, err = d.agentRef(ctx, a.OrgID, *in.Agent); err != nil {
			return nil, err
		}
	}
	// Checked before anything is written, as the status moves first.
	after := c
	after.Status = cmp.Or(in.Status, c.Status)
	if GivesWork(before, after) {
		if err := d.mayGiveFor(ctx, t, *after.AgentID); err != nil {
			return nil, err
		}
		// The new task runs for the person this one is for, not the card's
		// creator.
		c.RequestedBy, c.RequestedByID = cmp.Or(t.RequestedBy, a.Name), t.RequestedByID
	}
	if after.Status != c.Status {
		if err := d.st.MoveCard(ctx, c.ID, after.Status, nil); err != nil {
			return nil, err
		}
		moved, _ := d.st.Card(ctx, c.ID)
		c.Status, c.Position = moved.Status, moved.Position
	}
	out, err := d.st.UpdateCard(ctx, c)
	if err != nil {
		return nil, err
	}
	return viewCard(out), nil
}

func (d *Dispatcher) toolComment(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	body := value(in.Body)
	if body == "" {
		return nil, errors.New("the comment is empty")
	}
	c, err := d.orgCard(ctx, a, in.CardID)
	if err != nil {
		return nil, err
	}
	if CommentGivesWork(c) {
		if err := d.mayGiveFor(ctx, t, *c.AgentID); err != nil {
			return nil, err
		}
	}
	agentID := a.ID
	if _, err := d.st.AddComment(ctx, store.Comment{
		ID: id.New("cmt"), CardID: c.ID, Author: a.Name, AgentID: &agentID, Body: body,
	}); err != nil {
		return nil, err
	}
	return "commented", nil
}

// ------------------------------------------------------------ files

// grants remembers the file and folder ids a task has seen in its folders,
// so it can open those and nothing else the connection could reach. It is
// per replica; a task whose calls land on another replica lists again.
type grants struct {
	mu  sync.Mutex
	ids map[string]time.Time // task|binding|id -> when granted
}

func newGrants() *grants { return &grants{ids: map[string]time.Time{}} }

func (g *grants) add(taskID, bindingID, id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.ids[taskID+"|"+bindingID+"|"+id] = now
	// Tasks end within hours; forget older grants.
	if len(g.ids) > 10000 {
		for k, at := range g.ids {
			if now.Sub(at) > 24*time.Hour {
				delete(g.ids, k)
			}
		}
	}
}

func (g *grants) has(taskID, bindingID, id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.ids[taskID+"|"+bindingID+"|"+id]
	return ok
}

// folder returns the agent's file binding, its adapter and its connection,
// for task t.
func (d *Dispatcher) folder(ctx context.Context, a store.Agent, t store.Task, bindingID string) (store.Binding, channels.Files, channels.Connection, error) {
	return bound(ctx, d, a, t, bindingID, "folders", channels.FilesFor, store.ModeWatch, store.ModeFiles)
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
	b, adapter, conn, err := d.folder(ctx, a, t, in.Folder)
	if err != nil {
		return nil, err
	}
	// A watch task may read the file it is about.
	watched := t.Origin.BindingID == b.ID && t.Origin.FileID == in.FileID
	if !watched && !d.files.has(t.ID, b.ID, in.FileID) {
		return nil, fmt.Errorf("list the folder first; %s is not a file you have seen in it", in.FileID)
	}
	f, content, err := adapter.Read(ctx, conn, in.FileID)
	if err != nil {
		return nil, err
	}
	return fmt.Sprintf("%s (%s, %d bytes)\n\n%s", f.Name, f.MimeType, len(content), content), nil
}

func (d *Dispatcher) toolFilesWrite(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	if in.Name == "" || strings.ContainsAny(in.Name, "/\\") {
		return nil, errors.New("give the file a name without slashes")
	}
	if len(in.Content) > channels.MaxFileBytes {
		return nil, fmt.Errorf("the file is larger than %d MiB", channels.MaxFileBytes>>20)
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
