// Package api serves the panel's JSON API under /api and sign-in under
// /auth. Everyone who uses it is a person signed in through Keera Gateway;
// the gateway says who they are, which organisation they are in and whether
// they administer it.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/billing"
	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/protocol"
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

	// whoCache keeps the gateway's answer to who a token is for a short
	// while, so each page load is not a round trip to the gateway.
	whoMu    sync.Mutex
	whoCache map[string]cachedWho

	connects connectLog
}

type cachedWho struct {
	me gateway.Me
	at time.Time
}

const whoTTL = 30 * time.Second

func New(st *store.Store, gw *gateway.Client, fl *fleet.Manager, disp *dispatch.Dispatcher, box *secret.Box, opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Server{
		st: st, gw: gw, fleet: fl, disp: disp, box: box, opts: opts, log: opts.Log,
		whoCache: map[string]cachedWho{},
	}
}

// Principal is the signed-in person, as the gateway sees them now.
type Principal struct {
	Token string
	gateway.Me
}

// CanAdmin reports whether they administer their current organisation.
func (p Principal) CanAdmin() bool { return p.CanAdminOrg || p.Role == "operator" }

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

	route("GET /api/v1/boards", s.listBoards)
	route("POST /api/v1/boards", s.createBoard)
	route("GET /api/v1/boards/{id}", s.getBoard)
	route("PATCH /api/v1/boards/{id}", s.renameBoard)
	route("DELETE /api/v1/boards/{id}", s.deleteBoard)
	route("POST /api/v1/boards/{id}/cards", s.createCard)
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
	route("POST /api/v1/connections/{id}/account", s.startSignIn)
	route("DELETE /api/v1/connections/{id}/account", s.disconnectAccount)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "there is no such API route")
	})
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, p Principal)

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
			s.gatewayFailed(w, err)
			return
		}
		if !bearer && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get("X-CSRF-Token") != csrfFor(token) {
			writeError(w, http.StatusForbidden, "csrf", "reload the page and try again")
			return
		}
		if me.OrgID == "" {
			writeError(w, http.StatusForbidden, "no_org",
				"you are not in an organisation; ask an administrator to add you in Keera Gateway")
			return
		}
		next(w, r, Principal{Token: token, Me: me})
	})
}

// bearerToken reads "Authorization: Bearer <token>".
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

func (s *Server) whoIs(ctx context.Context, token string) (gateway.Me, error) {
	key := tokenKey(token)
	s.whoMu.Lock()
	c, ok := s.whoCache[key]
	s.whoMu.Unlock()
	if ok && time.Since(c.at) < whoTTL {
		return c.me, nil
	}
	me, err := s.gw.Me(ctx, token)
	if err != nil {
		return me, err
	}
	s.whoMu.Lock()
	if len(s.whoCache) > 10000 {
		clear(s.whoCache)
	}
	s.whoCache[key] = cachedWho{me: me, at: time.Now()}
	s.whoMu.Unlock()
	return me, nil
}

