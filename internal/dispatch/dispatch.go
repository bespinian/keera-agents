// Package dispatch turns what happens in the world into tasks for agents,
// and their results back into messages, comments and files.
//
// Tasks come from four places: a message in a chat an agent listens on, a
// schedule that is due, a card assigned to an agent in To do, and a new or
// changed file in a folder an agent watches. People can also give a task in the panel.
// Each task is queued on its agent, which runs one at a time; the fleet wakes
// the agent if it was suspended.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
)

type Options struct {
	// PublicURL is where the chat services send webhooks.
	PublicURL string
	// WatchInterval is how often a watched folder is checked.
	WatchInterval time.Duration
	// OnQueued is called with the agent of each queued task, so a runtime
	// waiting for work gets it at once.
	OnQueued func(agentID string)
	Log      *slog.Logger
}

type Dispatcher struct {
	st    *store.Store
	fleet *fleet.Manager
	gw    *gateway.Client
	box   *secret.Box
	opts  Options
	log   *slog.Logger
	files *grants
}

func New(st *store.Store, fl *fleet.Manager, gw *gateway.Client, box *secret.Box, opts Options) *Dispatcher {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Dispatcher{st: st, fleet: fl, gw: gw, box: box, opts: opts, log: opts.Log, files: newGrants()}
}

// HookPath is where a chat connection's webhooks arrive.
const HookPath = "/hooks/"

// WebhookURL is the address to register with the chat service.
func (d *Dispatcher) WebhookURL(connectionID string) string {
	return d.opts.PublicURL + HookPath + connectionID
}

// ------------------------------------------------------------ connections

func secretsName(connectionID string) string { return "connection:" + connectionID }

// SealSecrets seals a connection's secret fields under its id.
func (d *Dispatcher) SealSecrets(connectionID string, secrets map[string]string) []byte {
	plain, _ := json.Marshal(secrets)
	return d.box.Seal(secretsName(connectionID), plain)
}

// OpenSecrets returns a connection's secret fields.
func (d *Dispatcher) OpenSecrets(c store.Connection) (map[string]string, error) {
	out := map[string]string{}
	if len(c.Secrets) == 0 {
		return out, nil
	}
	plain, err := d.box.Open(secretsName(c.ID), c.Secrets)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(plain, &out)
	return out, err
}

// Conn turns a stored connection into what an adapter takes.
func (d *Dispatcher) Conn(c store.Connection) (channels.Connection, error) {
	secrets, err := d.OpenSecrets(c)
	if err != nil {
		return channels.Connection{}, fmt.Errorf("the credentials of %s cannot be read; enter them again: %w",
			c.Name, err)
	}
	return channels.Connection{
		ID: c.ID, Kind: channels.Kind(c.Kind), Config: c.Config, Secrets: secrets,
	}, nil
}

// connection loads a stored connection as an adapter takes it.
func (d *Dispatcher) connection(ctx context.Context, id string) (channels.Connection, error) {
	c, err := d.st.Connection(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return channels.Connection{}, errors.New("the connector is gone")
	}
	if err != nil {
		return channels.Connection{}, err
	}
	return d.Conn(c)
}

// binding returns one of the agent's bindings, if it is in one of modes.
func (d *Dispatcher) binding(ctx context.Context, a store.Agent, id string, modes ...string) (store.Binding, bool) {
	b, err := d.st.Binding(ctx, id)
	return b, err == nil && b.AgentID == a.ID && slices.Contains(modes, b.Mode)
}

// bound returns one of the agent's bindings in one of modes, with its
// adapter and connection. noun names such bindings for the model.
func bound[T any](ctx context.Context, d *Dispatcher, a store.Agent, id, noun string,
	adapterFor func(channels.Kind) (T, bool), modes ...string,
) (store.Binding, T, channels.Connection, error) {
	var none T
	b, ok := d.binding(ctx, a, id, modes...)
	if !ok {
		return b, none, channels.Connection{}, fmt.Errorf("%s is not one of your %s", id, noun)
	}
	adapter, ok := adapterFor(channels.Kind(b.Kind))
	if !ok {
		return b, none, channels.Connection{}, fmt.Errorf("keera-agents cannot work with %s %s", b.Kind, noun)
	}
	conn, err := d.connection(ctx, b.ConnectionID)
	return b, adapter, conn, err
}

