package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/schedule"
	"github.com/bespinian/keera-agents/internal/store"
)

type scheduleView struct {
	store.Schedule
	Description string `json:"description"`
}

func viewSchedules(list []store.Schedule) []scheduleView {
	out := make([]scheduleView, 0, len(list))
	for _, sc := range list {
		out = append(out, viewSchedule(sc))
	}
	return out
}

func viewSchedule(sc store.Schedule) scheduleView {
	v := scheduleView{Schedule: sc, Description: sc.Cron}
	if c, err := schedule.Parse(sc.Cron); err == nil {
		v.Description = c.Describe()
	}
	return v
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, p Principal) {
	list, err := s.st.Schedules(r.Context(), p.OrgID, r.URL.Query().Get("agent_id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": viewSchedules(list)})
}

type scheduleInput struct {
	AgentID        *string `json:"agent_id"`
	Name           *string `json:"name"`
	Cron           *string `json:"cron"`
	Timezone       *string `json:"timezone"`
	Prompt         *string `json:"prompt"`
	ReplyBindingID *string `json:"reply_binding_id"`
	Enabled        *bool   `json:"enabled"`
}

// applySchedule checks in, writes it onto sc and works out the next run.
func (s *Server) applySchedule(r *http.Request, sc *store.Schedule, in scheduleInput) error {
	if in.Name != nil {
		sc.Name = trim(*in.Name, 100)
	}
	if sc.Name == "" {
		return errors.New("give the schedule a name")
	}
	if in.Cron != nil {
		sc.Cron = strings.TrimSpace(*in.Cron)
	}
	if in.Timezone != nil {
		sc.Timezone = strings.TrimSpace(*in.Timezone)
	}
	if sc.Timezone == "" {
		sc.Timezone = defaultZone
	}
	if in.Prompt != nil {
		sc.Prompt = strings.TrimSpace(*in.Prompt)
	}
	if sc.Prompt == "" {
		return errors.New("say what the agent should do each time")
	}
	if len(sc.Prompt) > 32<<10 {
		return errors.New("the prompt is longer than 32 KiB")
	}
	if in.ReplyBindingID != nil {
		if *in.ReplyBindingID == "" {
			sc.ReplyBindingID = nil
		} else {
			b, err := s.st.Binding(r.Context(), *in.ReplyBindingID)
			if err != nil || b.AgentID != sc.AgentID || b.Mode != store.ModeChat {
				return errors.New("the results can only go to one of the agent's chats")
			}
			if b.Target == store.AnyConversation {
				return errors.New("that chat takes every conversation, so it names none to post in; " +
					"connect the agent to one channel and choose that")
			}
			sc.ReplyBindingID = &b.ID
		}
	}
	if in.Enabled != nil {
		sc.Enabled = *in.Enabled
	}
	next, err := schedule.NextRun(sc.Cron, sc.Timezone, time.Now())
	if err != nil {
		return err
	}
	sc.NextRunAt = &next
	if !sc.Enabled {
		sc.NextRunAt = nil
	}
	return nil
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, p Principal) {
	var in scheduleInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.AgentID == nil {
		badRequest(w, "choose an agent")
		return
	}
	a, ok := s.editableAgent(w, r, p, *in.AgentID)
	if !ok {
		return
	}
	sc := store.Schedule{
		ID: id.New("sch"), AgentID: a.ID, Enabled: true, CreatedBy: p.Email, CreatedByID: p.UserID,
		RequestedBy: p.Email, RequestedByID: p.UserID,
	}
	if err := s.applySchedule(r, &sc, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.CreateSchedule(r.Context(), sc)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "schedule.create", out.ID, map[string]string{"agent": a.Name, "cron": out.Cron})
	writeJSON(w, http.StatusCreated, viewSchedule(out))
}

func (s *Server) loadSchedule(w http.ResponseWriter, r *http.Request, p Principal) (store.Schedule, bool) {
	sc, err := s.st.Schedule(r.Context(), r.PathValue("id"))
	if !s.found(w, err, sc.OrgID == p.OrgID, "schedule") {
		return sc, false
	}
	if _, ok := s.editableAgent(w, r, p, sc.AgentID); !ok {
		return sc, false
	}
	return sc, true
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request, p Principal) {
	sc, ok := s.loadSchedule(w, r, p)
	if !ok {
		return
	}
	var in scheduleInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.AgentID != nil && *in.AgentID != sc.AgentID {
		badRequest(w, "a schedule stays with its agent; create a new one for another agent")
		return
	}
	if err := s.applySchedule(r, &sc, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	// It runs for whoever changed it last, so nobody can borrow the rights
	// of the person who set it up.
	sc.RequestedBy, sc.RequestedByID = p.Email, p.UserID
	out, err := s.st.UpdateSchedule(r.Context(), sc)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "schedule.update", out.ID, map[string]any{"cron": out.Cron, "enabled": out.Enabled})
	writeJSON(w, http.StatusOK, viewSchedule(out))
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, p Principal) {
	sc, ok := s.loadSchedule(w, r, p)
	if !ok {
		return
	}
	if err := s.st.DeleteSchedule(r.Context(), sc.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "schedule.delete", sc.ID, map[string]string{"name": sc.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request, p Principal) {
	sc, ok := s.loadSchedule(w, r, p)
	if !ok {
		return
	}
	a, err := s.st.Agent(r.Context(), sc.AgentID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Otherwise the task waits, and runs whenever someone starts the agent.
	if !a.Enabled {
		writeError(w, http.StatusConflict, "agent_stopped", a.Name+" is stopped; start it first")
		return
	}
	// Run now, it runs for whoever asked.
	sc.RequestedBy, sc.RequestedByID = p.Email, p.UserID
	t, err := s.disp.SubmitSchedule(r.Context(), sc)
	if err != nil {
		s.submitFailed(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// previewSchedule says when an expression runs, so the panel can show it
// while someone types.
func (s *Server) previewSchedule(w http.ResponseWriter, r *http.Request, _ Principal) {
	var in struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	c, err := schedule.Parse(in.Cron)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if in.Timezone == "" {
		in.Timezone = defaultZone
	}
	loc, err := schedule.Zone(in.Timezone)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	next := []time.Time{}
	at := time.Now()
	for range 5 {
		t, err := c.Next(at, loc)
		if err != nil {
			break
		}
		next = append(next, t)
		at = t
	}
	if len(next) == 0 {
		badRequest(w, schedule.ErrNever.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"description": c.Describe(), "next": next})
}
