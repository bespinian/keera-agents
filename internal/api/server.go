// Package api serves the panel's JSON API under /api and sign-in under
// /auth. Everyone who uses it is a person signed in through Keera Gateway;
// the gateway says who they are, which organisation they are in and whether
// they administer it. An operator sees every organisation, and names the one
// a request is for with ?org_id, as on the gateway.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/billing"
	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
)

type Options struct {
	PublicURL     string
	GatewayURL    string
	SecureCookies bool
	// Billing meters machines and asks the gateway about credit. Nil, or
	// without prices, is no billing.
	Billing *billing.Biller
	Log     *slog.Logger
}

type Server struct {
	st    *store.Store
	gw    *gateway.Client
	fleet *fleet.Manager
	disp  *dispatch.Dispatcher
	box   *secret.Box
	opts  Options
	log   *slog.Logger

	// who and orgList keep who a token is, and the organisations an
	// operator sees.
	who     tokenCache[gateway.Me]
	orgList tokenCache[[]gateway.Org]

	connects connectLog
}

// tokenCache keeps the gateway's answers about a token for a short while,
// so each page load is not a round trip to the gateway.
type tokenCache[T any] struct {
	mu sync.Mutex
	m  map[string]cached[T]
}

type cached[T any] struct {
	v  T
	at time.Time
}

const cacheTTL = 30 * time.Second

// get answers from the cache, or asks the gateway and keeps the answer.
func (c *tokenCache[T]) get(ctx context.Context, token string, ask func(context.Context, string) (T, error)) (T, error) {
	key := tokenKey(token)
	c.mu.Lock()
	hit, ok := c.m[key]
	c.mu.Unlock()
	if ok && time.Since(hit.at) < cacheTTL {
		return hit.v, nil
	}
	v, err := ask(ctx, token)
	if err != nil {
		return v, err
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) > 10000 {
		c.m = map[string]cached[T]{}
	}
	c.m[key] = cached[T]{v, time.Now()}
	c.mu.Unlock()
	return v, nil
}

func (c *tokenCache[T]) forget(token string) {
	c.mu.Lock()
	delete(c.m, tokenKey(token))
	c.mu.Unlock()
}

func New(st *store.Store, gw *gateway.Client, fl *fleet.Manager, disp *dispatch.Dispatcher, box *secret.Box, opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Server{
		st: st, gw: gw, fleet: fl, disp: disp, box: box, opts: opts, log: opts.Log,
	}
}

// Principal is the signed-in person, as the gateway sees them now.
type Principal struct {
	Token string
	gateway.Me
}

// CanAdmin reports whether they administer the organisation in view.
func (p Principal) CanAdmin() bool { return p.CanAdminOrg || p.Unrestricted }