func (s *Server) forget(token string) {
	s.whoMu.Lock()
	delete(s.whoCache, tokenKey(token))
	s.whoMu.Unlock()
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, p Principal) {
	out := map[string]any{
		"user_id": p.UserID, "email": p.Email, "name": p.Name, "role": p.Role,
		"org_id": p.OrgID, "org_name": p.OrgName, "can_admin": p.CanAdmin(),
		"csrf": csrfFor(p.Token), "gateway_url": s.opts.GatewayURL,
		// Where people chat with agents, so the panel can link to it.
		"chat_url": p.ChatURL,
		"runtime": map[string]any{
			"driver": s.fleet.DriverName(), "tiers": s.fleet.Tiers(),
		},
		"builtins":         protocol.Builtins,
		"timezone_default": defaultZone,
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
		// Every screen says why no machine starts. Everyone sees it, since
		// it stops everyone's.
		if c, ok := b.Credit(r.Context(), p.OrgID); ok && c.Refusal != nil {
			out["credit_refusal"] = c.Refusal
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) switchOrg(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		OrgID string `json:"org_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.gw.SwitchOrg(r.Context(), p.Token, in.OrgID); err != nil {
		s.gatewayFailed(w, err)
		return
	}
	s.forget(p.Token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// catalog is what an agent in a project may use. Only an administrator
// chooses a project; a member's agents go in the organisation's default one,
// as a member's keys do in the gateway.
func (s *Server) catalog(w http.ResponseWriter, r *http.Request, p Principal) {
	project := r.URL.Query().Get("project_id")
	if !p.CanAdmin() {
		project = ""
	}
	cat, err := s.gw.Catalog(r.Context(), p.OrgID, project)
	if err != nil {
		s.gatewayFailed(w, err)
		return
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
	if cat.MCPServers == nil {
		cat.MCPServers = []gateway.MCPRef{}
	}
	writeJSON(w, http.StatusOK, cat)
}

// members lists the people an administrator can create an agent for.
func (s *Server) members(w http.ResponseWriter, r *http.Request, p Principal) {
	if !p.CanAdmin() {
		forbid(w, "only administrators see the organisation's members")
		return
	}
	users, err := s.gw.Members(r.Context(), p.Token, p.OrgID)
	if err != nil {
		s.gatewayFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": users})
}

// defaultZone is the time zone of schedules that name none, and the one the
// dashboard's days are counted in.
const defaultZone = "Europe/Zurich"

func (s *Server) overview(w http.ResponseWriter, r *http.Request, p Principal) {
	ctx := r.Context()
	agents, err := s.st.Agents(ctx, p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
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
	act, ok := s.taskActivity(w, r, p.OrgID, "")
	if !ok {
		return
	}
	recent, err := s.st.Tasks(ctx, visibleTo(p, store.TaskQuery{OrgID: p.OrgID, Limit: 10}))
	if err != nil {
		s.fail(w, err)
		return
	}
	upcoming, err := s.st.Upcoming(ctx, p.OrgID, 5)
	if err != nil {
		s.fail(w, err)
		return
	}
	next := make([]map[string]any, 0, len(upcoming))
	for _, sc := range upcoming {
		next = append(next, map[string]any{
			"schedule_id": sc.ID, "schedule_name": sc.Name, "agent_id": sc.AgentID,
			"agent_name": sc.AgentName, "next_run_at": sc.NextRunAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": counts, "tasks": act.Tasks, "series": act.Series, "bucket": act.Bucket,
		"recent_tasks": recent, "upcoming": next,
	})
}

// setup is what the panel's getting started guide ticks off. The panel reads
// it at start, to name its first screen.
func (s *Server) setup(w http.ResponseWriter, r *http.Request, p Principal) {
	ctx := r.Context()
	agents, err := s.st.Agents(ctx, p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	conns, err := s.st.Connections(ctx, p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	started, err := s.st.Started(ctx, p.OrgID, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
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
// window in ?since. It answers the request itself when it fails.
func (s *Server) taskActivity(w http.ResponseWriter, r *http.Request, orgID, agentID string) (activity, bool) {
	ctx := r.Context()
	window, err := time.ParseDuration(cmp.Or(r.URL.Query().Get("since"), "168h"))
	if err != nil || window < time.Hour || window > 90*24*time.Hour {
		badRequest(w, "since is a duration from 1h to 2160h, such as 168h")
		return activity{}, false
	}
	since := time.Now().Add(-window)
	counts, err := s.st.TaskCounts(ctx, orgID, agentID, since)
	if err != nil {
		s.fail(w, err)
		return activity{}, false
	}
	// Hours read badly past a day, and days say little within one.
	daily := window > 48*time.Hour
	zone, _ := time.LoadLocation(defaultZone)
	series, err := s.st.TaskSeries(ctx, orgID, agentID, since, daily, zone)
	if err != nil {
		s.fail(w, err)
		return activity{}, false
	}
	bucket := "hour"
	if daily {
		bucket = "day"
	}
	return activity{Tasks: counts, Series: series, Bucket: bucket}, true
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, p Principal) {
	if !p.CanAdmin() {
		forbid(w, "only an administrator can read the audit log")
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.st.AuditLog(r.Context(), p.OrgID, before, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": entries})
}

// record writes an audit entry. A failure is logged, not fatal: the change
// itself already happened.
func (s *Server) record(ctx context.Context, p Principal, action, target string, detail any) {
	if err := s.st.Audit(ctx, p.OrgID, p.Email, action, target, detail); err != nil {
		s.log.Warn("could not write an audit entry", "action", action, "error", err)
	}
}

// ------------------------------------------------------------ helpers

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the request is too large")
			return false
		}
		badRequest(w, "the request is not valid: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
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
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": typ, "code": code, "message": msg}})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, "invalid_request", msg)
}

func notFound(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotFound, "not_found", "there is no such "+what)
}

// found answers a lookup that did not find a row of the caller's
// organisation: another organisation's rows answer 404 like missing ones,
// and any other error is ours.
func (s *Server) found(w http.ResponseWriter, err error, sameOrg bool, what string) bool {
	switch {
	case err == nil && sameOrg:
		return true
	case err == nil || errors.Is(err, store.ErrNotFound):
		notFound(w, what)
	default:
		s.fail(w, err)
	}
	return false
}

// saved answers a write: a conflict is 409 with code and msg, and any other
// error is ours.
func (s *Server) saved(w http.ResponseWriter, err error, code, msg string) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, code, msg)
	default:
		s.fail(w, err)
	}
	return false
}

func forbid(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusForbidden, "forbidden", msg)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "thing")
		return
	}
	s.log.Error("request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "something went wrong on our side; try again")
}

// gatewayFailed passes on the gateway's refusal in its own words.
func (s *Server) gatewayFailed(w http.ResponseWriter, err error) {
	if e, ok := errors.AsType[*gateway.Error](err); ok {
		status := e.Status
		if status < 400 || status == http.StatusUnauthorized {
			status = http.StatusBadGateway
		}
		code := e.Code
		if code == "" {
			code = "gateway_error"
		}
		writeError(w, status, code, e.Message)
		return
	}
	s.fail(w, err)
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
