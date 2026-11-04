package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/metrics"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/schedule"
	"github.com/bespinian/keera-agents/internal/store"
)

// The loops below run on one replica at a time (see server), so a schedule,
// a card or a file makes one task, not one per replica. The store's
// compare-and-set claims back that up.

// RunSchedules starts every schedule that is due.
func (d *Dispatcher) RunSchedules(ctx context.Context, now time.Time) {
	due, err := d.st.DueSchedules(ctx, now)
	if err != nil {
		d.log.Warn("could not read due schedules", "error", err)
		return
	}
	for _, sc := range due {
		var nextAt *time.Time
		if next, err := schedule.NextRun(sc.Cron, sc.Timezone, now); err == nil {
			nextAt = &next
		} else {
			// As when its time zone left the zone database: it stops.
			d.log.Warn("a schedule will not run again", "schedule", sc.ID, "error", err)
		}
		won, err := d.st.ClaimRun(ctx, sc.ID, *sc.NextRunAt, nextAt)
		if err != nil || !won {
			continue
		}
		// A run missed by more than a day, as after a long outage, is
		// skipped rather than run late.
		if now.Sub(*sc.NextRunAt) > 24*time.Hour {
			continue
		}
		if _, err := d.SubmitSchedule(ctx, sc); err != nil && !errors.Is(err, errDuplicate) {
			d.log.Warn("could not queue a scheduled task", "schedule", sc.ID, "error", err)
		}
	}
}

// SubmitSchedule queues one run of a schedule, as when it is due or someone
// runs it now.
func (d *Dispatcher) SubmitSchedule(ctx context.Context, sc store.Schedule) (store.Task, error) {
	o := store.Origin{Label: "Schedule " + sc.Name, ScheduleID: sc.ID}
	if sc.ReplyBindingID != nil {
		o.ReplyBindingID = *sc.ReplyBindingID
	}
	return d.Submit(ctx, store.Task{
		OrgID: sc.OrgID, AgentID: sc.AgentID, Source: protocol.SourceSchedule, Origin: o,
		Prompt:        sc.Prompt,
		Context:       fmt.Sprintf("The schedule %q (%s), run for %s.", sc.Name, describe(sc.Cron), sc.RequestedBy),
		RequestedBy:   sc.RequestedBy,
		RequestedByID: sc.RequestedByID,
	})
}

func describe(expr string) string {
	c, err := schedule.Parse(expr)
	if err != nil {
		return expr
	}
	return strings.ToLower(c.Describe()[:1]) + c.Describe()[1:]
}

// PickUpCards starts a task for every card in To do that has an agent.
func (d *Dispatcher) PickUpCards(ctx context.Context) {
	cards, err := d.st.PickupCards(ctx)
	if err != nil {
		d.log.Warn("could not read cards to pick up", "error", err)
		return
	}
	for _, c := range cards {
		if c.AgentID == nil {
			continue
		}
		comments, _ := d.st.Comments(ctx, c.ID)
		board, _ := d.st.Board(ctx, c.BoardID)
		dedupe := "card:" + c.ID + ":" + strconv.FormatInt(c.UpdatedAt.UnixNano(), 36)
		t, err := d.Submit(ctx, store.Task{
			OrgID: c.OrgID, AgentID: *c.AgentID, Source: protocol.SourceBoard,
			Origin:        store.Origin{Label: "Card " + c.Title, CardID: c.ID},
			DedupeKey:     &dedupe,
			Prompt:        cardPrompt(c, comments),
			Context:       fmt.Sprintf("Card %s on the board %q, given to you by %s.", c.ID, board.Name, c.RequestedBy),
			RequestedBy:   c.RequestedBy,
			RequestedByID: c.RequestedByID,
		})
		if errors.Is(err, errDuplicate) {
			continue
		}
		if err != nil {
			d.log.Warn("could not queue a card", "card", c.ID, "error", err)
			continue
		}
		_ = d.st.MoveCard(ctx, c.ID, store.CardInProgress, &t.ID)
	}
}

// cardPrompt is the task a card becomes.
func cardPrompt(c store.Card, comments []store.Comment) string {
	var prompt strings.Builder
	// The title first: the task list shows a task by its first line.
	prompt.WriteString(c.Title + "\n\nThis task is a card on the board. Do what it asks.\n")
	if c.Body != "" {
		prompt.WriteString("\n" + c.Body + "\n")
	}
	if len(comments) > 0 {
		prompt.WriteString("\nComments so far:\n")
		for _, cm := range comments {
			prompt.WriteString("- " + cm.Author + ": " + cm.Body + "\n")
		}
	}
	// Weaker models otherwise comment on the card with board tools
	// instead of doing the work.
	prompt.WriteString("\nYour final answer is the result: write the work itself there, not a " +
		"summary of it. It is added to this card as a comment and the card moves to Review on " +
		"its own. Use the board tools only for other cards.")
	return prompt.String()
}

