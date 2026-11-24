package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/store"
)

// Squads belong to the organisation: everyone in it may read and change
// them, as on a shared wall. Each agent's access rule still decides who may
// give it work.

func (s *Server) listSquads(w http.ResponseWriter, r *http.Request, p Principal) {
	squads, err := s.st.Squads(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": squads})
}

// squadInput is what a squad is created or changed with. A PATCH changes
// only what it gives.
type squadInput struct {
	Name    *string         `json:"name"`
	Goal    *string         `json:"goal"`
	Members *[]string       `json:"members"`
	LeadID  json.RawMessage `json:"lead_id"`
}

// maxSquadGoal bounds a goal: it goes into every card's task.
const maxSquadGoal = 4 << 10

// applySquad checks in and writes it to q.
func (s *Server) applySquad(ctx context.Context, p Principal, q *store.Squad, in squadInput) error {
	if in.Name != nil {
		q.Name = trim(*in.Name, 100)
	}
	if q.Name == "" {
		return errors.New("give the squad a name")
	}
	if in.Goal != nil {
		if len(*in.Goal) > maxSquadGoal {
			return errors.New("the goal is longer than 4 KiB")
		}
		q.Goal = strings.TrimSpace(*in.Goal)
	}
	if in.Members != nil {
		members := []store.Member{}
		for _, agentID := range *in.Members {
			if slices.ContainsFunc(members, func(m store.Member) bool { return m.ID == agentID }) {
				continue
			}
			a, err := s.st.Agent(ctx, agentID)
			if err != nil || a.OrgID != p.OrgID || a.Kind != store.KindAgent {
				return errors.New("there is no such agent")
			}
			members = append(members, store.Member{ID: a.ID})
		}
		q.Members = members
	}
	// A lead left out of the members stops being the lead; see setMembers.
	if len(in.LeadID) > 0 {
		lead, err := optionalID(in.LeadID, "lead_id")
		if err != nil {
			return err
		}
		if lead != nil && !q.Has(*lead) {
			return errors.New("the lead must be a member")
		}
		q.LeadID = lead
	}
	return nil
}

// optionalID reads an id, or null; "" is null too.
func optionalID(raw json.RawMessage, field string) (*string, error) {
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errors.New(field + " must be an id or null")
	}
	if v != nil && *v == "" {
		return nil, nil
	}
	return v, nil
}

func (s *Server) createSquad(w http.ResponseWriter, r *http.Request, p Principal) {
	var in squadInput
	if !readJSON(w, r, &in) {
		return
	}
	q := store.Squad{ID: id.New("sqd"), OrgID: p.OrgID, CreatedBy: p.Email}
	if err := s.applySquad(r.Context(), p, &q, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	q, err := s.st.CreateSquad(r.Context(), q, q.MemberIDs())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "squad.create", q.ID, map[string]string{"name": q.Name})
	writeJSON(w, http.StatusCreated, q)
}

func (s *Server) loadSquad(w http.ResponseWriter, r *http.Request, p Principal, squadID string) (store.Squad, bool) {
	q, err := s.st.Squad(r.Context(), squadID)
	return q, s.found(w, err, q.OrgID == p.OrgID, "squad")
}

func (s *Server) getSquad(w http.ResponseWriter, r *http.Request, p Principal) {
	q, ok := s.loadSquad(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	cards, err := s.st.SquadCards(r.Context(), q.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		store.Squad
		CardsList []store.Card `json:"cards_list"`
	}{q, cards})
}

