package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// maxHookBody bounds a webhook. Chat events are small.
const maxHookBody = 1 << 20

// ServeHook takes a chat service's webhook at /hooks/{connection id}. The
// connection's id is in the address, and the adapter checks the request came
// from the service, so a guessed address alone does nothing.
func (d *Dispatcher) ServeHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "webhooks are POST", http.StatusMethodNotAllowed)
		return
	}
	connID := strings.TrimPrefix(r.URL.Path, HookPath)
	c, err := d.st.Connection(r.Context(), connID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	adapter, ok := channels.ChatFor(channels.Kind(c.Kind))
	if !ok {
		http.NotFound(w, r)
		return
	}
	conn, err := d.Conn(c)
	if err != nil {
		d.log.Warn("a webhook arrived for a connection whose credentials cannot be read",
			"connection", c.ID, "error", err)
		http.Error(w, "this connector is not set up", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	receipt, err := adapter.Receive(r.Context(), conn, r, body)
	if errors.Is(err, channels.ErrUnverified) {
		http.Error(w, "unverified", http.StatusUnauthorized)
		return
	}
	if err != nil {
		d.log.Warn("could not read a webhook", "connection", c.ID, "kind", c.Kind, "error", err)
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}

	// Answer first: services retry a webhook that takes more than a few
	// seconds, and asking the gateway who wrote and waking agents can take
	// longer. The adapters' own short lookups happen before this.
	resp := receipt.Response
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)

	for _, msg := range receipt.Messages {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			d.receive(ctx, adapter, conn, msg)
		}()
	}
}

// receive turns one message into a task for the agent it is meant for.
func (d *Dispatcher) receive(ctx context.Context, adapter channels.Chat, conn channels.Connection, msg channels.Inbound) {
	reply := func(text string) {
		if _, err := adapter.Send(ctx, conn, msg.Target, text); err != nil {
			d.log.Warn("could not answer a message", "connection", conn.ID, "error", err)
		}
	}
	bindings, err := d.st.ChatBindings(ctx, conn.ID)
	if err != nil {
		d.log.Warn("could not read bindings", "connection", conn.ID, "error", err)
		return
	}
	b, ok := route(bindings, msg)
	if !ok {
		// Nobody listens here; stay quiet rather than answer every message
		// in a busy channel.
		return
	}
	a, err := d.st.Agent(ctx, b.AgentID)
	if err != nil {
		return
	}
	if msg.Sender.Outside {
		reply("I only take work from people in this organisation's own workspace.")
		return
	}
	if msg.Sender.Email == "" {
		reply("I could not tell who you are, so I cannot take work from you. " +
			"Ask an administrator to let the app read people's email addresses.")
		return
	}
	u, err := d.gw.UserByEmail(ctx, a.OrgID, msg.Sender.Email)
	if gateway.IsNotFound(err) {
		reply("I only take work from people in this organisation, and " + msg.Sender.Email + " is not one of them.")
		return
	}
	if err != nil {
		d.log.Warn("could not look up who sent a message", "error", err)
		reply("I cannot check who you are right now; try again in a moment.")
		return
	}
	if ok, why := May(a, u); !ok {
		reply("Sorry, " + why + ".")
		return
	}

	desc, _ := channels.Describe(channels.Kind(conn.Kind))
	where := msg.ConversationLabel
	if where == "" {
		where = msg.Target.Conversation
	}
	label := desc.Label + " " + where
	if msg.Direct {
		label = desc.Label + " direct message"
	}
	dedupe := conn.ID + ":" + msg.MessageID
	task := store.Task{
		OrgID: a.OrgID, AgentID: a.ID, Source: protocol.SourceChat,
		Origin: store.Origin{
			Label: label, ConnectionID: conn.ID, Conversation: msg.Target.Conversation,
			Thread: msg.Target.Thread, Extra: msg.Target.Extra,
		},
		Conversation:  conn.ID + ":" + msg.Target.Conversation + ":" + msg.Target.Thread,
		Prompt:        stripAgentName(msg.Text, a.Name),
		Context:       "A message from " + person(u.Name, u.Email) + " in " + label + ". Your answer is posted as the reply.",
		RequestedBy:   u.Email,
		RequestedByID: u.ID,
	}
	if msg.MessageID != "" {
		task.DedupeKey = &dedupe
	}
	if _, err := d.Submit(ctx, task); err != nil && !errors.Is(err, errDuplicate) {
		d.log.Warn("could not queue a task from a message", "agent", a.ID, "error", err)
		reply("I could not take this on: " + err.Error())
	}
}

// route picks the agent a message is for. Several agents can share one bot:
// a message that starts with an agent's name goes to that agent, one in a
// conversation only one agent listens on goes to it, and otherwise to the
// agent bound to that conversation first.
func route(bindings []store.Binding, msg channels.Inbound) (store.Binding, bool) {
	var here []store.Binding
	for _, b := range bindings {
		if b.Target == msg.Target.Conversation || b.Target == store.AnyConversation {
			here = append(here, b)
		}
	}
	if len(here) == 0 {
		return store.Binding{}, false
	}
	for _, b := range here {
		if b.AgentName != "" && namePrefix(b.AgentName).MatchString(msg.Text) {
			return b, true
		}
	}
	// An exact conversation beats the catch-all.
	for _, b := range here {
		if b.Target == msg.Target.Conversation {
			return b, true
		}
	}
	return here[0], true
}

// namePrefix matches "Name:", "Name," or "@Name" at the start of a message,
// but not "Names" or "Nameless".
func namePrefix(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*@?` + regexp.QuoteMeta(name) + `(?:[\s,:]+|$)`)
}

// stripAgentName drops the agent's name from the start of a message.
func stripAgentName(text, name string) string {
	if out := namePrefix(name).ReplaceAllString(text, ""); strings.TrimSpace(out) != "" {
		return out
	}
	return text
}