// CheckWatches reads the change feed of every watched folder that is due.
func (d *Dispatcher) CheckWatches(ctx context.Context, now time.Time) {
	watches, err := d.st.Watches(ctx)
	if err != nil {
		d.log.Warn("could not read watched folders", "error", err)
		return
	}
	for _, b := range watches {
		if b.CheckedAt != nil && now.Sub(*b.CheckedAt) < d.opts.WatchInterval {
			continue
		}
		err := d.checkWatch(ctx, b)
		d.opts.Metrics.FolderCheck(b.Kind, metrics.Outcome(err))
		if err != nil {
			d.log.Warn("could not check a watched folder", "binding", b.ID, "error", err)
			// Wait a full interval before asking again.
			_ = d.st.SetWatchCursor(ctx, b.ID, b.Cursor)
		}
	}
}

func (d *Dispatcher) checkWatch(ctx context.Context, b store.Binding) error {
	adapter, conn, err := connect(ctx, d, b.ConnectionID, "folders", channels.FilesFor)
	if err != nil {
		return err
	}
	// A watch works for whoever added it, as its tasks do.
	if conn, err = d.asPerson(ctx, conn, b.ConnectionName, b.CreatedByID, b.CreatedBy); err != nil {
		return err
	}
	files, cursor, err := adapter.Changes(ctx, conn, b.Target, b.Cursor)
	if err != nil {
		return err
	}
	a, err := d.st.Agent(ctx, b.AgentID)
	if err != nil {
		return err
	}
	desc := adapter.Descriptor()
	folder := cmp.Or(b.Label, b.Target)
	for _, f := range latest(files) {
		if f.Folder {
			continue
		}
		if own, err := d.st.Wrote(ctx, a.ID, b.ConnectionID, f.ID, f.Modified); err != nil {
			return err
		} else if own {
			continue
		}
		dedupe := b.ID + ":" + f.ID + ":" + strconv.FormatInt(f.Modified.Unix(), 36)
		prompt := b.Prompt
		if strings.TrimSpace(prompt) == "" {
			prompt = "A file was added or changed in a folder you watch. Read it and do what it needs."
		}
		_, err := d.Submit(ctx, store.Task{
			OrgID: a.OrgID, AgentID: a.ID, Source: protocol.SourceFile,
			Origin:    store.Origin{Label: desc.Label + " " + folder, BindingID: b.ID, FileID: f.ID},
			DedupeKey: &dedupe,
			Prompt:    prompt,
			Context: fmt.Sprintf("The file %q (id %s, %s, modified %s) in the %s folder %q. "+
				"Read it with the files tools, using folder %s.",
				f.Name, f.ID, f.MimeType, f.Modified.Format(time.RFC3339), desc.Label, folder, b.ID),
			RequestedBy:   b.CreatedBy,
			RequestedByID: b.CreatedByID,
		})
		if err != nil && !errors.Is(err, errDuplicate) {
			return err
		}
	}
	return d.st.SetWatchCursor(ctx, b.ID, cursor)
}

// latest lists each file once, where it first appears, as it was last
// listed: a file changed twice is one task, about the file as it is now.
func latest(files []channels.File) []channels.File {
	var out []channels.File
	at := map[string]int{}
	for _, f := range files {
		if i, ok := at[f.ID]; ok {
			out[i] = f
			continue
		}
		at[f.ID] = len(out)
		out = append(out, f)
	}
	return out
}

// Deliver sends each finished task's result back to where it came from.
func (d *Dispatcher) Deliver(ctx context.Context) {
	tasks, err := d.st.Undelivered(ctx)
	if err != nil {
		d.log.Warn("could not read results to deliver", "error", err)
		return
	}
	for _, t := range tasks {
		// A round out of time leaves the rest for the next one. Claimed now,
		// they would fail to send and be lost.
		if ctx.Err() != nil {
			return
		}
		won, err := d.st.MarkDelivered(ctx, t.ID)
		if err != nil || !won {
			continue
		}
		d.deliverClaimed(ctx, t)
	}
}

// sendTimeout bounds one delivery. It covers a service asking to wait three
// times for 30 seconds.
const sendTimeout = 2 * time.Minute

// deliverClaimed sends a result this replica has claimed. A claimed result is
// not tried again, so the send gets its own time: the round running out
// mid-send would lose it.
func (d *Dispatcher) deliverClaimed(ctx context.Context, t store.Task) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	err := d.deliver(ctx, t)
	if errors.Is(err, errNotSent) && (t.FinishedAt == nil || time.Since(*t.FinishedAt) < retryFor) {
		d.log.Warn("could not deliver a result", "task", t.ID, "error", err)
		d.opts.Metrics.Delivery(t.Source, "retry")
		// Nothing went out, so trying again cannot post twice.
		_ = d.st.UnmarkDelivered(ctx, t.ID)
		return
	}
	// Every finished task is claimed here once, on one replica, so this is
	// where it is counted.
	d.opts.Metrics.TaskEnded(t.Source, t.State, t.CreatedAt, t.StartedAt, t.FinishedAt)
	d.opts.Metrics.Delivery(t.Source, metrics.Outcome(err))
	if err == nil {
		return
	}
	d.log.Warn("could not deliver a result", "task", t.ID, "error", err)
	_ = d.st.AddEvents(ctx, t.ID, []store.Event{{
		Kind: protocol.EventLog, Text: "The result could not be delivered: " + err.Error(),
	}})
}

