// Package runner serves the runtime API that agents' containers call; see
// internal/protocol for the routes and why the container always calls in.
package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/egress"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

type Options struct {
	// GatewayURL is the gateway as a container reaches it.
	GatewayURL string
	// TaskTimeout is the most a task may run.
	TaskTimeout time.Duration
	// Egress is where the git proxy may connect.
	Egress egress.Policy
	Log    *slog.Logger
}

type Runner struct {
	st    *store.Store
	fleet *fleet.Manager
	disp  *dispatch.Dispatcher
	gw    *gateway.Client
	opts  Options
	log   *slog.Logger
	// wakeups lets a queued task end a long poll at once.
	wakeups *notifier
	// gitHTTP is where the git proxy's requests leave, as Egress allows.
	gitHTTP *http.Client
}

func New(st *store.Store, fl *fleet.Manager, disp *dispatch.Dispatcher, gw *gateway.Client, opts Options) *Runner {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	opts.GatewayURL = strings.TrimRight(opts.GatewayURL, "/")
	return &Runner{
		st: st, fleet: fl, disp: disp, gw: gw, opts: opts, log: opts.Log, wakeups: newNotifier(),
		gitHTTP: gitClient(opts.Egress),
	}
}

// Notify ends a waiting long poll of the agent, as when a task is queued.
func (rn *Runner) Notify(agentID string) { rn.wakeups.notify(agentID) }

func (rn *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.BootstrapPath, rn.auth(rn.bootstrap))
	mux.HandleFunc("GET "+protocol.NextTaskPath, rn.auth(rn.next))
	mux.HandleFunc("POST "+protocol.TaskPath+"/{id}/events", rn.auth(rn.events))
	mux.HandleFunc("POST "+protocol.TaskPath+"/{id}/finish", rn.auth(rn.finish))
	mux.HandleFunc("POST "+protocol.TaskPath+"/{id}/tools/{name}", rn.auth(rn.tool))
	mux.HandleFunc(protocol.GitPath+"/{binding}/{path...}", rn.auth(rn.git))
	mux.HandleFunc("POST "+protocol.WorkspaceToolPath+"/{name}", rn.auth(rn.workspaceTool))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, a store.Agent)

// auth finds the agent by its runtime token. A deleted or replaced container
// has a token nobody knows any more. A stopped agent's token works again
// only once the agent is started, so a token that leaked cannot fetch a key
// meanwhile.
func (rn *Runner) auth(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a runtime token is required")
			return
		}
		a, err := rn.st.AgentByRuntimeToken(r.Context(), fleet.TokenHash(token))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "this runtime token is not valid")
			return
		}
		if !a.Enabled {
			writeError(w, http.StatusForbidden, "stopped", "this agent is stopped")
			return
		}
		next(w, r, a)
	}
}

func (rn *Runner) bootstrap(w http.ResponseWriter, r *http.Request, a store.Agent) {
	key, err := rn.fleet.OpenKey(a)
	if err != nil {
		// A key revoked by the owner leaving, or never minted: try once to
		// mint one, so a resumed agent recovers by itself.
		if key, err = rn.fleet.RecoverKey(r.Context(), a.ID); err != nil {
			writeError(w, http.StatusConflict, "no_key", "this agent has no gateway key: "+err.Error())
			return
		}
	}
	b, err := rn.build(r.Context(), a)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not list the agent's tools")
		return
	}
	b.Gateway.APIKey = key
	// A workspace fetches its bootstrap now and then whether or not anybody
	// is connected, so that is no sign of use.
	if a.Kind != store.KindWorkspace {
		_ = rn.st.TouchAgent(r.Context(), a.ID)
	}
	if a.State != store.AgentRunning {
		_ = rn.st.SetAgentState(r.Context(), a.ID, store.AgentRunning, "")
	}
	writeJSON(w, http.StatusOK, b)
}

