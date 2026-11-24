package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// fakeFiles is a file store held in memory.
type fakeFiles struct {
	mu      sync.Mutex
	folders map[string][]connectors.File
	content map[string]string
	changes []connectors.File
	failing error
	// hangs is a folder whose changes never come.
	hangs   string
	written []string
}

const filesKind = "dispatchfiles"

func newFakeFiles() *fakeFiles {
	return &fakeFiles{folders: map[string][]connectors.File{}, content: map[string]string{}}
}

func (f *fakeFiles) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{Kind: filesKind, Label: "Test Drive"}
}

func (f *fakeFiles) Check(context.Context, connectors.Connection) error { return nil }

func (f *fakeFiles) put(folder string, file connectors.File, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.folders[folder] = append(f.folders[folder], file)
	f.content[file.ID] = content
}

func (f *fakeFiles) List(_ context.Context, _ connectors.Connection, folder string) ([]connectors.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.folders[folder]), nil
}

func (f *fakeFiles) Read(_ context.Context, _ connectors.Connection, fileID string, limit int64) (connectors.File, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, files := range f.folders {
		for _, file := range files {
			if file.ID == fileID && file.Size > limit {
				return file, nil, fmt.Errorf("%s is too large", file.Name)
			}
			if file.ID == fileID {
				return file, []byte(f.content[fileID]), nil
			}
		}
	}
	return connectors.File{}, nil, errors.New("no such file")
}

func (f *fakeFiles) Write(_ context.Context, _ connectors.Connection, folder, name, mime string, content []byte) (connectors.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, folder+"/"+name)
	file := connectors.File{
		ID: "w_" + name, Name: name, MimeType: mime, Modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	f.content[file.ID] = string(content)
	return file, nil
}

// Changes starts a cursor at c, and then lists f.changes on every call.
func (f *fakeFiles) Changes(ctx context.Context, _ connectors.Connection, folder, cursor string) ([]connectors.File, string, error) {
	if folder != "" && folder == f.hangs {
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing != nil {
		return nil, "", f.failing
	}
	if cursor == "" {
		return nil, "c", nil
	}
	return slices.Clone(f.changes), cursor + "+", nil
}

// fakeTools is an API kind with a tool that reads and one that writes.
type fakeTools struct {
	mu    sync.Mutex
	calls []string
}

const apiKind = "dispatchapi"

func (f *fakeTools) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{Kind: apiKind, Label: "Test API", TargetLabel: "Project"}
}

func (f *fakeTools) Check(context.Context, connectors.Connection) error { return nil }

func (f *fakeTools) Tools() []connectors.Tool {
	return []connectors.Tool{{Name: "find"}, {Name: "change", Write: true}}
}

func (f *fakeTools) Target(_ context.Context, _ connectors.Connection, t string) (string, error) {
	return t, nil
}

func (f *fakeTools) Call(_ context.Context, _ connectors.Connection, target, tool string, args json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, target+" "+tool+" "+string(args))
	return map[string]string{"tool": tool}, nil
}

func (f *fakeTools) made() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeCode is a code host whose repositories all have main as their
// default branch.
type fakeCode struct {
	mu      sync.Mutex
	changes []connectors.Change
}

const codeKind = "dispatchcode"

func (f *fakeCode) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{Kind: codeKind, Label: "Test Code"}
}

func (f *fakeCode) Check(context.Context, connectors.Connection) error { return nil }

func (f *fakeCode) Repo(_ context.Context, _ connectors.Connection, path string) (connectors.Repo, error) {
	return connectors.Repo{Path: path, DefaultBranch: "main"}, nil
}

func (f *fakeCode) Remote(_ context.Context, _ connectors.Connection, path string) (connectors.Remote, error) {
	return connectors.Remote{URL: "https://code.example.ch/" + path + ".git"}, nil
}

func (f *fakeCode) Issues(context.Context, connectors.Connection, string, string, bool) ([]connectors.Issue, error) {
	return []connectors.Issue{{Number: 1, Title: "Bug"}}, nil
}

func (f *fakeCode) Issue(_ context.Context, _ connectors.Connection, _ string, n int, _ bool) (connectors.Issue, error) {
	return connectors.Issue{Number: n, Title: "Bug"}, nil
}