// User is the principal as the gateway's service routes describe a person.
func (p Principal) User() gateway.User {
	return gateway.User{ID: p.UserID, Email: p.Email, Name: p.Name}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/config", s.authConfig)
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET "+callbackPath, s.callback)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("POST /auth/local", s.operatorLogin)
	mux.HandleFunc("GET "+dispatch.AccountCallbackPath, s.signInCallback)

	route := func(pattern string, h handler) { mux.Handle(pattern, s.signedIn(h)) }
	route("GET /api/v1/me", s.me)
	route("POST /api/v1/me/org", s.switchOrg)
	route("GET /api/v1/catalog", s.catalog)
	route("GET /api/v1/members", s.members)
	route("GET /api/v1/overview", s.overview)
	route("GET /api/v1/setup", s.setup)
	route("GET /api/v1/audit", s.audit)
	route("GET /api/v1/billing", s.billing)
	route("GET /api/v1/orgs", s.orgs)

	route("GET /api/v1/agents", s.listAgents)
	route("POST /api/v1/agents", s.createAgent)
	route("GET /api/v1/agents/{id}", s.getAgent)
	route("PATCH /api/v1/agents/{id}", s.updateAgent)
	route("DELETE /api/v1/agents/{id}", s.deleteAgent)
	route("POST /api/v1/agents/{id}/start", s.setEnabled(true, false))
	route("POST /api/v1/agents/{id}/stop", s.setEnabled(false, false))
	route("POST /api/v1/agents/{id}/restart", s.setEnabled(true, true))
	route("POST /api/v1/agents/{id}/take-over", s.takeOverAgent)
	route("GET /api/v1/agents/{id}/activity", s.agentActivity)
	route("GET /api/v1/agents/{id}/cost", s.agentCost)
	route("GET /api/v1/agents/{id}/logs", s.agentLogs)
	route("POST /api/v1/agents/{id}/tasks", s.giveTask)
	route("POST /api/v1/agents/{id}/bindings", s.createBinding)
	route("DELETE /api/v1/bindings/{id}", s.deleteBinding)

	// {id} is a workspace's id, or the name of one of the caller's own.
	route("GET /api/v1/workspaces", s.listWorkspaces)
	route("POST /api/v1/workspaces", s.createWorkspace)
	route("GET /api/v1/workspaces/{id}", s.getWorkspace)
	route("PATCH /api/v1/workspaces/{id}", s.updateWorkspace)
	route("DELETE /api/v1/workspaces/{id}", s.deleteWorkspace)
	route("POST /api/v1/workspaces/{id}/start", s.setWorkspaceEnabled(true))
	route("POST /api/v1/workspaces/{id}/stop", s.setWorkspaceEnabled(false))
	route("GET /api/v1/workspaces/{id}/logs", s.workspaceLogs)
	route("GET /api/v1/workspaces/{id}/cost", s.workspaceCost)
	route("POST /api/v1/workspaces/{id}/bindings", s.createWorkspaceBinding)
	route("POST /api/v1/workspaces/{id}/connect", s.connectWorkspace)

	route("GET /api/v1/me/ssh-keys", s.listSSHKeys)
	route("POST /api/v1/me/ssh-keys", s.addSSHKey)
	route("DELETE /api/v1/me/ssh-keys/{id}", s.deleteSSHKey)

	route("GET /api/v1/schedules", s.listSchedules)
	route("POST /api/v1/schedules", s.createSchedule)
	route("POST /api/v1/schedules/preview", s.previewSchedule)
	route("PATCH /api/v1/schedules/{id}", s.updateSchedule)
	route("DELETE /api/v1/schedules/{id}", s.deleteSchedule)
	route("POST /api/v1/schedules/{id}/run", s.runSchedule)

	route("GET /api/v1/squads", s.listSquads)
	route("POST /api/v1/squads", s.createSquad)
	route("GET /api/v1/squads/{id}", s.getSquad)
	route("PATCH /api/v1/squads/{id}", s.updateSquad)
	route("DELETE /api/v1/squads/{id}", s.deleteSquad)
	route("POST /api/v1/squads/{id}/cards", s.createCard)
	route("GET /api/v1/cards/{id}", s.getCard)
	route("PATCH /api/v1/cards/{id}", s.updateCard)
	route("DELETE /api/v1/cards/{id}", s.deleteCard)
	route("POST /api/v1/cards/{id}/comments", s.addComment)

	route("GET /api/v1/chat/agents", s.chatAgents)
	route("POST /api/v1/chat/agents/{id}/messages", s.chatMessage)

	route("GET /api/v1/tasks", s.listTasks)
	route("GET /api/v1/tasks/facets", s.taskFacets)
	route("GET /api/v1/tasks/{id}", s.getTask)
	route("GET /api/v1/tasks/{id}/events", s.taskEvents)
	route("POST /api/v1/tasks/{id}/cancel", s.cancelTask)

	route("GET /api/v1/connection-kinds", s.connectionKinds)
	route("GET /api/v1/connections", s.listConnections)
	route("POST /api/v1/connections", s.createConnection)
	route("GET /api/v1/connections/{id}", s.getConnection)
	route("PATCH /api/v1/connections/{id}", s.updateConnection)
	route("DELETE /api/v1/connections/{id}", s.deleteConnection)
	route("POST /api/v1/connections/{id}/check", s.checkConnection)
	route("GET /api/v1/connections/{id}/folders", s.connectionFolders)
	route("GET /api/v1/connections/{id}/channels", s.connectionChannels)
	route("POST /api/v1/connections/{id}/account", s.startSignIn)
	route("DELETE /api/v1/connections/{id}/account", s.disconnectAccount)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "there is no such API route")
	})
	return mux
}

// handler serves a signed-in request. answer writes the response to an
// error it returns, so handlers do not write their failures themselves.
type handler func(w http.ResponseWriter, r *http.Request, p Principal) error

