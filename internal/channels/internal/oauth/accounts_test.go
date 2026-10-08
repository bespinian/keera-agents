package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestAccounts(t *testing.T) {
	refresh := "r1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"refresh_token":"` + refresh + `"}`))
	}))
	defer srv.Close()
	asked := 0
	s := NewAccounts(AccountsConfig{
		Service: "Acme", HTTP: srv.Client(),
		App: func(channels.Connection) (App, error) { return App{TokenURL: srv.URL}, nil },
		Who: func(context.Context, channels.Connection, string) (string, error) {
			asked++
			return "ada@acme.ch", nil
		},
		Lifetime: 15 * time.Minute,
	})
	ctx := context.Background()
	g, err := s.Exchange(ctx, channels.Connection{}, "https://k/cb", "c", "v")
	if err != nil || g.Account != "ada@acme.ch" || asked != 1 || time.Until(g.Expiry) > 15*time.Minute {
		t.Fatalf("Exchange = %+v, %v after %d asks", g, err, asked)
	}
	if g, err := s.Refresh(ctx, channels.Connection{}, "r1"); err != nil || time.Until(g.Expiry) > 15*time.Minute {
		t.Fatalf("Refresh = %+v, %v", g, err)
	}
	// Without a refresh token, agents could not keep working as the person.
	refresh = ""
	if _, err := s.Exchange(ctx, channels.Connection{}, "https://k/cb", "c", "v"); err == nil ||
		!strings.Contains(err.Error(), "no refresh token came from Acme") {
		t.Errorf("no refresh token: %v", err)
	}
}
