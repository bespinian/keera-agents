package fleet

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
)

func TestWithoutADriverNothingStarts(t *testing.T) {
	m := New(nil, nil, nil, nil, Options{})
	ctx := context.Background()
	a := store.Agent{ID: "agt_1", Enabled: true}

	if m.Runs() || m.DriverName() != "" || len(m.Tiers()) != 0 {
		t.Fatalf("Runs = %v, DriverName = %q, Tiers = %v", m.Runs(), m.DriverName(), m.Tiers())
	}
	if err := m.Start(ctx, a); !errors.Is(err, ErrNoRuntime) {
		t.Errorf("Start = %v", err)
	}
	if err := m.Wake(ctx, a.ID); !errors.Is(err, ErrNoRuntime) {
		t.Errorf("Wake = %v", err)
	}
	if _, err := m.Logs(ctx, a.ID, false); !errors.Is(err, ErrNoRuntime) {
		t.Errorf("Logs = %v", err)
	}
	if _, _, err := m.Reach(ctx, a.ID); !errors.Is(err, ErrNoRuntime) {
		t.Errorf("Reach = %v", err)
	}
	// It touches no store, which is nil here.
	m.Reconcile(ctx)
}

func TestTiersStopAtTheOperatorsMinimum(t *testing.T) {
	d := newFakeDriver()
	d.tiers = []runtime.Isolation{runtime.Standard, runtime.Isolated, runtime.VM}
	m := New(nil, nil, d, nil, Options{Limits: runtime.Limits{MinIsolation: runtime.Isolated}})

	if got := m.Tiers(); !slices.Equal(got, []runtime.Isolation{runtime.Isolated, runtime.VM}) {
		t.Fatalf("Tiers = %v", got)
	}
	if m.DriverName() != "fake" || !m.Runs() {
		t.Fatalf("DriverName = %q, Runs = %v", m.DriverName(), m.Runs())
	}
	if err := m.CheckClass(runtime.Class{Isolation: runtime.Standard}.WithDefaults()); err == nil {
		t.Fatal("a class below the minimum was allowed")
	}
}

func TestStaleOnlyWhenARunningContainerHasAnotherToken(t *testing.T) {
	a := store.Agent{RuntimeTokenHash: TokenHash("kat_new")}
	for _, c := range []struct {
		name string
		st   runtime.Status
		want bool
	}{
		{"same token", runtime.Status{State: runtime.Running, Token: "kat_new"}, false},
		{"old token", runtime.Status{State: runtime.Running, Token: "kat_old"}, true},
		{"token unknown", runtime.Status{State: runtime.Running}, false},
		{"not running", runtime.Status{State: runtime.Pending, Token: "kat_old"}, false},
	} {
		if got := stale(a, c.st); got != c.want {
			t.Errorf("%s: stale = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIngressSecretChangesWithEachStart(t *testing.T) {
	box, _ := secret.New("0123456789abcdef")
	m := New(nil, nil, nil, box, Options{})

	if s := m.IngressSecret(store.Agent{ID: "agt_1"}); s != "" {
		t.Fatalf("a workspace that never started has secret %q", s)
	}
	first := m.IngressSecret(store.Agent{ID: "agt_1", RuntimeTokenHash: TokenHash("a")})
	again := m.IngressSecret(store.Agent{ID: "agt_1", RuntimeTokenHash: TokenHash("a")})
	next := m.IngressSecret(store.Agent{ID: "agt_1", RuntimeTokenHash: TokenHash("b")})
	other := m.IngressSecret(store.Agent{ID: "agt_2", RuntimeTokenHash: TokenHash("a")})
	if first == "" || first != again {
		t.Fatalf("not the same secret twice: %q, %q", first, again)
	}
	if first == next || first == other {
		t.Fatal("another start or another workspace has the same secret")
	}
}

func TestKeyNamesSayWhatSpentThem(t *testing.T) {
	if got := keyName(store.Agent{Kind: store.KindAgent, Name: "Scout"}); got != "Agent Scout" {
		t.Errorf("agent key = %q", got)
	}
	if got := keyName(store.Agent{Kind: store.KindWorkspace, Name: "Lab"}); got != "Workspace Lab" {
		t.Errorf("workspace key = %q", got)
	}
}
