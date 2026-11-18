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

// Boards belong to the organisation: everyone in it may read and change
// them, as on a shared wall.

func (s *Server) listBoards(w http.ResponseWriter, r *http.Request, p Principal) {
	if _, err := s.disp.EnsureBoard(r.Context(), p.OrgID, p.Email); err != nil {
		s.fail(w, err)
		return
	}
	boards, err := s.st.Boards(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": boards})
}

// readBoardName reads the name a board is created or renamed with.
func readBoardName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return "", false
	}
	if in.Name = trim(in.Name, 100); in.Name == "" {
		badRequest(w, "give the board a name")
		return "", false
	}
	return in.Name, true
}

func (s *Server) createBoard(w http.ResponseWriter, r *http.Request, p Principal) {
	name, ok := readBoardName(w, r)
	if !ok {
		return
	}
	b, err := s.st.CreateBoard(r.Context(), store.Board{ID: id.New("brd"), OrgID: p.OrgID, Name: name, CreatedBy: p.Email})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "board.create", b.ID, map[string]string{"name": b.Name})
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) loadBoard(w http.ResponseWriter, r *http.Request, p Principal, boardID string) (store.Board, bool) {
	b, err := s.st.Board(r.Context(), boardID)
	return b, s.found(w, err, b.OrgID == p.OrgID, "board")
}

func (s *Server) getBoard(w http.ResponseWriter, r *http.Request, p Principal) {
	b, ok := s.loadBoard(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	cards, err := s.st.BoardCards(r.Context(), b.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": b.ID, "name": b.Name, "cards": cards})
}

func (s *Server) renameBoard(w http.ResponseWriter, r *http.Request, p Principal) {
	b, ok := s.loadBoard(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	name, ok := readBoardName(w, r)
	if !ok {
		return
	}
	if err := s.st.RenameBoard(r.Context(), b.ID, name); err != nil {
		s.fail(w, err)
		return
	}
	b.Name = name
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) deleteBoard(w http.ResponseWriter, r *http.Request, p Principal) {
	b, ok := s.loadBoard(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.st.DeleteBoard(r.Context(), b.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.record(r.Context(), p, "board.delete", b.ID, map[string]string{"name": b.Name})
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

func (s *Server) applyCard(ctx context.Context, p Principal, c *store.Card, in cardInput) error {
	before := *c
	if in.Title != nil {
		c.Title = trim(*in.Title, 300)
	}
	if c.Title == "" {
		return errors.New("give the card a title")
	}
	if in.Body != nil {
		if len(*in.Body) > 64<<10 {
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
		var agentID *string
		if err := json.Unmarshal(in.AgentID, &agentID); err != nil {
			return errors.New("agent_id must be an id or null")
		}
		if agentID != nil && *agentID != "" {
			a, err := s.st.Agent(ctx, *agentID)
			if err != nil || a.OrgID != p.OrgID || a.Kind != store.KindAgent {
				return errors.New("there is no such agent")
			}
			c.AgentID = &a.ID
		} else {
			c.AgentID = nil
		}
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
	b, ok := s.loadBoard(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	var in cardInput
	if !readJSON(w, r, &in) {
		return
	}
	c := store.Card{
		ID: id.New("card"), BoardID: b.ID, Status: store.CardBacklog, CreatedBy: p.Email,
		RequestedBy: p.Email, RequestedByID: p.UserID,
	}
	if err := s.applyCard(r.Context(), p, &c, in); err != nil {
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
	before := c
	if err := s.applyCard(r.Context(), p, &c, in); err != nil {
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
	if in.Body = strings.TrimSpace(in.Body); in.Body == "" || len(in.Body) > 16<<10 {
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
