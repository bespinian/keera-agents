package api

import (
	"cmp"
	"context"
	"encoding/json"
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

	"github.com/bespinian/keera-agents/internal/connectors"
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
	// BindingCount counts what the agent is bound to: chats, folders,
	// repositories and services.
	BindingCount int `json:"binding_count"`
}

// canEdit: the owner and the organisation's administrators change an agent.
func canEdit(p Principal, a store.Agent) bool { return a.OwnerID == p.UserID || p.CanAdmin() }

func canGive(p Principal, a store.Agent) bool {
	ok, _ := dispatch.May(a, p.User())
	return ok
}

func viewAgent(p Principal, a store.Agent, l store.AgentLoad, bindings int) agentView {
	return agentView{
		Agent: a, Queued: l.Queued, RunningTaskID: l.RunningTaskID,
		CanEdit: canEdit(p, a), CanGive: canGive(p, a), BindingCount: bindings,
	}
}

// writeAgent answers a changed agent as the list shows it.
func (s *Server) writeAgent(w http.ResponseWriter, r *http.Request, p Principal, a store.Agent) error {
	l, err := s.st.AgentLoad(r.Context(), a.ID)
	if err != nil {
		return err
	}
	bindings, err := s.st.AgentBindings(r.Context(), a.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, viewAgent(p, a, l, len(bindings)))
}

// loadAgent finds an agent in the caller's organisation. Another
// organisation's agent is not found, not forbidden, so ids say nothing. Nor
// is a workspace: the agents' routes do not serve them.
func (s *Server) loadAgent(r *http.Request, p Principal, id string) (store.Agent, error) {
	a, err := s.st.Agent(r.Context(), id)
	return a, found(err, a.OrgID == p.OrgID && a.Kind == store.KindAgent, "agent")
}

func (s *Server) editableAgent(r *http.Request, p Principal, id string) (store.Agent, error) {
	a, err := s.loadAgent(r, p, id)
	if err == nil && !canEdit(p, a) {
		return a, forbid("only " + a.OwnerEmail + " and administrators can change " + a.Name)
	}
	return a, err
}

// givableAgent finds an agent the caller may give work to now.
func (s *Server) givableAgent(r *http.Request, p Principal, id string) (store.Agent, error) {
	a, err := s.loadAgent(r, p, id)
	if err != nil {
		return a, err
	}
	if allowed, why := dispatch.May(a, p.User()); !allowed {
		return a, forbid(why)
	}
	if !a.Enabled {
		return a, agentStopped(p, a)
	}
	return a, nil
}

