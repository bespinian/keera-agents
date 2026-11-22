package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
	"github.com/bespinian/keera-agents/internal/store/storetest"
)

// stubGateway answers the gateway's service API: it mints keys, lists one
// MCP server and knows every user.
type stubGateway struct {
	mu     sync.Mutex
	minted []gateway.KeyRequest
}

func (g *stubGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/control/v1/service/keys":
		var in gateway.KeyRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		g.minted = append(g.minted, in)
		n := len(g.minted)
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("key_%d", n), "key": fmt.Sprintf("sk-%d", n)})
	case "/control/v1/service/catalog":
		_ = json.NewEncoder(w).Encode(gateway.Catalog{MCPServers: []gateway.MCPRef{{Alias: "docs"}}})
	case "/control/v1/service/users":
		_ = json.NewEncoder(w).Encode(gateway.User{ID: r.URL.Query().Get("id"), Email: "ada@example.ch"})
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
	}
}

func (g *stubGateway) mints() []gateway.KeyRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gateway.KeyRequest(nil), g.minted...)
}

type testRunner struct {
	ctx   context.Context
	st    *store.Store
	fleet *fleet.Manager
	rn    *Runner
	gw    *stubGateway
	gwURL string
	url   string
}

// openTest starts a runner against KEERA_AGENTS_TEST_DATABASE_URL, on an
// empty schema, or skips.
func openTest(t *testing.T) *testRunner {
	t.Helper()
	st, _ := storetest.Open(t)
	ctx := context.Background()

	gw := &stubGateway{}
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	client := gateway.New(gwSrv.URL, strings.Repeat("s", 32))
	box, _ := secret.New("0123456789abcdef")
	log := slog.New(slog.DiscardHandler)
	fl := fleet.New(st, client, nil, box, fleet.Options{Log: log})
	disp := dispatch.New(st, fl, client, box, dispatch.Options{Log: log})
	rn := New(st, fl, disp, client, Options{GatewayURL: gwSrv.URL + "/", TaskTimeout: time.Minute, Log: log})
	srv := httptest.NewServer(rn.Handler())
	t.Cleanup(srv.Close)
	return &testRunner{ctx: ctx, st: st, fleet: fl, rn: rn, gw: gw, gwURL: gwSrv.URL, url: srv.URL}
}

