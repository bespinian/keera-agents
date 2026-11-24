package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
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
	adapter, ok := connectors.ChatFor(connectors.Kind(c.Kind))
	if !ok {
		http.NotFound(w, r)
		return
	}
	conn, err := d.Conn(c)
	if err != nil {
		d.opts.Metrics.Webhook(c.Kind, "unconfigured")
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
	if errors.Is(err, connectors.ErrUnverified) {
		d.opts.Metrics.Webhook(c.Kind, "unverified")
		http.Error(w, "unverified", http.StatusUnauthorized)
		return
	}
	if err != nil {
		d.opts.Metrics.Webhook(c.Kind, "unreadable")
		d.log.Warn("could not read a webhook", "connection", c.ID, "kind", c.Kind, "error", err)
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}

	d.opts.Metrics.Webhook(c.Kind, "accepted")

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

	// The service has its answer and will not send these again, so shutdown
	// waits for them.
	for _, msg := range receipt.Messages {
		d.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			d.receive(ctx, adapter, conn, msg)
		})
	}
}

// receive turns one message into a task for the agent it is meant for.
func (d *Dispatcher) receive(ctx context.Context, adapter connectors.Chat, conn connectors.Connection, msg connectors.Inbound) {
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

	desc, _ := connectors.Describe(conn.Kind)
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
	out, err := d.Submit(ctx, task)
	switch {
	case err == nil:
		d.ackLater(adapter, conn, msg.Target, out.ID)
	case errors.Is(err, errDuplicate):
	case errors.Is(err, ErrNoPrompt):
		reply("I could not take this on: " + err.Error() + ".")
	default:
		// The error may be the database's, which is no one's business in
		// the chat.
		d.log.Warn("could not queue a task from a message", "agent", a.ID, "error", err)
		reply("I could not take this on because of a problem on my side; try again in a moment.")
	}
}

// ackAfter is how long a chat task may take before the agent says it is on
// it. Most answers come sooner, and then the thread gets only the answer.
const ackAfter = 10 * time.Second

// ackLater tells the sender the agent is on it, if the task has not finished
// after d.ackAfter. Without it, the thread stays silent while the agent
// starts up or finishes another task, and people wonder if it heard them.
// It is only a courtesy: a restart in between loses it, and nothing else
// waits for it.
func (d *Dispatcher) ackLater(adapter connectors.Chat, conn connectors.Connection, to connectors.Target, taskID string) {
	time.AfterFunc(d.ackAfter, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		t, err := d.st.Task(ctx, taskID)
		if err != nil || t.Finished() {
			return
		}
		text := "I am working on it and will answer here when I am done."
		if t.State == store.TaskQueued {
			if l, err := d.st.AgentLoad(ctx, t.AgentID); err == nil && l.RunningTaskID != "" {
				text = "I am finishing another task first. Then I will work on this and answer here."
			}
		}
		if _, err := adapter.Send(ctx, conn, to, text); err != nil {
			d.log.Warn("could not say a task is under way", "connection", conn.ID, "task", taskID, "error", err)
		}
	})
}

// route picks the agent a message is for, since several agents can share one
// bot. A message that starts with an agent's name goes to that agent. Others
// go to the first agent bound to that conversation, or else to the first one
// bound to every conversation.
func route(bindings []store.Binding, msg connectors.Inbound) (store.Binding, bool) {
	var here []store.Binding
	for _, b := range bindings {
		if bindsTo(b, msg) || b.Target == store.AnyConversation {
			here = append(here, b)
		}
	}
	if len(here) == 0 {
		return store.Binding{}, false
	}
	// The longest name wins, so an agent called "Sales" does not take
	// "Sales Bot, …" from the agent called "Sales Bot".
	named := -1
	for i, b := range here {
		if b.AgentName != "" && namePrefix(b.AgentName).MatchString(msg.Text) &&
			(named < 0 || len(b.AgentName) > len(here[named].AgentName)) {
			named = i
		}
	}
	if named >= 0 {
		return here[named], true
	}
	// An exact conversation beats the catch-all.
	for _, b := range here {
		if bindsTo(b, msg) {
			return b, true
		}
	}
	return here[0], true
}

// bindsTo reports whether a binding names the conversation a message is in.
// A binding may name a person instead, for their direct messages with the
// bot.
func bindsTo(b store.Binding, msg connectors.Inbound) bool {
	return b.Target == msg.Target.Conversation ||
		msg.Direct && msg.Sender.ID != "" && b.Target == msg.Sender.ID
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
