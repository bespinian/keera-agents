package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/store"
	"github.com/bespinian/keera-agents/internal/stream"
)

// A workspace is a machine of one person's, with a coding agent, that they
// reach through the `keera` CLI. It is personal: other members do not see
// it, and only its owner connects to it. Administrators see, change, stop
// and delete every workspace in their organisation, but cannot get in.

type workspaceView struct {
	store.Agent
	CanEdit bool `json:"can_edit"`
	// CanConnect is whether the caller may open it: its owner only.
	CanConnect bool `json:"can_connect"`
}

func viewWorkspace(p Principal, a store.Agent) workspaceView {
	return workspaceView{Agent: a, CanEdit: canEdit(p, a), CanConnect: a.OwnerID == p.UserID}
}

// workspaceRef finds a workspace the caller may see, by its id or by name
// among the caller's own, which is how the CLI names one. Another member's
// is not found, as another organisation's is, so ids and names say nothing.
// Only the owner and administrators see it, so whoever does may change it.
func (s *Server) workspaceRef(r *http.Request, p Principal) (store.Agent, error) {
	ref := r.PathValue("id")
	if !strings.HasPrefix(ref, "wks_") {
		a, err := s.st.WorkspaceByName(r.Context(), p.OrgID, p.UserID, ref)
		if err != nil {
			return a, found(err, false, "workspace")
		}
		ref = a.ID
	}
	a, err := s.st.Agent(r.Context(), ref)
	visible := a.Kind == store.KindWorkspace && a.OrgID == p.OrgID && (a.OwnerID == p.UserID || p.CanAdmin())
	return a, found(err, visible, "workspace")
}

// listWorkspaces lists the caller's workspaces, or with ?all=true every one
// in the organisation, for an administrator.
func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request, p Principal) error {
	owner := p.UserID
	if all, _ := strconv.ParseBool(r.URL.Query().Get("all")); all {
		if !p.CanAdmin() {
			return forbid("only an administrator sees other people's workspaces")
		}
		owner = ""
	}
	list, err := s.st.Workspaces(r.Context(), p.OrgID, owner)
	if err != nil {
		return err
	}
	out := make([]workspaceView, 0, len(list))
	for _, a := range list {
		out = append(out, viewWorkspace(p, a))
	}
	return writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.workspaceRef(r, p)
	if err != nil {
		return err
	}
	bindings, err := s.st.AgentBindings(r.Context(), a.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		workspaceView
		Bindings []store.Binding `json:"bindings"`
	}{viewWorkspace(p, a), bindings})
}

// workspaceInput is what people set on a workspace: an agent's machine
// fields and the experience.
type workspaceInput struct {
	Name        *string        `json:"name"`
	Icon        *string        `json:"icon"`
	Description *string        `json:"description"`
	Experience  *string        `json:"experience"`
	Model       *string        `json:"model"`
	ProjectID   *string        `json:"project_id"`
	Budget      *int64         `json:"budget_micros"`
	Class       *runtime.Class `json:"class"`
	Image       *string        `json:"image"`
	// OwnerID is only read on create. A workspace is never taken over: that
	// would hand its files and keys to someone else.
	OwnerID *string `json:"owner_id"`
}

