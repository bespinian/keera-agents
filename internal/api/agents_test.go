package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/store"
)

func TestHeldByOther(t *testing.T) {
	mine := store.Agent{ID: "agt_1", OwnerID: "usr_mallory"}
	own := store.Binding{AgentID: "agt_2", AgentOwnerID: "usr_mallory"}
	theirs := store.Binding{AgentID: "agt_3", AgentOwnerID: "usr_ada", AgentName: "Scout"}
	if _, ok := heldByOther(nil, mine); ok {
		t.Error("an empty conversation is held")
	}
	if _, ok := heldByOther([]store.Binding{own}, mine); ok {
		t.Error("the owner's own agent blocks them")
	}
	if b, ok := heldByOther([]store.Binding{own, theirs}, mine); !ok || b.AgentName != "Scout" {
		t.Errorf("another's agent does not block: %+v, %v", b, ok)
	}
}

func TestMayNameMailbox(t *testing.T) {
	mail := connectors.Descriptor{Mailbox: true}
	shared := store.Connection{}
	for _, c := range []struct {
		name   string
		p      Principal
		desc   connectors.Descriptor
		conn   store.Connection
		target string
		ok     bool
	}{
		{"a member's own mailbox", member, mail, shared, "Bob@Acme.ch", true},
		{"another's mailbox", member, mail, shared, "ceo@acme.ch", false},
		{"an administrator names any", admin, mail, shared, "ceo@acme.ch", true},
		{"me on a connector people sign in to", member, mail, store.Connection{PerUser: true}, "me", true},
		{"not a mail kind", member, connectors.Descriptor{}, shared, "PROJ", true},
	} {
		if err := mayNameMailbox(c.p, c.desc, c.conn, c.target); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// An administrator may create an agent for an active member of their own
// organisation, and nobody else may create one for someone else.
func TestNewOwner(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("org_id") == "org_a" && q.Get("id") == "usr_bob":
			_, _ = w.Write([]byte(`{"id":"usr_bob","email":"bob@acme.ch"}`))
		case q.Get("org_id") == "org_a" && q.Get("id") == "usr_gone":
			_, _ = w.Write([]byte(`{"id":"usr_gone","email":"gone@acme.ch","disabled":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"that person is not in this organisation"}}`))
		}
	}))
	defer gw.Close()
	s := testServer(t)
	s.gw = gateway.New(gw.URL, "service")

	for _, tc := range []struct {
		name string
		p    Principal
		user string
		want int
	}{
		{"an admin, for a member", admin, "usr_bob", http.StatusOK},
		{"an operator, for a member", operator, "usr_bob", http.StatusOK},
		{"a member, for another", owner, "usr_bob", http.StatusForbidden},
		{"for a disabled member", admin, "usr_gone", http.StatusBadRequest},
		{"for someone in another organisation", outsider, "usr_bob", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		u, ok := s.newOwner(w, httptest.NewRequest("POST", "/", nil), tc.p, tc.user)
		if ok != (tc.want == http.StatusOK) || w.Code != tc.want {
			t.Errorf("%s: ok %v, %d %s; want %d", tc.name, ok, w.Code, w.Body, tc.want)
		}
		if ok && u.Email != "bob@acme.ch" {
			t.Errorf("%s: owner %+v", tc.name, u)
		}
	}
}

// A machine's disk keeps the size it was made with, as its volume does, so
// it is billed for what it has.
func TestTheDiskIsFixedOnceTheMachineExists(t *testing.T) {
	s := testServer(t)
	s.fleet = fleet.New(nil, nil, nil, nil, fleet.Options{})
	existing := store.Agent{
		Name: "Scout", Access: store.AccessOwner,
		Class: runtime.Class{Isolation: runtime.Isolated, CPU: "1", Memory: "2Gi", Disk: "5Gi"},
	}
	for _, c := range []struct {
		name string
		a    store.Agent
		disk string
		ok   bool
	}{
		{"a new agent chooses", store.Agent{Name: "Scout", Access: store.AccessOwner}, "100Gi", true},
		{"the same size", existing, "5Gi", true},
		{"the same size written otherwise", existing, "5120Mi", true},
		{"smaller", existing, "64Mi", false},
		{"larger", existing, "100Gi", false},
	} {
		a := c.a
		class := existing.Class
		class.Disk = c.disk
		err := s.apply(t.Context(), admin, &a, agentInput{Class: &class})
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
		if err == nil && c.a.Class.Disk != "" && a.Class.Disk != c.a.Class.Disk {
			t.Errorf("%s: disk became %q", c.name, a.Class.Disk)
		}
	}
}

