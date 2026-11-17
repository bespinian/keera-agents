package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

func (r *runner) runTask(ctx context.Context, task protocol.Task) {
	start := r.now()
	log := r.log.With("task", task.ID, "source", task.Source)
	log.Info("agent: task started")

	taskCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if !task.Deadline.IsZero() {
		var stop context.CancelFunc
		taskCtx, stop = context.WithDeadlineCause(taskCtx, task.Deadline,
			fmt.Errorf("the task ran out of time at %s", task.Deadline.UTC().Format(time.RFC3339)))
		defer stop()
	}

	ev := newEventSink(ctx, r, task.ID, cancel)
	result, err := r.work(taskCtx, task, ev)
	if err != nil && taskCtx.Err() != nil {
		// Why the task ended matters more than which call noticed.
		err = context.Cause(taskCtx)
		if ctx.Err() != nil {
			err = errStopped
		}
	}

	f := protocol.Finish{State: protocol.Succeeded, Result: result}
	if err != nil {
		f.State, f.Error = protocol.Failed, err.Error()
		ev.add(protocol.EventLog, "failed: "+truncate(err.Error(), 1000), nil)
	}
	ev.close()

	if task.Conversation != "" && !errors.Is(err, errTaskGone) {
		answer := result
		if err != nil {
			answer = strings.TrimSpace(result + "\n\n[This task failed: " + err.Error() + "]")
		}
		path := historyPath(r.cfg.Workspace, task.Conversation)
		now := r.now()
		if herr := appendHistory(path, turn{Role: "user", Content: taskMessage(task), At: start},
			turn{Role: "assistant", Content: answer, At: now}); herr != nil {
			log.Warn("agent: saving the conversation failed", "error", herr)
		}
	}

	if !errors.Is(err, errTaskGone) {
		r.postFinish(ctx, task.ID, f)
	}
	log.Info("agent: task finished", "state", f.State, "duration", r.now().Sub(start).Round(time.Millisecond))
}

// postFinish retries until the control plane has the finish. Giving up
// would leave the task running, and the next poll would hand it back to run
// again, with every message and change made twice. A repeat is safe: a
// task that already ended answers 409. While the runtime stops, it tries
// once.
func (r *runner) postFinish(ctx context.Context, taskID string, f protocol.Finish) {
	base := context.WithoutCancel(ctx)
	backoff := r.backoffMin
	for {
		fctx, cancel := context.WithTimeout(base, 15*time.Second)
		err := r.cp.finish(fctx, taskID, f)
		cancel()
		if err == nil {
			return
		}
		status := statusOf(err)
		if (status >= 400 && status < 500 && status != http.StatusTooManyRequests &&
			status != http.StatusRequestTimeout) || ctx.Err() != nil {
			r.log.Warn("agent: reporting how the task ended failed", "task", taskID, "error", err)
			return
		}
		r.log.Warn("agent: reporting how the task ended failed; trying again", "task", taskID,
			"error", err, "retry_in", backoff)
		sleep(ctx, backoff)
		backoff = min(backoff*2, r.backoffMax)
	}
}

// conversation is a task's messages: the system prompt, the history, the
// task, and its steps.
type conversation struct {
	msgs []message
	// history is how many messages after the system prompt are history.
	history int
}

// trim makes the conversation smaller after the model refused it for its
// size: it drops the history, and shortens long tool results but the last.
// It reports whether anything changed.
func (c *conversation) trim() bool {
	changed := false
	if c.history > 0 {
		c.msgs = slices.Delete(c.msgs, 1, 1+c.history)
		c.history = 0
		changed = true
	}
	last := -1
	for i, m := range c.msgs {
		if m.Role == "tool" {
			last = i
		}
	}
	const keep = 2 << 10
	for i := range c.msgs {
		if m := &c.msgs[i]; m.Role == "tool" && i != last && len(m.Content) > keep {
			m.Content = truncate(m.Content, keep) + "\n[shortened to fit the model's context]"
			changed = true
		}
	}
	return changed
}

