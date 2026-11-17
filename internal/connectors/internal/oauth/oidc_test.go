package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/jwttest"
)

func TestOIDC(t *testing.T) {
	key := jwttest.NewKey(t, "k")
	exp := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	idToken := key.Sign(t, map[string]any{"email": "ada@acme.ch", "exp": exp.Unix()})
	withID := true
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `","authorization_endpoint":"` + srv.URL + `/auth",
				"token_endpoint":"` + srv.URL + `/token","scopes_supported":["openid","email","offline_access"]}`))
		case "/other/.well-known/openid-configuration":
			_, _ = w.Write([]byte(`{"issuer":"https://evil.example","authorization_endpoint":"https://evil.example/a","token_endpoint":"https://evil.example/t"}`))
		case "/token":
			if !withID {
				_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"refresh_token":"r"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"refresh_token":"r","id_token":"` + idToken + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	accounts := NewOIDC(srv.Client()).Accounts("Register {callback}.")
	conn := connectors.Connection{
		PerUser: true,
		Config:  map[string]string{"issuer_url": srv.URL + "/", "client_id": "cid", "scopes": "groups"},
		Secrets: map[string]string{"client_secret": "pw"},
	}
	ctx := context.Background()

	u, err := accounts.SignInURL(conn, "https://k/cb", "st", "ch")
	q, _ := url.Parse(u)
	if err != nil || !strings.HasPrefix(u, srv.URL+"/auth?") || q.Query().Get("scope") != "openid email groups offline_access" {
		t.Fatalf("SignInURL = %s, %v", u, err)
	}
	// The ID token is what the service is called with, until it expires.
	g, err := accounts.Exchange(ctx, conn, "https://k/cb", "code", "v")
	if err != nil || g.Access != idToken || !g.Expiry.Equal(exp) || g.Account != "ada@acme.ch" {
		t.Fatalf("Exchange = %+v, %v", g, err)
	}
	withID = false
	if _, err := accounts.Refresh(ctx, conn, "r"); err == nil || !strings.Contains(err.Error(), "no ID token") {
		t.Errorf("a refresh without an ID token: %v", err)
	}

	// A discovery document of another issuer is not trusted.
	conn.Config["issuer_url"] = srv.URL + "/other"
	if _, err := accounts.SignInURL(conn, "https://k/cb", "st", "ch"); err == nil || !strings.Contains(err.Error(), "evil.example") {
		t.Errorf("another issuer's document: %v", err)
	}
	conn.Config["issuer_url"] = "http://plain.example"
	if _, err := accounts.SignInURL(conn, "https://k/cb", "st", "ch"); err == nil {
		t.Error("an issuer without TLS passed")
	}
}
