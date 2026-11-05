package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/store"
)

func TestRoute(t *testing.T) {
	bindings := []store.Binding{
		{ID: "any", AgentName: "Scout", Target: store.AnyConversation},
		{ID: "sales", AgentName: "Closer", Target: "C-sales"},
		{ID: "ops", AgentName: "Ops Bot", Target: "C-ops"},
		{ID: "ops-short", AgentName: "Ops", Target: "C-ops"},
	}
	for _, tc := range []struct {
		conv, text, want string
	}{
		{"C-sales", "how are we doing?", "sales"}, // the exact channel beats the catch-all
		{"C-sales", "scout: summarise", "any"},    // a name picks its agent
		{"C-ops", "@ops bot restart it", "ops"},   // the longest name wins
		{"C-ops", "ops: restart it", "ops-short"},
		{"C-sales", "scouting report please", "sales"}, // a longer word is not the name
		{"D-123", "hello", "any"},                      // only the catch-all listens
	} {
		b, ok := route(bindings, channels.Inbound{Target: channels.Target{Conversation: tc.conv}, Text: tc.text})
		if !ok || b.ID != tc.want {
			t.Errorf("%s %q: got %q, want %q", tc.conv, tc.text, b.ID, tc.want)
		}
	}
	if _, ok := route(bindings[1:], channels.Inbound{Target: channels.Target{Conversation: "C-other"}}); ok {
		t.Error("routed a message nobody listens to")
	}
}

func TestMay(t *testing.T) {
	owner := gateway.User{ID: "u1", Email: "ada@example.ch"}
	other := gateway.User{ID: "u2", Email: "Bob@Example.ch"}
	for _, tc := range []struct {
		access string
		allow  []string
		who    gateway.User
		want   bool
	}{
		{store.AccessOrg, nil, other, true},
		{store.AccessOwner, nil, owner, true},
		{store.AccessOwner, nil, other, false},
		{store.AccessPeople, []string{"bob@example.ch"}, other, true},
		{store.AccessPeople, []string{"eve@example.ch"}, other, false},
		{store.AccessPeople, nil, owner, true},
		{store.AccessOrg, nil, gateway.User{ID: "u3", Disabled: true}, false},
	} {
		a := store.Agent{Name: "Scout", OwnerID: "u1", OwnerEmail: owner.Email, Access: tc.access, AllowedEmails: tc.allow}
		if ok, why := May(a, tc.who); ok != tc.want || (!ok && why == "") {
			t.Errorf("%s %v %s: may = %v (%q)", tc.access, tc.allow, tc.who.Email, ok, why)
		}
	}
}

