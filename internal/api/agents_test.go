package api

import (
	"testing"

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