func (f *fakeCode) Comment(context.Context, connectors.Connection, string, int, bool, string) error {
	return nil
}

func (f *fakeCode) OpenChange(_ context.Context, _ connectors.Connection, _ string, c connectors.Change) (connectors.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, c)
	return connectors.Issue{Number: 7, Change: true, Branch: c.Branch}, nil
}

// call runs a platform tool as a for task, and fails the test when the
// result is not what want says: "ok", or part of the error.
func (e *testEnv) call(t *testing.T, a store.Agent, task store.Task, name, args, want string) string {
	t.Helper()
	res := e.d.CallTool(e.ctx, a, task, name, json.RawMessage(args))
	switch {
	case want == "ok" && res.IsError:
		t.Errorf("%s %s: %s", name, args, res.Content)
	case want != "ok" && !res.IsError:
		t.Errorf("%s %s: allowed (%s), want %q", name, args, res.Content, want)
	case want != "ok" && !strings.Contains(res.Content, want):
		t.Errorf("%s %s: %q, want %q", name, args, res.Content, want)
	}
	return res.Content
}

// taskFor is a task of a's, asked for by a member of org_a.
func taskFor(a store.Agent, userID, email string) store.Task {
	return store.Task{
		ID: id.New("task"), OrgID: a.OrgID, AgentID: a.ID, Source: protocol.SourceManual,
		State: store.TaskRunning, RequestedBy: email, RequestedByID: userID,
	}
}

