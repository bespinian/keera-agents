// Package fleet keeps each agent's container in the state it should be in.
//
// An agent that is enabled and has work runs. One that has had nothing to do
// for KEERA_AGENTS_IDLE_SUSPEND is suspended, keeping its workspace, and is
// resumed by its next task. A disabled agent is suspended. A deleted one is
// removed with its volume and its key.
//
// The manager also owns the agent's credentials: its gateway key, minted
// with the service key and attributed to the agent's owner, and the runtime
// token its container authenticates with.
package fleet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
)

// ErrNoRuntime is starting an agent on a deployment with no driver.
var ErrNoRuntime = errors.New("this deployment runs no agents yet; an operator sets KEERA_AGENTS_DRIVER")

type Options struct {
	// Image is the default agent image.
	Image string
	// RuntimeURL is keera-agents as a container reaches it.
	RuntimeURL string
	// IdleSuspend is how long an agent may sit with nothing to do before it
	// is suspended. Zero never suspends.
	IdleSuspend time.Duration
	// Limits bound the classes agents may run with.
	Limits runtime.Limits
	Log    *slog.Logger
}

type Manager struct {
	st     *store.Store
	gw     *gateway.Client
	driver runtime.Driver // nil without a runtime
	box    *secret.Box
	opts   Options
	log    *slog.Logger
	// locks serialises changes to one agent within this replica. Across
	// replicas, the reconcile loop runs on one at a time.
	locks sync.Map
	// retries holds when Reconcile next tries to start an agent whose start
	// failed.
	retryMu sync.Mutex
	retries map[string]retry
}

type retry struct {
	n  int
	at time.Time
}

const (
	// The first retry of a failed start waits this long, each next one
	// twice as long, up to maxRetryWait.
	firstRetryWait = 30 * time.Second
	maxRetryWait   = 10 * time.Minute
	// A task that waited this long for an agent that cannot start fails,
	// so the person who asked hears back.
	maxStartWait = time.Hour
)

func New(st *store.Store, gw *gateway.Client, driver runtime.Driver, box *secret.Box, opts Options) *Manager {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Manager{
		st: st, gw: gw, driver: driver, box: box, opts: opts, log: opts.Log,
		retries: map[string]retry{},
	}
}

// Runs reports whether this deployment has a driver, so agents can start.
func (m *Manager) Runs() bool { return m.driver != nil }

// DriverName is the driver's name, or empty without one.
func (m *Manager) DriverName() string {
	if m.driver == nil {
		return ""
	}
	return m.driver.Name()
}

// Tiers lists the isolation tiers agents may ask for: those the driver has,
// down to the operator's minimum.
func (m *Manager) Tiers() []runtime.Isolation {
	tiers := []runtime.Isolation{}
	if m.driver == nil {
		return tiers
	}
	for _, t := range m.driver.Tiers() {
		if t.AtLeast(m.opts.Limits.MinIsolation) {
			tiers = append(tiers, t)
		}
	}
	return tiers
}

// CheckClass refuses a class outside the operator's limits.
func (m *Manager) CheckClass(c runtime.Class) error { return m.opts.Limits.Check(c) }

