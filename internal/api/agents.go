package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/store"
)

// agentView is an agent as the panel shows it.
type agentView struct {
	store.Agent
	Queued        int    `json:"queued"`
	RunningTaskID string `json:"running_task_id"`
	CanEdit       bool   `json:"can_edit"`
	// CanGive is whether the caller may give it work.
	CanGive bool `json:"can_give"`
	// Channels counts the agent's chats and folders.
	Channels int `json:"channels"`
}

// canEdit: the owner and the organisation's administrators change an agent.
func canEdit(p Principal, a store.Agent) bool { return a.OwnerID == p.UserID || p.CanAdmin() }

func canGive(p Principal, a store.Agent) bool {
	ok, _ := dispatch.May(a, p.User())
	return ok
}

func viewAgent(p Principal, a store.Agent, l store.AgentLoad, channels int) agentView {
	return agentView{
		Agent: a, Queued: l.Queued, RunningTaskID: l.RunningTaskID,
		CanEdit: canEdit(p, a), CanGive: canGive(p, a), Channels: channels,
	}
}

// writeAgent answers a changed agent as the list shows it.
func (s *Server) writeAgent(w http.ResponseWriter, r *http.Request, p Principal, a store.Agent) {
	l, err := s.st.AgentLoad(r.Context(), a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	bindings, err := s.st.AgentBindings(r.Context(), a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewAgent(p, a, l, len(bindings)))
}

// loadAgent finds an agent in the caller's organisation. Another
// organisation's agent is not found, not forbidden, so ids say nothing. Nor
// is a workspace: the agents' routes do not serve them.
func (s *Server) loadAgent(w http.ResponseWriter, r *http.Request, p Principal, id string) (store.Agent, bool) {
	a, err := s.st.Agent(r.Context(), id)
	return a, s.found(w, err, a.OrgID == p.OrgID && a.Kind == store.KindAgent, "agent")
}

func (s *Server) editableAgent(w http.ResponseWriter, r *http.Request, p Principal, id string) (store.Agent, bool) {
	a, ok := s.loadAgent(w, r, p, id)
	if ok && !canEdit(p, a) {
		forbid(w, "only "+a.OwnerEmail+" and administrators can change "+a.Name)
		return a, false
	}
	return a, ok
}

// givableAgent finds an agent the caller may give work to now.
func (s *Server) givableAgent(w http.ResponseWriter, r *http.Request, p Principal, id string) (store.Agent, bool) {
	a, ok := s.loadAgent(w, r, p, id)
	if !ok {
		return a, false
	}
	if allowed, why := dispatch.May(a, p.User()); !allowed {
		forbid(w, why)
		return a, false
	}
	if !a.Enabled {
		agentStopped(w, p, a)
		return a, false
	}
	return a, true
}

// agentStopped refuses work for a stopped agent, which would otherwise wait
// until someone starts it.
func agentStopped(w http.ResponseWriter, p Principal, a store.Agent) {
	then := "start it first"
	if !canEdit(p, a) {
		then = "ask " + a.OwnerEmail + " to start it"
	}
	writeError(w, http.StatusConflict, "agent_stopped", a.Name+" is stopped; "+then)
}

func runtimeFailed(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadGateway, "runtime_error", err.Error())
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, p Principal) {
	agents, err := s.st.Agents(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	loads, err := s.st.AgentLoads(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	channels, err := s.st.AgentBindingCounts(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]agentView, 0, len(agents))
	for _, a := range agents {
		out = append(out, viewAgent(p, a, loads[a.ID], channels[a.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, p Principal) {
	a, ok := s.loadAgent(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	ctx := r.Context()
	bindings, err := s.st.AgentBindings(ctx, a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	schedules, err := s.st.Schedules(ctx, p.OrgID, a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	l, err := s.st.AgentLoad(ctx, a.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	type stats struct {
		LastTaskAt *time.Time `json:"last_task_at"`
	}
	writeJSON(w, http.StatusOK, struct {
		agentView
		Bindings  []store.Binding `json:"bindings"`
		Schedules []scheduleView  `json:"schedules"`
		Stats     stats           `json:"stats"`
	}{
		agentView: viewAgent(p, a, l, len(bindings)),
		Bindings:  bindings, Schedules: viewSchedules(schedules), Stats: stats{l.LastTaskAt},
	})
}

// agentActivity is the dashboard's chart for one agent. Everyone in the
// organisation sees the agent, so everyone sees its counts.
func (s *Server) agentActivity(w http.ResponseWriter, r *http.Request, p Principal) {
	a, ok := s.loadAgent(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	if act, ok := s.taskActivity(w, r, p.OrgID, a.ID); ok {
		writeJSON(w, http.StatusOK, act)
	}
}

// agentInput is what people set. Pointers tell a PATCH which fields it
// changes.
type agentInput struct {
	Name          *string        `json:"name"`
	Icon          *string        `json:"icon"`
	Description   *string        `json:"description"`
	Instructions  *string        `json:"instructions"`
	Model         *string        `json:"model"`
	Builtins      *[]string      `json:"builtins"`
	MaxSteps      *int           `json:"max_steps"`
	Access        *string        `json:"access"`
	AllowedEmails *[]string      `json:"allowed_emails"`
	BudgetMicros  *int64         `json:"budget_micros"`
	Class         *runtime.Class `json:"class"`
	Image         *string        `json:"image"`
	ProjectID     *string        `json:"project_id"`
	Enabled       *bool          `json:"enabled"`
}

var (
	agentName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,59}$`)
	// The panel holds the icons; a name it does not know shows the default.
	agentIcon = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)
)

// apply checks in and writes it onto a. Only administrators choose a
// project or an image: both decide what the agent may reach.
func (s *Server) apply(ctx context.Context, p Principal, a *store.Agent, in agentInput) error {
	if in.Name != nil {
		a.Name = strings.TrimSpace(*in.Name)
	}
	if !agentName.MatchString(a.Name) {
		return errors.New("a name is 1 to 60 letters, digits, spaces, dots, hyphens or underscores, " +
			"starting with a letter or digit")
	}
	if in.Icon != nil {
		if !agentIcon.MatchString(*in.Icon) {
			return errors.New("an icon is named with up to 32 lowercase letters, digits or hyphens")
		}
		a.Icon = *in.Icon
	}
	if in.Description != nil {
		a.Description = trim(*in.Description, 500)
	}
	if in.Instructions != nil {
		if len(*in.Instructions) > 32<<10 {
			return errors.New("the instructions are longer than 32 KiB")
		}
		a.Instructions = strings.TrimSpace(*in.Instructions)
	}
	if in.ProjectID != nil && *in.ProjectID != a.ProjectID {
		if !p.CanAdmin() {
			return errors.New("only an administrator can choose an agent's project")
		}
		a.ProjectID = *in.ProjectID
	}
	if in.Model != nil {
		a.Model = strings.TrimSpace(*in.Model)
	}
	if in.Model != nil || in.ProjectID != nil {
		cat, err := s.gw.Catalog(ctx, a.OrgID, a.ProjectID)
		if err != nil {
			return fmt.Errorf("could not check the model with Keera Gateway: %w", err)
		}
		a.ProjectID = cat.ProjectID
		if !slices.ContainsFunc(cat.Models, func(m gateway.CatalogRef) bool { return m.Alias == a.Model }) {
			return fmt.Errorf("the project does not allow the model %q; choose one from the list", a.Model)
		}
	}
	if in.Builtins != nil {
		for _, b := range *in.Builtins {
			if !slices.Contains(protocol.Builtins, b) {
				return fmt.Errorf("there is no tool called %q", b)
			}
		}
		a.Builtins = slices.Compact(slices.Sorted(slices.Values(*in.Builtins)))
	}
	if in.MaxSteps != nil {
		a.MaxSteps = *in.MaxSteps
		// The panel sends 0 for an empty field.
		if a.MaxSteps == 0 {
			a.MaxSteps = protocol.DefaultMaxSteps
		}
	}
	if a.MaxSteps < 1 || a.MaxSteps > 200 {
		return errors.New("max steps must be from 1 to 200")
	}
	if in.Access != nil {
		a.Access = *in.Access
	}
	if !slices.Contains([]string{store.AccessOwner, store.AccessOrg, store.AccessPeople}, a.Access) {
		return errors.New("access must be owner, org or people")
	}
	if in.AllowedEmails != nil {
		var emails []string
		for _, e := range *in.AllowedEmails {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" {
				continue
			}
			if _, err := mail.ParseAddress(e); err != nil {
				return fmt.Errorf("%q is not an email address", e)
			}
			emails = append(emails, e)
		}
		a.AllowedEmails = slices.Compact(slices.Sorted(slices.Values(emails)))
	}
	if in.BudgetMicros != nil {
		if *in.BudgetMicros < 0 {
			return errors.New("the budget cannot be negative")
		}
		a.BudgetMicros = *in.BudgetMicros
	}
	if in.Class != nil {
		c := in.Class.WithDefaults()
		if err := s.fleet.CheckClass(c); err != nil {
			return err
		}
		if tiers := s.fleet.Tiers(); len(tiers) > 0 && !slices.Contains(tiers, c.Isolation) {
			return fmt.Errorf("this deployment has no %q isolation", c.Isolation)
		}
		a.Class = c
	}
	if in.Image != nil && *in.Image != a.Image {
		if !p.CanAdmin() {
			return errors.New("only an administrator can choose an agent's image")
		}
		a.Image = strings.TrimSpace(*in.Image)
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	return nil
}

// newClass is a new agent's or workspace's machine. With no tier named, it
// is the strongest this deployment has, since an agent runs code a model
// wrote.
func (s *Server) newClass(in *runtime.Class) *runtime.Class {
	c := runtime.DefaultClass
	if in != nil {
		c = *in
	}
	if c.Isolation == "" {
		c.Isolation = strongestTier(s.fleet.Tiers())
	}
	return &c
}

func strongestTier(tiers []runtime.Isolation) runtime.Isolation {
	for _, t := range []runtime.Isolation{runtime.VM, runtime.Isolated} {
		if slices.Contains(tiers, t) {
			return t
		}
	}
	if len(tiers) == 0 {
		// No runtime yet: the default, checked again when one is set up.
		return runtime.DefaultClass.Isolation
	}
	return runtime.Standard
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, p Principal) {
	var in agentInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Model == nil {
		badRequest(w, "choose a model")
		return
	}
	a := store.Agent{
		ID: id.New("agt"), OrgID: p.OrgID, OwnerID: p.UserID, OwnerEmail: p.Email,
		MaxSteps: protocol.DefaultMaxSteps, Access: store.AccessOrg, Enabled: true, Class: runtime.DefaultClass,
		Builtins: []string{protocol.BuiltinMemory, protocol.BuiltinFiles},
	}
	in.Class = s.newClass(in.Class)
	if err := s.apply(r.Context(), p, &a, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.CreateAgent(r.Context(), a)
	if !s.saved(w, err, "name_taken", "another agent in this organisation is called "+a.Name) {
		return
	}
	s.record(r.Context(), p, "agent.create", out.ID, map[string]any{"name": out.Name, "model": out.Model})
	s.startLater(out)
	// A new agent has no tasks and no channels yet.
	writeJSON(w, http.StatusCreated, viewAgent(p, out, store.AgentLoad{}, 0))
}

// rekeyIfChanged gives a changed agent or workspace a new key when its
// project or budget changed, as the key carries both.
func (s *Server) rekeyIfChanged(ctx context.Context, before, after store.Agent) {
	if after.GatewayKeyID == "" || (before.ProjectID == after.ProjectID && before.BudgetMicros == after.BudgetMicros) {
		return
	}
	if err := s.fleet.Rekey(ctx, after); err != nil {
		s.log.Warn("could not replace a key", after.Kind, after.ID, "error", err)
	}
}

// startLater starts a new or changed agent's machine in the background:
// pulling an image can take minutes.
func (s *Server) startLater(a store.Agent) {
	if !s.fleet.Runs() || !a.Enabled {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.fleet.Start(ctx, a); err != nil {
			s.log.Warn("could not start an agent", "agent", a.ID, "error", err)
		}
	}()
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, p Principal) {
	a, ok := s.editableAgent(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	var in agentInput
	if !readJSON(w, r, &in) {
		return
	}
	before := a
	if err := s.apply(r.Context(), p, &a, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.UpdateAgent(r.Context(), a)
	if !s.saved(w, err, "name_taken", "another agent in this organisation is called "+a.Name) {
		return
	}
	s.record(r.Context(), p, "agent.update", out.ID, map[string]any{"name": out.Name})
	// Instructions and the model reach the runtime at its next task.
	s.rekeyIfChanged(r.Context(), before, out)
	if before.Enabled != out.Enabled {
		// As the start and stop routes do.
		s.record(r.Context(), p, "agent"+startOrStop(out.Enabled), out.ID, nil)
	}
	switch {
	case before.Enabled && !out.Enabled:
		if err := s.fleet.Stop(r.Context(), out, "the agent was stopped"); err != nil {
			s.log.Warn("could not stop an agent", "agent", out.ID, "error", err)
		}
	case !before.Enabled && out.Enabled,
		out.Enabled && (before.Image != out.Image || before.Class != out.Class):
		s.startLater(out)
	}
	s.writeAgent(w, r, p, out)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, p Principal) {
	a, ok := s.editableAgent(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.fleet.Delete(r.Context(), a); err != nil {
		runtimeFailed(w, err)
		return
	}
	s.record(r.Context(), p, "agent.delete", a.ID, map[string]any{"name": a.Name})
	w.WriteHeader(http.StatusNoContent)
}

// setEnabled starts, stops or restarts an agent. A restart replaces the
// container and keeps its files.
func (s *Server) setEnabled(enabled, restart bool) handler {
	return func(w http.ResponseWriter, r *http.Request, p Principal) {
		a, ok := s.editableAgent(w, r, p, r.PathValue("id"))
		if !ok {
			return
		}
		if out, ok := s.setMachineEnabled(w, r, p, a, enabled, restart); ok {
			s.writeAgent(w, r, p, out)
		}
	}
}

// setMachineEnabled starts, stops or restarts an agent or a workspace, and
// answers only when that fails.
func (s *Server) setMachineEnabled(w http.ResponseWriter, r *http.Request, p Principal, a store.Agent,
	enabled, restart bool,
) (store.Agent, bool) {
	if !s.fleet.Runs() && enabled {
		writeError(w, http.StatusConflict, "no_runtime", fleet.ErrNoRuntime.Error())
		return a, false
	}
	out, err := s.st.SetEnabled(r.Context(), a.ID, enabled)
	if err != nil {
		s.fail(w, err)
		return a, false
	}
	action := startOrStop(enabled)
	if restart {
		action = ".restart"
	}
	s.record(r.Context(), p, a.Kind+action, a.ID, nil)
	if enabled {
		err = s.fleet.Start(r.Context(), out)
	} else {
		err = s.fleet.Stop(r.Context(), out, "the "+a.Kind+" was stopped")
	}
	if err != nil {
		runtimeFailed(w, err)
		return a, false
	}
	out, _ = s.st.Agent(r.Context(), a.ID)
	return out, true
}

// startOrStop is the end of the audit action for starting or stopping.
func startOrStop(enabled bool) string {
	if enabled {
		return ".start"
	}
	return ".stop"
}

// takeOverAgent makes the caller the owner, as when the owner left. Only an
// administrator may: the agent's spend and actions become the new owner's.
func (s *Server) takeOverAgent(w http.ResponseWriter, r *http.Request, p Principal) {
	a, ok := s.loadAgent(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	if !p.CanAdmin() {
		forbid(w, "only an administrator can take over an agent")
		return
	}
	previous := a.OwnerEmail
	if err := s.st.SetOwner(r.Context(), a.ID, p.UserID, p.Email); err != nil {
		s.fail(w, err)
		return
	}
	a.OwnerID, a.OwnerEmail = p.UserID, p.Email
	if err := s.fleet.Rekey(r.Context(), a); err != nil {
		writeError(w, http.StatusBadGateway, "gateway_error", err.Error())
		return
	}
	_ = s.st.SetAgentState(r.Context(), a.ID, a.State, "")
	s.record(r.Context(), p, "agent.take_over", a.ID, map[string]string{"previous_owner": previous})
	out, _ := s.st.Agent(r.Context(), a.ID)
	s.writeAgent(w, r, p, out)
}

func (s *Server) agentLogs(w http.ResponseWriter, r *http.Request, p Principal) {
	if a, ok := s.editableAgent(w, r, p, r.PathValue("id")); ok {
		s.streamLogs(w, r, a)
	}
}

// streamLogs answers with a machine's output, and with ?follow=1 what it
// prints next.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, a store.Agent) {
	follow, _ := strconv.ParseBool(r.URL.Query().Get("follow"))
	rc, err := s.fleet.Logs(r.Context(), a.ID, follow)
	if errors.Is(err, runtime.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no_machine", "this "+a.Kind+" has no running machine to read")
		return
	}
	if err != nil {
		runtimeFailed(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Streaming outlives the server's write timeout.
	rw := http.NewResponseController(w)
	_ = rw.SetWriteDeadline(time.Time{})
	buf := make([]byte, 32<<10)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rw.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && r.Context().Err() == nil {
				s.log.Debug("log stream ended", "agent", a.ID, "error", err)
			}
			return
		}
	}
}

// giveTask queues a task typed in the panel. The agent's access rule
// applies, as for a chat message.
func (s *Server) giveTask(w http.ResponseWriter, r *http.Request, p Principal) {
	a, ok := s.givableAgent(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	var in struct {
		Prompt string `json:"prompt"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	t, err := s.disp.Submit(r.Context(), store.Task{
		OrgID: p.OrgID, AgentID: a.ID, Source: protocol.SourceManual,
		Origin:        store.Origin{Label: "Panel"},
		Prompt:        in.Prompt,
		Context:       "A task from " + p.Email + ", typed in the Keera Agents panel. Your answer is shown there.",
		RequestedBy:   p.Email,
		RequestedByID: p.UserID,
	})
	if err != nil {
		s.submitFailed(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// submitFailed answers a task that could not be queued.
func (s *Server) submitFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, dispatch.ErrNoPrompt) {
		badRequest(w, err.Error())
		return
	}
	s.fail(w, err)
}

// ------------------------------------------------------------ bindings

func (s *Server) createBinding(w http.ResponseWriter, r *http.Request, p Principal) {
	if a, ok := s.editableAgent(w, r, p, r.PathValue("id")); ok {
		s.bind(w, r, p, a)
	}
}

// bind connects an agent or a workspace to a connection.
func (s *Server) bind(w http.ResponseWriter, r *http.Request, p Principal, a store.Agent) {
	var in struct {
		ConnectionID string `json:"connection_id"`
		Target       string `json:"target"`
		Label        string `json:"label"`
		Mode         string `json:"mode"`
		Prompt       string `json:"prompt"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	c, ok := s.loadConnection(w, r, p, in.ConnectionID)
	if !ok {
		return
	}
	desc, _ := channels.Describe(channels.Kind(c.Kind))
	in.Target = strings.TrimSpace(in.Target)
	apiMode := in.Mode == store.ModeRead || in.Mode == store.ModeWrite
	// "*" is every chat a bot is in, or all an API connector reaches.
	anyAllowed := in.Mode == store.ModeChat || apiMode && (desc.AnyTarget || desc.TargetLabel == "")
	// An API kind with nothing to narrow to reaches everything.
	if desc.API && desc.TargetLabel == "" {
		in.Target = channels.AnyTarget
	}
	switch {
	case !slices.Contains([]string{
		store.ModeChat, store.ModeWatch, store.ModeFiles, store.ModeRepo, store.ModeRead, store.ModeWrite,
	}, in.Mode):
		badRequest(w, "mode must be chat, watch, files, repo, read or write")
		return
	case a.Kind == store.KindWorkspace && (in.Mode == store.ModeChat || in.Mode == store.ModeWatch):
		// Both give work, and a workspace takes none.
		badRequest(w, "a workspace takes no tasks, so it connects with files, repo, read or write")
		return
	case in.Target == "":
		badRequest(w, "name the "+strings.ToLower(desc.TargetLabel))
		return
	case in.Mode == store.ModeChat && !desc.Chat:
		badRequest(w, desc.Label+" is not a chat")
		return
	case (in.Mode == store.ModeWatch || in.Mode == store.ModeFiles) && !desc.Files:
		badRequest(w, desc.Label+" holds no files")
		return
	case (in.Mode == store.ModeRepo) != desc.Code:
		badRequest(w, desc.Label+" takes the mode repo, and only it does")
		return
	case apiMode != desc.API:
		badRequest(w, desc.Label+" takes the mode read or write, and only it does")
		return
	case in.Target == store.AnyConversation && !anyAllowed:
		badRequest(w, "name the "+strings.ToLower(cmp.Or(desc.TargetLabel, "target"))+"; * is not allowed here")
		return
	}
	// A connector people sign in to works for whoever adds the channel, as
	// for a folder watch, so they must have signed in.
	if c.PerUser {
		if _, err := s.disp.ConnFor(r.Context(), c, p.UserID); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	// Checked now, so a typo, or credentials that cannot reach it, fail
	// here and not in the middle of a task.
	switch {
	case in.Mode == store.ModeRepo:
		repo, err := s.disp.LookupRepo(r.Context(), c, in.Target, p.UserID)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		in.Target = repo.Path
	case apiMode:
		target, err := s.disp.LookupTarget(r.Context(), c, in.Target, p.UserID)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		in.Target = target
	}
	b, err := s.st.CreateBinding(r.Context(), store.Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID, Target: in.Target,
		Label: trim(in.Label, 200), Mode: in.Mode, Prompt: trim(in.Prompt, 8<<10),
		CreatedBy: p.Email, CreatedByID: p.UserID,
	})
	if !s.saved(w, err, "exists", a.Name+" is already connected there") {
		return
	}
	s.record(r.Context(), p, "binding.create", b.ID, map[string]string{
		"agent": a.Name, "connection": c.Name, "target": b.Target, "mode": b.Mode,
	})
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) deleteBinding(w http.ResponseWriter, r *http.Request, p Principal) {
	b, err := s.st.Binding(r.Context(), r.PathValue("id"))
	if !s.found(w, err, b.OrgID == p.OrgID, "channel") {
		return
	}
	// Another's workspace is not found, as on its own routes; another's
	// agent is only forbidden.
	a, err := s.st.Agent(r.Context(), b.AgentID)
	if !s.found(w, err, a.OrgID == p.OrgID && (a.Kind == store.KindAgent || canEdit(p, a)), "channel") {
		return
	}
	if !canEdit(p, a) {
		forbid(w, "only "+a.OwnerEmail+" and administrators can change "+a.Name)
		return
	}
	if err := s.st.DeleteBinding(r.Context(), b.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "binding.delete", b.ID, map[string]string{"target": b.Target})
	w.WriteHeader(http.StatusNoContent)
}
