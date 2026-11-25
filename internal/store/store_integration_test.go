package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
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
	if _, err := st.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
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

func TestMigrateRefusesANewerDatabase(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ('9999_future.sql')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); !errors.Is(err, ErrNewerSchema) || !strings.Contains(err.Error(), "9999_future.sql") {
		t.Fatalf("Migrate = %v", err)
	}
}

func TestMigrateKnowsRenamedMigrations(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx,
		`UPDATE schema_migrations SET version = '0002_retries.sql' WHERE version = '0002_v0.2.0.sql'`,
	); err != nil {
		t.Fatal(err)
	}
	if applied, err := st.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("Migrate = %v, %v", applied, err)
	}
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
	// It waits before it is listed again, so other results go first.
	if undelivered, err := st.Undelivered(ctx); err != nil || len(undelivered) != 0 {
		t.Fatalf("Undelivered right after handing back = %d, %v", len(undelivered), err)
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

// An organisation has started once it has had a task, and stays started
// after its tasks are pruned.
func TestStarted(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	now := time.Now()
	a := newAgent(t, st, "org_a", "Scout")
	if started, err := st.Started(ctx, "org_a", now); started || err != nil {
		t.Fatalf("with no task: %v, %v", started, err)
	}
	if _, err := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if started, err := st.Started(ctx, "org_a", now); !started || err != nil {
		t.Fatalf("with a task: %v, %v", started, err)
	}
	if started, _ := st.Started(ctx, "org_b", now); started {
		t.Fatal("another organisation's task counted")
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM tasks`); err != nil {
		t.Fatal(err)
	}
	if started, _ := st.Started(ctx, "org_a", now.Add(TaskRetention+time.Hour)); !started {
		t.Fatal("an agent older than the retention did not count")
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
	// A task that cannot be stored leaves the run unclaimed, to try again.
	broken := &Task{ID: id.New("task"), OrgID: "org_a", AgentID: "agt_gone", Source: "schedule", Prompt: "p"}
	if won, err := st.ClaimRun(ctx, sc.ID, *list[0].NextRunAt, &next, broken); won || err == nil {
		t.Fatalf("claim with a broken task = %v, %v", won, err)
	}
	if got, _ := st.Schedule(ctx, sc.ID); got.NextRunAt == nil || !got.NextRunAt.Equal(*list[0].NextRunAt) {
		t.Fatalf("a failed claim moved the run to %v", got.NextRunAt)
	}
	run := &Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "schedule", Prompt: "p"}
	if won, err := st.ClaimRun(ctx, sc.ID, *list[0].NextRunAt, &next, run); !won || err != nil {
		t.Fatalf("first claim = %v, %v", won, err)
	}
	if _, err := st.Task(ctx, run.ID); err != nil {
		t.Fatalf("the claimed run's task: %v", err)
	}
	if won, _ := st.ClaimRun(ctx, sc.ID, *list[0].NextRunAt, &next, nil); won {
		t.Fatal("claimed twice")
	}
	// One that never runs again is switched off.
	if won, _ := st.ClaimRun(ctx, sc.ID, next, nil, nil); !won {
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
	b, err := st.CreateSquad(ctx, Squad{ID: id.New("sqd"), OrgID: "org_a", Name: "Ops", CreatedBy: "ada"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := st.CreateCard(ctx, Card{ID: id.New("card"), SquadID: b.ID, Title: "one", Status: CardTodo, AgentID: &a.ID, CreatedBy: "ada"})
	c2, _ := st.CreateCard(ctx, Card{ID: id.New("card"), SquadID: b.ID, Title: "two", Status: CardTodo, CreatedBy: "ada"})
	if c2.Position <= c1.Position {
		t.Fatalf("new cards go last: %v then %v", c1.Position, c2.Position)
	}
	if c1.AgentName != "Scout" || c1.OrgID != "org_a" {
		t.Fatalf("card joins: %+v", c1)
	}
	// Only a member's card is picked up.
	if pick, err := st.PickupCards(ctx); err != nil || len(pick) != 0 {
		t.Fatalf("PickupCards of a non-member = %v, %v", pick, err)
	}
	if _, err := st.UpdateSquad(ctx, b, []string{a.ID}); err != nil {
		t.Fatal(err)
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
	if got, _ := st.Card(ctx, c1.ID); got.Comments != 1 || got.RequestedByID != "" {
		t.Fatalf("after a comment that gives no work: %+v", got)
	}
	// A comment that gives work makes its writer the one the task runs for.
	if _, err := st.AddComment(ctx, Comment{
		ID: id.New("cmt"), CardID: c1.ID, Author: "Cy", Body: "also this",
		RequestedBy: "cy@example.ch", RequestedByID: "usr_cy",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Card(ctx, c1.ID); got.Comments != 2 || got.RequestedByID != "usr_cy" {
		t.Fatalf("after a comment that gives work: %+v", got)
	}
	// Whoever last gave the work is kept apart from who created the card.
	got.RequestedBy, got.RequestedByID = "bob@example.ch", "usr_bob"
	if _, err := st.UpdateCard(ctx, got, got); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Card(ctx, c1.ID); got.RequestedByID != "usr_bob" || got.CreatedBy != "ada" {
		t.Fatalf("after giving work: %+v", got)
	}
	cards, err := st.SquadCards(ctx, b.ID)
	if err != nil || len(cards) != 2 || cards[0].ID != c2.ID {
		t.Fatalf("board order: %v, %v", cards, err)
	}
	// A deleted agent's card reads as unassigned, so it can be edited.
	if err := st.DeleteAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Card(ctx, c1.ID); got.AgentID != nil || got.AgentName != "" {
		t.Fatalf("a deleted agent's card: %+v", got)
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
	if _, err := st.SetEnabled(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if chats, _ := st.ChatBindings(ctx, c.ID); len(chats) != 0 {
		t.Fatalf("a stopped agent still listens: %v", chats)
	}
	// But it still holds its conversation, and one listening everywhere
	// holds every conversation.
	if _, err := st.CreateBinding(ctx, Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID,
		Target: AnyConversation, Mode: ModeChat, CreatedBy: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	holders, err := st.ChatHolders(ctx, c.ID, "C1")
	if err != nil || len(holders) != 2 || holders[0].AgentOwnerID != "usr_1" {
		t.Fatalf("ChatHolders(C1) = %+v, %v", holders, err)
	}
	if holders, _ := st.ChatHolders(ctx, c.ID, "C2"); len(holders) != 1 || holders[0].Target != AnyConversation {
		t.Fatalf("ChatHolders(C2) = %+v", holders)
	}
	// Joining every conversation joins C1 too, so its agent holds it.
	if holders, _ := st.ChatHolders(ctx, c.ID, AnyConversation); len(holders) != 2 {
		t.Fatalf("ChatHolders(*) = %+v", holders)
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

func newWorkspace(t *testing.T, st *Store, org, owner, name string) Agent {
	t.Helper()
	a, err := st.CreateAgent(context.Background(), Agent{
		ID: id.New("wks"), Kind: KindWorkspace, Experience: ExperienceTerminal, OrgID: org,
		OwnerID: owner, OwnerEmail: owner + "@example.ch", Name: name, Model: "keera-speed",
		MaxSteps: 10, Access: AccessOwner, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWorkspacesAreNotAgents(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	agent := newAgent(t, st, "org_a", "dev")
	// A workspace's name is its owner's: it can share an agent's, and
	// another person's workspace's.
	mine := newWorkspace(t, st, "org_a", "usr_1", "dev")
	newWorkspace(t, st, "org_a", "usr_2", "dev")
	if _, err := st.CreateAgent(ctx, Agent{
		ID: id.New("wks"), Kind: KindWorkspace, Experience: ExperienceVSCode, OrgID: "org_a",
		OwnerID: "usr_1", OwnerEmail: "x", Name: "DEV", Model: "m", Access: AccessOwner,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second workspace of the same name for one person: %v", err)
	}
	// The schema keeps the experience to workspaces.
	if _, err := st.CreateAgent(ctx, Agent{
		ID: id.New("wks"), Kind: KindWorkspace, OrgID: "org_a", OwnerID: "usr_3", OwnerEmail: "x",
		Name: "none", Model: "m", Access: AccessOwner,
	}); err == nil {
		t.Fatal("a workspace without an experience was stored")
	}

	agents, err := st.Agents(ctx, "org_a")
	if err != nil || len(agents) != 1 || agents[0].ID != agent.ID {
		t.Fatalf("Agents = %v, %v", agents, err)
	}
	ws, err := st.Workspaces(ctx, "org_a", "usr_1")
	if err != nil || len(ws) != 1 || ws[0].ID != mine.ID || ws[0].Experience != ExperienceTerminal {
		t.Fatalf("Workspaces(usr_1) = %v, %v", ws, err)
	}
	if all, _ := st.Workspaces(ctx, "org_a", ""); len(all) != 2 {
		t.Fatalf("every workspace of the organisation: %d", len(all))
	}
	if got, err := st.WorkspaceByName(ctx, "org_a", "usr_1", "DEV"); err != nil || got.ID != mine.ID {
		t.Fatalf("WorkspaceByName = %v, %v", got.ID, err)
	}
	if all, _ := st.Machines(ctx); len(all) != 3 {
		t.Fatalf("Machines lists %d, want every agent and workspace", len(all))
	}

	mine.Experience = ExperienceVSCode
	if out, err := st.UpdateAgent(ctx, mine); err != nil || out.Experience != ExperienceVSCode {
		t.Fatalf("changing the experience: %v, %v", out.Experience, err)
	}
}

func TestSSHKeys(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	k, err := st.AddSSHKey(ctx, SSHKey{
		ID: id.New("ssh"), OrgID: "org_a", UserID: "usr_1", Name: "laptop",
		PublicKey: "ssh-ed25519 AAAA laptop", Fingerprint: "SHA256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSSHKey(ctx, SSHKey{
		ID: id.New("ssh"), OrgID: "org_a", UserID: "usr_1", PublicKey: "x", Fingerprint: "SHA256:abc",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("the same key twice: %v", err)
	}
	if keys, _ := st.SSHKeys(ctx, "org_a", "usr_2"); len(keys) != 0 {
		t.Fatalf("another person sees %v", keys)
	}
	if err := st.DeleteSSHKey(ctx, "org_a", "usr_2", k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person deleted a key: %v", err)
	}
	if err := st.DeleteSSHKey(ctx, "org_a", "usr_1", k.ID); err != nil {
		t.Fatal(err)
	}
	if keys, _ := st.SSHKeys(ctx, "org_a", "usr_1"); len(keys) != 0 {
		t.Fatalf("after deleting: %v", keys)
	}
}

func TestGauges(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	b := newAgent(t, st, "org_b", "Other")
	gone := newAgent(t, st, "org_a", "Gone")
	if err := st.SetAgentState(ctx, a.ID, AgentRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	g, err := st.Gauges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.Queued != 0 || g.Running != 0 || g.OldestQueued != nil || g.Undelivered != 0 {
		t.Fatalf("an empty queue: %+v", g)
	}
	total := 0
	for _, n := range g.Machines {
		total += n
	}
	if total != 2 || g.Machines[[2]string{KindAgent, AgentRunning}] != 1 {
		t.Fatalf("machines, without the deleted one: %v", g.Machines)
	}

	newTask := func(agentID string) Task {
		task, err := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: agentID, Source: "manual", Prompt: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	first := newTask(a.ID)
	newTask(a.ID)
	newTask(b.ID)
	if _, err := st.ClaimTask(ctx, a.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if g, err = st.Gauges(ctx); err != nil {
		t.Fatal(err)
	}
	if g.Queued != 2 || g.Running != 1 || g.OldestQueued == nil || g.OldestQueued.Before(first.CreatedAt) {
		t.Fatalf("one running, two queued: %+v", g)
	}
	if _, err := st.FinishTask(ctx, first.ID, TaskSucceeded, "done", ""); err != nil {
		t.Fatal(err)
	}
	if g, _ = st.Gauges(ctx); g.Running != 0 || g.Undelivered != 1 {
		t.Fatalf("one finished, not delivered: %+v", g)
	}
	if _, err := st.MarkDelivered(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ = st.Gauges(ctx); g.Undelivered != 0 {
		t.Fatalf("delivered: %+v", g)
	}
}

func TestAccounts(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	c, err := st.CreateConnection(ctx, Connection{
		ID: id.New("con"), OrgID: "org_a", Kind: "gdocs", Name: "Docs", Config: map[string]string{},
		PerUser: true, CreatedBy: "ada",
	})
	if err != nil || !c.PerUser {
		t.Fatalf("CreateConnection = %+v, %v", c, err)
	}
	// Changing a connector keeps whom it acts as.
	c.PerUser = false
	if c, err = st.UpdateConnection(ctx, c); err != nil || !c.PerUser {
		t.Fatalf("UpdateConnection = %+v, %v", c, err)
	}
	if _, err := st.Account(ctx, c.ID, "usr_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Account before signing in: %v", err)
	}
	for _, a := range []Account{
		{ConnectionID: c.ID, UserID: "usr_1", Account: "old@acme.ch", Token: []byte("t0")},
		{ConnectionID: c.ID, UserID: "usr_1", Account: "ada@acme.ch", Token: []byte("t1")},
		{ConnectionID: c.ID, UserID: "usr_2", Account: "bob@acme.ch", Token: []byte("t2")},
	} {
		if err := st.SaveAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	a, err := st.Account(ctx, c.ID, "usr_1")
	if err != nil || a.Account != "ada@acme.ch" || string(a.Token) != "t1" || a.OrgID != "org_a" {
		t.Fatalf("Account = %+v, %v", a, err)
	}
	rotate := func(from, to string) {
		t.Helper()
		if err := st.RotateAccountToken(ctx, c.ID, "usr_1", func(token []byte) ([]byte, error) {
			if string(token) != from {
				t.Errorf("rotating from %q, want %q", token, from)
			}
			if to == "" {
				return nil, nil
			}
			return []byte(to), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rotate("t1", "t3")
	rotate("t3", "") // a service that does not rotate keeps the token
	if a, _ := st.Account(ctx, c.ID, "usr_1"); string(a.Token) != "t3" {
		t.Errorf("rotated token = %q", a.Token)
	}
	// One that fails keeps it too.
	if err := st.RotateAccountToken(ctx, c.ID, "usr_1", func([]byte) ([]byte, error) {
		return []byte("t4"), errors.New("refused")
	}); err == nil {
		t.Fatal("a failed refresh did not fail")
	}
	if a, _ := st.Account(ctx, c.ID, "usr_1"); string(a.Token) != "t3" {
		t.Errorf("token after a failed refresh = %q", a.Token)
	}
	if err := st.RotateAccountToken(ctx, c.ID, "usr_9", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("RotateAccountToken for nobody: %v", err)
	}
	if mine, _ := st.UserAccounts(ctx, "org_a", "usr_1"); len(mine) != 1 || mine[0].ConnectionID != c.ID {
		t.Errorf("UserAccounts = %+v", mine)
	}
	if other, _ := st.UserAccounts(ctx, "org_b", "usr_1"); len(other) != 0 {
		t.Errorf("another organisation's accounts: %+v", other)
	}
	if holders, _ := st.AccountHolders(ctx); len(holders) != 2 {
		t.Errorf("AccountHolders = %+v", holders)
	}
	if n, err := st.DeleteUserAccounts(ctx, "org_a", "usr_2"); err != nil || n != 1 {
		t.Errorf("DeleteUserAccounts = %d, %v", n, err)
	}
	if err := st.DeleteAccount(ctx, c.ID, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAccount(ctx, c.ID, "usr_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteAccount: %v", err)
	}
	// Deleting the connector forgets everyone's sign-in.
	_ = st.SaveAccount(ctx, Account{ConnectionID: c.ID, UserID: "usr_1", Token: []byte("t")})
	if err := st.DeleteConnection(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if holders, _ := st.AccountHolders(ctx); len(holders) != 0 {
		t.Errorf("accounts outlived their connector: %+v", holders)
	}
}

func TestSquadMembers(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	scout := newAgent(t, st, "org_a", "Scout")
	writer := newAgent(t, st, "org_a", "Writer")
	q, err := st.CreateSquad(ctx, Squad{ID: id.New("sqd"), OrgID: "org_a", Name: "Sales", Goal: "Sell.", CreatedBy: "ada"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	q.LeadID = &writer.ID
	if _, err := st.UpdateSquad(ctx, q, []string{scout.ID, writer.ID}); err != nil {
		t.Fatal(err)
	}
	other, _ := st.CreateSquad(ctx, Squad{ID: id.New("sqd"), OrgID: "org_a", Name: "Support", CreatedBy: "ada"}, nil)
	if _, err := st.UpdateSquad(ctx, other, []string{writer.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Squad(ctx, q.ID)
	if err != nil || len(got.Members) != 2 || got.Members[0].Name != "Scout" || got.LeadID == nil || *got.LeadID != writer.ID {
		t.Fatalf("Squad = %+v, %v", got, err)
	}
	// An agent can be in several squads.
	if mine, _ := st.AgentSquads(ctx, writer.ID); len(mine) != 2 {
		t.Fatalf("Writer's squads: %+v", mine)
	}

	card := func(status string) Card {
		c, err := st.CreateCard(ctx, Card{ID: id.New("card"), SquadID: q.ID, Title: status, Status: status, AgentID: &writer.ID, CreatedBy: "ada"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	waiting, working := card(CardTodo), card(CardInProgress)
	// Writer leaves: no longer the lead, and its cards not in progress lose
	// their agent.
	if _, err := st.UpdateSquad(ctx, got, []string{scout.ID}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Squad(ctx, q.ID)
	if len(got.Members) != 1 || got.LeadID != nil {
		t.Fatalf("after Writer left: %+v", got)
	}
	if c, _ := st.Card(ctx, waiting.ID); c.AgentID != nil {
		t.Fatal("the card in To do kept its agent")
	}
	if c, _ := st.Card(ctx, working.ID); c.AgentID == nil {
		t.Fatal("the card in progress lost its agent")
	}
	if mine, _ := st.AgentSquads(ctx, writer.ID); len(mine) != 1 || mine[0].Name != "Support" {
		t.Fatalf("Writer's squads: %+v", mine)
	}
}

func TestWaitingCardsWakeWhenWhatTheyHandedOutIsFinished(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	q, _ := st.CreateSquad(ctx, Squad{ID: id.New("sqd"), OrgID: "org_a", Name: "Ops", CreatedBy: "ada"}, nil)
	task, _ := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "board", Prompt: "x"})
	parent, _ := st.CreateCard(ctx, Card{ID: id.New("card"), SquadID: q.ID, Title: "Plan", Status: CardInProgress, AgentID: &a.ID, CreatedBy: "ada"})
	if err := st.MoveCard(ctx, parent.ID, CardInProgress, &task.ID); err != nil {
		t.Fatal(err)
	}
	child, _ := st.CreateCard(ctx, Card{ID: id.New("card"), SquadID: q.ID, Title: "Part", Status: CardTodo, CreatedBy: "Scout", ParentID: &parent.ID, Depth: 1})
	if err := st.WaitCard(ctx, parent.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if woken, err := st.WakeCards(ctx); err != nil || woken != 0 {
		t.Fatalf("woke %v, %v while a part is open", woken, err)
	}
	if err := st.MoveCard(ctx, child.ID, CardReview, nil); err != nil {
		t.Fatal(err)
	}
	if woken, err := st.WakeCards(ctx); err != nil || woken != 1 {
		t.Fatalf("woke %v, %v", woken, err)
	}
	if got, _ := st.Card(ctx, parent.ID); got.Status != CardTodo || got.Waiting {
		t.Fatalf("woken: %+v", got)
	}
	// A person moving a waiting card ends the wait.
	if err := st.MoveCard(ctx, parent.ID, CardInProgress, &task.ID); err != nil {
		t.Fatal(err)
	}
	_ = st.WaitCard(ctx, parent.ID, task.ID)
	before, _ := st.Card(ctx, parent.ID)
	after := before
	after.Status = CardDone
	if got, err := st.UpdateCard(ctx, before, after); err != nil || got.Waiting {
		t.Fatalf("after a move: %+v, %v", got, err)
	}
}

func TestOrgSummaries(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	newAgent(t, st, "org_a", "Idle")
	b := newAgent(t, st, "org_b", "Other")
	if err := st.SetAgentState(ctx, a.ID, AgentRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentState(ctx, b.ID, AgentFailed, ""); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := st.CreateTask(ctx, Task{ID: id.New("task"), OrgID: "org_a", AgentID: a.ID, Source: "manual", Prompt: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.OrgSummaries(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if o := got["org_a"]; o.Agents != 2 || o.AgentsRunning != 1 || o.AgentsFailed != 0 || o.Tasks != 3 || o.LastTaskAt == nil {
		t.Fatalf("org_a: %+v", o)
	}
	if o := got["org_b"]; o.Agents != 1 || o.AgentsFailed != 1 || o.Tasks != 0 || o.LastTaskAt != nil {
		t.Fatalf("org_b: %+v", o)
	}

	// Tasks before the window are not counted; machines always are.
	if got, err = st.OrgSummaries(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if o := got["org_a"]; o.Tasks != 0 || o.Agents != 2 {
		t.Fatalf("org_a, later: %+v", o)
	}
}
