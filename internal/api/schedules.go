package api

import (
	"context"
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

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, p Principal) error {
	list, err := s.st.Schedules(r.Context(), p.OrgID, r.URL.Query().Get("agent_id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"data": viewSchedules(list)})
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
func (s *Server) applySchedule(ctx context.Context, sc *store.Schedule, in scheduleInput) error {
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
			b, err := s.st.Binding(ctx, *in.ReplyBindingID)
			if err != nil || b.AgentID != sc.AgentID || b.Mode != store.ModeChat {
				return errors.New("the results can only go to one of the agent's chats")
			}
			if b.Target == store.AnyConversation {
				return errors.New("that chat takes every conversation, so it names none to post in; " +
					"connect the agent to one chat and choose that")
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

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in scheduleInput
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	if in.AgentID == nil {
		return badRequest("choose an agent")
	}
	a, err := s.editableAgent(r, p, *in.AgentID)
	if err != nil {
		return err
	}
	sc := store.Schedule{
		ID: id.New("sch"), AgentID: a.ID, Enabled: true, CreatedBy: p.Email,
		RequestedBy: p.Email, RequestedByID: p.UserID,
	}
	if err := s.applySchedule(r.Context(), &sc, in); err != nil {
		return badRequest(err.Error())
	}
	out, err := s.st.CreateSchedule(r.Context(), sc)
	if err != nil {
		return err
	}
	s.record(r.Context(), p, "schedule.create", out.ID, map[string]string{"agent": a.Name, "cron": out.Cron})
	return writeJSON(w, http.StatusCreated, viewSchedule(out))
}

// loadSchedule finds a schedule the caller may change, and its agent.
func (s *Server) loadSchedule(r *http.Request, p Principal) (store.Schedule, store.Agent, error) {
	sc, err := s.st.Schedule(r.Context(), r.PathValue("id"))
	if err := found(err, sc.OrgID == p.OrgID, "schedule"); err != nil {
		return sc, store.Agent{}, err
	}
	a, err := s.editableAgent(r, p, sc.AgentID)
	return sc, a, err
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request, p Principal) error {
	sc, _, err := s.loadSchedule(r, p)
	if err != nil {
		return err
	}
	var in scheduleInput
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	if in.AgentID != nil && *in.AgentID != sc.AgentID {
		return badRequest("a schedule stays with its agent; create a new one for another agent")
	}
	if err := s.applySchedule(r.Context(), &sc, in); err != nil {
		return badRequest(err.Error())
	}
	// It runs for whoever changed it last, so nobody can borrow the rights
	// of the person who set it up.
	sc.RequestedBy, sc.RequestedByID = p.Email, p.UserID
	out, err := s.st.UpdateSchedule(r.Context(), sc)
	if err != nil {
		return err
	}
	s.record(r.Context(), p, "schedule.update", out.ID, map[string]any{"cron": out.Cron, "enabled": out.Enabled})
	return writeJSON(w, http.StatusOK, viewSchedule(out))
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, p Principal) error {
	sc, _, err := s.loadSchedule(r, p)
	if err != nil {
		return err
	}
	if err := s.st.DeleteSchedule(r.Context(), sc.ID); err != nil {
		return err
	}
	s.record(r.Context(), p, "schedule.delete", sc.ID, map[string]string{"name": sc.Name})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request, p Principal) error {
	sc, a, err := s.loadSchedule(r, p)
	if err != nil {
		return err
	}
	if !a.Enabled {
		return agentStopped(p, a)
	}
	// Run now, it runs for whoever asked.
	sc.RequestedBy, sc.RequestedByID = p.Email, p.UserID
	t, err := s.disp.SubmitSchedule(r.Context(), sc)
	if err != nil {
		return submitErr(err)
	}
	return writeJSON(w, http.StatusCreated, t)
}

// previewSchedule says when an expression runs, so the panel can show it
// while someone types.
func (s *Server) previewSchedule(w http.ResponseWriter, r *http.Request, _ Principal) error {
	var in struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	c, err := schedule.Parse(in.Cron)
	if err != nil {
		return badRequest(err.Error())
	}
	if in.Timezone == "" {
		in.Timezone = defaultZone
	}
	loc, err := schedule.Zone(in.Timezone)
	if err != nil {
		return badRequest(err.Error())
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
		return badRequest(schedule.ErrNever.Error())
	}
	return writeJSON(w, http.StatusOK, map[string]any{"description": c.Describe(), "next": next})
}