func (m *Manager) lock(id string) func() {
	mu, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// keyName is how an agent's key is named in the gateway, so people reading
// its usage reports know what spent it.
func keyName(a store.Agent) string { return "Agent " + a.Name }

// keySealName binds a sealed key to its agent, so one agent's key cannot be
// moved onto another's row.
func keySealName(agentID string) string { return "agent-key:" + agentID }

// EnsureKey mints the agent's gateway key if it has none.
func (m *Manager) EnsureKey(ctx context.Context, a *store.Agent) error {
	if a.GatewayKeyID != "" && len(a.GatewayKey) > 0 {
		return nil
	}
	k, err := m.gw.MintKey(ctx, gateway.KeyRequest{
		OrgID: a.OrgID, ProjectID: a.ProjectID, UserID: a.OwnerID,
		Name: keyName(*a), BudgetMicros: a.BudgetMicros,
	})
	if err != nil {
		return fmt.Errorf("the gateway did not issue a key for this agent: %w", err)
	}
	sealed := m.box.Seal(keySealName(a.ID), []byte(k.Secret))
	if err := m.st.SetAgentKey(ctx, a.ID, k.ID, sealed); err != nil {
		_ = m.gw.RevokeKey(context.WithoutCancel(ctx), a.OrgID, k.ID)
		return err
	}
	a.GatewayKeyID, a.GatewayKey = k.ID, sealed
	return nil
}

// Rekey replaces the agent's key, as when its project or budget changes. A
// running agent's next gateway request fails with 401, and its runtime then
// fetches the new key.
func (m *Manager) Rekey(ctx context.Context, a store.Agent) error {
	defer m.lock(a.ID)()
	old := a.GatewayKeyID
	a.GatewayKeyID, a.GatewayKey = "", nil
	if err := m.EnsureKey(ctx, &a); err != nil {
		return err
	}
	if old != "" {
		if err := m.gw.RevokeKey(ctx, a.OrgID, old); err != nil {
			m.log.Warn("could not revoke an agent's old key", "agent", a.ID, "key", old, "error", err)
		}
	}
	return nil
}

// OpenKey returns the agent's gateway key. Only the runtime API asks.
func (m *Manager) OpenKey(a store.Agent) (string, error) {
	if len(a.GatewayKey) == 0 {
		return "", errors.New("this agent has no gateway key")
	}
	plain, err := m.box.Open(keySealName(a.ID), a.GatewayKey)
	return string(plain), err
}

// RecoverKey mints a key for an agent that has none, and returns it. It
// reads the agent again under its lock, so two runtime calls at once mint
// one key, not two with one never revoked.
func (m *Manager) RecoverKey(ctx context.Context, agentID string) (string, error) {
	defer m.lock(agentID)()
	a, err := m.st.Agent(ctx, agentID)
	if err != nil {
		return "", err
	}
	if !a.Enabled {
		return "", errors.New("the agent is stopped")
	}
	if key, err := m.OpenKey(a); err == nil {
		return key, nil
	}
	if err := m.EnsureKey(ctx, &a); err != nil {
		return "", err
	}
	return m.OpenKey(a)
}

// TokenHash is how a runtime token is stored and looked up.
func TokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Start creates the agent's container, or replaces it so changed settings
// apply. The workspace is kept.
func (m *Manager) Start(ctx context.Context, a store.Agent) error {
	defer m.lock(a.ID)()
	return m.start(ctx, a)
}

func (m *Manager) start(ctx context.Context, a store.Agent) error {
	if m.driver == nil {
		return ErrNoRuntime
	}
	// A row's class can still lack fields, as the schema's default '{}' does.
	// Checked again here, since the limits may have changed since the
	// class was saved.
	class := a.Class.WithDefaults()
	if err := m.opts.Limits.Check(class); err != nil {
		_ = m.st.SetAgentState(ctx, a.ID, store.AgentFailed, err.Error())
		return err
	}
	if err := m.EnsureKey(ctx, &a); err != nil {
		_ = m.st.SetAgentState(ctx, a.ID, store.AgentFailed, err.Error())
		return err
	}
	// The new token is stored only once the container has it. A start that
	// fails leaves the old container working with the old token.
	token := randomToken()
	image := a.Image
	if image == "" {
		image = m.opts.Image
	}
	spec := runtime.Spec{
		AgentID: a.ID, Name: a.Name, Image: image, Class: class,
		Owner: a.OwnerEmail, Org: a.OrgID, Project: a.ProjectID,
		Env: map[string]string{
			protocol.EnvRuntimeURL:   m.opts.RuntimeURL,
			protocol.EnvRuntimeToken: token,
			protocol.EnvAgentID:      a.ID,
			protocol.EnvWorkspace:    protocol.Workspace,
		},
	}
	st, err := m.driver.Create(ctx, spec)
	if err != nil {
		detail := "the agent's machine could not be started: " + err.Error()
		if errors.Is(err, runtime.ErrTier) {
			detail = fmt.Sprintf("this deployment has no %q isolation; choose another in the agent's settings",
				class.Isolation)
		}
		_ = m.st.SetAgentState(ctx, a.ID, store.AgentFailed, detail)
		return errors.New(detail)
	}
	// The container has the token now; store it even if the caller gave up.
	// If this fails, Reconcile starts the agent again.
	if err := m.st.SetRuntimeToken(context.WithoutCancel(ctx), a.ID, TokenHash(token)); err != nil {
		return err
	}
	_ = m.st.TouchAgent(ctx, a.ID)
	return m.st.SetAgentState(ctx, a.ID, stateOf(st), st.Detail)
}

// stale reports whether a running container has a token other than the
// stored one, so every call it makes is refused.
func stale(a store.Agent, st runtime.Status) bool {
	return st.State == runtime.Running && st.Token != "" && !bytes.Equal(TokenHash(st.Token), a.RuntimeTokenHash)
}

// restartStale starts an agent again if its container's token is stale. It
// checks again under the lock, so it does not undo a start that is just
// storing its token.
func (m *Manager) restartStale(ctx context.Context, agentID string) error {
	defer m.lock(agentID)()
	a, err := m.st.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	st, err := m.driver.Status(ctx, agentID)
	if err != nil || !a.Enabled || !stale(a, st) {
		return err
	}
	m.log.Warn("starting an agent again: its container has an old runtime token", "agent", a.ID)
	return m.start(ctx, a)
}

// Wake makes sure an agent with work is running. It returns quickly; the
// container takes its task once it is up.
func (m *Manager) Wake(ctx context.Context, agentID string) error {
	if m.driver == nil {
		return ErrNoRuntime
	}
	defer m.lock(agentID)()
	a, err := m.st.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	if !a.Enabled {
		return nil
	}
	st, err := m.driver.Status(ctx, a.ID)
	if err != nil {
		return err
	}
	switch st.State {
	case runtime.Gone, runtime.Failed:
		return m.start(ctx, a)
	case runtime.Suspended:
		if err := m.driver.Resume(ctx, a.ID); err != nil {
			return err
		}
		_ = m.st.TouchAgent(ctx, a.ID)
		return m.st.SetAgentState(ctx, a.ID, store.AgentStarting, "")
	}
	return nil
}

// Stop cancels the agent's unfinished tasks with reason, and suspends its
// container. The workspace stays.
func (m *Manager) Stop(ctx context.Context, a store.Agent, reason string) error {
	defer m.lock(a.ID)()
	_ = m.st.CancelAgentTasks(ctx, a.ID, reason)
	if m.driver != nil {
		if err := m.driver.Suspend(ctx, a.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
	}
	return m.st.SetAgentState(ctx, a.ID, store.AgentStopped, "")
}

// Delete cancels the agent's unfinished tasks and removes its container,
// workspace and key.
func (m *Manager) Delete(ctx context.Context, a store.Agent) error {
	defer m.lock(a.ID)()
	_ = m.st.CancelAgentTasks(ctx, a.ID, "the agent was deleted")
	if m.driver != nil {
		if err := m.driver.Terminate(ctx, a.ID); err != nil {
			return fmt.Errorf("the agent's machine could not be removed: %w", err)
		}
	}
	if a.GatewayKeyID != "" {
		if err := m.gw.RevokeKey(ctx, a.OrgID, a.GatewayKeyID); err != nil {
			m.log.Warn("could not revoke a deleted agent's key", "agent", a.ID, "error", err)
		}
	}
	return m.st.DeleteAgent(ctx, a.ID)
}

func stateOf(st runtime.Status) string {
	switch st.State {
	case runtime.Running:
		return store.AgentRunning
	case runtime.Pending:
		return store.AgentStarting
	case runtime.Suspended:
		return store.AgentSuspended
	case runtime.Failed:
		return store.AgentFailed
	default:
		return store.AgentStopped
	}
}

// Reconcile brings every agent's row and container in line once. It runs on
// one replica at a time.
func (m *Manager) Reconcile(ctx context.Context) {
	if m.driver == nil {
		return
	}
	agents, err := m.st.Agents(ctx, "")
	if err != nil {
		m.log.Warn("could not list agents", "error", err)
		return
	}
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		m.reconcileOne(ctx, a)
	}
}

func (m *Manager) reconcileOne(ctx context.Context, a store.Agent) {
	st, err := m.driver.Status(ctx, a.ID)
	if err != nil {
		m.log.Warn("could not read an agent's machine", "agent", a.ID, "error", err)
		return
	}
	busy, err := m.st.Busy(ctx, a.ID)
	if err != nil {
		return
	}
	if st.State == runtime.Running || !a.Enabled {
		m.retryMu.Lock()
		delete(m.retries, a.ID)
		m.retryMu.Unlock()
	}

	switch {
	case !a.Enabled && (st.State == runtime.Running || st.State == runtime.Pending):
		if err := m.driver.Suspend(ctx, a.ID); err != nil {
			m.log.Warn("could not suspend a stopped agent", "agent", a.ID, "error", err)
		}
		st.State = runtime.Suspended
	case a.Enabled && busy && st.State != runtime.Running && st.State != runtime.Pending:
		// A task is waiting and nothing is running it. A failed start is
		// tried again, less and less often, so a short outage does not
		// leave the task waiting until someone notices.
		if a.State == store.AgentFailed {
			if m.giveUp(ctx, a) || !m.retryDue(a.ID, time.Now()) {
				return
			}
		}
		if err := m.Wake(ctx, a.ID); err != nil {
			m.log.Warn("could not wake an agent", "agent", a.ID, "error", err)
		}
		return
	case a.Enabled && stale(a, st):
		// As after a start whose answer was lost, or two at once on different
		// replicas. The runtime would wait for tasks it can never fetch.
		if err := m.restartStale(ctx, a.ID); err != nil {
			m.log.Warn("could not start an agent again", "agent", a.ID, "error", err)
		}
		return
	case a.Enabled && !busy && st.State == runtime.Running && m.opts.IdleSuspend > 0 &&
		a.LastActiveAt != nil && time.Since(*a.LastActiveAt) > m.opts.IdleSuspend:
		if !m.suspendIdle(ctx, a) {
			return
		}
		st.State = runtime.Suspended
	}

	state := stateOf(st)
	if !a.Enabled {
		state = store.AgentStopped
	}
	// Keep the reason a start failed until something else happens.
	if a.State == store.AgentFailed && st.State == runtime.Gone {
		return
	}
	if state != a.State || st.Detail != a.StateDetail {
		_ = m.st.SetAgentState(ctx, a.ID, state, st.Detail)
	}
}

// suspendIdle suspends an agent that is still idle. The list Reconcile went
// by may be old, and a task may have come in since, so the store checks
// again and holds off claims until the container has stopped. It reports
// whether the agent stayed suspended.
func (m *Manager) suspendIdle(ctx context.Context, a store.Agent) bool {
	unlock := m.lock(a.ID)
	suspended, err := m.st.SuspendIdle(ctx, a.ID, time.Now().Add(-m.opts.IdleSuspend), func() error {
		return m.driver.Suspend(ctx, a.ID)
	})
	unlock()
	if err != nil {
		m.log.Warn("could not suspend an idle agent", "agent", a.ID, "error", err)
		return false
	}
	if !suspended {
		return false
	}
	m.log.Info("suspended an idle agent", "agent", a.ID, "name", a.Name)
	// A task queued while the container stopped found it still running, so
	// nothing woke it.
	if busy, err := m.st.Busy(ctx, a.ID); err == nil && busy {
		if err := m.Wake(ctx, a.ID); err != nil {
			m.log.Warn("could not wake an agent", "agent", a.ID, "error", err)
		}
		return false
	}
	return true
}

// retryDue reports whether a failed start may be tried again now, and if so
// counts the try. The first retry is due at once.
func (m *Manager) retryDue(agentID string, now time.Time) bool {
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	r := m.retries[agentID]
	if now.Before(r.at) {
		return false
	}
	wait := maxRetryWait
	if r.n < 5 { // 30s << 5 is past the maximum.
		wait = min(firstRetryWait<<r.n, maxRetryWait)
	}
	m.retries[agentID] = retry{n: r.n + 1, at: now.Add(wait)}
	return true
}

// giveUp fails the tasks that waited too long for an agent that cannot
// start. It reports whether it failed any.
func (m *Manager) giveUp(ctx context.Context, a store.Agent) bool {
	n, err := m.st.FailQueuedTasks(ctx, a.ID, time.Now().Add(-maxStartWait),
		"the agent could not start, so this task did not run; its owner sees why in the panel")
	if err != nil {
		m.log.Warn("could not fail an agent's waiting tasks", "agent", a.ID, "error", err)
		return false
	}
	if n > 0 {
		m.log.Info("failed tasks of an agent that could not start", "agent", a.ID, "count", n)
	}
	return n > 0
}

// CheckOwners stops agents whose owner left the organisation or was
// disabled, and revokes their keys: an agent acts for its owner and spends
// their budget, so it must not outlive their access.
func (m *Manager) CheckOwners(ctx context.Context) {
	agents, err := m.st.Agents(ctx, "")
	if err != nil {
		return
	}
	for _, a := range agents {
		if !a.Enabled && a.GatewayKeyID == "" {
			continue
		}
		u, err := m.gw.UserByID(ctx, a.OrgID, a.OwnerID)
		// Only the gateway's own answer counts: a stray 404 would stop every
		// agent and revoke every key.
		gone := gateway.IsNotFound(err) || (err == nil && u.Disabled)
		if !gone {
			continue
		}
		m.log.Info("stopping an agent whose owner left", "agent", a.ID, "owner", a.OwnerEmail)
		a.Enabled = false
		if _, err := m.st.UpdateAgent(ctx, a); err != nil {
			continue
		}
		_ = m.Stop(ctx, a, "the agent's owner no longer has access")
		if a.GatewayKeyID != "" {
			_ = m.gw.RevokeKey(ctx, a.OrgID, a.GatewayKeyID)
			_ = m.st.SetAgentKey(ctx, a.ID, "", nil)
		}
		_ = m.st.SetAgentState(ctx, a.ID, store.AgentStopped,
			a.OwnerEmail+" no longer has access, so this agent was stopped; an administrator can take it over")
		_ = m.st.Audit(ctx, a.OrgID, "keera-agents", "agent.owner_left", a.ID,
			map[string]string{"owner": a.OwnerEmail})
	}
}

// Logs streams the agent's container output.
func (m *Manager) Logs(ctx context.Context, agentID string, follow bool) (io.ReadCloser, error) {
	if m.driver == nil {
		return nil, ErrNoRuntime
	}
	return m.driver.Logs(ctx, agentID, follow)
}

func randomToken() string { return "kat_" + rand.Text() }
