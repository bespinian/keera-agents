package dispatch

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// together runs f n times at once, as several replicas would.
func together(n int, f func()) {
	var wg sync.WaitGroup
	for range n {
		wg.Go(f)
	}
	wg.Wait()
}

func (e *testEnv) schedule(t *testing.T, a store.Agent, tz string, next time.Time, reply *string) store.Schedule {
	t.Helper()
	sc, err := e.st.CreateSchedule(e.ctx, store.Schedule{
		ID: id.New("sch"), AgentID: a.ID, Name: "Report", Cron: "0 9 * * *", Timezone: tz,
		Prompt: "Write the report", ReplyBindingID: reply, Enabled: true, NextRunAt: &next,
		CreatedBy: "ada@example.ch", RequestedBy: "bob@example.ch", RequestedByID: "usr_bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestADueScheduleMakesOneTask(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	due := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	now := due.Add(30 * time.Second)
	sc := e.schedule(t, a, "UTC", due, nil)

	together(4, func() { e.d.RunSchedules(e.ctx, now) })
	e.d.RunSchedules(e.ctx, now)
	ts := e.tasks(t, a)
	if len(ts) != 1 {
		t.Fatalf("one due run made %d tasks", len(ts))
	}
	got := ts[0]
	if got.Source != protocol.SourceSchedule || got.Origin.ScheduleID != sc.ID || got.Prompt != "Write the report" ||
		got.RequestedByID != "usr_bob" || !strings.Contains(got.Context, `"Report"`) {
		t.Fatalf("task = %+v", got)
	}
	after, _ := e.st.Schedule(e.ctx, sc.ID)
	if after.NextRunAt == nil || !after.NextRunAt.Equal(due.Add(24*time.Hour)) || !after.Enabled {
		t.Fatalf("next run = %v, enabled %v", after.NextRunAt, after.Enabled)
	}
}

func TestARunMissedByMoreThanADayIsSkipped(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	due := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	now := due.Add(49 * time.Hour)
	sc := e.schedule(t, a, "UTC", due, nil)
	e.d.RunSchedules(e.ctx, now)
	if n := len(e.tasks(t, a)); n != 0 {
		t.Fatalf("a stale run made %d tasks", n)
	}
	// It still moves on, so it runs at its next time.
	after, _ := e.st.Schedule(e.ctx, sc.ID)
	if after.NextRunAt == nil || !after.NextRunAt.After(now) {
		t.Fatalf("next run = %v", after.NextRunAt)
	}
}

func TestAScheduleThatCannotRunAgainRunsOnceAndStops(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	due := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	sc := e.schedule(t, a, "Mars/Olympus_Mons", due, nil)
	e.d.RunSchedules(e.ctx, due.Add(time.Second))
	if n := len(e.tasks(t, a)); n != 1 {
		t.Fatalf("%d tasks", n)
	}
	after, _ := e.st.Schedule(e.ctx, sc.ID)
	if after.Enabled || after.NextRunAt != nil {
		t.Fatalf("still on: enabled %v, next %v", after.Enabled, after.NextRunAt)
	}
}

func TestADisabledAgentsScheduleWaits(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	due := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	sc := e.schedule(t, a, "UTC", due, nil)
	if _, err := e.st.SetEnabled(e.ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	e.d.RunSchedules(e.ctx, due.Add(time.Minute))
	if n := len(e.tasks(t, a)); n != 0 {
		t.Fatalf("%d tasks", n)
	}
	if after, _ := e.st.Schedule(e.ctx, sc.ID); !after.NextRunAt.Equal(due) {
		t.Fatalf("next run moved to %v", after.NextRunAt)
	}
}

func (e *testEnv) card(t *testing.T, org, status string, agentID *string) store.Card {
	t.Helper()
	board, err := e.d.EnsureBoard(e.ctx, org, "ada@example.ch")
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.st.CreateCard(e.ctx, store.Card{
		ID: id.New("card"), BoardID: board.ID, Title: "Write the report", Body: "For March.", Status: status,
		AgentID: agentID, RequestedBy: "bob@example.ch", RequestedByID: "usr_bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestACardInToDoMakesOneTask(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	off := e.agent(t, "org_a", "Off", store.AccessOrg)
	if _, err := e.st.SetEnabled(e.ctx, off.ID, false); err != nil {
		t.Fatal(err)
	}
	c := e.card(t, "org_a", store.CardTodo, &a.ID)
	if _, err := e.st.AddComment(e.ctx, store.Comment{ID: id.New("cmt"), CardID: c.ID, Author: "Bob", Body: "Keep it short."}); err != nil {
		t.Fatal(err)
	}
	e.card(t, "org_a", store.CardBacklog, &a.ID)
	e.card(t, "org_a", store.CardTodo, nil)
	e.card(t, "org_a", store.CardTodo, &off.ID)

	together(4, func() { e.d.PickUpCards(e.ctx) })
	e.d.PickUpCards(e.ctx)
	ts := e.tasks(t, a)
	if len(ts) != 1 {
		t.Fatalf("one card made %d tasks", len(ts))
	}
	task := ts[0]
	if task.Origin.CardID != c.ID || task.RequestedByID != "usr_bob" ||
		!strings.HasPrefix(task.Prompt, "Write the report") || !strings.Contains(task.Prompt, "Bob: Keep it short.") {
		t.Fatalf("task = %+v", task)
	}
	if n := len(e.tasks(t, off)); n != 0 {
		t.Fatalf("a disabled agent got %d tasks", n)
	}
	got, _ := e.st.Card(e.ctx, c.ID)
	if got.Status != store.CardInProgress || got.TaskID == nil || *got.TaskID != task.ID {
		t.Fatalf("card = %s, task %v", got.Status, got.TaskID)
	}

	// Done, and put back in To do by a person, it is new work.
	e.finish(t, task.ID, store.TaskSucceeded, "Here it is.", "")
	e.d.Deliver(e.ctx)
	got, _ = e.st.Card(e.ctx, c.ID)
	got.Status = store.CardTodo
	if _, err := e.st.UpdateCard(e.ctx, got); err != nil {
		t.Fatal(err)
	}
	e.d.PickUpCards(e.ctx)
	if n := len(e.tasks(t, a)); n != 2 {
		t.Fatalf("a card back in To do made %d tasks in all", n)
	}
}

// pickUp picks up a card and returns its task.
func (e *testEnv) pickUp(t *testing.T, a store.Agent, c store.Card) store.Task {
	t.Helper()
	e.d.PickUpCards(e.ctx)
	for _, task := range e.tasks(t, a) {
		if task.Origin.CardID == c.ID {
			return task
		}
	}
	t.Fatalf("card %s was not picked up", c.ID)
	return store.Task{}
}

func TestDeliverMovesTheCardAndComments(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	for _, tc := range []struct {
		name, state string
		// moved is where a person put the card meanwhile, if anywhere.
		moved, want string
	}{
		{"done", store.TaskSucceeded, "", store.CardReview},
		{"failed", store.TaskFailed, "", store.CardBacklog},
		{"moved meanwhile", store.TaskSucceeded, store.CardDone, store.CardDone},
	} {
		c := e.card(t, "org_a", store.CardTodo, &a.ID)
		task := e.pickUp(t, a, c)
		if tc.moved != "" {
			if err := e.st.MoveCard(e.ctx, c.ID, tc.moved, nil); err != nil {
				t.Fatal(err)
			}
		}
		e.finish(t, task.ID, tc.state, "The report.", "")
		together(3, func() { e.d.Deliver(e.ctx) })
		got, _ := e.st.Card(e.ctx, c.ID)
		if got.Status != tc.want {
			t.Errorf("%s: card in %s, want %s", tc.name, got.Status, tc.want)
		}
		comments, _ := e.st.Comments(e.ctx, c.ID)
		if len(comments) != 1 || comments[0].Author != "Scout" || comments[0].AgentID == nil ||
			!strings.Contains(comments[0].Body, "The report.") {
			t.Errorf("%s: comments %+v", tc.name, comments)
		}
	}
}

func TestDeliverToADeletedCardDoesNothing(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.card(t, "org_a", store.CardTodo, &a.ID)
	task := e.pickUp(t, a, c)
	if err := e.st.DeleteCard(e.ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	e.finish(t, task.ID, store.TaskSucceeded, "x", "")
	e.d.Deliver(e.ctx)
	got, _ := e.st.Task(e.ctx, task.ID)
	events, _ := e.st.Events(e.ctx, task.ID, 0)
	if !got.Delivered || len(events) != 0 {
		t.Fatalf("delivered %v, events %+v", got.Delivered, events)
	}
}

func TestACardCommentThatFailsIsTriedAgain(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.card(t, "org_a", store.CardTodo, &a.ID)
	task := e.pickUp(t, a, c)
	e.finish(t, task.ID, store.TaskSucceeded, "The report.", "")

	e.exec(t, `CREATE FUNCTION no_comments() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION 'comments are down'; END $$;
		CREATE TRIGGER no_comments BEFORE INSERT ON card_comments
		FOR EACH ROW EXECUTE FUNCTION no_comments()`)
	e.d.Deliver(e.ctx)
	got, _ := e.st.Task(e.ctx, task.ID)
	if got.Delivered {
		t.Fatal("a result whose comment failed counts as delivered")
	}
	// The move went through first, so the retry does not move it again.
	if card, _ := e.st.Card(e.ctx, c.ID); card.Status != store.CardReview {
		t.Fatalf("card in %s", card.Status)
	}

	e.exec(t, `DROP TRIGGER no_comments ON card_comments`)
	e.d.Deliver(e.ctx)
	e.d.Deliver(e.ctx)
	got, _ = e.st.Task(e.ctx, task.ID)
	comments, _ := e.st.Comments(e.ctx, c.ID)
	if !got.Delivered || len(comments) != 1 {
		t.Fatalf("delivered %v, %d comments", got.Delivered, len(comments))
	}
	if card, _ := e.st.Card(e.ctx, c.ID); card.Status != store.CardReview {
		t.Fatalf("card in %s after the retry", card.Status)
	}
}

// chatTask is a finished task from a message in sales, thread m1.
func (e *testEnv) chatTask(t *testing.T, a store.Agent, c store.Connection, msgID, state, result string) store.Task {
	t.Helper()
	key := c.ID + ":" + msgID
	task, err := e.d.Submit(e.ctx, store.Task{
		OrgID: a.OrgID, AgentID: a.ID, Source: protocol.SourceChat, DedupeKey: &key,
		Origin: store.Origin{ConnectionID: c.ID, Conversation: "sales", Thread: msgID},
		Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e.finish(t, task.ID, state, result, "")
}

func TestDeliverAnswersAChatOnce(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	done := e.chatTask(t, a, c, "m1", store.TaskSucceeded, "Here it is.")
	e.chatTask(t, a, c, "m2", store.TaskFailed, "")
	// Keera Chat reads its results itself.
	keera, err := e.d.Submit(e.ctx, store.Task{OrgID: "org_a", AgentID: a.ID, Source: protocol.SourceChat, Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, keera.ID, store.TaskSucceeded, "x", "")

	together(4, func() { e.d.Deliver(e.ctx) })
	e.d.Deliver(e.ctx)
	got := e.chat.messages()
	slices.Sort(got)
	if want := []string{"sales/m1: Here it is.", "sales/m2: I could not finish this"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	for _, id := range []string{done.ID, keera.ID} {
		if task, _ := e.st.Task(e.ctx, id); !task.Delivered {
			t.Fatalf("task %s is not marked delivered", id)
		}
	}
}

func TestAChatSendThatFailsIsNotSentTwice(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	task := e.chatTask(t, a, c, "m1", store.TaskSucceeded, "Here it is.")
	// The service may have posted before it failed, so trying again could
	// post twice. The task says what happened instead.
	e.chat.failWith(errors.New("rate limited"))
	e.d.Deliver(e.ctx)
	e.chat.failWith(nil)
	e.d.Deliver(e.ctx)
	if got := e.chat.messages(); len(got) != 0 {
		t.Fatalf("sent %q", got)
	}
	events, _ := e.st.Events(e.ctx, task.ID, 0)
	if len(events) != 1 || events[0].Kind != protocol.EventLog || !strings.Contains(events[0].Text, "rate limited") {
		t.Fatalf("events = %+v", events)
	}
}

func TestDeliverToAConnectorThatIsGone(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	task := e.chatTask(t, a, c, "m1", store.TaskSucceeded, "x")
	if err := e.st.DeleteConnection(e.ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	e.d.Deliver(e.ctx)
	events, _ := e.st.Events(e.ctx, task.ID, 0)
	if len(events) != 1 || !strings.Contains(events[0].Text, "the connector is gone") {
		t.Fatalf("events = %+v", events)
	}
}

func TestDeliverPostsAScheduleToItsChannel(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	c := e.conn(t, "org_a", chatKind, map[string]string{"token": "x"})
	sales := e.bind(t, a, c, "sales", store.ModeChat)
	every := e.bind(t, a, c, store.AnyConversation, store.ModeChat)
	due := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		reply *string
		event string
	}{
		{"its channel", &sales.ID, ""},
		{"every conversation", &every.ID, "names none to post in"},
		{"no channel", nil, ""},
	} {
		sc := e.schedule(t, a, "UTC", due, tc.reply)
		task, err := e.d.SubmitSchedule(e.ctx, sc)
		if err != nil {
			t.Fatal(err)
		}
		e.finish(t, task.ID, store.TaskSucceeded, "Report for "+tc.name, "")
		e.d.Deliver(e.ctx)
		events, _ := e.st.Events(e.ctx, task.ID, 0)
		if tc.event == "" && len(events) != 0 || tc.event != "" && (len(events) != 1 || !strings.Contains(events[0].Text, tc.event)) {
			t.Errorf("%s: events %+v", tc.name, events)
		}
	}
	if got := e.chat.messages(); !slices.Equal(got, []string{"sales/: Report for its channel"}) {
		t.Fatalf("sent %q", got)
	}

	// A channel removed after the task was queued.
	sc := e.schedule(t, a, "UTC", due, &sales.ID)
	task, _ := e.d.SubmitSchedule(e.ctx, sc)
	if err := e.st.DeleteBinding(e.ctx, sales.ID); err != nil {
		t.Fatal(err)
	}
	e.finish(t, task.ID, store.TaskSucceeded, "x", "")
	e.d.Deliver(e.ctx)
	if events, _ := e.st.Events(e.ctx, task.ID, 0); len(events) != 1 || !strings.Contains(events[0].Text, "gone") {
		t.Fatalf("events = %+v", events)
	}
}

func TestAWatchMakesOneTaskPerChangedFile(t *testing.T) {
	e := setup(t)
	files := newFakeFiles()
	channels.Register(files)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	fc := e.conn(t, "org_a", filesKind, nil)
	w := e.bind(t, a, fc, "inbox", store.ModeWatch)
	t1 := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	later := func() time.Time { return time.Now().Add(2 * time.Minute) }

	// The first check only starts the cursor.
	e.d.CheckWatches(e.ctx, later())
	if b, _ := e.st.Binding(e.ctx, w.ID); b.Cursor != "c" || b.CheckedAt == nil {
		t.Fatalf("cursor %q, checked %v", b.Cursor, b.CheckedAt)
	}
	if err := e.st.RecordWrite(e.ctx, a.ID, fc.ID, "own", t1); err != nil {
		t.Fatal(err)
	}
	files.changes = []channels.File{
		{ID: "f1", Name: "a.txt", Modified: t1},
		{ID: "f2", Name: "b.txt", Modified: t1},
		{ID: "f1", Name: "a.txt", Modified: t1.Add(time.Minute)},
		{ID: "dir", Name: "sub", Folder: true, Modified: t1},
		{ID: "own", Name: "mine.txt", Modified: t1},
	}
	e.d.CheckWatches(e.ctx, later())
	// The same changes again, as a feed may repeat them, make nothing new.
	e.d.CheckWatches(e.ctx, later())
	ts := e.tasks(t, a)
	var got []string
	for _, task := range ts {
		got = append(got, task.Origin.FileID)
		if task.Origin.BindingID != w.ID || task.RequestedByID != "usr_ada" || task.Source != protocol.SourceFile {
			t.Errorf("task = %+v", task)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"f1", "f2"}) {
		t.Fatalf("tasks for %q", got)
	}
	if b, _ := e.st.Binding(e.ctx, w.ID); b.Cursor != "c++" {
		t.Fatalf("cursor %q", b.Cursor)
	}
}

func TestAWatchThatFailsKeepsItsCursor(t *testing.T) {
	e := setup(t)
	files := newFakeFiles()
	channels.Register(files)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	w := e.bind(t, a, e.conn(t, "org_a", filesKind, nil), "inbox", store.ModeWatch)
	e.d.CheckWatches(e.ctx, time.Now().Add(2*time.Minute))
	files.failing = errors.New("drive is down")
	e.d.CheckWatches(e.ctx, time.Now().Add(4*time.Minute))
	b, _ := e.st.Binding(e.ctx, w.ID)
	if b.Cursor != "c" {
		t.Fatalf("cursor %q after a failed check", b.Cursor)
	}
	// It waits a full interval before asking again.
	files.failing = nil
	files.changes = []channels.File{{ID: "f1", Name: "a.txt", Modified: time.Now()}}
	e.d.CheckWatches(e.ctx, b.CheckedAt.Add(30*time.Second))
	if n := len(e.tasks(t, a)); n != 0 {
		t.Fatalf("checked again within the interval: %d tasks", n)
	}
	e.d.CheckWatches(e.ctx, b.CheckedAt.Add(2*time.Minute))
	if n := len(e.tasks(t, a)); n != 1 {
		t.Fatalf("%d tasks after the interval", n)
	}
}

func TestExpireTasksFailsATaskPastItsDeadline(t *testing.T) {
	e := setup(t)
	a := e.agent(t, "org_a", "Scout", store.AccessOrg)
	if _, err := e.d.Submit(e.ctx, store.Task{OrgID: "org_a", AgentID: a.ID, Source: protocol.SourceManual, Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	task, err := e.st.ClaimTask(e.ctx, a.ID, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	e.d.ExpireTasks(e.ctx)
	if got, _ := e.st.Task(e.ctx, task.ID); got.State != store.TaskFailed {
		t.Fatalf("state = %s", got.State)
	}
}

func TestCheckAccountsForgetsWhoLeft(t *testing.T) {
	e := setup(t)
	c := e.conn(t, "org_a", filesKind, nil)
	for _, u := range []string{"usr_bob", "usr_gone", "usr_dis"} {
		if err := e.st.SaveAccount(e.ctx, store.Account{ConnectionID: c.ID, UserID: u, Account: u, Token: []byte("t")}); err != nil {
			t.Fatal(err)
		}
	}
	has := func(u string) bool {
		_, err := e.st.Account(e.ctx, c.ID, u)
		return err == nil
	}
	// The gateway being down must not sign everyone out.
	e.gw.down.Store(true)
	e.d.CheckAccounts(e.ctx)
	if !has("usr_bob") || !has("usr_gone") || !has("usr_dis") {
		t.Fatal("forgot sign-ins while the gateway was down")
	}
	e.gw.down.Store(false)
	e.d.CheckAccounts(e.ctx)
	if !has("usr_bob") || has("usr_gone") || has("usr_dis") {
		t.Fatalf("bob %v, gone %v, disabled %v", has("usr_bob"), has("usr_gone"), has("usr_dis"))
	}
}