func TestSendMessageGoesOnlyToTheAgentsChannels(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	other := e.agent(t, "org_a", "Other", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	sales := e.bind(t, scout, c, "sales", store.ModeChat)
	every := e.bind(t, scout, c, store.AnyConversation, store.ModeChat)
	otherSales := e.bind(t, other, c, "board-room", store.ModeChat)
	connectors.Register(newFakeFiles())
	folder := e.bind(t, scout, e.conn(t, "org_a", filesKind, nil), "root", store.ModeFiles)

	task := taskFor(scout, "usr_ada", "ada@example.ch")
	fromChat := task
	fromChat.Source = protocol.SourceChat
	fromChat.Origin = store.Origin{ConnectionID: c.ID, Conversation: "sales", Thread: "m1"}
	fromKeeraChat := task
	fromKeeraChat.Source = protocol.SourceChat

	msg := func(text, channel string) string {
		b, _ := json.Marshal(map[string]string{"text": text, "channel": channel})
		return string(b)
	}
	e.call(t, scout, task, "send_message", msg("hello", sales.ID), "ok")
	e.call(t, scout, fromChat, "send_message", msg("working on it", ""), "ok")
	e.call(t, scout, task, "send_message", msg("hi", otherSales.ID), "not one of your channels")
	e.call(t, scout, task, "send_message", msg("hi", every.ID), "not one of your channels")
	e.call(t, scout, task, "send_message", msg("hi", folder.ID), "not one of your channels")
	e.call(t, scout, task, "send_message", msg("hi", "bnd_nope"), "not one of your channels")
	e.call(t, scout, task, "send_message", msg("hi", ""), "did not come from a chat")
	e.call(t, scout, fromKeeraChat, "send_message", msg("hi", ""), "Keera Chat")
	e.call(t, scout, task, "send_message", msg("  ", sales.ID), "empty")
	want := []string{"sales/: hello", "sales/m1: working on it"}
	if got := e.chat.messages(); !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

func TestBoardToolsStayInTheAgentsSquads(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	outsider := e.agent(t, "org_a", "Outsider", store.AccessOrg)
	spy := e.agent(t, "org_b", "Spy", store.AccessOrg)
	e.squad(t, "org_a", scout.ID)
	theirs := e.squad(t, "org_b", spy.ID)
	// A squad of Scout's organisation that Scout is not in.
	other, err := e.st.CreateSquad(e.ctx, store.Squad{ID: id.New("sqd"), OrgID: "org_a", Name: "Other", CreatedBy: "ada"},
		[]string{outsider.ID})
	if err != nil {
		t.Fatal(err)
	}
	var hidden []store.Card
	for _, q := range []store.Squad{theirs, other} {
		c, err := e.st.CreateCard(e.ctx, store.Card{ID: id.New("card"), SquadID: q.ID, Title: "Secret", Status: store.CardBacklog})
		if err != nil {
			t.Fatal(err)
		}
		hidden = append(hidden, c)
	}
	task := taskFor(scout, "usr_ada", "ada@example.ch")

	e.call(t, scout, task, "board_create_card", `{"title":"Ours","agent":"Scout"}`, "ok")
	e.call(t, scout, task, "board_create_card", `{"title":"Theirs","agent":"Spy"}`, "Spy is not in the squad Tasks")
	e.call(t, scout, task, "board_create_card", `{"title":"Theirs","agent":"Outsider"}`, "Outsider is not in the squad Tasks")
	e.call(t, scout, task, "board_create_card", `{"title":"x","squad":"Other"}`, `not in a squad called "Other"`)
	e.call(t, scout, task, "board_create_card", `{"title":"  "}`, "needs a title")
	e.call(t, scout, task, "board_create_card", `{"title":"x","status":"later"}`, "unknown status")
	long := func(field string, n int) string {
		return fmt.Sprintf(`{"title":"x",%q:%q}`, field, strings.Repeat("a", n))
	}
	e.call(t, scout, task, "board_create_card", long("title", store.MaxCardTitle+1), "title is longer")
	e.call(t, scout, task, "board_create_card", long("body", store.MaxCardBody+1), "description is longer")
	e.call(t, scout, task, "board_list_cards", `{"status":"later"}`, "unknown status")

	var listed []cardView
	if err := json.Unmarshal([]byte(e.call(t, scout, task, "board_list_cards", `{}`, "ok")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Title != "Ours" || listed[0].Agent != "Scout" || listed[0].Squad != "Tasks" {
		t.Fatalf("listed %+v", listed)
	}
	for _, c := range hidden {
		e.call(t, scout, task, "board_update_card", fmt.Sprintf(`{"card_id":%q,"title":"Mine now"}`, c.ID), "there is no card")
		e.call(t, scout, task, "board_comment", fmt.Sprintf(`{"card_id":%q,"body":"hi"}`, c.ID), "there is no card")
		e.call(t, scout, task, "board_read_card", fmt.Sprintf(`{"card_id":%q}`, c.ID), "there is no card")
		if got, _ := e.st.Card(e.ctx, c.ID); got.Title != "Secret" || got.Comments != 0 {
			t.Fatalf("a card of another squad changed: %+v", got)
		}
	}
	e.call(t, scout, task, "board_comment", fmt.Sprintf(`{"card_id":%q,"body":" "}`, listed[0].ID), "empty")
	e.call(t, scout, task, "board_comment", fmt.Sprintf(`{"card_id":%q,"body":"noted"}`, listed[0].ID), "ok")
	e.call(t, scout, task, "board_comment", fmt.Sprintf(`{"card_id":%q,"body":%q}`, listed[0].ID,
		strings.Repeat("a", store.MaxComment+1)), "longer than")
	e.call(t, scout, task, "board_update_card", fmt.Sprintf(`{"card_id":%q,"body":%q}`, listed[0].ID,
		strings.Repeat("a", store.MaxCardBody+1)), "description is longer")
	if got := e.call(t, scout, task, "board_read_card", fmt.Sprintf(`{"card_id":%q}`, listed[0].ID), "ok"); !strings.Contains(got, `"body":"noted"`) {
		t.Fatalf("read %s", got)
	}

	loner := e.agent(t, "org_a", "Loner", store.AccessOrg)
	e.call(t, loner, taskFor(loner, "usr_ada", "ada@example.ch"), "board_create_card", `{"title":"x"}`, "you are in no squad")
}

func TestBoardToolsInSeveralSquads(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	writer := e.agent(t, "org_a", "Writer", store.AccessOrg)
	sales := e.squad(t, "org_a", scout.ID)
	if _, err := e.st.CreateSquad(e.ctx, store.Squad{ID: id.New("sqd"), OrgID: "org_a", Name: "Support", CreatedBy: "ada"},
		[]string{scout.ID, writer.ID}); err != nil {
		t.Fatal(err)
	}
	task := taskFor(scout, "usr_ada", "ada@example.ch")
	e.call(t, scout, task, "board_create_card", `{"title":"x"}`, "in several squads; name one: Tasks, Support")
	e.call(t, scout, task, "board_create_card", `{"title":"Reply","squad":"support","agent":"Writer"}`, "ok")
	e.call(t, scout, task, "board_create_card", `{"title":"Call","squad":"Tasks"}`, "ok")
	var listed []cardView
	if err := json.Unmarshal([]byte(e.call(t, scout, task, "board_list_cards", `{"squad":"Support"}`, "ok")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Title != "Reply" || listed[0].Squad != "Support" {
		t.Fatalf("listed %+v", listed)
	}
	// Working on a card, the card's squad is the one meant.
	c, err := e.st.CreateCard(e.ctx, store.Card{ID: id.New("card"), SquadID: sales.ID, Title: "Lead", Status: store.CardTodo, AgentID: &scout.ID})
	if err != nil {
		t.Fatal(err)
	}
	fromCard := task
	fromCard.Source, fromCard.Origin = protocol.SourceBoard, store.Origin{CardID: c.ID}
	if got := e.call(t, scout, fromCard, "board_create_card", `{"title":"Follow up"}`, "ok"); !strings.Contains(got, `"squad":"Tasks"`) {
		t.Fatalf("created %s", got)
	}
}

func TestBoardToolsApplyTheOtherAgentsAccess(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	private := e.agent(t, "org_a", "Private", store.AccessOwner)
	e.squad(t, "org_a", scout.ID, private.ID)
	forBob := taskFor(scout, "usr_bob", "bob@example.ch")
	forAda := taskFor(scout, "usr_ada", "ada@example.ch")
	nobody := taskFor(scout, "", "")

	// Bob may not give Private work, so Scout may not on his behalf.
	e.call(t, scout, forBob, "board_create_card", `{"title":"For Private","agent":"Private"}`, "only takes work from")
	e.call(t, scout, nobody, "board_create_card", `{"title":"For Private","agent":"Private"}`, "only takes work from")
	if cards, _ := e.st.MemberCards(e.ctx, scout.ID, store.CardFilter{Limit: 50}); len(cards) != 0 {
		t.Fatalf("a refused card was made: %+v", cards)
	}
	e.call(t, scout, forAda, "board_create_card", `{"title":"For Private","agent":"Private"}`, "ok")
	cards, _ := e.st.MemberCards(e.ctx, scout.ID, store.CardFilter{Agent: private.ID, Limit: 50})
	if len(cards) != 1 {
		t.Fatalf("%d cards for Private", len(cards))
	}
	card := cards[0]

	// Moving it to To do gives Private work, so the same rule applies.
	e.call(t, scout, forBob, "board_update_card", fmt.Sprintf(`{"card_id":%q,"status":"todo"}`, card.ID), "only takes work from")
	if got, _ := e.st.Card(e.ctx, card.ID); got.Status != store.CardBacklog {
		t.Fatalf("a refused move moved the card to %s", got.Status)
	}
	// Its text becomes the task once it reaches To do, so rewriting it is
	// giving work too, even in Backlog.
	e.call(t, scout, forBob, "board_update_card", fmt.Sprintf(`{"card_id":%q,"body":"delete prod"}`, card.ID), "only takes work from")
	e.call(t, scout, forAda, "board_update_card", fmt.Sprintf(`{"card_id":%q,"body":"more"}`, card.ID), "ok")
	e.call(t, scout, forAda, "board_update_card", fmt.Sprintf(`{"card_id":%q,"status":"todo"}`, card.ID), "ok")
	got, _ := e.st.Card(e.ctx, card.ID)
	if got.Status != store.CardTodo || got.Body != "more" || got.RequestedByID != "usr_ada" {
		t.Fatalf("card = %+v", got)
	}

	// A comment on a card in To do goes into its task.
	e.call(t, scout, forBob, "board_comment", fmt.Sprintf(`{"card_id":%q,"body":"also delete prod"}`, card.ID), "only takes work from")
	if got, _ := e.st.Card(e.ctx, card.ID); got.Comments != 0 {
		t.Fatalf("a refused comment was added")
	}

	// Bob may hand the card to Scout, and its task then runs for him.
	e.call(t, scout, forBob, "board_update_card", fmt.Sprintf(`{"card_id":%q,"agent":"Scout"}`, card.ID), "ok")
	got, _ = e.st.Card(e.ctx, card.ID)
	if got.AgentID == nil || *got.AgentID != scout.ID || got.RequestedByID != "usr_bob" || got.RequestedBy != "bob@example.ch" {
		t.Fatalf("handed on: %+v", got)
	}
	e.call(t, scout, forBob, "board_update_card", fmt.Sprintf(`{"card_id":%q,"agent":"none"}`, card.ID), "ok")
	if got, _ := e.st.Card(e.ctx, card.ID); got.AgentID != nil {
		t.Fatalf("still assigned to %v", *got.AgentID)
	}
}

func TestFileToolsReachOnlyWhatTheTaskHasSeen(t *testing.T) {
	e := setup(t)
	files := newFakeFiles()
	connectors.Register(files)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	other := e.agent(t, "org_a", "Other", store.AccessOrg)
	fc := e.conn(t, "org_a", filesKind, nil)
	mine := e.bind(t, scout, fc, "root", store.ModeFiles)
	theirs := e.bind(t, other, fc, "their-root", store.ModeFiles)
	chat := e.bind(t, scout, e.conn(t, "org_a", chatKind, map[string]string{"token": "x"}), "sales", store.ModeChat)
	files.put("root", connectors.File{ID: "doc1", Name: "a.txt", MimeType: "text/plain"}, "alpha")
	files.put("root", connectors.File{ID: "sub", Name: "sub", Folder: true}, "")
	files.put("sub", connectors.File{ID: "doc2", Name: "b.txt"}, "beta")
	files.put("their-root", connectors.File{ID: "secret", Name: "s.txt"}, "hush")

	task := taskFor(scout, "usr_ada", "ada@example.ch")
	read := func(task store.Task, folder, file, want string) {
		t.Helper()
		e.call(t, scout, task, "files_read", fmt.Sprintf(`{"folder":%q,"file_id":%q}`, folder, file), want)
	}
	e.call(t, scout, task, "files_list", fmt.Sprintf(`{"folder":%q}`, theirs.ID), "not one of your folders")
	e.call(t, scout, task, "files_list", fmt.Sprintf(`{"folder":%q}`, chat.ID), "not one of your folders")
	read(task, mine.ID, "doc1", "list the folder first")

	if got := e.call(t, scout, task, "files_list", fmt.Sprintf(`{"folder":%q}`, mine.ID), "ok"); !strings.Contains(got, "doc1") {
		t.Fatalf("listed %s", got)
	}
	if got := e.call(t, scout, task, "files_read", fmt.Sprintf(`{"folder":%q,"file_id":"doc1"}`, mine.ID), "ok"); !strings.HasSuffix(got, "alpha") {
		t.Fatalf("read %q", got)
	}
	// A file id from another folder the connection reaches stays closed.
	read(task, mine.ID, "secret", "list the folder first")
	read(task, theirs.ID, "secret", "not one of your folders")
	e.call(t, scout, task, "files_list", fmt.Sprintf(`{"folder":%q,"subfolder_id":"their-root"}`, mine.ID), "list the folder first")
	e.call(t, scout, task, "files_list", fmt.Sprintf(`{"folder":%q,"subfolder_id":"sub"}`, mine.ID), "ok")
	read(task, mine.ID, "doc2", "ok")

	// What one task has seen, another has not.
	read(taskFor(scout, "usr_ada", "ada@example.ch"), mine.ID, "doc1", "list the folder first")
	// A task about a watched file may read that file.
	watch := e.bind(t, scout, fc, "sub", store.ModeWatch)
	watched := taskFor(scout, "usr_ada", "ada@example.ch")
	watched.Origin = store.Origin{BindingID: watch.ID, FileID: "doc2"}
	read(watched, watch.ID, "doc2", "ok")
	read(watched, mine.ID, "doc2", "list the folder first")

	write := func(task store.Task, sub, name, want string) {
		t.Helper()
		e.call(t, scout, task, "files_write",
			fmt.Sprintf(`{"folder":%q,"subfolder_id":%q,"name":%q,"content":"x"}`, mine.ID, sub, name), want)
	}
	write(task, "", "../escape.txt", "without slashes")
	write(task, "", "a\\b.txt", "without slashes")
	write(task, "their-root", "x.txt", "list the folder first")
	write(task, "sub", "report.md", "ok")
	write(task, "", "notes.txt", "ok")
	if !slices.Equal(files.written, []string{"sub/report.md", "root/notes.txt"}) {
		t.Fatalf("wrote %q", files.written)
	}
	// The agent's own write is noted, so its watch skips it.
	if own, err := e.st.Wrote(e.ctx, scout.ID, fc.ID, "w_notes.txt", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil || !own {
		t.Fatalf("Wrote = %v, %v", own, err)
	}
	// And the task may read back what it wrote.
	files.put("root", connectors.File{ID: "w_notes.txt", Name: "notes.txt"}, "x")
	read(task, mine.ID, "w_notes.txt", "ok")
}

func TestFileToolsOfAConnectorPeopleSignInTo(t *testing.T) {
	e := setup(t)
	connectors.Register(newFakeFiles())
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	perUser, err := e.st.CreateConnection(e.ctx, store.Connection{
		ID: id.New("con"), OrgID: "org_a", Kind: filesKind, Name: "Their drive", PerUser: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := e.bind(t, scout, perUser, "root", store.ModeFiles)
	// A task nobody asked for has nobody to act as.
	e.call(t, scout, taskFor(scout, "", ""), "files_list", fmt.Sprintf(`{"folder":%q}`, b.ID), "nobody asked")
}

func TestAPIToolsCheckTheBinding(t *testing.T) {
	e := setup(t)
	api := &fakeTools{}
	connectors.Register(api)
	connectors.Register(fakeAPI{})
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	other := e.agent(t, "org_a", "Other", store.AccessOrg)
	ac := e.conn(t, "org_a", apiKind, nil)
	readOnly := e.bind(t, scout, ac, "ACME", store.ModeRead)
	theirs := e.bind(t, other, ac, "*", store.ModeWrite)
	chat := e.bind(t, scout, e.conn(t, "org_a", chatKind, map[string]string{"token": "x"}), "sales", store.ModeChat)
	task := taskFor(scout, "usr_ada", "ada@example.ch")

	if got := e.call(t, scout, task, apiKind+"_find", fmt.Sprintf(`{"connector":%q,"q":"x"}`, readOnly.ID), "ok"); got != `{"tool":"find"}` {
		t.Fatalf("find answered %s", got)
	}
	e.call(t, scout, task, apiKind+"_change", fmt.Sprintf(`{"connector":%q}`, readOnly.ID), "read only")
	e.call(t, scout, task, apiKind+"_find", fmt.Sprintf(`{"connector":%q}`, theirs.ID), "not one of your connectors")
	e.call(t, scout, task, apiKind+"_find", fmt.Sprintf(`{"connector":%q}`, chat.ID), "not one of your connectors")
	e.call(t, scout, task, apiKind+"_find", `{}`, "give the connector")
	e.call(t, scout, task, apiKind+"_find", `[1]`, "do not fit")
	e.call(t, scout, task, apiKind+"_nope", fmt.Sprintf(`{"connector":%q}`, readOnly.ID), "no tool called")
	// A connector of one kind does not run another kind's tools.
	e.call(t, scout, task, "fakeapi_search", fmt.Sprintf(`{"connector":%q}`, readOnly.ID), "no tool called fakeapi_search")

	// The adapter gets the binding's target and the tool's own arguments.
	if got := api.made(); !slices.Equal(got, []string{`ACME find {"q":"x"}`}) {
		t.Fatalf("calls = %q", got)
	}
}

func TestRepoToolsCheckTheBinding(t *testing.T) {
	e := setup(t)
	code := &fakeCode{}
	connectors.Register(code)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	other := e.agent(t, "org_a", "Other", store.AccessOrg)
	cc := e.conn(t, "org_a", codeKind, nil)
	mine := e.bind(t, scout, cc, "acme/web", store.ModeRepo)
	theirs := e.bind(t, other, cc, "acme/secret", store.ModeRepo)
	task := taskFor(scout, "usr_ada", "ada@example.ch")

	e.call(t, scout, task, "repo_list_issues", fmt.Sprintf(`{"repository":%q}`, mine.ID), "ok")
	e.call(t, scout, task, "repo_list_issues", fmt.Sprintf(`{"repository":%q}`, theirs.ID), "not one of your repositories")
	e.call(t, scout, task, "repo_list_issues", fmt.Sprintf(`{"repository":%q,"state":"merged"}`, mine.ID), "state must be")
	e.call(t, scout, task, "repo_read_issue", fmt.Sprintf(`{"repository":%q,"number":3,"kind":"change"}`, mine.ID), "ok")
	e.call(t, scout, task, "repo_read_issue", fmt.Sprintf(`{"repository":%q,"number":0}`, mine.ID), "give the number")
	e.call(t, scout, task, "repo_read_issue", fmt.Sprintf(`{"repository":%q,"number":1,"kind":"pr"}`, mine.ID), "kind must be")
	e.call(t, scout, task, "repo_comment", fmt.Sprintf(`{"repository":%q,"number":1,"body":"hi"}`, theirs.ID), "not one of your repositories")
	e.call(t, scout, task, "repo_comment", fmt.Sprintf(`{"repository":%q,"number":1,"body":""}`, mine.ID), "give the number")
	e.call(t, scout, task, "repo_comment", fmt.Sprintf(`{"repository":%q,"number":1,"body":"hi"}`, mine.ID), "ok")
	e.call(t, scout, task, "repo_open_change", fmt.Sprintf(`{"repository":%q,"branch":"main","title":"x"}`, mine.ID), "keera/")
	e.call(t, scout, task, "repo_open_change", fmt.Sprintf(`{"repository":%q,"branch":"keera/fix"}`, mine.ID), "title")
	e.call(t, scout, task, "repo_open_change", fmt.Sprintf(`{"repository":%q,"branch":"keera/fix","title":"Fix"}`, theirs.ID), "not one of your repositories")
	e.call(t, scout, task, "repo_open_change", fmt.Sprintf(`{"repository":%q,"branch":"keera/fix","title":"Fix"}`, mine.ID), "ok")
	if len(code.changes) != 1 || code.changes[0].Base != "main" {
		t.Fatalf("changes = %+v", code.changes)
	}

	if _, err := e.d.GitRemote(e.ctx, scout, task, theirs.ID); err == nil {
		t.Fatal("GitRemote gave another agent's repository")
	}
	if r, err := e.d.GitRemote(e.ctx, scout, task, mine.ID); err != nil || !strings.Contains(r.URL, "acme/web") {
		t.Fatalf("GitRemote = %+v, %v", r, err)
	}
}

func TestCallToolRefusesWhatItDoesNotKnow(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	task := taskFor(scout, "usr_ada", "ada@example.ch")
	e.call(t, scout, task, "shell", `{}`, "no tool called shell")
	e.call(t, scout, task, "board_list_cards", `{"mine":"yes"}`, "do not fit")
}

func TestToolsListWhatTheAgentIsBoundTo(t *testing.T) {
	e := setup(t)
	connectors.Register(newFakeFiles())
	connectors.Register(&fakeCode{})
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	names := func(a store.Agent) []string {
		t.Helper()
		specs, _, err := e.d.Tools(e.ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range specs {
			out = append(out, s.Name)
		}
		return out
	}
	if got := names(scout); slices.Contains(got, "files_list") || slices.Contains(got, "repo_list_issues") ||
		!slices.Contains(got, "send_message") {
		t.Fatalf("unbound: %v", got)
	}
	b := e.bind(t, scout, e.conn(t, "org_a", filesKind, nil), "root", store.ModeFiles)
	e.bind(t, scout, e.conn(t, "org_a", codeKind, nil), "acme/web", store.ModeRepo)
	got := names(scout)
	if !slices.Contains(got, "files_list") || !slices.Contains(got, "repo_open_change") {
		t.Fatalf("bound: %v", got)
	}
	specs, _, _ := e.d.Tools(e.ctx, scout)
	i := slices.IndexFunc(specs, func(s protocol.ToolSpec) bool { return s.Name == "files_list" })
	if !strings.Contains(specs[i].Description, b.ID) {
		t.Fatalf("files_list does not name the folder: %s", specs[i].Description)
	}

	ws := scout
	ws.Kind = store.KindWorkspace
	if got := names(ws); slices.Contains(got, "send_message") {
		t.Fatalf("a workspace may send messages: %v", got)
	}
}

func TestSquadListShowsWhoTheTaskMayGiveWork(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	writer := e.agent(t, "org_a", "Writer", store.AccessOrg)
	writer.Description = "Drafts posts."
	if _, err := e.st.UpdateAgent(e.ctx, writer); err != nil {
		t.Fatal(err)
	}
	private := e.agent(t, "org_a", "Private", store.AccessOwner)
	off := e.agent(t, "org_a", "Off", store.AccessOrg)
	if _, err := e.st.SetEnabled(e.ctx, off.ID, false); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "org_a", "Outsider", store.AccessOrg)
	q := e.squad(t, "org_a", scout.ID, writer.ID, private.ID, off.ID)
	q.Goal, q.LeadID = "Grow sales.", &writer.ID
	if _, err := e.st.UpdateSquad(e.ctx, q, []string{scout.ID, writer.ID, private.ID, off.ID}); err != nil {
		t.Fatal(err)
	}

	list := func(task store.Task) []agentView {
		t.Helper()
		var out []squadView
		if err := json.Unmarshal([]byte(e.call(t, scout, task, "squad_list", `{}`, "ok")), &out); err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Name != "Tasks" || out[0].Goal != "Grow sales." || out[0].Lead != "Writer" {
			t.Fatalf("squads %+v", out)
		}
		return out[0].Members
	}
	// Not itself, not one that is off, not one outside the squad.
	want := []agentView{{Name: "Private"}, {Name: "Writer", Description: "Drafts posts."}}
	if got := list(taskFor(scout, "usr_ada", "ada@example.ch")); !slices.Equal(got, want) {
		t.Fatalf("for Ada: %+v, want %+v", got, want)
	}
	// Bob may not give Private work, so Scout may not hand it any for him.
	want = []agentView{{Name: "Writer", Description: "Drafts posts."}}
	if got := list(taskFor(scout, "usr_bob", "bob@example.ch")); !slices.Equal(got, want) {
		t.Fatalf("for Bob: %+v, want %+v", got, want)
	}
	if got := list(taskFor(scout, "usr_gone", "gone@example.ch")); len(got) != 0 {
		t.Fatalf("for someone who left: %+v", got)
	}
}

func TestACardInToDoWithoutAnAgentGoesToTheLead(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	lead := e.agent(t, "org_a", "Lead", store.AccessOrg)
	q := e.squad(t, "org_a", scout.ID, lead.ID)
	q.LeadID = &lead.ID
	if _, err := e.st.UpdateSquad(e.ctx, q, []string{scout.ID, lead.ID}); err != nil {
		t.Fatal(err)
	}
	task := taskFor(scout, "usr_ada", "ada@example.ch")
	if got := e.call(t, scout, task, "board_create_card", `{"title":"Plan","status":"todo"}`, "ok"); !strings.Contains(got, `"agent":"Lead"`) {
		t.Fatalf("created %s", got)
	}
	if got := e.call(t, scout, task, "board_create_card", `{"title":"Later"}`, "ok"); strings.Contains(got, `"agent"`) {
		t.Fatalf("a card in Backlog went to the lead: %s", got)
	}
}

func TestToolsOfSquadsOnlyForMembers(t *testing.T) {
	e := setup(t)
	scout := e.agent(t, "org_a", "Scout", store.AccessOrg)
	has := func(name string) bool {
		t.Helper()
		specs, _, err := e.d.Tools(e.ctx, scout)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(specs, func(s protocol.ToolSpec) bool {
			return s.Name == name && (name != "board_create_card" || strings.Contains(s.Description, "Your squads: Tasks."))
		})
	}
	if has("board_list_cards") || has("squad_list") {
		t.Fatal("an agent in no squad has board tools")
	}
	q := e.squad(t, "org_a", scout.ID)
	q.LeadID = &scout.ID
	if _, err := e.st.UpdateSquad(e.ctx, q, []string{scout.ID}); err != nil {
		t.Fatal(err)
	}
	// The prompt lists the squad, as it lists channels and folders.
	_, conn, _ := e.d.Tools(e.ctx, scout)
	if !slices.Contains(conn, protocol.Connected{ID: q.ID, What: protocol.ConnectedSquad, Service: "board", Name: "Tasks, which you lead"}) {
		t.Fatalf("connected %+v", conn)
	}
	for _, name := range []string{"squad_list", "board_list_cards", "board_read_card", "board_create_card", "board_update_card", "board_comment"} {
		if !has(name) {
			t.Errorf("a member lacks %s", name)
		}
	}
}