// agentStopped refuses work for a stopped agent, which would otherwise wait
// until someone starts it.
func agentStopped(p Principal, a store.Agent) error {
	then := "start it first"
	if !canEdit(p, a) {
		then = "ask " + a.OwnerEmail + " to start it"
	}
	return refuse(http.StatusConflict, "agent_stopped", a.Name+" is stopped; "+then)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, p Principal) error {
	agents, err := s.st.Agents(r.Context(), p.OrgID)
	if err != nil {
		return err
	}
	loads, err := s.st.AgentLoads(r.Context(), p.OrgID)
	if err != nil {
		return err
	}
	counts, err := s.st.AgentBindingCounts(r.Context(), p.OrgID)
	if err != nil {
		return err
	}
	out := make([]agentView, 0, len(agents))
	for _, a := range agents {
		out = append(out, viewAgent(p, a, loads[a.ID], counts[a.ID]))
	}
	return writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.loadAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	ctx := r.Context()
	bindings, err := s.st.AgentBindings(ctx, a.ID)
	if err != nil {
		return err
	}
	schedules, err := s.st.Schedules(ctx, p.OrgID, a.ID)
	if err != nil {
		return err
	}
	l, err := s.st.AgentLoad(ctx, a.ID)
	if err != nil {
		return err
	}
	type stats struct {
		LastTaskAt *time.Time `json:"last_task_at"`
	}
	return writeJSON(w, http.StatusOK, struct {
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
func (s *Server) agentActivity(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.loadAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	act, err := s.taskActivity(r, p.OrgID, a.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, act)
}

// agentInput is what people set. Pointers tell a PATCH which fields it
// changes.
type agentInput struct {
	Name         *string `json:"name"`
	Icon         *string `json:"icon"`
	Description  *string `json:"description"`
	Instructions *string `json:"instructions"`
	Model        *string `json:"model"`
	// TranscriptionModel and AskModels are other models it may use as
	// tools. Empty offers none.
	TranscriptionModel *string   `json:"transcription_model"`
	AskModels          *[]string `json:"ask_models"`
	// MCPServers stays raw, so a PATCH tells null, every server the project
	// allows, from leaving the field out.
	MCPServers    json.RawMessage `json:"mcp_servers"`
	Builtins      *[]string       `json:"builtins"`
	MaxSteps      *int            `json:"max_steps"`
	Access        *string         `json:"access"`
	AllowedEmails *[]string       `json:"allowed_emails"`
	BudgetMicros  *int64          `json:"budget_micros"`
	Class         *runtime.Class  `json:"class"`
	Image         *string         `json:"image"`
	ProjectID     *string         `json:"project_id"`
	// OwnerID is only read on create. Taking over is how an owner changes
	// later, as it needs a new key.
	OwnerID *string `json:"owner_id"`
}

var (
	agentName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,59}$`)
	// The panel holds the icons; a name it does not know shows the default.
	agentIcon = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)
)

// checkCatalog asks the gateway whether the project allows the models and
// MCP servers that in sets. A new project checks them all.
func (s *Server) checkCatalog(ctx context.Context, a *store.Agent, in agentInput) error {
	all := in.ProjectID != nil
	if !all && in.Model == nil && in.TranscriptionModel == nil && in.AskModels == nil && in.MCPServers == nil {
		return nil
	}
	cat, err := s.gw.Catalog(ctx, a.OrgID, a.ProjectID)
	if err != nil {
		return fmt.Errorf("could not check the models with Keera Gateway: %w", err)
	}
	a.ProjectID = cat.ProjectID
	allowed := func(refs []gateway.CatalogRef, alias string) bool {
		return slices.ContainsFunc(refs, func(m gateway.CatalogRef) bool { return m.Alias == alias })
	}
	mcpAllowed := func(alias string) bool {
		return slices.ContainsFunc(cat.MCPServers, func(r gateway.MCPRef) bool { return r.Alias == alias })
	}
	if (all || in.Model != nil) && !allowed(cat.Models, a.Model) {
		return fmt.Errorf("the project does not allow the model %q; choose one from the list", a.Model)
	}
	if (all || in.TranscriptionModel != nil) && a.TranscriptionModel != "" &&
		!allowed(cat.TranscriptionModels, a.TranscriptionModel) {
		return fmt.Errorf("the project does not allow the transcription model %q; choose one from the list",
			a.TranscriptionModel)
	}
	if all || in.AskModels != nil {
		for _, m := range a.AskModels {
			if !allowed(cat.Models, m) {
				return fmt.Errorf("the project does not allow the model %q to ask; choose one from the list", m)
			}
		}
	}
	if all || in.MCPServers != nil {
		for _, m := range a.MCPServers {
			if !mcpAllowed(m) {
				return fmt.Errorf("the project does not allow the MCP server %q; choose one from the list", m)
			}
		}
	}
	return nil
}

// mcpServersOf reads mcp_servers: null is every server the project allows,
// a list only those.
func mcpServersOf(raw json.RawMessage) ([]string, error) {
	var in []string
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("mcp_servers is a list of MCP servers' aliases, or null for all")
	}
	if in == nil {
		return nil, nil
	}
	return cleanList(in), nil
}

// cleanList trims, sorts and dedupes xs, dropping empty ones. It never
// returns nil: for mcp_servers, nil means all.
func cleanList(xs []string) []string {
	out := []string{}
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// maxSteps is the highest step limit one can set, so a typo cannot overflow
// the column.
const maxSteps = 10000

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
	if in.TranscriptionModel != nil {
		a.TranscriptionModel = strings.TrimSpace(*in.TranscriptionModel)
	}
	if in.AskModels != nil {
		a.AskModels = cleanList(*in.AskModels)
	}
	if in.MCPServers != nil {
		servers, err := mcpServersOf(in.MCPServers)
		if err != nil {
			return err
		}
		a.MCPServers = servers
	}
	if err := s.checkCatalog(ctx, a, in); err != nil {
		return err
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
	}
	if a.MaxSteps < 0 || a.MaxSteps > maxSteps {
		return fmt.Errorf("max steps must be from 0, for no limit, to %d", maxSteps)
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
		// A machine's volume keeps the size it was made with: Kubernetes
		// cannot change a StatefulSet's claims. Billing goes by the class, so
		// it must keep saying that size. A new agent has no class yet.
		if _, _, was := a.Class.Size(); a.Class.Disk != "" {
			if _, _, now := c.Size(); now != was {
				return errors.New("the disk is fixed once the machine exists")
			}
			c.Disk = a.Class.Disk
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
	return nil
}

// newClass is a new agent's or workspace's machine. With no tier named, it
// is the strongest this deployment has, since an agent runs code a model
// wrote.
func (s *Server) newClass(in *runtime.Class) *runtime.Class {
	return classFor(in, s.fleet.Tiers())
}

func classFor(in *runtime.Class, tiers []runtime.Isolation) *runtime.Class {
	c := runtime.DefaultClass
	// DefaultClass names a tier this deployment may not have.
	c.Isolation = ""
	if in != nil {
		c = *in
	}
	if c.Isolation == "" {
		c.Isolation = strongestTier(tiers)
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

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in agentInput
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	if in.Model == nil {
		return badRequest("choose a model")
	}
	a := store.Agent{
		ID: id.New("agt"), OrgID: p.OrgID, OwnerID: p.UserID, OwnerEmail: p.Email, OwnerName: p.Name,
		Access: store.AccessOwner, Enabled: true,
		Builtins: []string{protocol.BuiltinMemory, protocol.BuiltinFiles},
	}
	if in.OwnerID != nil && *in.OwnerID != "" && *in.OwnerID != p.UserID {
		u, err := s.newOwner(r, p, *in.OwnerID)
		if err != nil {
			return err
		}
		a.OwnerID, a.OwnerEmail, a.OwnerName = u.ID, u.Email, u.Name
	}
	in.Class = s.newClass(in.Class)
	if err := s.apply(r.Context(), p, &a, in); err != nil {
		return badRequest(err.Error())
	}
	out, err := s.st.CreateAgent(r.Context(), a)
	if err = saved(err, "name_taken", "another agent in this organisation is called "+a.Name); err != nil {
		return err
	}
	s.record(r.Context(), p, "agent.create", out.ID, map[string]any{
		"name": out.Name, "model": out.Model, "owner": out.OwnerEmail,
	})
	s.startLater(out)
	// A new agent has no tasks and no bindings yet.
	return writeJSON(w, http.StatusCreated, viewAgent(p, out, store.AgentLoad{}, 0))
}

// newOwner checks the person an administrator creates an agent or a
// workspace for: an active member of the organisation. It spends in their
// name.
func (s *Server) newOwner(r *http.Request, p Principal, userID string) (gateway.User, error) {
	if !p.CanAdmin() {
		return gateway.User{}, forbid("only an administrator can create one for someone else")
	}
	u, err := s.gw.UserByID(r.Context(), p.OrgID, userID)
	if gateway.IsNotFound(err) || (err == nil && u.Disabled) {
		return gateway.User{}, badRequest("the owner must be an active member of this organisation")
	}
	if err != nil {
		return gateway.User{}, gatewayErr(err)
	}
	return u, nil
}

// rekeyIfChanged gives a changed agent or workspace a new key when its
// project, budget or MCP servers changed, as the key carries them. If that
// fails, the background rounds try again.
func (s *Server) rekeyIfChanged(ctx context.Context, a store.Agent) {
	if !a.KeyStale() {
		return
	}
	if err := s.fleet.Rekey(ctx, a.ID); err != nil {
		s.log.Warn("could not replace a key; trying again later", a.Kind, a.ID, "error", err)
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
			s.log.Warn("could not start a machine", a.Kind, a.ID, "error", err)
		}
	}()
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.editableAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var in agentInput
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	if in.OwnerID != nil && *in.OwnerID != a.OwnerID {
		return badRequest("an agent's owner changes only when an administrator takes it over")
	}
	before := a
	if err := s.apply(r.Context(), p, &a, in); err != nil {
		return badRequest(err.Error())
	}
	out, err := s.st.UpdateAgent(r.Context(), a)
	if err = saved(err, "name_taken", "another agent in this organisation is called "+a.Name); err != nil {
		return err
	}
	s.record(r.Context(), p, "agent.update", out.ID, map[string]any{"name": out.Name})
	// Instructions and the model reach the runtime at its next task.
	s.rekeyIfChanged(r.Context(), out)
	if out.Enabled && (before.Image != out.Image || before.Class != out.Class) {
		s.startLater(out)
	}
	return s.writeAgent(w, r, p, out)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.editableAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := s.fleet.Delete(r.Context(), a); err != nil {
		return runtimeErr(err)
	}
	s.record(r.Context(), p, "agent.delete", a.ID, map[string]any{"name": a.Name})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// setEnabled starts, stops or restarts an agent. A restart replaces the
// container and keeps its files.
func (s *Server) setEnabled(enabled, restart bool) handler {
	return func(w http.ResponseWriter, r *http.Request, p Principal) error {
		a, err := s.editableAgent(r, p, r.PathValue("id"))
		if err != nil {
			return err
		}
		out, err := s.setMachineEnabled(r, p, a, enabled, restart)
		if err != nil {
			return err
		}
		return s.writeAgent(w, r, p, out)
	}
}

// setMachineEnabled starts, stops or restarts an agent or a workspace.
func (s *Server) setMachineEnabled(r *http.Request, p Principal, a store.Agent,
	enabled, restart bool,
) (store.Agent, error) {
	if !s.fleet.Runs() && enabled {
		return a, refuse(http.StatusConflict, "no_runtime", fleet.ErrNoRuntime.Error())
	}
	out, err := s.st.SetEnabled(r.Context(), a.ID, enabled)
	if err != nil {
		return a, err
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
		return a, runtimeErr(err)
	}
	out, _ = s.st.Agent(r.Context(), a.ID)
	return out, nil
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
func (s *Server) takeOverAgent(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.loadAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !p.CanAdmin() {
		return forbid("only an administrator can take over an agent")
	}
	if err := s.st.SetOwner(r.Context(), a.ID, p.UserID, p.Email, p.Name); err != nil {
		return err
	}
	// The agent is taken over once the owner is saved. A key the gateway
	// could not replace now stays stale, and RekeyStale replaces it soon.
	if err := s.fleet.Rekey(r.Context(), a.ID); err != nil {
		s.log.Warn("could not replace a key; trying again later", "agent", a.ID, "error", err)
	}
	_ = s.st.SetAgentState(r.Context(), a.ID, a.State, "")
	s.record(r.Context(), p, "agent.take_over", a.ID, map[string]string{"previous_owner": a.OwnerEmail})
	out, err := s.st.Agent(r.Context(), a.ID)
	if err != nil {
		return err
	}
	return s.writeAgent(w, r, p, out)
}

func (s *Server) agentLogs(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.editableAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.streamLogs(w, r, a)
}

// streamLogs answers with a machine's output, and with ?follow=1 what it
// prints next. Once the output has started, it returns nil: the status is
// already sent.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, a store.Agent) error {
	follow, _ := strconv.ParseBool(r.URL.Query().Get("follow"))
	rc, err := s.fleet.Logs(r.Context(), a.ID, follow)
	if errors.Is(err, runtime.ErrNotFound) {
		return refuse(http.StatusNotFound, "no_machine", "this "+a.Kind+" has no running machine to read")
	}
	if err != nil {
		return runtimeErr(err)
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
				return nil
			}
			_ = rw.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && r.Context().Err() == nil {
				s.log.Debug("log stream ended", "agent", a.ID, "error", err)
			}
			return nil
		}
	}
}

// giveTask queues a task typed in the panel. The agent's access rule
// applies, as for a chat message.
func (s *Server) giveTask(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.givableAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
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
		return submitErr(err)
	}
	return writeJSON(w, http.StatusCreated, t)
}

// submitErr is why a task could not be queued.
func submitErr(err error) error {
	if errors.Is(err, dispatch.ErrNoPrompt) {
		return badRequest(err.Error())
	}
	return err
}

// ------------------------------------------------------------ bindings

func (s *Server) createBinding(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.editableAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.bind(w, r, p, a)
}

// bind connects an agent or a workspace to a connection.
func (s *Server) bind(w http.ResponseWriter, r *http.Request, p Principal, a store.Agent) error {
	var in struct {
		ConnectionID string `json:"connection_id"`
		Target       string `json:"target"`
		Label        string `json:"label"`
		Mode         string `json:"mode"`
		Prompt       string `json:"prompt"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	c, err := s.loadConnection(r, p, in.ConnectionID)
	if err != nil {
		return err
	}
	desc, _ := connectors.Describe(connectors.Kind(c.Kind))
	in.Target = strings.TrimSpace(in.Target)
	apiMode := in.Mode == store.ModeRead || in.Mode == store.ModeWrite
	// "*" is every chat a bot is in, or all an API connector reaches.
	anyAllowed := in.Mode == store.ModeChat || apiMode && (desc.AnyTarget || desc.TargetLabel == "")
	// An API kind with nothing to narrow to reaches everything.
	if desc.API && desc.TargetLabel == "" {
		in.Target = connectors.AnyTarget
	}
	switch {
	case !slices.Contains([]string{
		store.ModeChat, store.ModeWatch, store.ModeFiles, store.ModeRepo, store.ModeRead, store.ModeWrite,
	}, in.Mode):
		return badRequest("mode must be chat, watch, files, repo, read or write")
	case a.Kind == store.KindWorkspace && (in.Mode == store.ModeChat || in.Mode == store.ModeWatch):
		// Both give work, and a workspace takes none.
		return badRequest("a workspace takes no tasks, so it connects with files, repo, read or write")
	case in.Target == "":
		return badRequest("name the " + strings.ToLower(desc.TargetLabel))
	case in.Mode == store.ModeChat && !desc.Chat:
		return badRequest(desc.Label + " is not a chat")
	case (in.Mode == store.ModeWatch || in.Mode == store.ModeFiles) && !desc.Files:
		return badRequest(desc.Label + " holds no files")
	case (in.Mode == store.ModeRepo) != desc.Code:
		return badRequest(desc.Label + " takes the mode repo, and only it does")
	case apiMode != desc.API:
		return badRequest(desc.Label + " takes the mode read or write, and only it does")
	case in.Target == store.AnyConversation && !anyAllowed:
		return badRequest("name the " + strings.ToLower(cmp.Or(desc.TargetLabel, "target")) + "; * is not allowed here")
	}
	// All agents answer through the same bot, so people cannot tell whose
	// agent picks up a message. A member may not join a conversation where
	// another person's agent already listens; an administrator decides that.
	if in.Mode == store.ModeChat && !p.CanAdmin() {
		holders, err := s.st.ChatHolders(r.Context(), c.ID, in.Target)
		if err != nil {
			return err
		}
		if b, ok := heldByOther(holders, a); ok {
			return forbid(b.AgentName + " already listens there; ask an administrator to add " + a.Name + " too")
		}
	}
	// Checked now, so a typo, or credentials that cannot reach it, fail
	// here and not in the middle of a task. A connector people sign in to
	// works for whoever adds the channel, so they must have signed in; the
	// lookups check that too.
	switch {
	case in.Mode == store.ModeRepo:
		repo, err := s.disp.LookupRepo(r.Context(), c, in.Target, p.UserID)
		if err != nil {
			return badRequest(err.Error())
		}
		in.Target = repo.Path
	case apiMode:
		target, err := s.disp.LookupTarget(r.Context(), c, in.Target, p.UserID)
		if err != nil {
			return badRequest(err.Error())
		}
		in.Target = target
		if err := mayNameMailbox(p, desc, c, target); err != nil {
			return forbid(err.Error())
		}
	case c.PerUser:
		if _, err := s.disp.ConnFor(r.Context(), c, p.UserID); err != nil {
			return badRequest(err.Error())
		}
	}
	b, err := s.st.CreateBinding(r.Context(), store.Binding{
		ID: id.New("bnd"), AgentID: a.ID, ConnectionID: c.ID, Target: in.Target,
		Label: trim(in.Label, 200), Mode: in.Mode, Prompt: trim(in.Prompt, 8<<10),
		CreatedBy: p.Email, CreatedByID: p.UserID,
	})
	if err = saved(err, "exists", a.Name+" is already connected there"); err != nil {
		return err
	}
	s.record(r.Context(), p, "binding.create", b.ID, map[string]string{
		"agent": a.Name, "connection": c.Name, "target": b.Target, "mode": b.Mode,
	})
	return writeJSON(w, http.StatusCreated, b)
}

// mayNameMailbox stops a member from binding someone else's mailbox or
// meetings. A shared connector acts as anyone in the domain, so without this
// a member's agent could read the mail of anyone there.
func mayNameMailbox(p Principal, desc connectors.Descriptor, c store.Connection, target string) error {
	if !desc.Mailbox || c.PerUser || p.CanAdmin() || strings.EqualFold(target, p.Email) {
		return nil
	}
	return errors.New("only an administrator can connect an agent to someone other than yourself")
}

// heldByOther returns a chat binding of an agent someone other than a's
// owner owns.
func heldByOther(holders []store.Binding, a store.Agent) (store.Binding, bool) {
	for _, b := range holders {
		if b.AgentOwnerID != a.OwnerID {
			return b, true
		}
	}
	return store.Binding{}, false
}

func (s *Server) deleteBinding(w http.ResponseWriter, r *http.Request, p Principal) error {
	b, err := s.st.Binding(r.Context(), r.PathValue("id"))
	if err = found(err, b.OrgID == p.OrgID, "binding"); err != nil {
		return err
	}
	// Another's workspace is not found, as on its own routes; another's
	// agent is only forbidden.
	a, err := s.st.Agent(r.Context(), b.AgentID)
	if err = found(err, a.OrgID == p.OrgID && (a.Kind == store.KindAgent || canEdit(p, a)), "binding"); err != nil {
		return err
	}
	if !canEdit(p, a) {
		return forbid("only " + a.OwnerEmail + " and administrators can change " + a.Name)
	}
	if err := s.st.DeleteBinding(r.Context(), b.ID); err != nil {
		return err
	}
	s.record(r.Context(), p, "binding.delete", b.ID, map[string]string{"target": b.Target})
	w.WriteHeader(http.StatusNoContent)
	return nil
}