func (s *Server) applyWorkspace(ctx context.Context, p Principal, a *store.Agent, in workspaceInput) error {
	if in.Experience != nil {
		a.Experience = strings.TrimSpace(*in.Experience)
	}
	if a.Experience != store.ExperienceTerminal && a.Experience != store.ExperienceVSCode {
		return errors.New("experience must be terminal or vscode")
	}
	// The image runs with the owner's token and volume, so choosing it for
	// someone else would let an administrator in.
	if in.Image != nil && strings.TrimSpace(*in.Image) != a.Image && a.OwnerID != p.UserID {
		return errors.New("only " + a.OwnerEmail + " can change this workspace's image")
	}
	return s.apply(ctx, p, a, agentInput{
		Name: in.Name, Icon: in.Icon, Description: in.Description, Model: in.Model,
		ProjectID: in.ProjectID, BudgetMicros: in.Budget, Class: in.Class, Image: in.Image,
	})
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in workspaceInput
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	a := store.Agent{
		ID: id.New("wks"), Kind: store.KindWorkspace, OrgID: p.OrgID, OwnerID: p.UserID, OwnerEmail: p.Email,
		OwnerName: p.Name, Access: store.AccessOwner, Enabled: true,
	}
	// The owner comes first: whether an image may be chosen depends on it.
	if in.OwnerID != nil && *in.OwnerID != "" && *in.OwnerID != p.UserID {
		u, err := s.newOwner(r, p, *in.OwnerID)
		if err != nil {
			return err
		}
		a.OwnerID, a.OwnerEmail, a.OwnerName = u.ID, u.Email, u.Name
	}
	if in.Model == nil {
		// The coding agent lets its user pick any model the key may use;
		// this is only the one it starts with, from the chosen project.
		// Only an administrator chooses one, which apply checks.
		project := ""
		if in.ProjectID != nil && p.CanAdmin() {
			project = *in.ProjectID
		}
		cat, err := s.gw.Catalog(r.Context(), p.OrgID, project)
		if err != nil {
			return gatewayErr(err)
		}
		i := slices.IndexFunc(cat.Models, gateway.CatalogRef.CallsTools)
		if i < 0 {
			return badRequest("your project allows no model that calls tools; ask an administrator")
		}
		in.Model = &cat.Models[i].Alias
	}
	in.Class = s.newClass(in.Class)
	if err := s.applyWorkspace(r.Context(), p, &a, in); err != nil {
		return badRequest(err.Error())
	}
	out, err := s.st.CreateAgent(r.Context(), a)
	if err = saved(err, "name_taken", nameTaken(p, a)); err != nil {
		return err
	}
	s.record(r.Context(), p, "workspace.create", out.ID, map[string]any{
		"name": out.Name, "experience": out.Experience, "owner": out.OwnerEmail,
	})
	s.startLater(out)
	return writeJSON(w, http.StatusCreated, viewWorkspace(p, out))
}

// nameTaken says whose workspace already has a's name: names are unique per
// owner, and an administrator may be acting for someone else.
func nameTaken(p Principal, a store.Agent) string {
	if a.OwnerID == p.UserID {
		return "you already have a workspace called " + a.Name
	}
	return a.OwnerEmail + " already has a workspace called " + a.Name
}

func (s *Server) updateWorkspace(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.workspaceRef(r, p)
	if err != nil {
		return err
	}
	var in workspaceInput
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	if in.OwnerID != nil && *in.OwnerID != a.OwnerID {
		return badRequest("a workspace's owner never changes")
	}
	before := a
	if err := s.applyWorkspace(r.Context(), p, &a, in); err != nil {
		return badRequest(err.Error())
	}
	out, err := s.st.UpdateAgent(r.Context(), a)
	if err = saved(err, "name_taken", nameTaken(p, a)); err != nil {
		return err
	}
	s.record(r.Context(), p, "workspace.update", out.ID, map[string]any{"name": out.Name})
	s.rekeyIfChanged(r.Context(), out)
	// The runtime starts the experience once, so a new one needs a new
	// container, as a new image or machine does. The volume stays.
	if out.Enabled && (before.Image != out.Image || before.Class != out.Class || before.Experience != out.Experience) {
		s.startLater(out)
	}
	return writeJSON(w, http.StatusOK, viewWorkspace(p, out))
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.workspaceRef(r, p)
	if err != nil {
		return err
	}
	if err := s.fleet.Delete(r.Context(), a); err != nil {
		return runtimeErr(err)
	}
	s.record(r.Context(), p, "workspace.delete", a.ID, map[string]any{"name": a.Name})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// setWorkspaceEnabled starts or stops a workspace.
func (s *Server) setWorkspaceEnabled(enabled bool) handler {
	return func(w http.ResponseWriter, r *http.Request, p Principal) error {
		a, err := s.workspaceRef(r, p)
		if err != nil {
			return err
		}
		out, err := s.setMachineEnabled(r, p, a, enabled, false)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, viewWorkspace(p, out))
	}
}

func (s *Server) workspaceLogs(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.workspaceRef(r, p)
	if err != nil {
		return err
	}
	return s.streamLogs(w, r, a)
}

