// Package fleet keeps each agent's container in the state it should be in.
//
// An agent that is enabled and has work runs. One that has had nothing to do
// for KEERA_AGENTS_IDLE_SUSPEND is suspended, keeping its workspace, and is
// resumed by its next task. A disabled agent is suspended. A deleted one is
// removed with its volume and its key.
//
// A workspace is handled the same way, with three differences: no task wakes
// it, only its owner connecting; it is idle after
// KEERA_AGENTS_WORKSPACE_IDLE_SUSPEND without an open connection; and its
// container takes connections, on its front.
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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/metrics"
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
	// WorkspaceImage is the default workspace image.
	WorkspaceImage string
	// RuntimeURL is keera-agents as a container reaches it.
	RuntimeURL string
	// IdleSuspend is how long an agent may sit with nothing to do before it
	// is suspended. Zero never suspends.
	IdleSuspend time.Duration
	// WorkspaceIdleSuspend is the same for a workspace nobody is connected
	// to.
	WorkspaceIdleSuspend time.Duration
	// Limits bound the classes agents may run with.
	Limits runtime.Limits
	// Credit says whether an organisation may start a machine, as the
	// gateway, which holds its credit, decides. Nil lets every one start.
	Credit interface {
		MayRun(ctx context.Context, orgID string) error
	}
	// Metrics counts container starts. Nil counts nothing.
	Metrics *metrics.Registry
	Log     *slog.Logger
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
	// pendingSince holds when this replica first saw a busy agent's
	// container waiting to start, under retryMu.
	pendingSince map[string]time.Time
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
		retries: map[string]retry{}, pendingSince: map[string]time.Time{},
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

// lock takes the agent's lock, or gives up when ctx ends: a start pulling
// an image can hold it for minutes, and callers behind it have deadlines of
// their own.
func (m *Manager) lock(ctx context.Context, id string) (func(), error) {
	held, _ := m.locks.LoadOrStore(id, make(chan struct{}, 1))
	ch := held.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("the agent is busy with another change: %w", ctx.Err())
	}
}

// forget drops what this replica holds for a deleted agent. Anyone still
// waiting on its old lock finds the agent gone once they get it.
func (m *Manager) forget(id string) {
	m.locks.Delete(id)
	m.retryMu.Lock()
	delete(m.retries, id)
	delete(m.pendingSince, id)
	m.retryMu.Unlock()
}