func (s *Server) updateSquad(w http.ResponseWriter, r *http.Request, p Principal) {
	q, ok := s.loadSquad(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	var in squadInput
	if !readJSON(w, r, &in) {
		return
	}
	before := q
	if err := s.applySquad(r.Context(), p, &q, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.UpdateSquad(r.Context(), q, q.MemberIDs())
	if err != nil {
		s.fail(w, err)
		return
	}
	// Who joins a squad decides who agents may hand work to, so a change is
	// recorded; a save that changes nothing is not.
	if before.Name != out.Name || before.Goal != out.Goal || !sameID(before.LeadID, out.LeadID) ||
		!slices.Equal(before.MemberIDs(), out.MemberIDs()) {
		s.record(r.Context(), p, "squad.update", q.ID, map[string]string{"name": out.Name})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteSquad(w http.ResponseWriter, r *http.Request, p Principal) {
	q, ok := s.loadSquad(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.st.DeleteSquad(r.Context(), q.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "squad.delete", q.ID, map[string]string{"name": q.Name})
	w.WriteHeader(http.StatusNoContent)
}

// cardInput uses raw messages so a PATCH can tell "set to null" from "left
// out", which matters for agent_id and due_at.
type cardInput struct {
	Title    *string         `json:"title"`
	Body     *string         `json:"body"`
	Status   *string         `json:"status"`
	Position *float64        `json:"position"`
	AgentID  json.RawMessage `json:"agent_id"`
	Labels   *[]string       `json:"labels"`
	DueAt    json.RawMessage `json:"due_at"`
}

func (s *Server) applyCard(ctx context.Context, p Principal, q store.Squad, c *store.Card, in cardInput) error {
	before := *c
	if in.Title != nil {
		c.Title = trim(*in.Title, store.MaxCardTitle)
	}
	if c.Title == "" {
		return errors.New("give the card a title")
	}
	if in.Body != nil {
		if len(*in.Body) > store.MaxCardBody {
			return errors.New("the description is longer than 64 KiB")
		}
		c.Body = *in.Body
	}
	if in.Status != nil {
		if !slices.Contains(store.CardStatuses, *in.Status) {
			return errors.New("status must be backlog, todo, in_progress, review or done")
		}
		c.Status = *in.Status
	}
	if in.Position != nil {
		c.Position = *in.Position
	}
	if len(in.AgentID) > 0 {
		agentID, err := optionalID(in.AgentID, "agent_id")
		if err != nil {
			return err
		}
		if agentID != nil && !q.Has(*agentID) {
			return errors.New("the agent is not in the squad " + q.Name)
		}
		c.AgentID = agentID
	}
	if in.Labels != nil {
		var labels []string
		for _, l := range *in.Labels {
			if l = trim(l, 40); l != "" && !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
		c.Labels = labels
	}
	if len(in.DueAt) > 0 {
		var due *time.Time
		if string(in.DueAt) != "null" && strings.Trim(string(in.DueAt), `"`) != "" {
			var t time.Time
			if err := json.Unmarshal(in.DueAt, &t); err != nil {
				return errors.New("due_at must be a date and time, or null")
			}
			due = &t
		}
		c.DueAt = due
	}
	dispatch.ToLead(q, c)
	if dispatch.GivesWork(before, *c) {
		a, err := s.st.Agent(ctx, *c.AgentID)
		if err != nil {
			return errors.New("there is no such agent")
		}
		if allowed, why := dispatch.May(a, p.User()); !allowed {
			return errors.New(why)
		}
		// The task runs for whoever gave the work, so editing someone else's
		// card never borrows their rights.
		c.RequestedBy, c.RequestedByID = p.Email, p.UserID
	}
	return nil
}

func (s *Server) createCard(w http.ResponseWriter, r *http.Request, p Principal) {
	q, ok := s.loadSquad(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	var in cardInput
	if !readJSON(w, r, &in) {
		return
	}
	c := store.Card{
		ID: id.New("card"), SquadID: q.ID, Status: store.CardBacklog, CreatedBy: p.Email,
		RequestedBy: p.Email, RequestedByID: p.UserID,
	}
	if err := s.applyCard(r.Context(), p, q, &c, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.CreateCard(r.Context(), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) loadCard(w http.ResponseWriter, r *http.Request, p Principal) (store.Card, bool) {
	c, err := s.st.Card(r.Context(), r.PathValue("id"))
	return c, s.found(w, err, c.OrgID == p.OrgID, "card")
}

func (s *Server) getCard(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadCard(w, r, p)
	if !ok {
		return
	}
	comments, err := s.st.Comments(r.Context(), c.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var task *store.Task
	if c.TaskID != nil {
		// The card is the whole organisation's; its task is not.
		if t, err := s.st.Task(r.Context(), *c.TaskID); err == nil && canSeeTask(p, t) {
			task = &t
		}
	}
	writeJSON(w, http.StatusOK, struct {
		store.Card
		CommentsList []store.Comment `json:"comments_list"`
		Task         *store.Task     `json:"task"`
	}{c, comments, task})
}

func (s *Server) updateCard(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadCard(w, r, p)
	if !ok {
		return
	}
	var in cardInput
	if !readJSON(w, r, &in) {
		return
	}
	q, err := s.st.Squad(r.Context(), c.SquadID)
	if err != nil {
		s.fail(w, err)
		return
	}
	before := c
	if err := s.applyCard(r.Context(), p, q, &c, in); err != nil {
		badRequest(w, err.Error())
		return
	}
	out, err := s.st.UpdateCard(r.Context(), before, c)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteCard(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadCard(w, r, p)
	if !ok {
		return
	}
	if err := s.st.DeleteCard(r.Context(), c.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request, p Principal) {
	c, ok := s.loadCard(w, r, p)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Body = strings.TrimSpace(in.Body); in.Body == "" || len(in.Body) > store.MaxComment {
		badRequest(w, "a comment is 1 character to 16 KiB")
		return
	}
	author := p.Name
	if author == "" {
		author = p.Email
	}
	cm := store.Comment{ID: id.New("cmt"), CardID: c.ID, Author: author, Body: in.Body}
	if dispatch.CommentGivesWork(c) {
		a, err := s.st.Agent(r.Context(), *c.AgentID)
		if err != nil {
			badRequest(w, "there is no such agent")
			return
		}
		if allowed, why := dispatch.May(a, p.User()); !allowed {
			forbid(w, why)
			return
		}
		// The comment goes into the task, so the task runs for whoever
		// wrote it, never with the earlier giver's rights.
		cm.RequestedBy, cm.RequestedByID = p.Email, p.UserID
	}
	out, err := s.st.AddComment(r.Context(), cm)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// sameID reports whether two optional ids are the same.
func sameID(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