func (r *runner) work(ctx context.Context, task protocol.Task, ev *eventSink) (string, error) {
	boot := r.snapshot()
	tools := r.toolbox(boot, task)

	var history []turn
	if task.Conversation != "" {
		history = loadHistory(historyPath(r.cfg.Workspace, task.Conversation), historyTurns)
	}
	conv := &conversation{history: len(history)}
	conv.msgs = append(conv.msgs, message{Role: "system", Content: r.systemPrompt(boot)})
	for _, t := range history {
		conv.msgs = append(conv.msgs, message{Role: t.Role, Content: t.Content})
	}
	conv.msgs = append(conv.msgs, message{Role: "user", Content: taskMessage(task)})

	maxSteps := boot.Agent.MaxSteps
	if maxSteps <= 0 {
		maxSteps = protocol.DefaultMaxSteps
	}
	last := ""
	retold := false
	for range maxSteps {
		msg, err := r.step(ctx, conv, tools, task.ID, ev)
		if err != nil {
			return last, err
		}
		conv.msgs = append(conv.msgs, msg)
		text := strings.TrimSpace(msg.Content)
		if len(msg.ToolCalls) == 0 {
			if text == "" {
				return last, errors.New("the model gave an empty answer")
			}
			// A tool call written as text would be delivered as the answer.
			// Tell the model once; then fail rather than deliver it.
			if name := tools.writtenCall(text); name != "" {
				if retold {
					return last, fmt.Errorf("the model wrote a call to %s as text instead of calling it; "+
						"its model or backend may not support tool calling", name)
				}
				retold = true
				ev.add(protocol.EventLog, "the model wrote a call to "+name+" as text, so it did not run; "+
					"asking it again", nil)
				conv.msgs = append(conv.msgs, message{Role: "user", Content: "Your last reply wrote a tool " +
					"call as text, so it did not run. Call tools through tool calls, or give your final answer."})
				continue
			}
			ev.add(protocol.EventMessage, truncate(text, protocol.MaxEventText), nil)
			return text, nil
		}
		if text != "" {
			ev.add(protocol.EventMessage, truncate(text, protocol.MaxEventText), nil)
			last = text
		}
		if err := runCalls(ctx, tools, msg.ToolCalls, conv, ev); err != nil {
			return last, err
		}
	}
	return last, fmt.Errorf("stopped after %d model calls without a final answer; raise the agent's "+
		"step limit or split the task", maxSteps)
}

// runCalls runs the model's tool calls in order and adds their results to
// the conversation.
func runCalls(ctx context.Context, tools *toolbox, calls []toolCall, conv *conversation, ev *eventSink) error {
	for _, tc := range calls {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := tc.Function.Name
		ev.add(protocol.EventToolCall, tools.summary(name, tc.Function.Arguments), map[string]any{
			"tool": name, "call_id": tc.ID, "arguments": truncate(tc.Function.Arguments, 2<<10),
		})
		out, failed := tools.call(ctx, name, tc.Function.Arguments)
		if strings.TrimSpace(out) == "" {
			out = "(no output)"
		}
		ev.add(protocol.EventToolResult, truncate(out, 500), map[string]any{
			"tool": name, "call_id": tc.ID, "is_error": failed,
		})
		conv.msgs = append(conv.msgs, message{Role: "tool", ToolCallID: tc.ID, Content: out})
	}
	return nil
}

// step makes one model call. A 401 means the key was replaced, so it fetches
// the bootstrap again and retries once; a request too long for the model is
// trimmed and retried once.
func (r *runner) step(ctx context.Context, conv *conversation, tools *toolbox, taskID string, ev *eventSink,
) (message, error) {
	reauthed, trimmed := false, false
	for {
		boot := r.snapshot()
		req := chatRequest{Model: boot.Agent.Model, Messages: conv.msgs, Tools: tools.defs()}
		msg, err := r.gw.complete(ctx, boot.Gateway.BaseURL, boot.Gateway.APIKey, taskID, req)
		switch {
		case err == nil:
			return msg, nil
		case statusOf(err) == http.StatusUnauthorized && !reauthed:
			reauthed = true
			ev.add(protocol.EventLog, "the gateway refused the agent's key; fetching a new one", nil)
			if rerr := r.refresh(ctx); rerr != nil {
				return message{}, fmt.Errorf("%w; fetching a new key failed too: %v", err, rerr)
			}
		case contextTooLong(err) && !trimmed && conv.trim():
			trimmed = true
			ev.add(protocol.EventLog, "the conversation was too long for the model; "+
				"dropped older history and long tool results, and retrying", nil)
		default:
			return message{}, err
		}
	}
}