// tryLock takes the agent's lock if it is free.
func (m *Manager) tryLock(id string) (func(), bool) {
	held, _ := m.locks.LoadOrStore(id, make(chan struct{}, 1))
	ch := held.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

// keyName is how an agent's key is named in the gateway, so people reading
// its usage reports know what spent it.
func keyName(a store.Agent) string {
	if a.Kind == store.KindWorkspace {
		return "Workspace " + a.Name
	}
	return "Agent " + a.Name
}

// keySealName binds a sealed key to its agent, so one agent's key cannot be
// moved onto another's row.
func keySealName(agentID string) string { return "agent-key:" + agentID }

// ensureKey mints the agent's gateway key if it has none. If another
// replica stored one meanwhile, that one is kept and a's is read again.
func (m *Manager) ensureKey(ctx context.Context, a *store.Agent) error {
	if a.GatewayKeyID != "" && len(a.GatewayKey) > 0 {
		return nil
	}
	err := m.replaceKey(ctx, a)
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	fresh, ferr := m.st.Agent(ctx, a.ID)
	if ferr != nil || fresh.GatewayKeyID == "" || len(fresh.GatewayKey) == 0 {
		return err
	}
	*a = fresh
	return nil
}

// replaceKey mints a key for a's settings and stores it in place of
// a.GatewayKeyID, which it does not revoke. A key that cannot be stored is
// revoked; ErrNotFound means the agent holds another key by now.
func (m *Manager) replaceKey(ctx context.Context, a *store.Agent) error {
	k, err := m.gw.MintKey(ctx, gateway.KeyRequest{
		OrgID: a.OrgID, ProjectID: a.ProjectID, UserID: a.OwnerID,
		Name: keyName(*a), BudgetMicros: a.BudgetMicros, AllowedTools: a.MCPServers,
	})
	if err != nil {
		return fmt.Errorf("the gateway did not issue a key for this agent: %w", err)
	}
	sealed := m.box.Seal(keySealName(a.ID), []byte(k.Secret))
	if err := m.st.ReplaceAgentKey(ctx, a.ID, a.GatewayKeyID, k.ID, sealed, a.KeyFor()); err != nil {
		_ = m.gw.RevokeKey(context.WithoutCancel(ctx), a.OrgID, k.ID)
		return err
	}
	a.GatewayKeyID, a.GatewayKey, a.Key = k.ID, sealed, a.KeyFor()
	return nil
}

// Rekey replaces the agent's key, as when its project, budget or MCP servers
// change. A running agent's next gateway request fails with 401, and its
// runtime then fetches the new key.
//
// It reads the agent, so the key carries what is saved now, and replaces the
// key the agent holds now, not one it held when the caller read it.
// The old key is revoked before the new one is stored: an old key left
// working would keep the old settings. If the gateway cannot be reached, the
// key stays stale and RekeyStale tries again.
func (m *Manager) Rekey(ctx context.Context, id string) error {
	return m.rekey(ctx, id, false)
}

// RekeyStale replaces the keys minted for other settings than their agents'
// now, as when a change could not reach the gateway at once.
func (m *Manager) RekeyStale(ctx context.Context) {
	agents, err := m.st.StaleKeys(ctx)
	if err != nil {
		m.log.Warn("could not list stale keys", "error", err)
		return
	}
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		if err := m.rekey(ctx, a.ID, true); err != nil {
			m.log.Warn("could not replace a stale key", a.Kind, a.ID, "error", err)
		}
	}
}

// rekey is Rekey. With onlyStale, it leaves a key that matches the agent's
// settings by the time it holds the lock.
func (m *Manager) rekey(ctx context.Context, id string, onlyStale bool) error {
	unlock, err := m.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	a, err := m.st.Agent(ctx, id)
	if err != nil {
		return err
	}
	if onlyStale && !a.KeyStale() {
		return nil
	}
	old := a.GatewayKeyID
	if old != "" {
		if err := m.gw.RevokeKey(ctx, a.OrgID, old); err != nil {
			return fmt.Errorf("the gateway did not revoke the agent's old key: %w", err)
		}
	}
	err = m.replaceKey(ctx, &a)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errors.New("the agent's key was replaced meanwhile; try again")
	case err != nil:
		// The revoked key must not be handed out; without one, the
		// runtime's next request for a key mints it.
		_ = m.st.ReplaceAgentKey(context.WithoutCancel(ctx), a.ID, old, "", nil, store.KeyFor{})
		return err
	}
	return nil
}

