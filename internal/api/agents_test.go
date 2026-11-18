package api

import (
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
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