// signedIn finds the person behind the session cookie, or behind a gateway
// sign-in sent as a bearer token, as Keera Chat does. Requests that change
// something with the cookie must carry the CSRF token too. A browser never
// adds a bearer token on its own, so that needs none.
func (s *Server) signedIn(next handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, bearer := bearerToken(r)
		if !bearer {
			sess, ok := s.readSession(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
				return
			}
			token = sess.Token
		}
		me, err := s.whoIs(r.Context(), token)
		if err != nil {
			if gateway.IsUnauthorized(err) {
				if !bearer {
					s.clearCookie(w, sessionCookie, "/")
				}
				writeError(w, http.StatusUnauthorized, "unauthenticated", "your sign-in has ended; sign in again")
				return
			}
			s.answer(w, gatewayErr(err))
			return
		}
		if !bearer && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get("X-CSRF-Token") != csrfFor(token) {
			writeError(w, http.StatusForbidden, "csrf", "reload the page and try again")
			return
		}
		p := Principal{Token: token, Me: me}
		if err := s.viewOrg(r, &p); err != nil {
			s.answer(w, err)
			return
		}
		if p.OrgID == "" {
			writeError(w, http.StatusForbidden, "no_org",
				"you are not in an organisation; ask an administrator to add you in Keera Gateway")
			return
		}
		if err := next(w, r, p); err != nil {
			s.answer(w, err)
		}
	})
}

// viewOrg puts an operator in the organisation ?org_id names, so every
// route works on it as on their own. Anyone else naming another is refused,
// not quietly given their own.
func (s *Server) viewOrg(r *http.Request, p *Principal) error {
	want := r.URL.Query().Get("org_id")
	if want == "" || want == p.OrgID {
		return nil
	}
	if !p.Unrestricted {
		return forbid("you can only see your own organisation; switch to the other one first")
	}
	orgs, err := s.orgList.get(r.Context(), p.Token, s.gw.Orgs)
	if err != nil {
		return gatewayErr(err)
	}
	i := slices.IndexFunc(orgs, func(o gateway.Org) bool { return o.ID == want })
	if i < 0 {
		return notFound("organisation")
	}
	p.OrgID, p.OrgName = orgs[i].ID, orgs[i].Name
	return nil
}

// bearerToken reads "Authorization: Bearer <token>".
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

func (s *Server) whoIs(ctx context.Context, token string) (gateway.Me, error) {
	return s.who.get(ctx, token, s.gw.Me)
}