func TestGivesWork(t *testing.T) {
	scout, closer := "agt_scout", "agt_closer"
	card := store.Card{Title: "Report", Body: "Weekly", Status: store.CardBacklog, AgentID: &scout}
	with := func(change func(*store.Card)) store.Card {
		c := card
		change(&c)
		return c
	}
	for _, tc := range []struct {
		name          string
		before, after store.Card
		want          bool
	}{
		{"a new card for an agent", store.Card{}, card, true},
		{"another agent", card, with(func(c *store.Card) { c.AgentID = &closer }), true},
		{"moved to To do", card, with(func(c *store.Card) { c.Status = store.CardTodo }), true},
		{
			"rewritten in To do",
			with(func(c *store.Card) { c.Status = store.CardTodo }),
			with(func(c *store.Card) { c.Status = store.CardTodo; c.Body = "Daily" }),
			true,
		},
		// It becomes the task once someone moves it to To do.
		{"rewritten in Backlog", card, with(func(c *store.Card) { c.Body = "Daily" }), true},
		{"relabelled in Backlog", card, with(func(c *store.Card) { c.Labels = []string{"q3"} }), false},
		{"moved to Done", card, with(func(c *store.Card) { c.Status = store.CardDone }), false},
		{"unassigned", card, with(func(c *store.Card) { c.AgentID = nil; c.Status = store.CardTodo }), false},
	} {
		if got := GivesWork(tc.before, tc.after); got != tc.want {
			t.Errorf("%s: GivesWork = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCommentGivesWork(t *testing.T) {
	scout := "agt_scout"
	for _, tc := range []struct {
		name string
		card store.Card
		want bool
	}{
		{"assigned in To do", store.Card{Status: store.CardTodo, AgentID: &scout}, true},
		{"assigned in Backlog", store.Card{Status: store.CardBacklog, AgentID: &scout}, true},
		{"assigned in progress", store.Card{Status: store.CardInProgress, AgentID: &scout}, true},
		{"unassigned in To do", store.Card{Status: store.CardTodo}, false},
	} {
		if got := CommentGivesWork(tc.card); got != tc.want {
			t.Errorf("%s: CommentGivesWork = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStripAgentName(t *testing.T) {
	for in, want := range map[string]string{
		"Scout: find the invoice": "find the invoice",
		"@scout, find it":         "find it",
		"scoutmaster says hi":     "scoutmaster says hi",
		"Scout":                   "Scout",
	} {
		if got := stripAgentName(in, "Scout"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestResultText(t *testing.T) {
	if got := resultText(store.Task{State: store.TaskSucceeded}); got != "Done." {
		t.Errorf("empty success: %q", got)
	}
	got := resultText(store.Task{State: store.TaskFailed, Error: "budget spent", Result: "half"})
	if !strings.Contains(got, "budget spent") || !strings.Contains(got, "half") {
		t.Errorf("failure: %q", got)
	}
	if got := resultText(store.Task{State: store.TaskCancelled, Error: "cancelled by ada"}); !strings.Contains(got, "cancelled by ada") {
		t.Errorf("cancelled: %q", got)
	}
}

func TestCardPrompt(t *testing.T) {
	c := store.Card{Title: "Write the report", Body: "For October."}
	got := cardPrompt(c, []store.Comment{{Author: "ada", Body: "Keep it short."}})
	// The title comes first, since the task list shows a task by its first line.
	if !strings.HasPrefix(got, "Write the report\n") {
		t.Errorf("prompt does not start with the title: %q", got)
	}
	for _, want := range []string{"For October.", "Comments so far:\n- ada: Keep it short.", "Your final answer is the result"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q: %q", want, got)
		}
	}
	if got := cardPrompt(store.Card{Title: "T"}, nil); strings.Contains(got, "Comments") {
		t.Errorf("no comments: %q", got)
	}
}

func TestLatest(t *testing.T) {
	got := latest([]channels.File{{ID: "a", Name: "old"}, {ID: "b"}, {ID: "a", Name: "new"}})
	if len(got) != 2 || got[0].ID != "a" || got[0].Name != "new" || got[1].ID != "b" {
		t.Errorf("latest = %+v", got)
	}
}

func TestGrants(t *testing.T) {
	g := newGrants()
	g.add("t1", "b1", "f1")
	if !g.has("t1", "b1", "f1") || g.has("t2", "b1", "f1") || g.has("t1", "b2", "f1") {
		t.Fatal("a grant leaked to another task or folder")
	}
}

func TestCodeTools(t *testing.T) {
	if tools := codeTools([]store.Binding{{ID: "bnd_1", Mode: store.ModeFiles}}); tools != nil {
		t.Fatalf("tools without a repository: %v", tools)
	}
	tools := codeTools([]store.Binding{{ID: "bnd_2", Mode: store.ModeRepo, Kind: "gitlab", Target: "acme/apps/web"}})
	if len(tools) != 4 {
		t.Fatalf("tools = %v", tools)
	}
	// The model learns the id and where to clone from.
	if !strings.Contains(tools[0].Description, "bnd_2: acme/apps/web (gitlab), clone with: git clone http://127.0.0.1:7417/bnd_2/web.git") {
		t.Fatalf("description = %s", tools[0].Description)
	}
	for _, tool := range tools {
		var v any
		if err := json.Unmarshal(tool.Parameters, &v); err != nil {
			t.Errorf("%s: %v", tool.Name, err)
		}
	}
}

func TestIssueState(t *testing.T) {
	if st, err := issueState(""); err != nil || st != "open" {
		t.Fatalf("no state = %q, %v", st, err)
	}
	if _, err := issueState("merged"); err == nil {
		t.Fatal("unknown state passed")
	}
}

func TestDecodeArgs(t *testing.T) {
	in, err := decodeArgs(json.RawMessage(`{"card_id":" c1 ","body":"  hi \n","agent":"","content":" x "}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.CardID != "c1" || value(in.Body) != "hi" || in.Content != " x " {
		t.Errorf("trimmed = %+v", in)
	}
	// Given but empty differs from not given: "agent":"" unassigns, and a
	// missing title is left as it was.
	if in.Agent == nil || in.Title != nil {
		t.Errorf("agent %v, title %v", in.Agent, in.Title)
	}
	if _, err := decodeArgs(json.RawMessage(`{"number":"five"}`)); err == nil {
		t.Error("a string number was accepted")
	}
	// A name of spaces must not reach a file adapter: it trims to "", which
	// files_write refuses.
	if in, _ := decodeArgs(json.RawMessage(`{"name":"   "}`)); in.Name != "" {
		t.Errorf("name = %q", in.Name)
	}
	if _, err := decodeArgs(nil); err != nil {
		t.Errorf("no arguments: %v", err)
	}
}

func TestBindingList(t *testing.T) {
	bindings := []store.Binding{
		{ID: "bnd_1", Mode: store.ModeChat, Kind: "slack", Target: "C1", Label: "#support"},
		{ID: "bnd_2", Mode: store.ModeChat, Kind: "slack", Target: store.AnyConversation},
		{ID: "bnd_3", Mode: store.ModeFiles, Kind: "drive", Target: "F1"},
	}
	if got := bindingList(bindings, "Your channels", isChannel); got != " Your channels:\n- bnd_1: #support (slack)" {
		t.Errorf("channels = %q", got)
	}
	if got := bindingList(bindings, "Your folders", isFolder); got != " Your folders:\n- bnd_3: F1 (drive)" {
		t.Errorf("folders = %q", got)
	}
	if got := bindingList(bindings[:2], "Your folders", isFolder); got != "" {
		t.Errorf("no folders = %q", got)
	}
}

func TestWaitWaitsForWorkStartedWithGo(t *testing.T) {
	var d Dispatcher
	release := make(chan struct{})
	d.Go(func() { <-release })
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := d.Wait(short); err == nil {
		t.Fatal("Wait returned while work was running")
	}
	close(release)
	if err := d.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