// OpenKey returns the agent's gateway key: for the runtime API, and for the
// model tools, which call the gateway as the agent.
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
	unlock, err := m.lock(ctx, agentID)
	if err != nil {
		return "", err
	}
	defer unlock()
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
	if err := m.ensureKey(ctx, &a); err != nil {
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
// apply. The workspace is kept. It reads the agent again under its lock: two
// quick edits start in turn, and each must apply what is saved now, not
// what it was given, nor start an agent stopped meanwhile.
func (m *Manager) Start(ctx context.Context, a store.Agent) error {
	if m.driver == nil {
		return ErrNoRuntime
	}
	unlock, err := m.lock(ctx, a.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if a, err = m.st.Agent(ctx, a.ID); err != nil {
		return err
	}
	if !a.Enabled {
		return nil
	}
	return m.start(ctx, a)
}

func (m *Manager) start(ctx context.Context, a store.Agent) (err error) {
	if m.driver == nil {
		return ErrNoRuntime
	}
	defer func() { m.opts.Metrics.MachineStart(a.Kind, metrics.Outcome(err)) }()
	// A row's class can still lack fields, as the schema's default '{}' does.
	// Checked again here, since the limits may have changed since the
	// class was saved.
	class := a.Class.WithDefaults()
	if err := m.opts.Limits.Check(class); err != nil {
		_ = m.st.SetAgentState(ctx, a.ID, store.AgentFailed, err.Error())
		return err
	}
	// Refused for credit, the machine stays as it is: not failed, since
	// nothing is wrong with it.
	if err := m.mayRun(ctx, a); err != nil {
		return err
	}
	if err := m.ensureKey(ctx, &a); err != nil {
		_ = m.st.SetAgentState(ctx, a.ID, store.AgentFailed, err.Error())
		return err
	}
	// The new token is stored only once the container has it. A start that
	// fails leaves the old container working with the old token.
	token := randomToken()
	image := a.Image
	if image == "" {
		image = m.opts.Image
		if a.Kind == store.KindWorkspace {
			image = m.opts.WorkspaceImage
		}
	}
	spec := runtime.Spec{
		AgentID: a.ID, Workspace: a.Kind == store.KindWorkspace, Name: a.Name, Image: image, Class: class,
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
	_ = m.st.SetHasDisk(context.WithoutCancel(ctx), a.ID, true)
	// The container has the token now; store it even if the caller gave up.
	// If this fails, Reconcile starts the agent again.
	if err := m.st.SetRuntimeToken(context.WithoutCancel(ctx), a.ID, TokenHash(token)); err != nil {
		// Deleted meanwhile, as through another replica, whose lock this
		// start did not wait for. Nothing else would remove this machine.
		if errors.Is(err, store.ErrNotFound) {
			_ = m.driver.Terminate(context.WithoutCancel(ctx), a.ID)
		}
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
// storing its token. A held lock means that start is under way.
func (m *Manager) restartStale(ctx context.Context, agentID string) error {
	unlock, ok := m.tryLock(agentID)
	if !ok {
		return nil
	}
	defer unlock()
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
	return m.wake(ctx, agentID, true)
}

// wake is Wake. Unless it may wait, it leaves an agent whose lock is held,
// as by a start pulling an image, for the next round: Reconcile goes
// through the agents one by one, and one must not hold up the others.
func (m *Manager) wake(ctx context.Context, agentID string, wait bool) error {
	if m.driver == nil {
		return ErrNoRuntime
	}
	var unlock func()
	if wait {
		var err error
		if unlock, err = m.lock(ctx, agentID); err != nil {
			return err
		}
	} else {
		var ok bool
		if unlock, ok = m.tryLock(agentID); !ok {
			return nil
		}
	}
	defer unlock()
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
		// The limits may have changed since it was suspended. start marks
		// it failed, saying why.
		if m.opts.Limits.Check(a.Class.WithDefaults()) != nil {
			return m.start(ctx, a)
		}
		if err := m.mayRun(ctx, a); err != nil {
			return err
		}
		if err := m.driver.Resume(ctx, a.ID); err != nil {
			return err
		}
		_ = m.st.TouchAgent(ctx, a.ID)
		return m.st.SetAgentState(ctx, a.ID, store.AgentStarting, "")
	}
	return nil
}

// failQueued fails the agent's tasks queued before the time with why, so
// whoever asked hears it. It returns how many it failed.
func (m *Manager) failQueued(ctx context.Context, a store.Agent, before time.Time, why string) int64 {
	n, err := m.st.FailQueuedTasks(ctx, a.ID, before, why)
	if err != nil {
		m.log.Warn("could not fail an agent's waiting tasks", "agent", a.ID, "error", err)
	}
	return n
}

// mayRun says whether the agent's organisation may start a machine now.
func (m *Manager) mayRun(ctx context.Context, a store.Agent) error {
	if m.opts.Credit == nil {
		return nil
	}
	return m.opts.Credit.MayRun(ctx, a.OrgID)
}

// Stop cancels the agent's unfinished tasks with reason, and suspends its
// container. The workspace stays.
func (m *Manager) Stop(ctx context.Context, a store.Agent, reason string) error {
	return m.stop(ctx, a, reason, "")
}

// stop is Stop, with detail saying why on the agent's state.
func (m *Manager) stop(ctx context.Context, a store.Agent, reason, detail string) error {
	unlock, err := m.lock(ctx, a.ID)
	if err != nil {
		return err
	}
	defer unlock()
	_ = m.st.CancelAgentTasks(ctx, a.ID, reason)
	if m.driver != nil {
		if err := m.driver.Suspend(ctx, a.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
	}
	return m.st.SetAgentState(ctx, a.ID, store.AgentStopped, detail)
}

// Delete cancels the agent's unfinished tasks and removes its container,
// workspace and key.
func (m *Manager) Delete(ctx context.Context, a store.Agent) error {
	unlock, err := m.lock(ctx, a.ID)
	if err != nil {
		return err
	}
	defer unlock()
	_ = m.st.CancelAgentTasks(ctx, a.ID, "the agent was deleted")
	if m.driver != nil {
		if err := m.driver.Terminate(ctx, a.ID); err != nil {
			return fmt.Errorf("the agent's machine could not be removed: %w", err)
		}
	}
	if err := m.st.DeleteAgent(ctx, a.ID); err != nil {
		return err
	}
	m.forget(a.ID)
	m.RevokeDeletedKeys(ctx)
	return nil
}

// RevokeDeletedKeys revokes the keys of deleted agents. A deleted agent
// keeps its key's id until the gateway has revoked it, so a revocation that
// fails is tried again in the next round.
func (m *Manager) RevokeDeletedKeys(ctx context.Context) {
	agents, err := m.st.DeletedKeys(ctx)
	if err != nil {
		m.log.Warn("could not list deleted agents' keys", "error", err)
		return
	}
	for _, a := range agents {
		if err := m.gw.RevokeKey(ctx, a.OrgID, a.GatewayKeyID); err != nil {
			m.log.Warn("could not revoke a deleted agent's key", "agent", a.ID, "error", err)
			continue
		}
		_ = m.st.KeyRevoked(ctx, a.ID, a.GatewayKeyID)
	}
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
	agents, err := m.st.Machines(ctx)
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
	// Its disk is billed while the machine exists. A start meanwhile sets
	// it again, so a stale read here costs at most a round's disk.
	if has := st.State != runtime.Gone; has != a.HasDisk {
		_ = m.st.SetHasDisk(ctx, a.ID, has)
	}
	busy, err := m.st.Busy(ctx, a.ID)
	if err != nil {
		return
	}
	// A container can wait to start for good, as when no node has room for
	// its pod. Its tasks then fail, as after a failed start.
	if m.stuck(a.ID, a.Enabled && busy && st.State == runtime.Pending, time.Now()) {
		m.giveUp(ctx, a)
	}
	if st.State == runtime.Running || !a.Enabled {
		m.retryMu.Lock()
		delete(m.retries, a.ID)
		m.retryMu.Unlock()
	}

	// Only a running machine needs asking: one that is not running asks
	// when it would start.
	var refused error
	if a.Enabled && st.State == runtime.Running {
		refused = m.mayRun(ctx, a)
	}

	switch {
	case refused != nil:
		// Its organisation may not run machines. What waits fails now, and
		// a task under way finishes first. A workspace has no tasks, so it
		// is suspended at once.
		if busy {
			m.failQueued(ctx, a, time.Now(), refused.Error())
			if busy, err = m.st.Busy(ctx, a.ID); err != nil || busy {
				return
			}
		}
		if !m.suspendIdle(ctx, a, time.Now(), "suspended a machine whose organisation may not run one") {
			return
		}
		st.State = runtime.Suspended
	case !a.Enabled && (st.State == runtime.Running || st.State == runtime.Pending):
		if !m.suspendStopped(ctx, a.ID) {
			return
		}
		st.State = runtime.Suspended
	case a.Enabled && busy && st.State != runtime.Running && st.State != runtime.Pending:
		// A task is waiting and its organisation may not start a machine:
		// the task fails now, so the person who asked hears why.
		if err := m.mayRun(ctx, a); err != nil {
			m.failQueued(ctx, a, time.Now(), err.Error())
			return
		}
		// A task is waiting and nothing is running it. A failed start is
		// tried again, less and less often, so a short outage does not
		// leave the task waiting until someone notices.
		if a.State == store.AgentFailed {
			if m.giveUp(ctx, a) || !m.retryDue(a.ID, time.Now()) {
				return
			}
		}
		if err := m.wake(ctx, a.ID, false); err != nil {
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
	case a.Enabled && !busy && st.State == runtime.Running && m.idleLimit(a) > 0 &&
		a.LastActiveAt != nil && time.Since(*a.LastActiveAt) > m.idleLimit(a):
		if !m.suspendIdle(ctx, a, time.Now().Add(-m.idleLimit(a)), "suspended an idle agent") {
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

// suspendStopped suspends the machine of an agent that is stopped. The list
// Reconcile went by may be old, and someone may have started the agent
// since, so it checks again under the lock. A held lock means a change is
// under way, such as a start pulling an image: the next round looks again.
// It reports whether the agent is still stopped.
func (m *Manager) suspendStopped(ctx context.Context, agentID string) bool {
	unlock, ok := m.tryLock(agentID)
	if !ok {
		return false
	}
	defer unlock()
	a, err := m.st.Agent(ctx, agentID)
	if err != nil || a.Enabled {
		return false
	}
	if err := m.driver.Suspend(ctx, agentID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		m.log.Warn("could not suspend a stopped agent", "agent", agentID, "error", err)
	}
	return true
}

// suspendIdle suspends an agent that has been idle since idleSince and still
// is, and logs msg. The list Reconcile went by may be old, and a task may
// have come in since, so the store checks again and holds off claims until
// the container has stopped. A held lock means a change is under way: the
// next round looks again. It reports whether the agent stayed suspended.
func (m *Manager) suspendIdle(ctx context.Context, a store.Agent, idleSince time.Time, msg string) bool {
	unlock, ok := m.tryLock(a.ID)
	if !ok {
		return false
	}
	suspended, err := m.st.SuspendIdle(ctx, a.ID, idleSince, func() error {
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
	m.log.Info(msg, "agent", a.ID, "name", a.Name)
	// A task queued while the container stopped found it still running, so
	// nothing woke it.
	if busy, err := m.st.Busy(ctx, a.ID); err == nil && busy {
		if err := m.wake(ctx, a.ID, false); err != nil {
			m.log.Warn("could not wake an agent", "agent", a.ID, "error", err)
		}
		return false
	}
	return true
}

// idleLimit is how long a machine may sit idle before it is suspended. Zero
// never suspends.
func (m *Manager) idleLimit(a store.Agent) time.Duration {
	if a.Kind == store.KindWorkspace {
		return m.opts.WorkspaceIdleSuspend
	}
	return m.opts.IdleSuspend
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

// stuck reports whether an agent's container has waited to start for longer
// than maxStartWait, counting from when this replica first saw it pending.
func (m *Manager) stuck(agentID string, pending bool, now time.Time) bool {
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	if !pending {
		delete(m.pendingSince, agentID)
		return false
	}
	since, ok := m.pendingSince[agentID]
	if !ok {
		m.pendingSince[agentID] = now
		return false
	}
	return now.Sub(since) > maxStartWait
}

// giveUp fails the tasks that waited too long for an agent that cannot
// start. It reports whether it failed any.
func (m *Manager) giveUp(ctx context.Context, a store.Agent) bool {
	n := m.failQueued(ctx, a, time.Now().Add(-maxStartWait),
		"the agent could not start, so this task did not run; its owner sees why in the panel")
	if n > 0 {
		m.log.Info("failed tasks of an agent that could not start", "agent", a.ID, "count", n)
	}
	return n > 0
}

// CheckOwners stops agents whose owner left the organisation or was
// disabled, and revokes their keys: an agent acts for its owner and spends
// their budget, so it must not outlive their access.
func (m *Manager) CheckOwners(ctx context.Context) {
	agents, err := m.st.Machines(ctx)
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
			// People change their name in the gateway; the panel shows it.
			if err == nil && u.Name != a.OwnerName {
				_ = m.st.SetOwnerName(ctx, a.ID, a.OwnerID, u.Name)
			}
			continue
		}
		// The list was read before the gateway calls, so an administrator
		// may have taken the agent over since. Only the owner checked counts.
		if a.Enabled {
			m.log.Info("stopping an agent whose owner left", "agent", a.ID, "owner", a.OwnerEmail)
			if a, err = m.st.DisableIfOwner(ctx, a.ID, a.OwnerID); err != nil {
				continue
			}
			_ = m.stop(ctx, a, "the agent's owner no longer has access",
				a.OwnerEmail+" no longer has access, so this agent was stopped; an administrator can take it over")
			_ = m.st.Audit(ctx, a.OrgID, "keera-agents", "agent.owner_left", a.ID,
				map[string]string{"owner": a.OwnerEmail})
		}
		// A key that is not revoked stays, so the next round tries again.
		// It is cleared only if it is still the one revoked: an
		// administrator taking the agent over meanwhile has a new one.
		if a.GatewayKeyID != "" {
			if err := m.gw.RevokeKey(ctx, a.OrgID, a.GatewayKeyID); err != nil {
				m.log.Warn("could not revoke the key of an agent whose owner left", "agent", a.ID, "error", err)
				continue
			}
			_ = m.st.ReplaceAgentKey(ctx, a.ID, a.GatewayKeyID, "", nil, store.KeyFor{})
		}
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

// IngressSecret is what keera-agents presents to a workspace's front. It is
// derived from the stored runtime token's hash, so it changes at each start,
// is never stored, and any replica can make it. It is empty before the
// workspace's first start.
func (m *Manager) IngressSecret(a store.Agent) string {
	if len(a.RuntimeTokenHash) == 0 {
		return ""
	}
	return hex.EncodeToString(m.box.Derive("workspace-ingress:"+a.ID, a.RuntimeTokenHash))
}

// readyWait bounds how long a connection waits for its workspace to start.
const readyWait = 3 * time.Minute

// Reach wakes a workspace if it is not running, waits until its runtime is
// up, and returns its front's address and the workspace as it is then, with
// the token its container has. A stopped workspace is refused: its owner
// starts it first.
func (m *Manager) Reach(ctx context.Context, agentID string) (string, store.Agent, error) {
	if m.driver == nil {
		return "", store.Agent{}, ErrNoRuntime
	}
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	if err := m.Wake(ctx, agentID); err != nil {
		return "", store.Agent{}, err
	}
	for {
		a, err := m.st.Agent(ctx, agentID)
		if err != nil {
			return "", a, err
		}
		if !a.Enabled {
			return "", a, ErrStopped
		}
		switch a.State {
		case store.AgentFailed:
			return "", a, errors.New(a.StateDetail)
		case store.AgentRunning:
			// Running means the runtime fetched its bootstrap. Its front
			// may not listen yet, which the caller allows for.
			addr, err := m.driver.Address(ctx, agentID)
			if err == nil {
				return addr, a, nil
			}
			if !errors.Is(err, runtime.ErrNotFound) {
				return "", a, err
			}
		}
		select {
		case <-ctx.Done():
			return "", a, errors.New("the workspace did not start in time; see its logs in the panel")
		case <-time.After(time.Second):
		}
	}
}

// ErrStopped is reaching a workspace someone stopped.
var ErrStopped = errors.New("the workspace is stopped; start it first")
