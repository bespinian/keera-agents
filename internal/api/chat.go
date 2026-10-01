package api

import (
	"net/http"
	"regexp"

	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// Keera Chat lets people chat with agents. Its server calls these routes with
// the person's gateway sign-in as a bearer token. Each message is a chat task;
// Keera Chat reads its steps and result with the task routes, and the agent
// keeps the chat's history in its workspace, as for any other chat.

// chatAgent is an agent as Keera Chat offers it: no instructions, no keys.
type chatAgent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	State       string `json:"state"`
}

// chatAgents lists the running or resumable agents the caller may give work
// to.
func (s *Server) chatAgents(w http.ResponseWriter, r *http.Request, p Principal) {
	agents, err := s.st.Agents(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []chatAgent{}
	for _, a := range agents {
		if canGive(a, p) && a.Enabled {
			out = append(out, chatAgent{
				ID: a.ID, Name: a.Name, Icon: a.Icon, Description: a.Description, State: a.State,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// chatID is Keera Chat's id of a conversation.
var chatID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Server) chatMessage(w http.ResponseWriter, r *http.Request, p Principal) {
	a, ok := s.loadAgent(w, r, p, r.PathValue("id"))
	if !ok {
		return
	}
	if allowed, why := mayGive(a, p); !allowed {
		forbid(w, why)
		return
	}
	if !a.Enabled {
		writeError(w, http.StatusConflict, "agent_stopped", a.Name+" is stopped; ask "+a.OwnerEmail+" to start it")
		return
	}
	var in struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !chatID.MatchString(in.Conversation) {
		badRequest(w, "conversation must be 1 to 64 letters, digits, - or _")
		return
	}
	t, err := s.disp.Submit(r.Context(), store.Task{
		OrgID: p.OrgID, AgentID: a.ID, Source: protocol.SourceChat,
		Origin: store.Origin{Label: store.KeeraChat},
		// Per person, so nobody joins another's chat by guessing its id.
		Conversation:  "keera-chat:" + p.UserID + ":" + in.Conversation,
		Prompt:        in.Text,
		Context:       "A message from " + p.Email + " in Keera Chat. Your answer is shown there.",
		RequestedBy:   p.Email,
		RequestedByID: p.UserID,
	})
	if err != nil {
		s.submitFailed(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}