func (s *Server) createWorkspaceBinding(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.workspaceRef(r, p)
	if err != nil {
		return err
	}
	return s.bind(w, r, p, a)
}

// ------------------------------------------------------------ connect

// frontDial bounds how long connect waits for a front that was just started
// to listen.
const frontDial = 30 * time.Second

// connectWorkspace passes the caller's connection through to a port in
// their workspace: ?port=web for its experience, ssh for its SSH server, or
// a number for anything else listening inside. It wakes a suspended
// workspace and waits for it.
//
// Only the owner connects. An administrator can stop or delete a workspace,
// but what is inside is its owner's.
func (s *Server) connectWorkspace(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.workspaceRef(r, p)
	if err != nil {
		return err
	}
	if a.OwnerID != p.UserID {
		return forbid("only " + a.OwnerEmail + " can connect to " + a.Name)
	}
	port := r.URL.Query().Get("port")
	if _, ok := protocol.Port(port); !ok {
		return badRequest("port must be web, ssh or a number from 1 to 65535")
	}
	if !stream.IsUpgrade(r) {
		return refuse(http.StatusUpgradeRequired, "upgrade_required",
			"connect with the keera CLI: keera workspace open "+shellArg(a.Name))
	}
	ctx := r.Context()
	addr, a, err := s.fleet.Reach(ctx, a.ID)
	switch {
	case errors.Is(err, fleet.ErrStopped):
		return refuse(http.StatusConflict, "workspace_stopped",
			a.Name+" is stopped; start it with: keera workspace start "+shellArg(a.Name))
	case err != nil:
		return runtimeErr(err)
	}
	front, err := s.dialFront(ctx, addr, s.fleet.IngressSecret(a), port)
	if err != nil {
		return runtimeErr(err)
	}
	client, err := stream.Accept(w, r)
	if err != nil {
		_ = front.Close()
		s.log.Warn("could not take over a connection to a workspace", "workspace", a.ID, "error", err)
		return nil
	}
	if s.connects.first(a.ID+"|"+port, time.Now()) {
		s.record(ctx, p, "workspace.connect", a.ID, map[string]string{"port": port})
	}

	// While a connection is open the workspace is in use, so it is not
	// suspended as idle.
	_ = s.st.TouchAgent(ctx, a.ID)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = s.st.TouchAgent(context.WithoutCancel(ctx), a.ID)
			}
		}
	}()
	stream.Join(client, front)
	close(done)
	_ = s.st.TouchAgent(context.WithoutCancel(ctx), a.ID)
	return nil
}

// dialFront opens a stream to port through the workspace's front. A front
// that was just started may not listen yet, so a failed connection is tried
// again for a while. An answer from the front is final.
func (s *Server) dialFront(ctx context.Context, addr, secret, port string) (io.ReadWriteCloser, error) {
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 20 * time.Second,
			DisableKeepAlives:     true,
		},
		// The owner chooses the workspace's image, so its front may answer
		// with a redirect to anywhere inside. Following it would fetch that
		// for them, and the answer's body comes back in the error.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.Now().Add(frontDial)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://"+addr+"/?port="+url.QueryEscape(port), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		conn, err := stream.Dial(hc, req)
		if err == nil {
			return conn, nil
		}
		if _, answered := errors.AsType[*stream.Error](err); answered || time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// connectLog keeps the audit log to one entry per workspace and port an
// hour: a browser opens many connections for one page. It is per replica.
type connectLog struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const connectLogEvery = time.Hour

// first reports whether key was not seen within the last hour, and notes it.
func (l *connectLog) first(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]time.Time{}
	}
	if at, ok := l.seen[key]; ok && now.Sub(at) < connectLogEvery {
		return false
	}
	if len(l.seen) > 10000 {
		for k, at := range l.seen {
			if now.Sub(at) >= connectLogEvery {
				delete(l.seen, k)
			}
		}
	}
	l.seen[key] = now
	return true
}

// shellArg quotes a name for a command we suggest, so a name with spaces stays
// one argument. Plain names stay as they are, to keep commands readable.
func shellArg(s string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("._/:@%+=,-", r)
	}
	plain := s != "" && strings.IndexFunc(s, func(r rune) bool { return !safe(r) }) < 0
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
