package api

import (
	"context"
	"net/http"
	"regexp"

	"github.com/bespinian/keera-agents/internal/gateway"
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
	// Vision says its model reads images, so Keera Chat sends a PDF as its
	// pages' images, as it would to the model.
	Vision bool `json:"vision"`
}

// chatAgents lists the running or resumable agents the caller may give work
// to.
func (s *Server) chatAgents(w http.ResponseWriter, r *http.Request, p Principal) error {
	agents, err := s.st.Agents(r.Context(), p.OrgID)
	if err != nil {
		return err
	}
	out := []chatAgent{}
	vision := s.visionOf(r.Context(), p.OrgID)
	for _, a := range agents {
		if canGive(p, a) && a.Enabled {
			out = append(out, chatAgent{
				ID: a.ID, Name: a.Name, Icon: a.Icon, Description: a.Description, State: a.State,
				Vision: vision(a),
			})
		}
	}
	return writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// visionOf returns whether an agent's model reads images, reading each
// project's catalogue from the gateway once. When the gateway cannot say, it
// answers no: text reaches every model.
func (s *Server) visionOf(ctx context.Context, orgID string) func(store.Agent) bool {
	catalogs := map[string]*gateway.Catalog{}
	return func(a store.Agent) bool {
		cat, seen := catalogs[a.ProjectID]
		if !seen {
			if c, err := s.gw.Catalog(ctx, orgID, a.ProjectID); err == nil {
				cat = &c
			} else {
				s.log.Warn("could not read the models from the gateway", "project", a.ProjectID, "error", err)
			}
			catalogs[a.ProjectID] = cat
		}
		if cat == nil {
			return false
		}
		m, ok := cat.Model(a.Model)
		return ok && m.ReadsImages()
	}
}

// chatID is Keera Chat's id of a conversation.
var chatID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Server) chatMessage(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.givableAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var in struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
		protocol.Attachments
	}
	if err := readJSONUpTo(w, r, &in, protocol.MaxMessageBytes); err != nil {
		return err
	}
	if !chatID.MatchString(in.Conversation) {
		return badRequest("conversation must be 1 to 64 letters, digits, - or _")
	}
	if msg := in.Check(); msg != "" {
		return badRequest(msg)
	}
	// The model would refuse them, and the task fail with the provider's
	// words.
	if len(in.Images) > 0 && !s.visionOf(r.Context(), p.OrgID)(a) {
		return badRequest(a.Name + " reads text only, so it cannot see images")
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
		Attachments:   in.Attachments,
	})
	if err != nil {
		return submitErr(err)
	}
	return writeJSON(w, http.StatusCreated, t)
}