// ------------------------------------------------------------ tasks

var (
	// errDuplicate is a task already queued for the same event.
	errDuplicate = errors.New("dispatch: this event already made a task")
	// ErrNoPrompt and ErrFinished are the person's mistake, and say so.
	ErrNoPrompt = errors.New("a task needs something to do")
	ErrFinished = errors.New("this task has already finished")
)

// Submit queues a task and wakes its agent.
func (d *Dispatcher) Submit(ctx context.Context, t store.Task) (store.Task, error) {
	if t.ID == "" {
		t.ID = id.New("task")
	}
	t.Prompt = strings.TrimSpace(t.Prompt)
	if t.Prompt == "" {
		return store.Task{}, ErrNoPrompt
	}
	out, err := d.st.CreateTask(ctx, t)
	if errors.Is(err, store.ErrConflict) {
		return store.Task{}, errDuplicate
	}
	if err != nil {
		return store.Task{}, err
	}
	if d.opts.OnQueued != nil {
		d.opts.OnQueued(out.AgentID)
	}
	d.wake(out.AgentID)
	return out, nil
}

// wake runs in the background: a webhook must be answered within seconds,
// and starting a container can take longer.
func (d *Dispatcher) wake(agentID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := d.fleet.Wake(ctx, agentID); err != nil && !errors.Is(err, fleet.ErrNoRuntime) {
			d.log.Warn("could not wake an agent for its task", "agent", agentID, "error", err)
		}
	}()
}

// Cancel ends a task that has not finished. A running task's runtime learns
// it when it reports.
func (d *Dispatcher) Cancel(ctx context.Context, t store.Task, by string) error {
	ok, err := d.st.FinishTask(ctx, t.ID, store.TaskCancelled, "", "cancelled by "+by)
	if err != nil {
		return err
	}
	if !ok {
		return ErrFinished
	}
	return nil
}

// May reports whether a person may give the agent work, and if not, why.
// Chats, the panel, Keera Chat and the board all apply it.
func May(a store.Agent, u gateway.User) (bool, string) {
	if u.Disabled {
		return false, "your Keera account is disabled"
	}
	switch a.Access {
	case store.AccessOwner:
		if u.ID == a.OwnerID {
			return true, ""
		}
		return false, a.Name + " only takes work from " + a.OwnerEmail
	case store.AccessPeople:
		if u.ID == a.OwnerID {
			return true, ""
		}
		for _, e := range a.AllowedEmails {
			if strings.EqualFold(e, u.Email) {
				return true, ""
			}
		}
		return false, "you are not on " + a.Name + "'s list of people it takes work from; ask " + a.OwnerEmail
	default:
		return true, ""
	}
}

// GivesWork reports whether a change to a card gives its agent work, so the
// agent's access rule applies to whoever makes it. A card in To do becomes a
// task, so assigning it counts, and so do moving or rewriting an assigned one
// there.
func GivesWork(before, after store.Card) bool {
	if after.AgentID == nil {
		return false
	}
	if before.AgentID == nil || *before.AgentID != *after.AgentID {
		return true
	}
	return after.Status == store.CardTodo &&
		(before.Status != after.Status || before.Title != after.Title || before.Body != after.Body)
}

// CommentGivesWork reports whether a comment on a card gives its agent
// work: the card is about to become a task, and its comments go into the
// prompt. So the agent's access rule applies to whoever comments.
func CommentGivesWork(c store.Card) bool {
	return c.AgentID != nil && c.Status == store.CardTodo
}

// person formats who asked, for a task's context.
func person(name, email string) string {
	switch {
	case name != "" && email != "":
		return name + " <" + email + ">"
	case email != "":
		return email
	default:
		return name
	}
}