// agent makes an enabled agent, or a workspace, with a runtime token. With
// key, it also has a gateway key, as after its first start.
func (tr *testRunner) agent(t *testing.T, org, name, kind string, key bool) (store.Agent, string) {
	t.Helper()
	experience := ""
	if kind == store.KindWorkspace {
		experience = store.ExperienceTerminal
	}
	a, err := tr.st.CreateAgent(tr.ctx, store.Agent{
		ID: id.New("agt"), Kind: kind, Experience: experience, OrgID: org, OwnerID: "usr_" + name, OwnerEmail: name + "@example.ch",
		Name: name, Model: "keera-speed", MaxSteps: 12, Instructions: "Be brief.",
		Builtins: []string{protocol.BuiltinShell}, Access: store.AccessOrg, Enabled: true,
		BudgetMicros: 5_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "kat_" + a.ID
	if err := tr.st.SetRuntimeToken(tr.ctx, a.ID, fleet.TokenHash(token)); err != nil {
		t.Fatal(err)
	}
	if key {
		if err := tr.fleet.Rekey(tr.ctx, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	a, _ = tr.st.Agent(tr.ctx, a.ID)
	return a, token
}

func (tr *testRunner) task(t *testing.T, a store.Agent, prompt string) store.Task {
	t.Helper()
	task, err := tr.st.CreateTask(tr.ctx, store.Task{
		ID: id.New("tsk"), OrgID: a.OrgID, AgentID: a.ID, Source: protocol.SourceManual,
		Prompt: prompt, RequestedBy: "ada@example.ch",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (tr *testRunner) reload(t *testing.T, taskID string) store.Task {
	t.Helper()
	task, err := tr.st.Task(tr.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (tr *testRunner) events(t *testing.T, taskID string) []store.Event {
	t.Helper()
	ev, err := tr.st.Events(tr.ctx, taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// do calls the runtime API with a token, sending in as JSON unless it is
// already bytes, and decodes a 200 answer into out.
func (tr *testRunner) do(t *testing.T, method, path, token string, in, out any) int {
	t.Helper()
	var body io.Reader
	switch v := in.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(v)
	default:
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, tr.url+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
	return res.StatusCode
}

// claim takes the agent's next task without waiting.
func (tr *testRunner) claim(t *testing.T, token string) (protocol.Task, int) {
	t.Helper()
	var got protocol.Task
	code := tr.do(t, "GET", protocol.NextTaskPath+"?wait=0s", token, nil, &got)
	return got, code
}

func taskPath(taskID, rest string) string { return protocol.TaskPath + "/" + taskID + rest }

func TestAContainerNeedsAValidRuntimeToken(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	task := tr.task(t, a, "Do it")
	stopped, stoppedToken := tr.agent(t, "org_a", "stopped", store.KindAgent, true)
	if _, err := tr.st.SetEnabled(tr.ctx, stopped.ID, false); err != nil {
		t.Fatal(err)
	}
	gone, goneToken := tr.agent(t, "org_a", "gone", store.KindAgent, true)
	if err := tr.st.DeleteAgent(tr.ctx, gone.ID); err != nil {
		t.Fatal(err)
	}

	routes := []struct{ method, path string }{
		{"GET", protocol.BootstrapPath},
		{"GET", protocol.NextTaskPath + "?wait=0s"},
		{"POST", taskPath(task.ID, "/events")},
		{"POST", taskPath(task.ID, "/finish")},
		{"POST", taskPath(task.ID, "/tools/board_list_cards")},
		{"POST", protocol.WorkspaceToolPath + "/board_list_cards"},
		{"GET", protocol.GitPath + "/bnd_1/web.git/info/refs?service=git-upload-pack"},
	}
	for _, rt := range routes {
		for name, c := range map[string]struct {
			auth string
			want int
		}{
			"no header":     {"", http.StatusUnauthorized},
			"not bearer":    {"Basic " + token, http.StatusUnauthorized},
			"empty bearer":  {"Bearer ", http.StatusUnauthorized},
			"wrong token":   {"Bearer kat_nobody", http.StatusUnauthorized},
			"deleted agent": {"Bearer " + goneToken, http.StatusUnauthorized},
			"stopped agent": {"Bearer " + stoppedToken, http.StatusForbidden},
		} {
			req, _ := http.NewRequest(rt.method, tr.url+rt.path, strings.NewReader(`{"state":"succeeded","events":[{"kind":"log"}]}`))
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != c.want {
				t.Errorf("%s %s with %s = %d, want %d", rt.method, rt.path, name, res.StatusCode, c.want)
			}
		}
	}
	// Nothing reached the task, and no key left.
	if got := tr.reload(t, task.ID); got.State != store.TaskQueued {
		t.Errorf("the task is %s after refused calls", got.State)
	}
	if ev := tr.events(t, task.ID); len(ev) != 0 {
		t.Errorf("refused calls stored %d events", len(ev))
	}
	if n := len(tr.gw.mints()); n != 3 {
		t.Errorf("the gateway minted %d keys, want only the 3 of the setup", n)
	}
}

func TestBootstrapGivesTheAgentItsSettingsAndKey(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)

	var b protocol.Bootstrap
	if code := tr.do(t, "GET", protocol.BootstrapPath, token, nil, &b); code != http.StatusOK {
		t.Fatalf("bootstrap = %d", code)
	}
	if b.Agent.ID != a.ID || b.Agent.MaxSteps != 12 || b.Agent.Instructions != "Be brief." ||
		b.Agent.Model != "keera-speed" || len(b.Agent.Builtins) != 1 || b.Agent.Owner.Email != "scout@example.ch" {
		t.Errorf("agent = %+v", b.Agent)
	}
	if b.Gateway.APIKey != "sk-1" || b.Gateway.BaseURL != tr.gwURL+"/api/v1" {
		t.Errorf("gateway = %+v", b.Gateway)
	}
	if len(b.Gateway.MCP) != 1 || b.Gateway.MCP[0].URL != tr.gwURL+"/api/mcp/docs" {
		t.Errorf("MCP = %+v", b.Gateway.MCP)
	}
	if b.Version == "" || b.Workspace != nil || len(b.Tools) == 0 {
		t.Errorf("version %q, workspace %v, %d tools", b.Version, b.Workspace, len(b.Tools))
	}
	got, _ := tr.st.Agent(tr.ctx, a.ID)
	if got.State != store.AgentRunning || got.LastActiveAt == nil {
		t.Errorf("after bootstrap the agent is %s, last active %v", got.State, got.LastActiveAt)
	}

	// A task carries the same version, so the runtime keeps what it has.
	tr.task(t, a, "Do it")
	pt, code := tr.claim(t, token)
	if code != http.StatusOK || pt.Version != b.Version {
		t.Errorf("task version = %q (%d), bootstrap's %q", pt.Version, code, b.Version)
	}
}

// An agent whose key was revoked, as when its owner left and came back,
// gets a new one from its next bootstrap, with its budget.
func TestBootstrapMintsAMissingKeyOnce(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, false)

	var first, second protocol.Bootstrap
	if code := tr.do(t, "GET", protocol.BootstrapPath, token, nil, &first); code != http.StatusOK {
		t.Fatalf("bootstrap = %d", code)
	}
	if code := tr.do(t, "GET", protocol.BootstrapPath, token, nil, &second); code != http.StatusOK {
		t.Fatalf("second bootstrap = %d", code)
	}
	mints := tr.gw.mints()
	if len(mints) != 1 {
		t.Fatalf("minted %d keys, want 1", len(mints))
	}
	if m := mints[0]; m.OrgID != "org_a" || m.UserID != a.OwnerID || m.BudgetMicros != 5_000_000 {
		t.Errorf("minted %+v, want the owner's key with the agent's budget", m)
	}
	if first.Gateway.APIKey != "sk-1" || second.Gateway.APIKey != "sk-1" {
		t.Errorf("keys = %q, %q", first.Gateway.APIKey, second.Gateway.APIKey)
	}
}

func TestWorkspaceBootstrapHoldsOnlyItsOwnersSSHKeys(t *testing.T) {
	tr := openTest(t)
	ws, token := tr.agent(t, "org_a", "lab", store.KindWorkspace, true)
	for _, k := range []store.SSHKey{
		{ID: id.New("ssh"), OrgID: "org_a", UserID: ws.OwnerID, Name: "laptop", PublicKey: "ssh-ed25519 AAAAmine", Fingerprint: "f1"},
		{ID: id.New("ssh"), OrgID: "org_a", UserID: "usr_other", Name: "laptop", PublicKey: "ssh-ed25519 AAAAtheirs", Fingerprint: "f2"},
		{ID: id.New("ssh"), OrgID: "org_b", UserID: ws.OwnerID, Name: "laptop", PublicKey: "ssh-ed25519 AAAAelsewhere", Fingerprint: "f3"},
	} {
		if _, err := tr.st.AddSSHKey(tr.ctx, k); err != nil {
			t.Fatal(err)
		}
	}

	var b protocol.Bootstrap
	if code := tr.do(t, "GET", protocol.BootstrapPath, token, nil, &b); code != http.StatusOK {
		t.Fatalf("bootstrap = %d", code)
	}
	if b.Workspace == nil {
		t.Fatal("a workspace's bootstrap has no workspace part")
	}
	if len(b.Workspace.SSHKeys) != 1 || b.Workspace.SSHKeys[0] != "ssh-ed25519 AAAAmine" {
		t.Errorf("SSH keys = %v, want only the owner's in this organisation", b.Workspace.SSHKeys)
	}
	if b.Workspace.IngressSecret == "" {
		t.Error("no ingress secret")
	}
	// A workspace polls its bootstrap on its own, which is no sign of use.
	got, _ := tr.st.Agent(tr.ctx, ws.ID)
	if got.State != store.AgentRunning || got.LastActiveAt != nil {
		t.Errorf("after bootstrap the workspace is %s, last active %v", got.State, got.LastActiveAt)
	}
}

func TestNextGivesOneTaskAtATimeOldestFirst(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	b, _ := tr.agent(t, "org_a", "other", store.KindAgent, true)
	first := tr.task(t, a, "first")
	second := tr.task(t, a, "second")
	theirs := tr.task(t, b, "theirs")

	got, code := tr.claim(t, token)
	if code != http.StatusOK || got.ID != first.ID || got.Prompt != "first" || got.RequestedBy.Email != "ada@example.ch" {
		t.Fatalf("first claim = %d %+v", code, got)
	}
	if left := time.Until(got.Deadline); left <= 0 || left > time.Minute {
		t.Errorf("deadline in %v, want within the task timeout", left)
	}
	// A runtime that restarted gets its running task back, not the next one.
	if again, code := tr.claim(t, token); code != http.StatusOK || again.ID != first.ID {
		t.Fatalf("claim while running = %d %s, want %s again", code, again.ID, first.ID)
	}
	if got := tr.reload(t, second.ID); got.State != store.TaskQueued {
		t.Fatalf("the second task is %s while the first runs", got.State)
	}

	if code := tr.do(t, "POST", taskPath(first.ID, "/finish"), token, protocol.Finish{State: protocol.Succeeded}, nil); code != http.StatusNoContent {
		t.Fatalf("finish = %d", code)
	}
	if got, code := tr.claim(t, token); code != http.StatusOK || got.ID != second.ID {
		t.Fatalf("claim after finishing = %d %s, want %s", code, got.ID, second.ID)
	}
	if code := tr.do(t, "POST", taskPath(second.ID, "/finish"), token, protocol.Finish{State: protocol.Succeeded}, nil); code != http.StatusNoContent {
		t.Fatalf("finish = %d", code)
	}
	if _, code := tr.claim(t, token); code != http.StatusNoContent {
		t.Fatalf("claim with nothing queued = %d, want 204", code)
	}
	if got := tr.reload(t, theirs.ID); got.State != store.TaskQueued {
		t.Errorf("another agent's task is %s", got.State)
	}
}

func TestNextWakesWhenATaskIsQueued(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)

	type answer struct {
		task protocol.Task
		code int
	}
	done := make(chan answer, 1)
	go func() {
		var got protocol.Task
		req, _ := http.NewRequest("GET", tr.url+protocol.NextTaskPath+"?wait=30s", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- answer{}
			return
		}
		defer res.Body.Close()
		_ = json.NewDecoder(res.Body).Decode(&got)
		done <- answer{got, res.StatusCode}
	}()
	// Queued while the poll may or may not wait yet: it is found either way.
	task := tr.task(t, a, "wake up")
	tr.rn.Notify(a.ID)
	select {
	case got := <-done:
		if got.code != http.StatusOK || got.task.ID != task.ID {
			t.Fatalf("long poll = %d %s, want %s", got.code, got.task.ID, task.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the long poll did not end when a task was queued")
	}
}

func TestAnotherAgentsTaskIsNotFound(t *testing.T) {
	tr := openTest(t)
	_, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	b, bToken := tr.agent(t, "org_a", "other", store.KindAgent, true)
	theirs := tr.task(t, b, "theirs")
	if _, code := tr.claim(t, bToken); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}

	for _, id := range []string{theirs.ID, "tsk_missing"} {
		for _, c := range []struct {
			path string
			body any
		}{
			{"/events", protocol.Events{Events: []protocol.Event{{Kind: protocol.EventLog, Text: "sneaky"}}}},
			{"/finish", protocol.Finish{State: protocol.Succeeded, Result: "sneaky"}},
			{"/tools/board_list_cards", map[string]any{}},
		} {
			if code := tr.do(t, "POST", taskPath(id, c.path), token, c.body, nil); code != http.StatusNotFound {
				t.Errorf("POST %s on %s = %d, want 404", c.path, id, code)
			}
		}
	}
	got := tr.reload(t, theirs.ID)
	if got.State != store.TaskRunning || got.Result != "" {
		t.Errorf("the other agent's task is %s with result %q", got.State, got.Result)
	}
	if ev := tr.events(t, theirs.ID); len(ev) != 0 {
		t.Errorf("%d events reached the other agent's task", len(ev))
	}
}

func TestEventsAreStoredInOrderAndCapped(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	task := tr.task(t, a, "Do it")
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	path := taskPath(task.ID, "/events")

	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	var many protocol.Events
	for i := range maxEvents + 50 {
		many.Events = append(many.Events, protocol.Event{Kind: protocol.EventLog, Text: fmt.Sprintf("e%03d", i), At: at})
	}
	if code := tr.do(t, "POST", path, token, many, nil); code != http.StatusNoContent {
		t.Fatalf("events = %d", code)
	}
	ev := tr.events(t, task.ID)
	if len(ev) != maxEvents {
		t.Fatalf("stored %d events, want the first %d", len(ev), maxEvents)
	}
	for i, e := range ev {
		if want := fmt.Sprintf("e%03d", i); e.Text != want || !e.At.Equal(at) {
			t.Fatalf("event %d = %q at %v, want %q at %v", i, e.Text, e.At, want, at)
		}
	}

	// A later report comes after, and an event too large or holding NULs
	// is stored in a shape Postgres takes, not refused.
	more := protocol.Events{Events: []protocol.Event{
		{Kind: protocol.EventToolCall, Text: "find -print0\x00", Data: json.RawMessage(`{"out":"a\u0000b"}`)},
		{Kind: protocol.EventToolResult, Text: strings.Repeat("x", protocol.MaxEventText+10), Data: json.RawMessage(`{"ok":true}`)},
	}}
	if code := tr.do(t, "POST", path, token, more, nil); code != http.StatusNoContent {
		t.Fatalf("second events = %d", code)
	}
	ev = tr.events(t, task.ID)
	if len(ev) != maxEvents+2 {
		t.Fatalf("stored %d events, want %d", len(ev), maxEvents+2)
	}
	nul, long := ev[maxEvents], ev[maxEvents+1]
	if nul.Kind != protocol.EventToolCall || nul.Text != "find -print0" || nul.Data != nil {
		t.Errorf("event with NULs = %s %q %s", nul.Kind, nul.Text, nul.Data)
	}
	if !strings.HasSuffix(long.Text, "…") || len(long.Text) > protocol.MaxEventText+len("…") {
		t.Errorf("a long event kept %d bytes", len(long.Text))
	}
	if !bytes.Contains(long.Data, []byte(`"ok"`)) {
		t.Errorf("a long event lost its small data: %s", long.Data)
	}

	if code := tr.do(t, "POST", path, token, []byte(`{"events": [`), nil); code != http.StatusBadRequest {
		t.Errorf("broken JSON = %d, want 400", code)
	}
	if got := len(tr.events(t, task.ID)); got != maxEvents+2 {
		t.Errorf("broken JSON changed the events to %d", got)
	}
}

// A task's events are capped in all, with a note where they stop, and an
// unknown kind is stored as a log line.
func TestEventsAreCappedPerTask(t *testing.T) {
	tr := openTest(t)
	defer func(n int) { maxTaskEvents = n }(maxTaskEvents)
	maxTaskEvents = 5
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	task := tr.task(t, a, "Do it")
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	path := taskPath(task.ID, "/events")
	report := func(n int, kind string) {
		var in protocol.Events
		for range n {
			in.Events = append(in.Events, protocol.Event{Kind: kind, Text: "e"})
		}
		if code := tr.do(t, "POST", path, token, in, nil); code != http.StatusNoContent {
			t.Fatalf("events = %d", code)
		}
	}
	report(3, "made-up")
	report(3, protocol.EventLog)
	report(3, protocol.EventLog)
	ev := tr.events(t, task.ID)
	if len(ev) != maxTaskEvents {
		t.Fatalf("stored %d events, want %d", len(ev), maxTaskEvents)
	}
	if ev[0].Kind != protocol.EventLog {
		t.Errorf("an unknown kind was stored as %q", ev[0].Kind)
	}
	if last := ev[len(ev)-1]; !strings.Contains(last.Text, "too many events") {
		t.Errorf("last event = %q, want the note", last.Text)
	}
}

func TestFinishStoresHowTheTaskEnded(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	for _, c := range []struct {
		in        protocol.Finish
		state     string
		result    string
		errorText string
	}{
		{protocol.Finish{State: protocol.Succeeded, Result: "all\x00 done"}, store.TaskSucceeded, "all done", ""},
		{protocol.Finish{State: protocol.Failed, Result: "half", Error: "no\x00 access"}, store.TaskFailed, "half", "no access"},
		// Anything but success is a failure, never a success.
		{protocol.Finish{State: "done!", Result: "maybe"}, store.TaskFailed, "maybe", ""},
	} {
		task := tr.task(t, a, "Do it")
		if got, code := tr.claim(t, token); code != http.StatusOK || got.ID != task.ID {
			t.Fatalf("claim = %d %s", code, got.ID)
		}
		if code := tr.do(t, "POST", taskPath(task.ID, "/finish"), token, c.in, nil); code != http.StatusNoContent {
			t.Fatalf("finish %q = %d", c.in.State, code)
		}
		got := tr.reload(t, task.ID)
		if got.State != c.state || got.Result != c.result || got.Error != c.errorText || got.FinishedAt == nil {
			t.Errorf("finish %q: %s %q %q, want %s %q %q", c.in.State, got.State, got.Result, got.Error,
				c.state, c.result, c.errorText)
		}
	}
	task := tr.task(t, a, "Do it")
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	if code := tr.do(t, "POST", taskPath(task.ID, "/finish"), token, []byte("succeeded"), nil); code != http.StatusBadRequest {
		t.Errorf("finish with broken JSON = %d, want 400", code)
	}
	if got := tr.reload(t, task.ID); got.State != store.TaskRunning {
		t.Errorf("broken JSON left the task %s", got.State)
	}
}

func TestAnEndedTaskTakesNoMoreChanges(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)

	done := tr.task(t, a, "done")
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	if code := tr.do(t, "POST", taskPath(done.ID, "/finish"), token, protocol.Finish{State: protocol.Succeeded, Result: "first"}, nil); code != http.StatusNoContent {
		t.Fatalf("finish = %d", code)
	}

	cancelled := tr.task(t, a, "cancelled")
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	if err := tr.st.CancelAgentTasks(tr.ctx, a.ID, "stopped by Ada"); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		task  store.Task
		state string
	}{{done, store.TaskSucceeded}, {cancelled, store.TaskCancelled}} {
		if code := tr.do(t, "POST", taskPath(c.task.ID, "/finish"), token, protocol.Finish{State: protocol.Failed, Result: "second"}, nil); code != http.StatusConflict {
			t.Errorf("finishing a %s task again = %d, want 409", c.state, code)
		}
		// 409 tells the runtime to stop working on it, and nothing is stored.
		before, _ := tr.st.Events(tr.ctx, c.task.ID, 0)
		late := protocol.Events{Events: []protocol.Event{{Kind: protocol.EventLog, Text: "late"}}}
		if code := tr.do(t, "POST", taskPath(c.task.ID, "/events"), token, late, nil); code != http.StatusConflict {
			t.Errorf("events on a %s task = %d, want 409", c.state, code)
		}
		if after, _ := tr.st.Events(tr.ctx, c.task.ID, 0); len(after) != len(before) {
			t.Errorf("a %s task took %d more events", c.state, len(after)-len(before))
		}
		if code := tr.do(t, "POST", taskPath(c.task.ID, "/tools/board_list_cards"), token, map[string]any{}, nil); code != http.StatusConflict {
			t.Errorf("a tool on a %s task = %d, want 409", c.state, code)
		}
		got := tr.reload(t, c.task.ID)
		if got.State != c.state || got.Result == "second" {
			t.Errorf("the %s task became %s with result %q", c.state, got.State, got.Result)
		}
	}
}

func TestAQueuedTaskTakesNoResultNorEvents(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	// Queued, and no runtime has claimed it yet.
	queued := tr.task(t, a, "later")

	if code := tr.do(t, "POST", taskPath(queued.ID, "/finish"), token, protocol.Finish{State: protocol.Succeeded, Result: "never ran"}, nil); code != http.StatusConflict {
		t.Errorf("finishing a queued task = %d, want 409", code)
	}
	ev := protocol.Events{Events: []protocol.Event{{Kind: protocol.EventLog, Text: "early"}}}
	if code := tr.do(t, "POST", taskPath(queued.ID, "/events"), token, ev, nil); code != http.StatusConflict {
		t.Errorf("events on a queued task = %d, want 409", code)
	}
	if got := tr.reload(t, queued.ID); got.State != store.TaskQueued || got.Result != "" {
		t.Errorf("the queued task became %s with result %q", got.State, got.Result)
	}
	if events, _ := tr.st.Events(tr.ctx, queued.ID, 0); len(events) != 0 {
		t.Errorf("a queued task took %d events", len(events))
	}
	// Once claimed, it finishes as usual.
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	if code := tr.do(t, "POST", taskPath(queued.ID, "/finish"), token, protocol.Finish{State: protocol.Succeeded, Result: "ran"}, nil); code != http.StatusNoContent {
		t.Errorf("finishing the claimed task = %d, want 204", code)
	}
}

func TestToolsRunThroughDispatchForTheAgentsOrganisation(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	for org, title := range map[string]string{"org_a": "Ours", "org_b": "Theirs"} {
		board, err := tr.st.CreateBoard(tr.ctx, store.Board{ID: id.New("brd"), OrgID: org, Name: "Work", CreatedBy: "ada"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.st.CreateCard(tr.ctx, store.Card{ID: id.New("crd"), BoardID: board.ID, Title: title, Status: store.CardTodo, CreatedBy: "ada"}); err != nil {
			t.Fatal(err)
		}
	}
	queued := tr.task(t, a, "later")
	if code := tr.do(t, "POST", taskPath(queued.ID, "/tools/board_list_cards"), token, map[string]any{}, nil); code != http.StatusConflict {
		t.Errorf("a tool on a queued task = %d, want 409", code)
	}
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	tool := func(name string, args any) protocol.ToolResult {
		t.Helper()
		var res protocol.ToolResult
		if code := tr.do(t, "POST", taskPath(queued.ID, "/tools/"+name), token, args, &res); code != http.StatusOK {
			t.Fatalf("tool %s = %d", name, code)
		}
		return res
	}

	if res := tool("board_list_cards", map[string]any{}); res.IsError ||
		!strings.Contains(res.Content, "Ours") || strings.Contains(res.Content, "Theirs") {
		t.Errorf("board_list_cards = %+v, want only this organisation's card", res)
	}
	if res := tool("no_such_tool", map[string]any{}); !res.IsError {
		t.Errorf("an unknown tool = %+v, want an error the model reads", res)
	}
	if res := tool("board_list_cards", []byte("not json")); !res.IsError {
		t.Errorf("arguments that are not JSON = %+v, want an error the model reads", res)
	}
	big := append([]byte(`{"status":"`), bytes.Repeat([]byte("x"), 8<<20)...)
	if code := tr.do(t, "POST", taskPath(queued.ID, "/tools/board_list_cards"), token, big, nil); code != http.StatusRequestEntityTooLarge {
		t.Errorf("arguments over 8 MiB = %d, want 413", code)
	}
}

// A tool may outlast the server's write timeout. Its answer must still
// reach the runtime, or the model might do the work again.
func TestAToolAnswersPastTheWriteTimeout(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	task := tr.task(t, a, "now")
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	// Every answer is late on this server.
	srv := httptest.NewUnstartedServer(tr.rn.Handler())
	srv.Config.WriteTimeout = time.Nanosecond
	srv.Start()
	t.Cleanup(srv.Close)
	late := *tr
	late.url = srv.URL
	var res protocol.ToolResult
	if code := late.do(t, "POST", taskPath(task.ID, "/tools/board_list_cards"), token, map[string]any{}, &res); code != http.StatusOK {
		t.Errorf("a tool past the write timeout = %d, want 200", code)
	}
}

func TestOnlyAWorkspaceCallsToolsWithoutATask(t *testing.T) {
	tr := openTest(t)
	_, agentToken := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	_, wsToken := tr.agent(t, "org_a", "lab", store.KindWorkspace, true)
	path := protocol.WorkspaceToolPath + "/board_list_cards"

	if code := tr.do(t, "POST", path, agentToken, map[string]any{}, nil); code != http.StatusNotFound {
		t.Errorf("an agent calling a workspace tool = %d, want 404", code)
	}
	var res protocol.ToolResult
	if code := tr.do(t, "POST", path, wsToken, map[string]any{}, &res); code != http.StatusOK || res.IsError {
		t.Errorf("a workspace calling a tool = %d %+v", code, res)
	}
}

func TestGitWorksOnlyDuringATaskAndOnlyForBoundRepositories(t *testing.T) {
	tr := openTest(t)
	a, token := tr.agent(t, "org_a", "scout", store.KindAgent, true)
	_, wsToken := tr.agent(t, "org_a", "lab", store.KindWorkspace, true)
	refs := protocol.GitPath + "/bnd_unknown/web.git/info/refs?service=git-upload-pack"

	if code := tr.do(t, "GET", refs, token, nil, nil); code != http.StatusConflict {
		t.Errorf("git with no task running = %d, want 409", code)
	}
	tr.task(t, a, "Do it")
	if _, code := tr.claim(t, token); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	for name, c := range map[string]struct {
		method, path, token string
	}{
		"an unbound repository":       {"GET", refs, token},
		"a path git's protocol lacks": {"GET", protocol.GitPath + "/bnd_unknown/web.git/HEAD", token},
		"the dumb protocol":           {"GET", protocol.GitPath + "/bnd_unknown/web.git/info/refs", token},
		"a workspace's unbound repo":  {"GET", refs, wsToken},
	} {
		if code := tr.do(t, c.method, c.path, c.token, nil, nil); code != http.StatusNotFound {
			t.Errorf("git on %s = %d, want 404", name, code)
		}
	}
}
