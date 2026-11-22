package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
	"github.com/bespinian/keera-agents/internal/store/storetest"
)

// fixture is a manager on a real store, with a fake driver and gateway.
type fixture struct {
	st     *store.Store
	db     *pgx.Conn
	driver *fakeDriver
	gw     *fakeGateway
	m      *Manager
}

// newFixture connects to KEERA_AGENTS_TEST_DATABASE_URL on an empty schema,
// or skips.
func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	st, dsn := storetest.Open(t)
	ctx := context.Background()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	gw := &fakeGateway{}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	box, _ := secret.New("0123456789abcdef")
	driver := newFakeDriver()
	if opts.Image == "" {
		opts.Image = "agent-img"
	}
	opts.Log = slog.New(slog.DiscardHandler)
	return &fixture{
		st: st, db: db, driver: driver, gw: gw,
		m: New(st, gateway.New(srv.URL, testServiceKey), driver, box, opts),
	}
}

// agent makes an enabled agent of org_a owned by usr_ada, changed by edit.
func (f *fixture) agent(t *testing.T, edit ...func(*store.Agent)) store.Agent {
	t.Helper()
	agentID := id.New("agt")
	a := store.Agent{
		ID: agentID, OrgID: "org_a", OwnerID: "usr_ada", OwnerEmail: "ada@example.ch",
		Name: "Agent " + agentID, Model: "keera-speed", MaxSteps: 10, Access: store.AccessOrg, Enabled: true,
	}
	for _, e := range edit {
		e(&a)
	}
	a, err := f.st.CreateAgent(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func workspace(a *store.Agent) { a.Kind, a.Experience = store.KindWorkspace, store.ExperienceTerminal }

func (f *fixture) queue(t *testing.T, agentID string) store.Task {
	t.Helper()
	task, err := f.st.CreateTask(context.Background(), store.Task{
		ID: id.New("task"), OrgID: "org_a", AgentID: agentID, Source: "manual", Prompt: "do it",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *fixture) load(t *testing.T, agentID string) store.Agent {
	t.Helper()
	a, err := f.st.Agent(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) taskState(t *testing.T, taskID string) (state, reason string) {
	t.Helper()
	task, err := f.st.Task(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task.State, task.Error
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// queuedLongAgo moves an agent's tasks back to before maxStartWait.
func (f *fixture) queuedLongAgo(t *testing.T, agentID string) {
	f.exec(t, `UPDATE tasks SET created_at = $2 WHERE agent_id = $1`, agentID, time.Now().Add(-maxStartWait-time.Minute))
}

func (f *fixture) setState(t *testing.T, agentID, state string) {
	t.Helper()
	if err := f.st.SetAgentState(context.Background(), agentID, state, ""); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileWakesABusyAgentWhoseContainerIsGone(t *testing.T) {
	f := newFixture(t, Options{RuntimeURL: "http://keera-agents:8080"})
	ctx := context.Background()
	a := f.agent(t)
	f.queue(t, a.ID)

	f.m.Reconcile(ctx)
	if n := f.driver.count("create " + a.ID); n != 1 {
		t.Fatalf("created %d times, want once", n)
	}
	got := f.load(t, a.ID)
	if got.State != store.AgentStarting || got.LastActiveAt == nil {
		t.Fatalf("state = %q, last active = %v", got.State, got.LastActiveAt)
	}
	spec := f.driver.spec(a.ID)
	if spec.Image != "agent-img" || spec.Env[protocol.EnvRuntimeURL] != "http://keera-agents:8080" ||
		spec.Env[protocol.EnvAgentID] != a.ID || spec.Class != runtime.DefaultClass {
		t.Fatalf("spec = %+v", spec)
	}
	if !bytes.Equal(got.RuntimeTokenHash, TokenHash(spec.Env[protocol.EnvRuntimeToken])) {
		t.Fatal("the stored token is not the container's")
	}
	if key, err := f.m.OpenKey(got); err != nil || got.GatewayKeyID != "key_1" || key != "sk-1" {
		t.Fatalf("key %q = %q, %v", got.GatewayKeyID, key, err)
	}

	// While it starts, the next pass leaves it be.
	f.m.Reconcile(ctx)
	if n := f.driver.count("create " + a.ID); n != 1 {
		t.Fatalf("created %d times while starting", n)
	}
}

func TestReconcileResumesASuspendedBusyAgent(t *testing.T) {
	f := newFixture(t, Options{})
	a := f.agent(t)
	f.driver.set(a.ID, runtime.Status{State: runtime.Suspended})
	f.setState(t, a.ID, store.AgentSuspended)
	f.queue(t, a.ID)

	f.m.Reconcile(context.Background())
	if f.driver.count("resume "+a.ID) != 1 || f.driver.count("create "+a.ID) != 0 {
		t.Fatalf("calls = %v, want one resume", f.driver.calls)
	}
	if got := f.load(t, a.ID); got.State != store.AgentStarting {
		t.Fatalf("state = %q", got.State)
	}
}

func TestReconcileStartsNothingForAnAgentWithoutWork(t *testing.T) {
	f := newFixture(t, Options{})
	idle := f.agent(t)
	off := f.agent(t, func(a *store.Agent) { a.Enabled = false })
	f.queue(t, off.ID)

	f.m.Reconcile(context.Background())
	if len(f.driver.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.driver.calls)
	}
	if mints, _ := f.gw.counts(); mints != 0 {
		t.Fatalf("minted %d keys", mints)
	}
	if got := f.load(t, idle.ID); got.State != store.AgentStopped {
		t.Fatalf("state = %q", got.State)
	}
}

func TestAFailedStartIsRetriedLessOftenUntilItsTasksFail(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.driver.createErr = errors.New("no room on any node")
	a := f.agent(t)
	task := f.queue(t, a.ID)
	creates := func() int { return f.driver.count("create " + a.ID) }

	f.m.Reconcile(ctx)
	got := f.load(t, a.ID)
	if creates() != 1 || got.State != store.AgentFailed || !strings.Contains(got.StateDetail, "no room on any node") {
		t.Fatalf("after the first try: %d creates, state %q %q", creates(), got.State, got.StateDetail)
	}
	// The first retry is due at once, the next only after a wait.
	f.m.Reconcile(ctx)
	f.m.Reconcile(ctx)
	if creates() != 2 {
		t.Fatalf("%d creates, want 2", creates())
	}
	f.m.retryMu.Lock()
	r := f.m.retries[a.ID]
	r.at = time.Now().Add(-time.Second)
	f.m.retries[a.ID] = r
	f.m.retryMu.Unlock()
	f.m.Reconcile(ctx)
	if creates() != 3 {
		t.Fatalf("%d creates once the wait was over, want 3", creates())
	}
	if state, _ := f.taskState(t, task.ID); state != store.TaskQueued {
		t.Fatalf("task %s before the hour was over", state)
	}
	if got := f.load(t, a.ID); got.State != store.AgentFailed || !strings.Contains(got.StateDetail, "no room") {
		t.Fatalf("the reason it failed was lost: %q %q", got.State, got.StateDetail)
	}

	// After maxStartWait the task fails instead of another try.
	f.queuedLongAgo(t, a.ID)
	f.m.retryMu.Lock()
	f.m.retries[a.ID] = retry{}
	f.m.retryMu.Unlock()
	f.m.Reconcile(ctx)
	if creates() != 3 {
		t.Fatalf("tried again after giving up: %d creates", creates())
	}
	if state, reason := f.taskState(t, task.ID); state != store.TaskFailed || !strings.Contains(reason, "could not start") {
		t.Fatalf("task = %s %q, want failed", state, reason)
	}
	f.m.Reconcile(ctx)
	if creates() != 3 {
		t.Fatalf("an agent with no work left was started: %d creates", creates())
	}
}

func TestAContainerStuckPendingFailsItsOldTasks(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	f.driver.set(a.ID, runtime.Status{State: runtime.Pending, Detail: "no node has room"})
	old := f.queue(t, a.ID)
	f.queuedLongAgo(t, a.ID)

	// The first sight starts the clock, even for a task queued long ago.
	f.m.Reconcile(ctx)
	if state, _ := f.taskState(t, old.ID); state != store.TaskQueued {
		t.Fatalf("task %s at first sight", state)
	}
	if got := f.load(t, a.ID); got.State != store.AgentStarting || got.StateDetail != "no node has room" {
		t.Fatalf("state = %q %q", got.State, got.StateDetail)
	}

	f.m.retryMu.Lock()
	f.m.pendingSince[a.ID] = time.Now().Add(-maxStartWait - time.Minute)
	f.m.retryMu.Unlock()
	fresh := f.queue(t, a.ID)
	f.m.Reconcile(ctx)
	if state, reason := f.taskState(t, old.ID); state != store.TaskFailed || !strings.Contains(reason, "could not start") {
		t.Fatalf("old task = %s %q, want failed", state, reason)
	}
	if state, _ := f.taskState(t, fresh.ID); state != store.TaskQueued {
		t.Fatalf("a task queued just now is %s", state)
	}
	if n := f.driver.count("create " + a.ID); n != 0 {
		t.Fatalf("a pending container was replaced %d times", n)
	}
}

func TestReconcileSuspendsADisabledAgentsContainer(t *testing.T) {
	for _, c := range []struct {
		state   runtime.State
		suspend bool
	}{
		{runtime.Running, true},
		{runtime.Pending, true},
		{runtime.Suspended, false},
	} {
		t.Run(string(c.state), func(t *testing.T) {
			f := newFixture(t, Options{})
			a := f.agent(t, func(a *store.Agent) { a.Enabled = false })
			f.driver.set(a.ID, runtime.Status{State: c.state})
			f.setState(t, a.ID, store.AgentRunning)

			f.m.Reconcile(context.Background())
			if got := f.driver.count("suspend "+a.ID) == 1; got != c.suspend {
				t.Fatalf("suspended = %v, want %v", got, c.suspend)
			}
			if got := f.load(t, a.ID); got.State != store.AgentStopped {
				t.Fatalf("state = %q, want stopped", got.State)
			}
		})
	}
}

// Reconcile's list can be older than a start: it must not suspend the
// machine the start just made, nor mark the agent stopped.
func TestReconcileLeavesAnAgentStartedSinceItsList(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	listed := f.agent(t, func(a *store.Agent) { a.Enabled = false })
	if _, err := f.st.SetEnabled(ctx, listed.ID, true); err != nil {
		t.Fatal(err)
	}
	f.driver.set(listed.ID, runtime.Status{State: runtime.Pending})
	f.setState(t, listed.ID, store.AgentStarting)

	f.m.reconcileOne(ctx, listed)
	if n := f.driver.count("suspend " + listed.ID); n != 0 {
		t.Fatalf("suspended %d times", n)
	}
	if got := f.load(t, listed.ID); got.State != store.AgentStarting {
		t.Fatalf("state = %q, want starting", got.State)
	}
}

func TestReconcileSuspendsAnAgentIdleForItsLimit(t *testing.T) {
	for _, c := range []struct {
		name    string
		kind    string
		idleFor time.Duration // zero: never active
		busy    bool
		suspend bool
	}{
		{"idle past its limit", store.KindAgent, 2 * time.Minute, false, true},
		{"busy", store.KindAgent, 2 * time.Minute, true, false},
		{"active just now", store.KindAgent, 10 * time.Second, false, false},
		{"never active", store.KindAgent, 0, false, false},
		{"a workspace has its own limit", store.KindWorkspace, 2 * time.Minute, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, Options{IdleSuspend: time.Minute, WorkspaceIdleSuspend: time.Hour})
			a := f.agent(t, func(a *store.Agent) {
				if c.kind == store.KindWorkspace {
					workspace(a)
				}
			})
			f.driver.set(a.ID, runtime.Status{State: runtime.Running})
			f.setState(t, a.ID, store.AgentRunning)
			if c.idleFor > 0 {
				f.exec(t, `UPDATE agents SET last_active_at = $2 WHERE id = $1`, a.ID, time.Now().Add(-c.idleFor))
			}
			if c.busy {
				f.queue(t, a.ID)
			}

			f.m.Reconcile(context.Background())
			if got := f.driver.count("suspend "+a.ID) == 1; got != c.suspend {
				t.Fatalf("suspended = %v, want %v", got, c.suspend)
			}
			want := store.AgentRunning
			if c.suspend {
				want = store.AgentSuspended
			}
			if got := f.load(t, a.ID); got.State != want {
				t.Fatalf("state = %q, want %q", got.State, want)
			}
		})
	}
}

func TestATaskQueuedWhileSuspendingWakesTheAgent(t *testing.T) {
	f := newFixture(t, Options{IdleSuspend: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := f.agent(t)
	f.driver.set(a.ID, runtime.Status{State: runtime.Running})
	f.setState(t, a.ID, store.AgentRunning)
	f.exec(t, `UPDATE agents SET last_active_at = $2 WHERE id = $1`, a.ID, time.Now().Add(-time.Hour))
	var task store.Task
	f.driver.onSuspend = func(agentID string) {
		// The suspend holds the agent's row; queueing must not wait for it.
		var err error
		task, err = f.st.CreateTask(ctx, store.Task{
			ID: id.New("task"), OrgID: "org_a", AgentID: agentID, Source: "manual", Prompt: "late",
		})
		if err != nil {
			t.Errorf("queueing during a suspend: %v", err)
		}
	}

	f.m.Reconcile(ctx)
	if f.driver.count("suspend "+a.ID) != 1 || f.driver.count("resume "+a.ID) != 1 {
		t.Fatalf("calls = %v, want a suspend then a resume", f.driver.calls)
	}
	if got := f.load(t, a.ID); got.State != store.AgentStarting {
		t.Fatalf("state = %q, want starting", got.State)
	}
	if state, _ := f.taskState(t, task.ID); state != store.TaskQueued {
		t.Fatalf("the late task is %s", state)
	}
}

func TestReconcileRestartsAContainerWithAnOldToken(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.driver.createState = runtime.Running
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.driver.set(a.ID, runtime.Status{State: runtime.Running, Token: "kat_old"})

	f.m.Reconcile(ctx)
	if n := f.driver.count("create " + a.ID); n != 2 {
		t.Fatalf("created %d times, want a start and a restart", n)
	}
	got := f.load(t, a.ID)
	if !bytes.Equal(got.RuntimeTokenHash, TokenHash(f.driver.spec(a.ID).Env[protocol.EnvRuntimeToken])) {
		t.Fatal("the stored token is not the new container's")
	}
	f.m.Reconcile(ctx)
	if n := f.driver.count("create " + a.ID); n != 2 {
		t.Fatalf("restarted again with a current token: %d creates", n)
	}
}

func TestTheAgentsStateFollowsItsContainer(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		status runtime.Status
		before string
		want   string
	}{
		{"running", runtime.Status{State: runtime.Running}, store.AgentStarting, store.AgentRunning},
		{"pending", runtime.Status{State: runtime.Pending, Detail: "pulling the image"}, store.AgentStopped, store.AgentStarting},
		{"suspended", runtime.Status{State: runtime.Suspended}, store.AgentRunning, store.AgentSuspended},
		{"failed", runtime.Status{State: runtime.Failed, Detail: "it crashed"}, store.AgentRunning, store.AgentFailed},
		{"gone", runtime.Status{State: runtime.Gone}, store.AgentRunning, store.AgentStopped},
	} {
		a := f.agent(t)
		f.setState(t, a.ID, c.before)
		f.driver.set(a.ID, c.status)
		f.m.Reconcile(ctx)
		if got := f.load(t, a.ID); got.State != c.want || got.StateDetail != c.status.Detail {
			t.Errorf("%s: state = %q %q, want %q %q", c.name, got.State, got.StateDetail, c.want, c.status.Detail)
		}
	}

	// A failed start's reason stays while nothing runs.
	failed := f.agent(t)
	if err := f.st.SetAgentState(ctx, failed.ID, store.AgentFailed, "the image does not exist"); err != nil {
		t.Fatal(err)
	}
	f.m.Reconcile(ctx)
	if got := f.load(t, failed.ID); got.State != store.AgentFailed || got.StateDetail != "the image does not exist" {
		t.Fatalf("failed agent = %q %q", got.State, got.StateDetail)
	}

	// A driver that cannot answer changes nothing.
	f.driver.statusErr = errors.New("the cluster is down")
	f.setState(t, failed.ID, store.AgentRunning)
	f.m.Reconcile(ctx)
	if got := f.load(t, failed.ID); got.State != store.AgentRunning {
		t.Fatalf("state = %q after a driver error", got.State)
	}
}

func TestCheckOwnersStopsAgentsWhoseOwnerLeft(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.driver.createState = runtime.Running
	type started struct {
		agent store.Agent
		task  store.Task
	}
	byOwner := map[string]started{}
	for _, owner := range []string{"usr_ada", "usr_gone", "usr_off", "usr_flaky", "usr_proxy"} {
		a := f.agent(t, func(a *store.Agent) { a.OwnerID, a.OwnerEmail = owner, owner+"@example.ch" })
		if err := f.m.Start(ctx, a); err != nil {
			t.Fatal(err)
		}
		f.setState(t, a.ID, store.AgentRunning)
		byOwner[owner] = started{f.load(t, a.ID), f.queue(t, a.ID)}
	}

	f.m.CheckOwners(ctx)
	audit, err := f.st.AuditLog(ctx, "org_a", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for owner, s := range byOwner {
		left := owner == "usr_gone" || owner == "usr_off"
		got := f.load(t, s.agent.ID)
		state, reason := f.taskState(t, s.task.ID)
		audited := false
		for _, e := range audit {
			audited = audited || (e.Action == "agent.owner_left" && e.Target == s.agent.ID)
		}
		suspended := f.driver.count("suspend "+s.agent.ID) == 1
		revoked := f.gw.wasRevoked("org_a", s.agent.GatewayKeyID)
		if !left {
			// A gateway that fails, or a 404 that is not its own, proves nothing.
			if !got.Enabled || got.State != store.AgentRunning || got.GatewayKeyID == "" ||
				suspended || revoked || audited || state != store.TaskQueued {
				t.Errorf("%s: stopped although the owner may still be there: enabled %v, state %q, key %q, "+
					"suspended %v, revoked %v, audited %v, task %s",
					owner, got.Enabled, got.State, got.GatewayKeyID, suspended, revoked, audited, state)
			}
			continue
		}
		if got.Enabled || got.State != store.AgentStopped || !strings.Contains(got.StateDetail, owner+"@example.ch") {
			t.Errorf("%s: enabled %v, state %q %q", owner, got.Enabled, got.State, got.StateDetail)
		}
		if got.GatewayKeyID != "" || len(got.GatewayKey) != 0 || !revoked {
			t.Errorf("%s: key %q kept, revoked %v", owner, got.GatewayKeyID, revoked)
		}
		if !suspended || !audited || state != store.TaskCancelled || !strings.Contains(reason, "owner") {
			t.Errorf("%s: suspended %v, audited %v, task %s %q", owner, suspended, audited, state, reason)
		}
	}

	// Stopped agents with no key are not looked up again.
	_, before := f.gw.counts()
	f.m.CheckOwners(ctx)
	if _, after := f.gw.counts(); after-before != 3 {
		t.Fatalf("looked up %d owners, want the 3 still running", after-before)
	}
}

// An administrator who takes an agent over while its old owner is looked up
// keeps it running, with its key.
func TestCheckOwnersLeavesAnAgentTakenOverMeanwhile(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t, func(a *store.Agent) { a.OwnerID, a.OwnerEmail = "usr_gone", "gone@example.ch" })
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	a = f.load(t, a.ID)
	f.gw.onLookup = func(id string) {
		if id == "usr_gone" {
			if err := f.st.SetOwner(ctx, a.ID, "usr_admin", "admin@example.ch"); err != nil {
				t.Error(err)
			}
		}
	}
	f.m.CheckOwners(ctx)
	got := f.load(t, a.ID)
	if !got.Enabled || got.GatewayKeyID == "" || f.driver.count("suspend "+a.ID) != 0 {
		t.Fatalf("stopped after a take-over: enabled %v, key %q, calls %v", got.Enabled, got.GatewayKeyID, f.driver.calls)
	}
}

// A key the gateway would not replace at once, as after a budget change, is
// replaced in a later round.
func TestAStaleKeyIsReplacedLater(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	a = f.load(t, a.ID)
	if a.KeyStale() {
		t.Fatal("a new key is stale")
	}
	f.m.RekeyStale(ctx)
	if got := f.load(t, a.ID); got.GatewayKeyID != a.GatewayKeyID {
		t.Fatalf("a key that matches was replaced: %q", got.GatewayKeyID)
	}

	a.BudgetMicros = 50_000_000
	if _, err := f.st.UpdateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.gw.failRevokes(true)
	f.m.RekeyStale(ctx)
	if got := f.load(t, a.ID); got.GatewayKeyID != a.GatewayKeyID || !got.KeyStale() {
		t.Fatalf("key %q, stale %v while the gateway refused", got.GatewayKeyID, got.KeyStale())
	}
	f.gw.failRevokes(false)
	f.m.RekeyStale(ctx)
	got := f.load(t, a.ID)
	if got.GatewayKeyID == a.GatewayKeyID || got.KeyStale() || !f.gw.wasRevoked("org_a", a.GatewayKeyID) {
		t.Fatalf("key %q, stale %v, old revoked %v", got.GatewayKeyID, got.KeyStale(), f.gw.wasRevoked("org_a", a.GatewayKeyID))
	}
}

func TestStopCancelsTasksAndKeepsTheWorkspace(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.driver.createState = runtime.Running
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	task := f.queue(t, a.ID)

	if err := f.m.Stop(ctx, a, "Ada stopped the agent"); err != nil {
		t.Fatal(err)
	}
	if state, reason := f.taskState(t, task.ID); state != store.TaskCancelled || reason != "Ada stopped the agent" {
		t.Fatalf("task = %s %q", state, reason)
	}
	if f.driver.count("suspend "+a.ID) != 1 || f.driver.count("terminate "+a.ID) != 0 {
		t.Fatalf("calls = %v, want a suspend only", f.driver.calls)
	}
	if got := f.load(t, a.ID); got.State != store.AgentStopped {
		t.Fatalf("state = %q", got.State)
	}

	// An agent with no container stops all the same.
	never := f.agent(t)
	if err := f.m.Stop(ctx, never, "stopped"); err != nil {
		t.Fatalf("Stop without a container = %v", err)
	}
}

func TestDeleteRemovesTheContainerAndRevokesTheKey(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	a = f.load(t, a.ID)
	task := f.queue(t, a.ID)

	if err := f.m.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	if f.driver.count("terminate "+a.ID) != 1 || !f.gw.wasRevoked("org_a", a.GatewayKeyID) {
		t.Fatalf("calls = %v, revoked = %v", f.driver.calls, f.gw.revoked)
	}
	if _, err := f.st.Agent(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted agent: %v", err)
	}
	if state, _ := f.taskState(t, task.ID); state != store.TaskCancelled {
		t.Fatalf("task = %s", state)
	}
}

func TestDeleteKeepsTheAgentWhenItsContainerStays(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	a = f.load(t, a.ID)
	f.driver.terminateErr = errors.New("the API server timed out")

	if err := f.m.Delete(ctx, a); err == nil {
		t.Fatal("Delete succeeded although the container stays")
	}
	if got := f.load(t, a.ID); got.GatewayKeyID != a.GatewayKeyID || f.gw.wasRevoked("org_a", a.GatewayKeyID) {
		t.Fatal("the key went although the agent stays")
	}
}

func TestAStartThatFailsSaysWhy(t *testing.T) {
	for _, c := range []struct {
		name   string
		opts   Options
		class  runtime.Class
		mint   bool // whether minting fails
		create error
		detail string
	}{
		{
			name: "class over the limits", opts: Options{Limits: runtime.Limits{MaxCPU: "2"}},
			class: runtime.Class{CPU: "4"}, detail: "CPU can be at most 2",
		},
		{name: "no key", mint: true, detail: "did not issue a key"},
		{
			name: "tier not mapped", class: runtime.Class{Isolation: runtime.VM},
			create: fmt.Errorf("%w: vm", runtime.ErrTier), detail: `no "vm" isolation`,
		},
		{name: "driver error", create: errors.New("image not found"), detail: "image not found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, c.opts)
			f.gw.mintFails = c.mint
			f.driver.createErr = c.create
			a := f.agent(t, func(a *store.Agent) { a.Class = c.class })

			if err := f.m.Start(context.Background(), a); err == nil || !strings.Contains(err.Error(), c.detail) {
				t.Fatalf("Start = %v, want %q", err, c.detail)
			}
			got := f.load(t, a.ID)
			if got.State != store.AgentFailed || !strings.Contains(got.StateDetail, c.detail) {
				t.Fatalf("state = %q %q", got.State, got.StateDetail)
			}
			if len(got.RuntimeTokenHash) != 0 {
				t.Fatal("a token was stored for a container that never got it")
			}
		})
	}
}

func TestAFailedRestartKeepsTheRunningToken(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	before := f.load(t, a.ID)
	f.driver.createErr = errors.New("quota exceeded")

	if err := f.m.Start(ctx, before); err == nil {
		t.Fatal("the restart did not fail")
	}
	after := f.load(t, a.ID)
	if !bytes.Equal(after.RuntimeTokenHash, before.RuntimeTokenHash) || after.GatewayKeyID != before.GatewayKeyID {
		t.Fatal("a failed restart replaced the token or the key the old container uses")
	}
	if mints, _ := f.gw.counts(); mints != 1 {
		t.Fatalf("minted %d keys, want 1", mints)
	}
}

func TestWakeLeavesADisabledAgentAlone(t *testing.T) {
	f := newFixture(t, Options{})
	a := f.agent(t, func(a *store.Agent) { a.Enabled = false })
	f.queue(t, a.ID)
	if err := f.m.Wake(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.driver.calls) != 0 {
		t.Fatalf("calls = %v", f.driver.calls)
	}
}

func TestRekeyReplacesAndRevokesTheKey(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	a = f.load(t, a.ID)

	if err := f.m.Rekey(ctx, a); err != nil {
		t.Fatal(err)
	}
	got := f.load(t, a.ID)
	if got.GatewayKeyID == a.GatewayKeyID || !f.gw.wasRevoked("org_a", a.GatewayKeyID) {
		t.Fatalf("key %q, old %q revoked %v", got.GatewayKeyID, a.GatewayKeyID, f.gw.revoked)
	}
	if key, err := f.m.OpenKey(got); err != nil || key != "sk-2" {
		t.Fatalf("OpenKey = %q, %v", key, err)
	}
	// Another agent's sealed key does not open under this one's id.
	other := got
	other.ID = "agt_other"
	if _, err := f.m.OpenKey(other); err == nil {
		t.Fatal("a key opened under another agent's id")
	}
}

func TestRekeyReplacesTheKeyHeldNow(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	stale := f.load(t, a.ID)
	// Two edits in turn, each with the agent as it was read before both.
	for range 2 {
		if err := f.m.Rekey(ctx, stale); err != nil {
			t.Fatal(err)
		}
	}
	got := f.load(t, a.ID)
	for _, key := range []string{"key_1", "key_2"} {
		if !f.gw.wasRevoked("org_a", key) {
			t.Errorf("%s still works; revoked %v, held %q", key, f.gw.revoked, got.GatewayKeyID)
		}
	}
	if got.GatewayKeyID != "key_3" {
		t.Errorf("held %q", got.GatewayKeyID)
	}

	// A key the gateway would not revoke stays, and so does the change.
	f.gw.failRevokes(true)
	if err := f.m.Rekey(ctx, got); err == nil {
		t.Fatal("Rekey succeeded although the old key works")
	}
	if after := f.load(t, a.ID); after.GatewayKeyID != "key_3" {
		t.Fatalf("held %q after a failed rekey", after.GatewayKeyID)
	}
}

func TestADeletedAgentsKeyIsRevokedLater(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)
	if err := f.m.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	a = f.load(t, a.ID)
	f.gw.failRevokes(true)
	if err := f.m.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	if f.gw.wasRevoked("org_a", a.GatewayKeyID) {
		t.Fatal("revoked while the gateway refused")
	}
	f.gw.failRevokes(false)
	f.m.RevokeDeletedKeys(ctx)
	if !f.gw.wasRevoked("org_a", a.GatewayKeyID) {
		t.Fatal("the key of a deleted agent was not revoked later")
	}
	if left, _ := f.st.DeletedKeys(ctx); len(left) != 0 {
		t.Fatalf("still to revoke: %v", left)
	}
}

func TestRecoverKeyMintsOneKeyForCallsAtOnce(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	a := f.agent(t)

	var wg sync.WaitGroup
	keys := make([]string, 4)
	for i := range keys {
		wg.Go(func() {
			key, err := f.m.RecoverKey(ctx, a.ID)
			if err != nil {
				t.Error(err)
			}
			keys[i] = key
		})
	}
	wg.Wait()
	if mints, _ := f.gw.counts(); mints != 1 {
		t.Fatalf("minted %d keys, want 1", mints)
	}
	for _, k := range keys {
		if k != "sk-1" {
			t.Fatalf("keys = %v", keys)
		}
	}

	off := f.agent(t, func(a *store.Agent) { a.Enabled = false })
	if _, err := f.m.RecoverKey(ctx, off.ID); err == nil {
		t.Fatal("a stopped agent got a key")
	}
}

func TestReachAnswersOnlyForARunningWorkspace(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	ws := f.agent(t, workspace)
	f.driver.set(ws.ID, runtime.Status{State: runtime.Running})
	f.setState(t, ws.ID, store.AgentRunning)

	addr, got, err := f.m.Reach(ctx, ws.ID)
	if err != nil || addr != "10.0.0.7:7000" || got.ID != ws.ID {
		t.Fatalf("Reach = %q, %q, %v", addr, got.ID, err)
	}

	if err := f.st.SetAgentState(ctx, ws.ID, store.AgentFailed, "the image does not exist"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Reach(ctx, ws.ID); err == nil || err.Error() != "the image does not exist" {
		t.Fatalf("Reach of a failed workspace = %v", err)
	}

	if _, err := f.st.SetEnabled(ctx, ws.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Reach(ctx, ws.ID); !errors.Is(err, ErrStopped) {
		t.Fatalf("Reach of a stopped workspace = %v", err)
	}
}

// A start applies the agent as it is saved now, not the copy it was given.
func TestAStartAppliesTheLatestSettings(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	stale := f.agent(t, func(a *store.Agent) { a.Image = "agent:a" })
	saved := stale
	saved.Image = "agent:c"
	if _, err := f.st.UpdateAgent(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Start(ctx, stale); err != nil {
		t.Fatal(err)
	}
	f.driver.mu.Lock()
	image := f.driver.specs[stale.ID].Image
	f.driver.mu.Unlock()
	if image != "agent:c" {
		t.Fatalf("started %q", image)
	}

	// One stopped meanwhile is not started.
	stopped := f.agent(t)
	if _, err := f.st.SetEnabled(ctx, stopped.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Start(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	f.driver.mu.Lock()
	_, made := f.driver.specs[stopped.ID]
	f.driver.mu.Unlock()
	if made {
		t.Fatal("started an agent stopped meanwhile")
	}
}

// noCredit refuses every organisation, as the gateway does once its credit
// runs out.
type noCredit struct{}

func (noCredit) MayRun(context.Context, string) error {
	return errors.New("your organisation has used up its credit")
}

func TestWithoutCreditNoMachineStartsAndItsTasksFail(t *testing.T) {
	f := newFixture(t, Options{Credit: noCredit{}})
	ctx := context.Background()
	gone := f.agent(t)
	suspended := f.agent(t)
	f.driver.set(suspended.ID, runtime.Status{State: runtime.Suspended})
	f.setState(t, suspended.ID, store.AgentSuspended)
	tasks := []store.Task{f.queue(t, gone.ID), f.queue(t, suspended.ID)}

	f.m.Reconcile(ctx)
	if len(f.driver.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.driver.calls)
	}
	for _, task := range tasks {
		if state, reason := f.taskState(t, task.ID); state != store.TaskFailed ||
			!strings.Contains(reason, "used up its credit") {
			t.Errorf("task = %s, %q; want failed for credit", state, reason)
		}
	}
	if got := f.load(t, suspended.ID); got.State != store.AgentSuspended {
		t.Errorf("state = %q; a machine refused for credit is not failed", got.State)
	}
	if err := f.m.Wake(ctx, suspended.ID); err == nil || f.driver.count("resume "+suspended.ID) != 0 {
		t.Errorf("Wake = %v, calls = %v; want refused", err, f.driver.calls)
	}
}

func TestWithoutCreditARunningMachineFinishesItsTaskAndIsSuspended(t *testing.T) {
	f := newFixture(t, Options{Credit: noCredit{}})
	working := f.agent(t)
	waiting := f.agent(t)
	idle := f.agent(t, workspace)
	for _, a := range []store.Agent{working, waiting, idle} {
		f.driver.set(a.ID, runtime.Status{State: runtime.Running})
		f.setState(t, a.ID, store.AgentRunning)
		f.exec(t, `UPDATE agents SET last_active_at = now() - interval '1 second' WHERE id = $1`, a.ID)
	}
	underWay := f.queue(t, working.ID)
	f.exec(t, `UPDATE tasks SET state = 'running' WHERE id = $1`, underWay.ID)
	next := f.queue(t, working.ID)
	queued := f.queue(t, waiting.ID)

	f.m.Reconcile(context.Background())
	if f.driver.count("suspend "+working.ID) != 0 {
		t.Error("an agent with a task under way was suspended; it finishes first")
	}
	if state, _ := f.taskState(t, underWay.ID); state != store.TaskRunning {
		t.Errorf("the task under way is %s", state)
	}
	for _, task := range []store.Task{next, queued} {
		if state, reason := f.taskState(t, task.ID); state != store.TaskFailed ||
			!strings.Contains(reason, "used up its credit") {
			t.Errorf("a waiting task = %s, %q; want failed for credit", state, reason)
		}
	}
	for _, a := range []store.Agent{waiting, idle} {
		if f.driver.count("suspend "+a.ID) != 1 {
			t.Errorf("%s: calls = %v; want it suspended", a.Kind, f.driver.calls)
		}
		if got := f.load(t, a.ID); got.State != store.AgentSuspended {
			t.Errorf("%s: state = %q", a.Kind, got.State)
		}
	}
}
