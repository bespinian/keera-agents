package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/id"
)

// openTest connects to KEERA_AGENTS_TEST_DATABASE_URL, or skips. Each run
// starts from an empty schema, so tests see only their own rows.
func openTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KEERA_AGENTS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KEERA_AGENTS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Pool().Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Twice is a no-op.
	if applied, err := st.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("second Migrate = %v, %v", applied, err)
	}
	return st
}

func newAgent(t *testing.T, st *Store, org, name string) Agent {
	t.Helper()
	a, err := st.CreateAgent(context.Background(), Agent{
		ID: id.New("agt"), OrgID: org, OwnerID: "usr_1", OwnerEmail: "ada@example.ch",
		Name: name, Model: "keera-speed", MaxSteps: 10, Access: AccessOrg, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAgentNamesAreUniquePerOrg(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	if _, err := st.CreateAgent(ctx, Agent{
		ID: id.New("agt"), OrgID: "org_a", OwnerID: "u", OwnerEmail: "x", Name: "scout", Model: "m",
		Access: AccessOrg,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("same name, other case: %v", err)
	}
	newAgent(t, st, "org_b", "Scout")
	if got, err := st.AgentByName(ctx, "org_a", "SCOUT"); err != nil || got.ID != a.ID {
		t.Fatalf("AgentByName = %v, %v", got.ID, err)
	}
	// A deleted agent frees its name.
	if err := st.DeleteAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	newAgent(t, st, "org_a", "Scout")
	if _, err := st.Agent(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted agent: %v", err)
	}
}

func TestOneTaskAtATime(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	dedupe := "slack:1"
	first, err := st.CreateTask(ctx, Task{
		ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "chat",
		Prompt: "one", DedupeKey: &dedupe, Origin: Origin{Label: "Slack #x", Conversation: "C1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.OriginLabel != "Slack #x" || first.Origin.Conversation != "C1" {
		t.Fatalf("origin not kept: %+v", first.Origin)
	}
	if _, err := st.CreateTask(ctx, Task{
		ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "chat",
		Prompt: "again", DedupeKey: &dedupe,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("redelivered: %v", err)
	}
	if _, err := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "two"}); err != nil {
		t.Fatal(err)
	}

	got, err := st.ClaimTask(ctx, a.ID, time.Minute)
	if err != nil || got.ID != first.ID || got.State != TaskRunning || got.Deadline == nil {
		t.Fatalf("ClaimTask = %+v, %v", got, err)
	}
	if _, err := st.ClaimTask(ctx, a.ID, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim while one runs: %v", err)
	}
	if running, err := st.RunningTask(ctx, a.ID); err != nil || running.ID != first.ID {
		t.Fatalf("RunningTask = %v, %v", running.ID, err)
	}
	if ok, err := st.FinishTask(ctx, first.ID, TaskSucceeded, "done", ""); !ok || err != nil {
		t.Fatalf("FinishTask = %v, %v", ok, err)
	}
	if ok, _ := st.FinishTask(ctx, first.ID, TaskFailed, "", "late"); ok {
		t.Fatal("finished twice")
	}
	if _, err := st.ClaimTask(ctx, a.ID, time.Minute); err != nil {
		t.Fatalf("next claim: %v", err)
	}

	undelivered, err := st.Undelivered(ctx)
	if err != nil || len(undelivered) != 1 {
		t.Fatalf("Undelivered = %d, %v", len(undelivered), err)
	}
	if won, _ := st.MarkDelivered(ctx, first.ID); !won {
		t.Fatal("first delivery claim lost")
	}
	if won, _ := st.MarkDelivered(ctx, first.ID); won {
		t.Fatal("delivered twice")
	}
	// A delivery that sent nothing is handed back and claimed again.
	if err := st.UnmarkDelivered(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if won, _ := st.MarkDelivered(ctx, first.ID); !won {
		t.Fatal("a handed-back delivery was not claimed again")
	}
}

// An idle agent is suspended only while it has no task, and no task is
// claimed while its container stops.
func TestSuspendIdle(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	_ = st.TouchAgent(ctx, a.ID)
	never := func() error { t.Error("suspended an agent that was not idle"); return nil }

	if ok, err := st.SuspendIdle(ctx, a.ID, time.Now().Add(-time.Hour), never); ok || err != nil {
		t.Fatalf("recently active: %v, %v", ok, err)
	}
	task, _ := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x"})
	if ok, err := st.SuspendIdle(ctx, a.ID, time.Now().Add(time.Hour), never); ok || err != nil {
		t.Fatalf("with a queued task: %v, %v", ok, err)
	}
	if _, err := st.FinishTask(ctx, task.ID, TaskCancelled, "", ""); err != nil {
		t.Fatal(err)
	}

	claimed := make(chan error, 1)
	ok, err := st.SuspendIdle(ctx, a.ID, time.Now().Add(time.Hour), func() error {
		// A task that comes in now waits for the container to stop.
		_, _ = st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "y"})
		go func() {
			_, err := st.ClaimTask(ctx, a.ID, time.Minute)
			claimed <- err
		}()
		select {
		case err := <-claimed:
			t.Errorf("a task was claimed while the container stopped: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	})
	if !ok || err != nil {
		t.Fatalf("idle: %v, %v", ok, err)
	}
	if err := <-claimed; err != nil {
		t.Fatalf("the claim after the suspend: %v", err)
	}

	if ok, err := st.SuspendIdle(ctx, "agt_missing", time.Now(), never); ok || err != nil {
		t.Fatalf("missing agent: %v, %v", ok, err)
	}
}

// A member sees the tasks they asked for and those of agents they own, but
// not those of an owned agent once it is deleted.
func TestTasksForAViewer(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	mine := newAgent(t, st, "org_a", "Mine") // owned by usr_1
	other, err := st.CreateAgent(ctx, Agent{
		ID: id.New("agt"), OrgID: "org_a", OwnerID: "usr_2", OwnerEmail: "bob@example.ch",
		Name: "Other", Model: "m", MaxSteps: 10, Access: AccessOrg, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	task := func(a Agent, byID, by string) Task {
		out, err := st.CreateTask(ctx, Task{
			ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x",
			RequestedBy: by, RequestedByID: byID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ofMine := task(mine, "usr_3", "eve@example.ch")
	askedByID := task(other, "usr_1", "")
	askedByEmail := task(other, "", "ada@example.ch")
	task(other, "usr_3", "eve@example.ch")

	seen := func() []string {
		list, err := st.Tasks(ctx, TaskQuery{OrgID: "org_a", ViewerID: "usr_1", ViewerEmail: "ada@example.ch"})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, x := range list {
			ids = append(ids, x.ID)
		}
		return ids
	}
	if got := seen(); len(got) != 3 || !slices.Contains(got, ofMine.ID) ||
		!slices.Contains(got, askedByID.ID) || !slices.Contains(got, askedByEmail.ID) {
		t.Fatalf("viewer sees %v", got)
	}
	if got, _ := st.Task(ctx, ofMine.ID); got.AgentOwnerID != "usr_1" {
		t.Fatalf("AgentOwnerID = %q", got.AgentOwnerID)
	}
	if all, _ := st.Tasks(ctx, TaskQuery{OrgID: "org_a"}); len(all) != 4 {
		t.Fatalf("without a viewer: %d tasks", len(all))
	}

	f, err := st.TaskFacets(ctx, TaskQuery{OrgID: "org_a", ViewerID: "usr_3", ViewerEmail: "eve@example.ch"})
	if err != nil || len(f.Agents) != 2 || !slices.Equal(f.States, []string{"queued"}) ||
		!slices.Equal(f.Sources, []string{"manual"}) {
		t.Fatalf("TaskFacets = %+v, %v", f, err)
	}
	f, _ = st.TaskFacets(ctx, TaskQuery{OrgID: "org_a", AgentID: other.ID, ViewerID: "usr_3", ViewerEmail: "eve@example.ch"})
	if len(f.Agents) != 1 || f.Agents[0] != (TaskAgent{ID: other.ID, Name: "Other"}) {
		t.Fatalf("one agent's facets: %+v", f.Agents)
	}
	if f, _ := st.TaskFacets(ctx, TaskQuery{OrgID: "org_b"}); f.Agents == nil || len(f.States) != 0 {
		t.Fatalf("another organisation's facets: %+v", f)
	}

	if err := st.DeleteAgent(ctx, mine.ID); err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 2 || slices.Contains(got, ofMine.ID) {
		t.Fatalf("after the delete, viewer sees %v", got)
	}
	if got, _ := st.Task(ctx, ofMine.ID); got.AgentOwnerID != "" {
		t.Fatalf("a deleted agent's AgentOwnerID = %q", got.AgentOwnerID)
	}

	load, err := st.AgentLoad(ctx, other.ID)
	if err != nil || load.Queued != 3 || load.RunningTaskID != "" || load.LastTaskAt == nil {
		t.Fatalf("AgentLoad = %+v, %v", load, err)
	}
}

func TestExpireTasks(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	task, _ := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x"})
	if _, err := st.ClaimTask(ctx, a.ID, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	ids, err := st.ExpireTasks(ctx)
	if err != nil || len(ids) != 1 || ids[0] != task.ID {
		t.Fatalf("ExpireTasks = %v, %v", ids, err)
	}
}

func TestFailQueuedTasks(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	b := newAgent(t, st, "org_a", "Other")
	old, _ := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x"})
	ofB, _ := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: b.ID, Source: "manual", Prompt: "x"})
	cut := time.Now().Add(500 * time.Millisecond)
	time.Sleep(time.Second)
	recent, _ := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x"})

	n, err := st.FailQueuedTasks(ctx, a.ID, cut, "could not start")
	if err != nil || n != 1 {
		t.Fatalf("FailQueuedTasks = %d, %v", n, err)
	}
	for _, c := range []struct {
		id, state string
	}{{old.ID, "failed"}, {recent.ID, "queued"}, {ofB.ID, "queued"}} {
		if got, _ := st.Task(ctx, c.id); got.State != c.state {
			t.Errorf("task %s is %s, want %s", c.id, got.State, c.state)
		}
	}
}

func TestTaskSeries(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	b := newAgent(t, st, "org_b", "Other")
	for _, ag := range []Agent{a, a, b} {
		if _, err := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: ag.OrgID, AgentID: ag.ID, Source: "manual", Prompt: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	zurich, _ := time.LoadLocation("Europe/Zurich")
	days, err := st.TaskSeries(ctx, "org_a", "", time.Now().Add(-48*time.Hour), true, zurich)
	if err != nil || len(days) != 3 {
		t.Fatalf("daily = %+v, %v", days, err)
	}
	if last := days[len(days)-1]; last.Tasks != 2 || days[0].Tasks != 0 {
		t.Fatalf("daily = %+v", days)
	}
	hours, err := st.TaskSeries(ctx, "org_a", "", time.Now().Add(-24*time.Hour), false, zurich)
	if err != nil || len(hours) != 25 || hours[24].Tasks != 2 {
		t.Fatalf("hourly = %+v, %v", hours, err)
	}
	other := newAgent(t, st, "org_a", "Quiet")
	if _, err := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: other.ID, Source: "manual", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	mine, err := st.TaskSeries(ctx, "org_a", a.ID, time.Now().Add(-24*time.Hour), false, zurich)
	if err != nil || mine[24].Tasks != 2 {
		t.Fatalf("one agent's hours = %+v, %v", mine, err)
	}
	counts, err := st.TaskCounts(ctx, "org_a", other.ID, time.Now().Add(-time.Hour))
	if err != nil || counts.Queued != 1 {
		t.Fatalf("one agent's counts = %+v, %v", counts, err)
	}
}

func TestClaimRunOnce(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	due := time.Now().Add(-time.Second).Truncate(time.Microsecond)
	sc, err := st.CreateSchedule(ctx, Schedule{
		ID: id.New("sch"), AgentID: a.ID, Name: "Digest",
		Cron: "0 8 * * *", Timezone: "UTC", Prompt: "p", Enabled: true, NextRunAt: &due, CreatedBy: "ada",
		RequestedBy: "ada", RequestedByID: "usr_ada",
	})
	if err != nil || sc.RequestedByID != "usr_ada" {
		t.Fatal(sc, err)
	}
	sc.RequestedBy, sc.RequestedByID = "bob@example.ch", "usr_bob"
	if sc, err = st.UpdateSchedule(ctx, sc); err != nil || sc.RequestedByID != "usr_bob" || sc.CreatedBy != "ada" {
		t.Fatalf("UpdateSchedule = %+v, %v", sc, err)
	}
	list, err := st.DueSchedules(ctx, time.Now())
	if err != nil || len(list) != 1 || list[0].AgentName != "Scout" || list[0].OrgID != "org_a" {
		t.Fatalf("DueSchedules = %+v, %v", list, err)
	}
	next := due.Add(24 * time.Hour)
	if won, err := st.ClaimRun(ctx, sc.ID, *list[0].NextRunAt, &next); !won || err != nil {
		t.Fatalf("first claim = %v, %v", won, err)
	}
	if won, _ := st.ClaimRun(ctx, sc.ID, *list[0].NextRunAt, &next); won {
		t.Fatal("claimed twice")
	}
	// One that never runs again is switched off.
	if won, _ := st.ClaimRun(ctx, sc.ID, next, nil); !won {
		t.Fatal("last claim lost")
	}
	if got, _ := st.Schedule(ctx, sc.ID); got.Enabled || got.NextRunAt != nil {
		t.Fatalf("after the last run: %+v", got)
	}
}

func TestCardPickup(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	b, err := st.CreateBoard(ctx, Board{ID: id.New("brd"), OrgID: "org_a", Name: "Ops", CreatedBy: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := st.CreateCard(ctx, Card{ID: id.New("card"), BoardID: b.ID, Title: "one", Status: CardTodo, AgentID: &a.ID, CreatedBy: "ada"})
	c2, _ := st.CreateCard(ctx, Card{ID: id.New("card"), BoardID: b.ID, Title: "two", Status: CardTodo, CreatedBy: "ada"})
	if c2.Position <= c1.Position {
		t.Fatalf("new cards go last: %v then %v", c1.Position, c2.Position)
	}
	if c1.AgentName != "Scout" || c1.OrgID != "org_a" {
		t.Fatalf("card joins: %+v", c1)
	}
	pick, err := st.PickupCards(ctx)
	if err != nil || len(pick) != 1 || pick[0].ID != c1.ID {
		t.Fatalf("PickupCards = %v, %v", pick, err)
	}
	task, _ := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "board", Prompt: "x"})
	if err := st.MoveCard(ctx, c1.ID, CardInProgress, &task.ID); err != nil {
		t.Fatal(err)
	}
	if pick, _ := st.PickupCards(ctx); len(pick) != 0 {
		t.Fatalf("picked up again: %v", pick)
	}
	got, _ := st.Card(ctx, c1.ID)
	if got.Status != CardInProgress || got.TaskState != TaskQueued {
		t.Fatalf("after the move: %+v", got)
	}
	if _, err := st.AddComment(ctx, Comment{ID: id.New("cmt"), CardID: c1.ID, Author: "ada", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Card(ctx, c1.ID); got.Comments != 1 {
		t.Fatalf("comments = %d", got.Comments)
	}
	// Whoever last gave the work is kept apart from who created the card.
	got.RequestedBy, got.RequestedByID = "bob@example.ch", "usr_bob"
	if _, err := st.UpdateCard(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Card(ctx, c1.ID); got.RequestedByID != "usr_bob" || got.CreatedBy != "ada" {
		t.Fatalf("after giving work: %+v", got)
	}
	cards, err := st.BoardCards(ctx, b.ID)
	if err != nil || len(cards) != 2 || cards[0].ID != c2.ID {
		t.Fatalf("board order: %v, %v", cards, err)
	}
}

func TestBindingsAndWatches(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	c, err := st.CreateConnection(ctx, Connection{
		ID: id.New("con"), OrgID: "org_a", Kind: "slack",
		Name: "Slack", Config: map[string]string{"x": "y"}, Secrets: []byte("sealed"), CreatedBy: "ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBinding(ctx, Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID,
		Target: "C1", Mode: ModeChat, CreatedBy: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBinding(ctx, Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID,
		Target: "C1", Mode: ModeChat, CreatedBy: "ada",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate binding: %v", err)
	}
	w, _ := st.CreateBinding(ctx, Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID,
		Target: "F1", Mode: ModeWatch, CreatedBy: "ada",
	})
	// A repository: the mode code connections use.
	if _, err := st.CreateBinding(ctx, Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID,
		Target: "acme/web", Mode: ModeRepo, CreatedBy: "ada",
	}); err != nil {
		t.Fatalf("repo binding: %v", err)
	}
	// The modes API connections use.
	for _, mode := range []string{ModeRead, ModeWrite} {
		if _, err := st.CreateBinding(ctx, Binding{
			ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID,
			Target: "ACME", Mode: mode, CreatedBy: "ada",
		}); err != nil {
			t.Fatalf("%s binding: %v", mode, err)
		}
	}
	chats, err := st.ChatBindings(ctx, c.ID)
	if err != nil || len(chats) != 1 || chats[0].AgentName != "Scout" || chats[0].Kind != "slack" {
		t.Fatalf("ChatBindings = %+v, %v", chats, err)
	}
	if err := st.SetWatchCursor(ctx, w.ID, "page-2"); err != nil {
		t.Fatal(err)
	}
	watches, _ := st.Watches(ctx)
	if len(watches) != 1 || watches[0].Cursor != "page-2" || watches[0].CheckedAt == nil {
		t.Fatalf("Watches = %+v", watches)
	}
	// A watch skips the agent's own write, until someone changes the file.
	written := time.Date(2026, 3, 1, 9, 30, 0, 123456000, time.UTC)
	if err := st.RecordWrite(ctx, a.ID, c.ID, "file-1", written); err != nil {
		t.Fatal(err)
	}
	if own, err := st.Wrote(ctx, a.ID, c.ID, "file-1", written); err != nil || !own {
		t.Fatalf("Wrote = %v, %v: want the agent's own write", own, err)
	}
	if own, _ := st.Wrote(ctx, a.ID, c.ID, "file-1", written.Add(time.Minute)); own {
		t.Fatal("a later change counted as the agent's own")
	}
	if err := st.PruneWrites(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if own, _ := st.Wrote(ctx, a.ID, c.ID, "file-1", written); own {
		t.Fatal("a pruned write is still remembered")
	}
	counts, _ := st.BindingCounts(ctx, "org_a")
	if counts[c.ID] != 5 {
		t.Fatalf("BindingCounts = %v", counts)
	}
	// A stopped agent listens to nothing.
	a.Enabled = false
	if _, err := st.UpdateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if chats, _ := st.ChatBindings(ctx, c.ID); len(chats) != 0 {
		t.Fatalf("a stopped agent still listens: %v", chats)
	}
}

func TestTryLock(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	release, ok, err := st.TryLock(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v", ok, err)
	}
	if _, ok, _ := st.TryLock(ctx, 42); ok {
		t.Fatal("locked twice")
	}
	release()
	release2, ok, _ := st.TryLock(ctx, 42)
	if !ok {
		t.Fatal("not free after release")
	}
	release2()
}

func TestAgentIcon(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	if a.Icon != "" {
		t.Fatalf("a new agent has icon %q", a.Icon)
	}
	a.Icon = "sparkles"
	if _, err := st.UpdateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Task(ctx, task.ID)
	if got.AgentIcon != "sparkles" {
		t.Fatalf("the task shows icon %q", got.AgentIcon)
	}
}