// build makes the agent's bootstrap, without its key, and its version.
func (rn *Runner) build(ctx context.Context, a store.Agent) (protocol.Bootstrap, error) {
	tools, err := rn.disp.Tools(ctx, a)
	if err != nil {
		return protocol.Bootstrap{}, err
	}
	b := protocol.Bootstrap{
		Agent: protocol.Agent{
			ID: a.ID, Name: a.Name, Instructions: a.Instructions, Model: a.Model,
			Builtins: a.Builtins, MaxSteps: a.MaxSteps,
			Owner: protocol.Person{Email: a.OwnerEmail},
		},
		Gateway: protocol.GatewayInfo{BaseURL: rn.opts.GatewayURL + "/api/v1"},
		Tools:   tools,
	}
	if cat, err := rn.gw.Catalog(ctx, a.OrgID, a.ProjectID); err == nil {
		for _, m := range cat.MCPServers {
			b.Gateway.MCP = append(b.Gateway.MCP, protocol.MCPServer{
				Alias: m.Alias, URL: rn.opts.GatewayURL + "/api/mcp/" + m.Alias,
			})
		}
	} else {
		rn.log.Warn("could not read the agent's MCP servers from the gateway", "agent", a.ID, "error", err)
	}
	if a.Kind == store.KindWorkspace {
		if b.Workspace, err = rn.workspaceInfo(ctx, a); err != nil {
			return protocol.Bootstrap{}, err
		}
	}
	b.Version = version(b, a.GatewayKeyID)
	return b, nil
}

func (rn *Runner) workspaceInfo(ctx context.Context, a store.Agent) (*protocol.WorkspaceInfo, error) {
	keys, err := rn.st.SSHKeys(ctx, a.OrgID, a.OwnerID)
	if err != nil {
		return nil, err
	}
	w := &protocol.WorkspaceInfo{
		Experience:    a.Experience,
		SSHKeys:       make([]string, 0, len(keys)),
		IngressSecret: rn.fleet.IngressSecret(a),
	}
	for _, k := range keys {
		w.SSHKeys = append(w.SSHKeys, k.PublicKey)
	}
	return w, nil
}

// version hashes all the bootstrap holds, so any change reaches a runtime
// at its next task. The key counts by its id, so no hash of it leaves.
func version(b protocol.Bootstrap, keyID string) string {
	b.Version, b.Gateway.APIKey = "", keyID
	raw, _ := json.Marshal(b)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

func (rn *Runner) next(w http.ResponseWriter, r *http.Request, a store.Agent) {
	wait := protocol.MaxWait
	if v := r.URL.Query().Get("wait"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 && d < wait {
			wait = d
		}
	}
	// Waiting is not activity: only work keeps an agent from being
	// suspended when idle.
	deadline := time.Now().Add(wait)
	for {
		// Waiting before looking at the queue, so a task queued in between
		// still wakes this request.
		woken := rn.wakeups.wait(a.ID)
		// A task left running by a runtime that restarted is handed back.
		t, err := rn.st.RunningTask(r.Context(), a.ID)
		if errors.Is(err, store.ErrNotFound) {
			t, err = rn.st.ClaimTask(r.Context(), a.ID, rn.opts.TaskTimeout)
		}
		if err == nil {
			_ = rn.st.TouchAgent(r.Context(), a.ID)
			writeJSON(w, http.StatusOK, rn.taskFor(r.Context(), a, t))
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "internal", "could not read the queue")
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Polling the queue every few seconds covers tasks queued on another
		// replica, which cannot reach this one's notifier.
		select {
		case <-r.Context().Done():
			return
		case <-woken:
		case <-time.After(min(left, 3*time.Second)):
		}
	}
}

func (rn *Runner) taskFor(ctx context.Context, a store.Agent, t store.Task) protocol.Task {
	pt := protocol.Task{
		ID: t.ID, Source: t.Source, Prompt: t.Prompt, Conversation: t.Conversation,
		RequestedBy: protocol.Person{Email: t.RequestedBy},
		Context:     t.Context,
	}
	// Without a version the runtime keeps what it has, so a failed build
	// only delays a change to the periodic refresh.
	if b, err := rn.build(ctx, a); err == nil {
		pt.Version = b.Version
	}
	if t.Deadline != nil {
		pt.Deadline = *t.Deadline
	}
	return pt
}

// task loads a task of this agent.
func (rn *Runner) task(w http.ResponseWriter, r *http.Request, a store.Agent) (store.Task, bool) {
	t, err := rn.st.Task(r.Context(), r.PathValue("id"))
	// Only a task that is really gone answers 404: the runtime then drops
	// it. A database blip must not, or the task would run again.
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.AgentID != a.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such task")
		return t, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read the task")
		return t, false
	}
	return t, true
}