func (r *runner) toolbox(boot protocol.Bootstrap, task protocol.Task) *toolbox {
	b := newToolbox(r.log)
	for _, name := range protocol.Builtins {
		if !slices.Contains(boot.Agent.Builtins, name) {
			continue
		}
		switch name {
		case protocol.BuiltinMemory:
			b.add(r.ws.memoryTools()...)
		case protocol.BuiltinFiles:
			b.add(r.ws.fileTools()...)
		case protocol.BuiltinShell:
			b.add(shellTool(r.cfg.Workspace, gitEnv(boot.Agent)))
		case protocol.BuiltinWeb:
			b.add(webTool(r.web))
		}
	}
	r.addRemoteTools(b, boot, task.ID, taskPath(task.ID, "tools"))
	return b
}

// addRemoteTools adds the tools that run elsewhere: the platform tools, which
// the control plane runs at the route tools, and the gateway's MCP servers'
// tools. session names the task, or the workspace, to the gateway.
func (r *runner) addRemoteTools(b *toolbox, boot protocol.Bootstrap, session, tools string) {
	for _, spec := range boot.Tools {
		b.add(tool{
			name:        spec.Name,
			description: spec.Description,
			parameters:  spec.Parameters,
			run: func(ctx context.Context, args json.RawMessage) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				res, err := r.cp.tool(ctx, tools, spec.Name, args)
				if err != nil {
					return "", err
				}
				if res.IsError {
					return "", errors.New(res.Content)
				}
				return res.Content, nil
			},
		})
	}
	for _, conn := range r.mcpConns() {
		for _, t := range conn.tools {
			b.add(tool{
				name:        mcpToolName(conn.client.alias, t.Name),
				description: truncate("["+conn.client.alias+"] "+t.Description, 1024),
				parameters:  t.InputSchema,
				run: func(ctx context.Context, args json.RawMessage) (string, error) {
					return conn.client.call(ctx, session, t.Name, args)
				},
			})
		}
	}
}

// taskMessage is the task as the model reads it: who asked, from where, and
// what.
func taskMessage(t protocol.Task) string {
	var b strings.Builder
	b.WriteString("[Task")
	if t.Source != "" {
		b.WriteString(" from " + t.Source)
	}
	if who := t.RequestedBy.Email; who != "" {
		b.WriteString(", requested by " + who)
	}
	b.WriteString("]\n")
	if t.Context != "" {
		b.WriteString("[Context: " + t.Context + "]\n")
	}
	if !t.Deadline.IsZero() {
		b.WriteString("[Finish by " + t.Deadline.UTC().Format("2006-01-02 15:04 MST") + "]\n")
	}
	b.WriteString("\n" + t.Prompt)
	return b.String()
}

func (r *runner) systemPrompt(boot protocol.Bootstrap) string {
	a := boot.Agent
	has := func(b string) bool { return slices.Contains(a.Builtins, b) }
	now := r.now()
	var b strings.Builder

	b.WriteString("You are " + a.Name + ", an AI agent running on Keera.")
	if owner := a.Owner.Email; owner != "" {
		b.WriteString(" " + owner + " is responsible for you.")
	}
	b.WriteString("\nIt is " + now.Format("Monday, 2 January 2006, 15:04 MST") + ".\n\n")

	b.WriteString("How you work:\n")
	b.WriteString("- Each task names the person who asked. You act for them: do what they ask, " +
		"within your instructions, and nothing beyond what the task needs.\n")
	b.WriteString("- Your final message is delivered to whoever asked: the chat thread, the card or " +
		"the channel. It is all they see, so make it complete, but keep it concise and easy to read " +
		"in a chat, with light Markdown.\n")
	if has(protocol.BuiltinFiles) || has(protocol.BuiltinShell) {
		b.WriteString("- Your workspace is " + r.cfg.Workspace + ", a disk that is kept between tasks " +
			"and restarts. Keep your files there.\n")
	}
	if has(protocol.BuiltinMemory) {
		b.WriteString("- Your memory is " + r.cfg.Workspace + "/" + memoryFile + ", shown below at the " +
			"start of every task. Save what is worth knowing in later tasks - preferences, decisions, " +
			"ongoing work - with memory_append, and keep it short and tidy with memory_write.\n")
	}
	b.WriteString("- Use your tools when they help, rather than guessing. If something fails, say so plainly.\n")
	b.WriteString("- Never reveal credentials, keys or tokens, and do not store them in files or memory.\n")

	b.WriteString(connectedSection(boot.Connected, has(protocol.BuiltinShell)))

	if s := strings.TrimSpace(a.Instructions); s != "" {
		b.WriteString("\n## Your instructions\n\n" + s + "\n")
	}
	if mem := r.ws.memory(); mem != "" {
		b.WriteString("\n## Your memory (" + memoryFile + ")\n\n" + mem + "\n")
	}
	return b.String()
}

