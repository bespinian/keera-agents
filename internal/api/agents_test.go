package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/gateway"
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