// maxEvents bounds one report, and protocol.MaxEventText one event, so a
// runaway tool cannot fill the database.
const maxEvents = 200

func (rn *Runner) events(w http.ResponseWriter, r *http.Request, a store.Agent) {
	t, ok := rn.task(w, r, a)
	if !ok {
		return
	}
	var in protocol.Events
	if !readJSON(w, r, &in, 4<<20) {
		return
	}
	if len(in.Events) > maxEvents {
		in.Events = in.Events[:maxEvents]
	}
	events := make([]store.Event, 0, len(in.Events))
	for _, e := range in.Events {
		events = append(events, storable(e))
	}
	if err := rn.st.AddEvents(r.Context(), t.ID, events); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not store the events")
		return
	}
	_ = rn.st.TouchAgent(r.Context(), a.ID)
	// 409 tells the runtime to stop: someone cancelled the task.
	if t.Finished() {
		writeError(w, http.StatusConflict, "task_finished", "this task has ended: "+t.State)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (rn *Runner) finish(w http.ResponseWriter, r *http.Request, a store.Agent) {
	t, ok := rn.task(w, r, a)
	if !ok {
		return
	}
	var in protocol.Finish
	if !readJSON(w, r, &in, 4<<20) {
		return
	}
	state := store.TaskFailed
	if in.State == protocol.Succeeded {
		state = store.TaskSucceeded
	}
	ended, err := rn.st.FinishTask(r.Context(), t.ID, state, noNUL(in.Result), noNUL(in.Error))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not store the result")
		return
	}
	_ = rn.st.TouchAgent(r.Context(), a.ID)
	if !ended {
		writeError(w, http.StatusConflict, "task_finished", "this task had already ended")
		return
	}
	// Deliver at once rather than at the next sweep, so a chat reply comes
	// as soon as the agent is done.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rn.disp.Deliver(ctx)
	}()
	w.WriteHeader(http.StatusNoContent)
}

// storable makes an event fit for Postgres. It refuses the whole batch over
// one NUL byte, and commands print them, as find -print0 does; the batch
// would then be sent again and again, and every later event lost.
func storable(e protocol.Event) store.Event {
	text := noNUL(e.Text)
	if len(text) > protocol.MaxEventText {
		// A cut can split a character, which Postgres refuses too.
		text = strings.ToValidUTF8(text[:protocol.MaxEventText], "") + "…"
	}
	var data json.RawMessage
	// jsonb refuses \u0000. The data only adds detail to the text, so it is
	// left out rather than rewritten.
	if len(e.Data) <= protocol.MaxEventText && !bytes.Contains(e.Data, []byte(`\u0000`)) {
		data = e.Data
	}
	return store.Event{Kind: e.Kind, Text: text, Data: data, At: e.At}
}

func noNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// tool runs a platform tool for a task.
func (rn *Runner) tool(w http.ResponseWriter, r *http.Request, a store.Agent) {
	t, ok := rn.task(w, r, a)
	if !ok {
		return
	}
	if t.State != store.TaskRunning {
		writeError(w, http.StatusConflict, "task_finished", "tools work only while the task runs")
		return
	}
	rn.callTool(w, r, a, t)
}

// workspaceTool runs a platform tool for a workspace, which has no task: it
// acts for its owner.
func (rn *Runner) workspaceTool(w http.ResponseWriter, r *http.Request, a store.Agent) {
	if a.Kind != store.KindWorkspace {
		writeError(w, http.StatusNotFound, "not_found", "only a workspace calls tools without a task")
		return
	}
	rn.callTool(w, r, a, dispatch.WorkspaceTask(a))
}

// callTool runs the tool the path names. The body is the tool's arguments,
// as the model wrote them.
func (rn *Runner) callTool(w http.ResponseWriter, r *http.Request, a store.Agent, t store.Task) {
	args, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "the arguments are too large")
		return
	}
	res := rn.disp.CallTool(r.Context(), a, t, r.PathValue("name"), args)
	_ = rn.st.TouchAgent(r.Context(), a.ID)
	writeJSON(w, http.StatusOK, res)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the body is not valid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"type": "runtime_error", "code": code, "message": msg,
	}})
}
