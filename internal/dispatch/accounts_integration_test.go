package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/id"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
)

// rotating is a service people sign in to that replaces the refresh token
// on every refresh and refuses the one it replaced.
type rotating struct {
	mu      sync.Mutex
	current string
	n       int
}

const rotatingKind = "dispatchrotating"

func (*rotating) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{Kind: rotatingKind, Label: "Rotating"}
}

func (*rotating) Check(context.Context, connectors.Connection) error       { return nil }
func (*rotating) CheckClient(context.Context, connectors.Connection) error { return nil }
func (*rotating) SignIn() connectors.SignIn                                { return connectors.SignIn{} }

func (*rotating) SignInURL(connectors.Connection, string, string, string) (string, error) {
	return "", errors.New("not here")
}

func (*rotating) Exchange(context.Context, connectors.Connection, string, string, string) (connectors.Grant, error) {
	return connectors.Grant{}, errors.New("not here")
}

func (r *rotating) Refresh(_ context.Context, _ connectors.Connection, refresh string) (connectors.Grant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if refresh != r.current {
		return connectors.Grant{}, fmt.Errorf("%s was replaced, so the sign-in is revoked", refresh)
	}
	r.n++
	r.current = fmt.Sprintf("r%d", r.n)
	// Short-lived, so every call refreshes.
	return connectors.Grant{Refresh: r.current, Access: "a-" + r.current, Expiry: time.Now().Add(time.Minute)}, nil
}

// Two replicas refreshing one person's token at once must not use the same
// refresh token: the second would be refused.
func TestReplicasRefreshingAtOnceTakeTurns(t *testing.T) {
	e := setup(t)
	svc := &rotating{current: "r0"}
	connectors.Register(svc)
	c, err := e.st.CreateConnection(e.ctx, store.Connection{
		ID: id.New("con"), OrgID: "org_a", Kind: rotatingKind, Name: "Theirs", PerUser: true,
		CreatedBy: "ada@example.ch",
	})
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secret.New("0123456789abcdef")
	if err := e.st.SaveAccount(e.ctx, store.Account{
		ConnectionID: c.ID, UserID: "usr_ada", Account: "ada@acme.ch",
		Token: box.Seal(accountName(c.ID, "usr_ada"), []byte("r0")),
	}); err != nil {
		t.Fatal(err)
	}
	// Another replica: its own token cache, the same database.
	other := New(e.st, nil, nil, box, Options{Log: slog.New(slog.DiscardHandler)})

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		d := e.d
		if i%2 == 1 {
			d = other
		}
		wg.Go(func() {
			conn, err := d.ConnFor(e.ctx, c, "usr_ada")
			if err == nil && !strings.HasPrefix(conn.Token, "a-r") {
				err = fmt.Errorf("token %q", conn.Token)
			}
			if err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	a, _ := e.st.Account(e.ctx, c.ID, "usr_ada")
	if stored, _ := box.Open(accountName(c.ID, "usr_ada"), a.Token); string(stored) != svc.current {
		t.Fatalf("stored %q, the service has %q", stored, svc.current)
	}
}