func (s *Server) forget(token string) {
	s.who.forget(token)
	s.orgList.forget(token)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, p Principal) error {
	out := map[string]any{
		"user_id": p.UserID, "email": p.Email, "name": p.Name, "role": p.Role,
		"org_id": p.OrgID, "org_name": p.OrgName, "can_admin": p.CanAdmin(),
		"csrf": csrfFor(p.Token), "gateway_url": s.opts.GatewayURL,
		// An operator sees every organisation, and picks one in the panel.
		"unrestricted": p.Unrestricted,
		// Where people chat with agents, so the panel can link to it.
		"chat_url": p.ChatURL,
		"runtime": map[string]any{
			"driver": s.fleet.DriverName(), "tiers": s.fleet.Tiers(),
		},
		"builtins":         protocol.Builtins,
		"timezone_default": defaultZone,
		// The forms show it in the sizes left empty, and price it.
		"machine_default": runtime.DefaultClass,
	}
	var orgs []map[string]string
	for _, m := range p.Orgs {
		if !m.Invited {
			orgs = append(orgs, map[string]string{"id": m.ID, "name": m.Name})
		}
	}
	if len(orgs) > 1 {
		out["orgs"] = orgs
	}
	if b := s.opts.Billing; b.On() {
		out["billing"] = true
		// The forms price the size being chosen. Anyone may know the prices.
		out["prices"] = b.Prices()
		// Every screen says why no machine starts. Everyone sees it, since
		// it stops everyone's.
		if c, ok := b.Credit(r.Context(), p.OrgID); ok && c.Refusal != nil {
			out["credit_refusal"] = c.Refusal
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) switchOrg(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in struct {
		OrgID string `json:"org_id"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	if err := s.gw.SwitchOrg(r.Context(), p.Token, in.OrgID); err != nil {
		return gatewayErr(err)
	}
	s.forget(p.Token)
	return writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// catalog is what an agent in a project may use. Only an administrator
// chooses a project; a member's agents go in the organisation's default one,
// as a member's keys do in the gateway.
func (s *Server) catalog(w http.ResponseWriter, r *http.Request, p Principal) error {
	project := r.URL.Query().Get("project_id")
	if !p.CanAdmin() {
		project = ""
	}
	cat, err := s.gw.Catalog(r.Context(), p.OrgID, project)
	if err != nil {
		return gatewayErr(err)
	}
	if !p.CanAdmin() {
		cat.Projects = nil
	}
	if cat.Projects == nil {
		cat.Projects = []gateway.Project{}
	}
	if cat.Models == nil {
		cat.Models = []gateway.CatalogRef{}
	}
	if cat.TranscriptionModels == nil {
		cat.TranscriptionModels = []gateway.CatalogRef{}
	}
	if cat.MCPServers == nil {
		cat.MCPServers = []gateway.MCPRef{}
	}
	return writeJSON(w, http.StatusOK, cat)
}

// members lists the people an administrator can create an agent for.
func (s *Server) members(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.CanAdmin() {
		return forbid("only administrators see the organisation's members")
	}
	users, err := s.gw.Members(r.Context(), p.Token, p.OrgID)
	if err != nil {
		return gatewayErr(err)
	}
	return writeJSON(w, http.StatusOK, map[string]any{"data": users})
}

// orgs is an operator's overview: every organisation, with its machines and
// its tasks this week.
func (s *Server) orgs(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.Unrestricted {
		return forbid("only an operator sees every organisation")
	}
	ctx := r.Context()
	orgs, err := s.orgList.get(ctx, p.Token, s.gw.Orgs)
	if err != nil {
		return gatewayErr(err)
	}
	since := time.Now().Add(-7 * 24 * time.Hour)
	sums, err := s.st.OrgSummaries(ctx, since)
	if err != nil {
		return err
	}
	type row struct {
		gateway.Org
		store.OrgSummary
	}
	out := make([]row, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, row{o, sums[o.ID]})
	}
	return writeJSON(w, http.StatusOK, map[string]any{"data": out, "since": since})
}

// defaultZone is the time zone of schedules that name none, and the one the
// dashboard's days are counted in.
const defaultZone = "Europe/Zurich"

func (s *Server) overview(w http.ResponseWriter, r *http.Request, p Principal) error {
	ctx := r.Context()
	agents, err := s.st.Agents(ctx, p.OrgID)
	if err != nil {
		return err
	}
	counts := map[string]int{"total": len(agents), "running": 0, "suspended": 0, "stopped": 0, "failed": 0}
	for _, a := range agents {
		switch a.State {
		case store.AgentRunning, store.AgentStarting:
			counts["running"]++
		case store.AgentSuspended:
			counts["suspended"]++
		case store.AgentFailed:
			counts["failed"]++
		default:
			counts["stopped"]++
		}
	}
	act, err := s.taskActivity(r, p.OrgID, "")
	if err != nil {
		return err
	}
	recent, err := s.st.Tasks(ctx, visibleTo(p, store.TaskQuery{OrgID: p.OrgID, Limit: 7}))
	if err != nil {
		return err
	}
	upcoming, err := s.st.Upcoming(ctx, p.OrgID, 7)
	if err != nil {
		return err
	}
	next := make([]map[string]any, 0, len(upcoming))
	for _, sc := range upcoming {
		next = append(next, map[string]any{
			"schedule_id": sc.ID, "schedule_name": sc.Name, "agent_id": sc.AgentID,
			"agent_name": sc.AgentName, "next_run_at": sc.NextRunAt,
		})
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"agents": counts, "tasks": act.Tasks, "series": act.Series, "bucket": act.Bucket,
		"recent_tasks": recent, "upcoming": next,
	})
}

// setup is what the panel's getting started guide ticks off. The panel reads
// it at start, to name its first screen.
func (s *Server) setup(w http.ResponseWriter, r *http.Request, p Principal) error {
	ctx := r.Context()
	agents, err := s.st.Agents(ctx, p.OrgID)
	if err != nil {
		return err
	}
	conns, err := s.st.Connections(ctx, p.OrgID)
	if err != nil {
		return err
	}
	started, err := s.st.Started(ctx, p.OrgID, time.Now())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"agents": len(agents), "connections": len(conns), "started": started,
	})
}

// activity is what the charts show: tasks over the window the request asks
// for, counted and per hour or day.
type activity struct {
	Tasks  store.TaskCounts  `json:"tasks"`
	Series []store.TaskPoint `json:"series"`
	Bucket string            `json:"bucket"`
}

// taskActivity counts the organisation's tasks, or one agent's, over the
// window in ?since.
func (s *Server) taskActivity(r *http.Request, orgID, agentID string) (activity, error) {
	ctx := r.Context()
	window, err := time.ParseDuration(cmp.Or(r.URL.Query().Get("since"), "168h"))
	if err != nil || window < time.Hour || window > 90*24*time.Hour {
		return activity{}, badRequest("since is a duration from 1h to 2160h, such as 168h")
	}
	since := time.Now().Add(-window)
	counts, err := s.st.TaskCounts(ctx, orgID, agentID, since)
	if err != nil {
		return activity{}, err
	}
	// Hours read badly past a day, and days say little within one.
	daily := window > 48*time.Hour
	zone, _ := time.LoadLocation(defaultZone)
	series, err := s.st.TaskSeries(ctx, orgID, agentID, since, daily, zone)
	if err != nil {
		return activity{}, err
	}
	bucket := "hour"
	if daily {
		bucket = "day"
	}
	return activity{Tasks: counts, Series: series, Bucket: bucket}, nil
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.CanAdmin() {
		return forbid("only an administrator can read the audit log")
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.st.AuditLog(r.Context(), p.OrgID, before, limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"data": entries})
}

// record writes an audit entry. A failure is logged, not fatal: the change
// itself already happened.
func (s *Server) record(ctx context.Context, p Principal, action, target string, detail any) {
	if err := s.st.Audit(ctx, p.OrgID, p.Email, action, target, detail); err != nil {
		s.log.Warn("could not write an audit entry", "action", action, "error", err)
	}
}

// ------------------------------------------------------------ helpers

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return readJSONUpTo(w, r, v, 1<<20)
}

func readJSONUpTo(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return refuse(http.StatusRequestEntityTooLarge, "too_large", "the request is too large")
		}
		return badRequest("the request is not valid: " + err.Error())
	}
	return nil
}

// writeJSON always returns nil, so a handler can end with it. Once the
// status is sent, a failed write cannot be answered any more.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	typ := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		typ = "authentication_error"
	case status == http.StatusForbidden:
		typ = "permission_error"
	case status == http.StatusNotFound:
		typ = "not_found_error"
	case status >= 500:
		typ = "api_error"
	}
	_ = writeJSON(w, status, map[string]any{"error": map[string]string{"type": typ, "code": code, "message": msg}})
}

// apiError is a refusal meant for the caller: answer sends its status, code
// and words as they are.
type apiError struct {
	status    int
	code, msg string
}

func (e *apiError) Error() string { return e.msg }

func refuse(status int, code, msg string) error { return &apiError{status, code, msg} }

func badRequest(msg string) error { return refuse(http.StatusBadRequest, "invalid_request", msg) }

func notFound(what string) error {
	return refuse(http.StatusNotFound, "not_found", "there is no such "+what)
}

func forbid(msg string) error { return refuse(http.StatusForbidden, "forbidden", msg) }

// found checks a lookup of a row of the caller's organisation: another
// organisation's rows answer 404 like missing ones, and any other error is
// ours.
func found(err error, sameOrg bool, what string) error {
	switch {
	case err == nil && sameOrg:
		return nil
	case err == nil || errors.Is(err, store.ErrNotFound):
		return notFound(what)
	}
	return err
}

// saved checks a write: a conflict is 409 with code and msg, and any other
// error is ours.
func saved(err error, code, msg string) error {
	if errors.Is(err, store.ErrConflict) {
		return refuse(http.StatusConflict, code, msg)
	}
	return err
}

// gatewayError and runtimeError mark where an error came from, so answer
// passes on the gateway's or the machine's words. Only errors marked on
// purpose are, so no other error's words reach the caller by mistake.
type (
	gatewayError struct{ err error }
	runtimeError struct{ err error }
)

func (e gatewayError) Error() string { return e.err.Error() }
func (e gatewayError) Unwrap() error { return e.err }
func (e runtimeError) Error() string { return e.err.Error() }
func (e runtimeError) Unwrap() error { return e.err }

// gatewayErr marks a failed call to the gateway.
func gatewayErr(err error) error {
	if err == nil {
		return nil
	}
	return gatewayError{err}
}

// runtimeErr marks a machine that would not start or answer.
func runtimeErr(err error) error {
	if err == nil {
		return nil
	}
	return runtimeError{err}
}

// answer writes the response to a handler's error. It is the one place that
// decides what each failure looks like to the caller.
func (s *Server) answer(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if e, ok := errors.AsType[*apiError](err); ok {
		writeError(w, e.status, e.code, e.msg)
		return
	}
	// The gateway's refusal is passed on in its own words.
	if g, ok := errors.AsType[gatewayError](err); ok {
		if e, ok := errors.AsType[*gateway.Error](g.err); ok {
			status := e.Status
			if status < 400 || status == http.StatusUnauthorized {
				status = http.StatusBadGateway
			}
			writeError(w, status, cmp.Or(e.Code, "gateway_error"), e.Message)
			return
		}
	}
	// Refused for credit, a machine is 402, as the gateway answers a model
	// call then.
	if m, ok := errors.AsType[runtimeError](err); ok {
		if e, ok := errors.AsType[*billing.Refused](m.err); ok {
			writeError(w, http.StatusPaymentRequired, e.Code, e.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "runtime_error", m.err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "there is no such thing")
		return
	}
	s.log.Error("request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "something went wrong on our side; try again")
}

// trim cuts s to at most n bytes without splitting a character, which
// Postgres would refuse.
func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}