// connectedSection tells the model what it is connected to. The same lists
// are in the tools' descriptions, but a model asked to clone a repository
// rarely looks for its address in the description of a tool for issues.
func connectedSection(list []protocol.Connected, shell bool) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## What you are connected to\n\nYour tools take these ids.\n\n")
	repos := false
	for _, c := range list {
		// The list comes over the wire; an empty What would panic below.
		if c.What == "" {
			continue
		}
		b.WriteString("- " + strings.ToUpper(c.What[:1]) + c.What[1:] + " " + c.Name + " (" + c.Service + ")")
		if c.ReadOnly {
			b.WriteString(", read only")
		}
		b.WriteString(", id " + c.ID)
		if c.Clone != "" && shell {
			b.WriteString(". Clone it with: " + c.Clone)
		}
		b.WriteString("\n")
		repos = repos || c.What == protocol.ConnectedRepository
	}
	switch {
	case repos && shell:
		b.WriteString("\nWork in a repository with git in your shell. Clone it with the command above, not from " +
			"the service's website: that address adds your access for you. You may push only branches whose name " +
			"starts with " + protocol.BranchPrefix + ". Open a pull or merge request with repo_open_change.\n")
	case repos:
		b.WriteString("\nYou have no shell, so you cannot clone these repositories or read their files: you can " +
			"only work with their issues and pull or merge requests. If a task needs their code, say that the " +
			"shell tool must be turned on in your settings.\n")
	}
	return b.String()
}

// eventSink posts a task's events in batches, so the panel follows along
// without a request per event. If the control plane says the task is gone,
// it cancels the task.
type eventSink struct {
	r      *runner
	ctx    context.Context
	taskID string
	cancel context.CancelCauseFunc

	mu      sync.Mutex
	buf     []protocol.Event
	done    chan struct{}
	stopped chan struct{}
}

// maxPendingEvents bounds what is kept while the control plane is away.
const maxPendingEvents = 1000

func newEventSink(ctx context.Context, r *runner, taskID string, cancel context.CancelCauseFunc) *eventSink {
	s := &eventSink{
		r: r, ctx: context.WithoutCancel(ctx), taskID: taskID, cancel: cancel,
		done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *eventSink) add(kind, text string, data any) {
	e := protocol.Event{Kind: kind, Text: text, At: s.r.now().UTC()}
	if data != nil {
		e.Data, _ = json.Marshal(data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) < maxPendingEvents {
		s.buf = append(s.buf, e)
	}
}

func (s *eventSink) loop() {
	defer close(s.stopped)
	t := time.NewTicker(s.r.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.flush()
		case <-s.done:
			s.flush()
			return
		}
	}
}

func (s *eventSink) flush() {
	s.mu.Lock()
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	err := s.r.cp.events(ctx, s.taskID, batch)
	if err == nil {
		return
	}
	switch statusOf(err) {
	case http.StatusNotFound, http.StatusConflict, http.StatusGone:
		s.cancel(errTaskGone)
		return
	}
	s.r.log.Warn("agent: posting events failed; keeping them for the next try", "task", s.taskID, "error", err)
	s.mu.Lock()
	s.buf = append(batch, s.buf...)
	if len(s.buf) > maxPendingEvents {
		s.buf = s.buf[len(s.buf)-maxPendingEvents:]
	}
	s.mu.Unlock()
}

// close posts what is left and stops.
func (s *eventSink) close() {
	close(s.done)
	<-s.stopped
}