// catalogServer is a server whose gateway allows prj_big two chat models,
// a transcription model and two MCP servers, and prj_small one of each.
func catalogServer(t *testing.T) *Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project_id")
		models := `[{"alias":"keera-speed","kind":"model"},{"alias":"keera-think","kind":"model"}]`
		servers := `[{"alias":"github"},{"alias":"jira"}]`
		if project == "prj_small" {
			models, servers = `[{"alias":"keera-speed","kind":"model"}]`, `[{"alias":"github"}]`
		}
		_, _ = w.Write([]byte(`{"project_id":"` + project + `","models":` + models +
			`,"transcription_models":[{"alias":"whisper","kind":"model"}],"mcp_servers":` + servers + `}`))
	}))
	t.Cleanup(srv.Close)
	s := testServer(t)
	s.gw = gateway.New(srv.URL, "service")
	return s
}

// The models an agent uses as tools are checked with the gateway like its
// own: a transcription model among the transcription models, models to ask
// among the chat models. A new project checks them all.
func TestModelsAsToolsAreWhatTheProjectAllows(t *testing.T) {
	s := catalogServer(t)
	str := func(s string) *string { return &s }
	list := func(s ...string) *[]string { return &s }
	for _, c := range []struct {
		name string
		in   agentInput
		want string
	}{
		{"a transcription model", agentInput{TranscriptionModel: str("whisper")}, ""},
		{"none", agentInput{TranscriptionModel: str("")}, ""},
		{"a chat model to transcribe", agentInput{TranscriptionModel: str("keera-think")}, "transcription model"},
		{"models to ask", agentInput{AskModels: list("keera-think", " keera-speed", "keera-think")}, ""},
		{"a transcription model to ask", agentInput{AskModels: list("whisper")}, `"whisper" to ask`},
		{"a project without a model to ask", agentInput{ProjectID: str("prj_small")}, `"keera-think" to ask`},
	} {
		a := store.Agent{
			Name: "Scout", Access: store.AccessOwner, ProjectID: "prj_big", Model: "keera-speed",
			AskModels: []string{"keera-think"},
		}
		err := s.apply(t.Context(), admin, &a, c.in)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if c.name == "models to ask" && strings.Join(a.AskModels, ",") != "keera-speed,keera-think" {
			t.Errorf("ask models = %q", a.AskModels)
		}
	}
}

// MCP servers are checked with the gateway like models. null is all the
// project allows, a list only those, and empty none; left out keeps them.
func TestMCPServersAreWhatTheProjectAllows(t *testing.T) {
	s := catalogServer(t)
	for _, c := range []struct {
		name, body string
		want       []string
		fails      string
	}{
		{"left out", `{}`, []string{"jira"}, ""},
		{"all", `{"mcp_servers":null}`, nil, ""},
		{"some", `{"mcp_servers":["jira"," github","jira",""]}`, []string{"github", "jira"}, ""},
		{"none", `{"mcp_servers":[]}`, []string{}, ""},
		{"one it does not have", `{"mcp_servers":["nope"]}`, nil, `MCP server "nope"`},
		{"not a list", `{"mcp_servers":"github"}`, nil, "list"},
		{"a project without one", `{"project_id":"prj_small"}`, nil, `MCP server "jira"`},
	} {
		var in agentInput
		if err := json.Unmarshal([]byte(c.body), &in); err != nil {
			t.Fatal(err)
		}
		a := store.Agent{
			Name: "Scout", Access: store.AccessOwner, ProjectID: "prj_big", Model: "keera-speed",
			MCPServers: []string{"jira"},
		}
		err := s.apply(t.Context(), admin, &a, in)
		switch {
		case c.fails == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.fails != "" && (err == nil || !strings.Contains(err.Error(), c.fails)):
			t.Errorf("%s: %v, want %q", c.name, err, c.fails)
		case c.fails == "" && ((a.MCPServers == nil) != (c.want == nil) || !slices.Equal(a.MCPServers, c.want)):
			t.Errorf("%s: servers %#v, want %#v", c.name, a.MCPServers, c.want)
		}
	}
}
