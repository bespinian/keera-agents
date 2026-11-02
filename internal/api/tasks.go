package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/store"
)

// Tasks hold what people asked in their chats, so a member sees the tasks
// they asked for and those of agents they may change. Administrators see
// every task in the organisation.

func canSeeTask(p Principal, t store.Task) bool {
	if t.OrgID != p.OrgID {
		return false
	}
	return p.CanAdmin() || t.RequestedByID == p.UserID || t.RequestedBy == p.Email ||
		t.AgentOwnerID == p.UserID
}

// visibleTasks lists the tasks the caller may see, as canSeeTask decides,
// with the database doing the filtering.
func (s *Server) visibleTasks(ctx context.Context, p Principal, q store.TaskQuery) ([]store.Task, error) {
	if !p.CanAdmin() {
		q.ViewerID, q.ViewerEmail = p.UserID, p.Email
	}
	return s.st.Tasks(ctx, q)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, p Principal) {
	q := r.URL.Query()
	tq := store.TaskQuery{
		OrgID: p.OrgID, AgentID: q.Get("agent_id"), State: q.Get("state"), Source: q.Get("source"),
	}
	tq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			badRequest(w, "before must be a time, as created_at gives it")
			return
		}
		tq.Before = &t
	}
	tasks, err := s.visibleTasks(r.Context(), p, tq)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": tasks})
}

func (s *Server) loadTask(w http.ResponseWriter, r *http.Request, p Principal) (store.Task, bool) {
	t, err := s.st.Task(r.Context(), r.PathValue("id"))
	return t, s.found(w, err, err == nil && canSeeTask(p, t), "task")
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, p Principal) {
	t, ok := s.loadTask(w, r, p)
	if !ok {
		return
	}
	events, err := s.st.Events(r.Context(), t.ID, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		store.Task
		Events []store.Event `json:"events"`
	}{t, events})
}

func (s *Server) taskEvents(w http.ResponseWriter, r *http.Request, p Principal) {
	t, ok := s.loadTask(w, r, p)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, err := s.st.Events(r.Context(), t.ID, after)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "state": t.State, "task": t})
}

// cancelTask may be done by whoever asked, and by whoever may change the
// agent.
func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request, p Principal) {
	t, ok := s.loadTask(w, r, p)
	if !ok {
		return
	}
	if err := s.disp.Cancel(r.Context(), t, p.Email); errors.Is(err, dispatch.ErrFinished) {
		writeError(w, http.StatusConflict, "finished", err.Error())
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "task.cancel", t.ID, nil)
	t, _ = s.st.Task(r.Context(), t.ID)
	writeJSON(w, http.StatusOK, t)
}
