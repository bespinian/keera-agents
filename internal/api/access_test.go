package api

import (
	"testing"

	"github.com/bespinian/keera-agents/internal/store"
)

var (
	admin    = Principal{UserID: "usr_root", Email: "root@acme.ch", OrgID: "org_a", CanAdminOrg: true}
	operator = Principal{UserID: "usr_op", Email: "op@keera.ch", OrgID: "org_a", Role: "operator"}
	owner    = Principal{UserID: "usr_ada", Email: "ada@acme.ch", OrgID: "org_a"}
	member   = Principal{UserID: "usr_bob", Email: "bob@acme.ch", OrgID: "org_a"}
	outsider = Principal{UserID: "usr_eve", Email: "eve@bravo.ch", OrgID: "org_b", CanAdminOrg: true}
)

func TestOnlyTheOwnerAndAdministratorsEditAnAgent(t *testing.T) {
	a := store.Agent{OrgID: "org_a", OwnerID: "usr_ada"}
	for _, tc := range []struct {
		name string
		p    Principal
		want bool
	}{
		{"owner", owner, true},
		{"administrator", admin, true},
		{"operator", operator, true},
		{"member", member, false},
	} {
		if got := canEdit(tc.p, a); got != tc.want {
			t.Errorf("%s: canEdit = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWhoSeesATask(t *testing.T) {
	asked := store.Task{OrgID: "org_a", RequestedByID: "usr_bob", RequestedBy: "bob@acme.ch", AgentOwnerID: "usr_ada"}
	// A chat message from someone the gateway did not know has an email
	// only.
	byEmail := store.Task{OrgID: "org_a", RequestedBy: "bob@acme.ch", AgentOwnerID: "usr_ada"}
	others := store.Task{OrgID: "org_a", RequestedByID: "usr_cy", RequestedBy: "cy@acme.ch", AgentOwnerID: "usr_ada"}
	for _, tc := range []struct {
		name string
		p    Principal
		t    store.Task
		want bool
	}{
		{"who asked", member, asked, true},
		{"who asked, by email", member, byEmail, true},
		{"the agent's owner", owner, others, true},
		{"an administrator", admin, others, true},
		{"another member", member, others, false},
		{"an administrator of another organisation", outsider, asked, false},
	} {
		if got := canSeeTask(tc.p, tc.t); got != tc.want {
			t.Errorf("%s: canSeeTask = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestVisibleToNarrowsOnlyMembers(t *testing.T) {
	q := store.TaskQuery{OrgID: "org_a"}
	if got := visibleTo(admin, q); got.ViewerID != "" || got.ViewerEmail != "" {
		t.Errorf("an administrator's query was narrowed: %+v", got)
	}
	if got := visibleTo(member, q); got.ViewerID != "usr_bob" || got.ViewerEmail != "bob@acme.ch" {
		t.Errorf("a member's query was not narrowed: %+v", got)
	}
}

func TestOnlyTheOwnerConnectsToAWorkspace(t *testing.T) {
	ws := store.Agent{Kind: store.KindWorkspace, OrgID: "org_a", OwnerID: "usr_ada"}
	if v := viewWorkspace(owner, ws); !v.CanEdit || !v.CanConnect {
		t.Errorf("owner: %+v", v)
	}
	if v := viewWorkspace(admin, ws); !v.CanEdit || v.CanConnect {
		t.Errorf("administrator: can edit %v, can connect %v", v.CanEdit, v.CanConnect)
	}
}

func TestAgentViewSaysWhoMayGiveWork(t *testing.T) {
	a := store.Agent{OrgID: "org_a", OwnerID: "usr_ada", OwnerEmail: "ada@acme.ch", Name: "Scout", Access: store.AccessOwner}
	if v := viewAgent(member, a, store.AgentLoad{}, 0); v.CanEdit || v.CanGive {
		t.Errorf("a member, on an agent for its owner only: %+v", v)
	}
	// An administrator changes it, but gives it work only as its rule
	// says.
	if v := viewAgent(admin, a, store.AgentLoad{}, 0); !v.CanEdit || v.CanGive {
		t.Errorf("an administrator: can edit %v, can give %v", v.CanEdit, v.CanGive)
	}
	a.Access, a.AllowedEmails = store.AccessPeople, []string{"bob@acme.ch"}
	if v := viewAgent(member, a, store.AgentLoad{}, 0); !v.CanGive {
		t.Error("a member on the list may not give work")
	}
}