// resultText is what goes back for a task.
func resultText(t store.Task) string {
	switch t.State {
	case store.TaskSucceeded:
		if strings.TrimSpace(t.Result) == "" {
			return "Done."
		}
		return t.Result
	case store.TaskCancelled:
		return "This task was cancelled: " + t.Error
	default:
		msg := "I could not finish this"
		if t.Error != "" {
			msg += ": " + t.Error
		}
		if t.Result != "" {
			msg += "\n\n" + t.Result
		}
		return msg
	}
}

func (d *Dispatcher) deliver(ctx context.Context, t store.Task) error {
	switch t.Source {
	case protocol.SourceChat:
		if t.Origin.ConnectionID == "" {
			return nil // Keera Chat reads the result itself
		}
		return d.send(ctx, t.Origin.ConnectionID, replyTarget(t.Origin), resultText(t))
	case protocol.SourceSchedule:
		if t.Origin.ReplyBindingID == "" {
			return nil
		}
		b, err := d.st.Binding(ctx, t.Origin.ReplyBindingID)
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("the schedule's channel is gone")
		}
		if err != nil {
			return fmt.Errorf("%w: %w", errNotSent, err)
		}
		if b.Target == store.AnyConversation {
			return errors.New("the schedule's channel takes every conversation, so it names none to post in")
		}
		return d.send(ctx, b.ConnectionID, channels.Target{Conversation: b.Target}, resultText(t))
	case protocol.SourceBoard:
		return d.deliverCard(ctx, t)
	}
	return nil
}

func (d *Dispatcher) send(ctx context.Context, connectionID string, to channels.Target, text string) error {
	adapter, conn, err := connect(ctx, d, connectionID, "channels", channels.ChatFor)
	if errors.Is(err, errConnectorGone) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errNotSent, err)
	}
	_, err = adapter.Send(ctx, conn, to, text)
	if unreached(err) {
		return fmt.Errorf("%w: %w", errNotSent, err)
	}
	return err
}

// unreached reports an error from before a request left: the service's name
// did not resolve or its address took no connection. Anything later may have
// posted, so it is not tried again.
func unreached(err error) bool {
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return true
	}
	op, ok := errors.AsType[*net.OpError](err)
	return ok && op.Op == "dial"
}

// replyTarget is where the answer to a chat task goes: the message's own
// conversation and thread.
func replyTarget(o store.Origin) channels.Target {
	return channels.Target{Conversation: o.Conversation, Thread: o.Thread, Extra: o.Extra}
}

// errNotSent marks a delivery that failed before anything went out, so it
// can be tried again, for up to retryFor after the task finished.
var errNotSent = errors.New("not sent")

// retryFor bounds the retries, so a result that cannot be sent is reported
// and stops being tried every round.
const retryFor = time.Hour

// deliverCard moves the card on and comments the result on it. Done work
// goes to Review and the rest to Backlog, so it is not picked up again in a
// loop. Both are our own writes, so a failure is tried again: the move
// first, since a retry skips a card that has moved, and the comment last.
func (d *Dispatcher) deliverCard(ctx context.Context, t store.Task) error {
	card, err := d.st.Card(ctx, t.Origin.CardID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the card was deleted meanwhile
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errNotSent, err)
	}
	// Only move a card that is still where the agent left it; a person may
	// have moved it meanwhile.
	if card.Status == store.CardInProgress && card.TaskID != nil && *card.TaskID == t.ID {
		status := store.CardReview
		if t.State != store.TaskSucceeded {
			status = store.CardBacklog
		}
		if err := d.st.MoveCard(ctx, card.ID, status, nil); err != nil {
			return fmt.Errorf("%w: %w", errNotSent, err)
		}
	}
	agentID := t.AgentID
	if _, err := d.st.AddComment(ctx, store.Comment{
		ID: id.New("cmt"), CardID: card.ID, Author: t.AgentName, AgentID: &agentID, Body: resultText(t),
	}); err != nil {
		return fmt.Errorf("%w: %w", errNotSent, err)
	}
	return nil
}

// ExpireTasks fails tasks past their deadline.
func (d *Dispatcher) ExpireTasks(ctx context.Context) {
	ids, err := d.st.ExpireTasks(ctx)
	if err != nil {
		d.log.Warn("could not expire tasks", "error", err)
		return
	}
	for _, id := range ids {
		d.log.Info("a task ran out of time", "task", id)
	}
}
