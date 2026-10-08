package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// OIDC signs people in with an OpenID Connect provider the organisation
// runs, such as Entra ID, Okta, Keycloak or Google, for a service that trusts
// that provider's ID tokens: a Kubernetes cluster, or AWS. Calls then carry
// the person's ID token.
type OIDC struct {
	hc *http.Client
	// providers are each issuer's endpoints, from its discovery document.
	providers *web.Cache[provider]
}

type provider struct {
	auth, token string
	// offline is a provider that wants the scope offline_access for a
	// refresh token.
	offline bool
}

func NewOIDC(hc *http.Client) *OIDC {
	return &OIDC{hc: hc, providers: web.NewCache[provider](time.Hour, 100)}
}

// OIDCFields are the settings of a connector people sign in to with the
// organisation's own provider.
func OIDCFields() []channels.Field {
	return []channels.Field{
		{
			Name: "issuer_url", Label: "Issuer URL", Required: true, Endpoint: true,
			Help: "Your identity provider's issuer, such as https://login.microsoftonline.com/{tenant}/v2.0, " +
				"https://acme.okta.com or https://accounts.google.com.",
		},
		{Name: "client_id", Label: "Client ID", Required: true},
		{Name: "client_secret", Label: "Client secret", Secret: true, Required: true},
		{
			Name: "scopes", Label: "More scopes",
			Help: "Optional scopes besides openid and email, such as groups, when the service reads them from the ID token.",
		},
	}
}

// Accounts signs people in for service. setup is the steps on the
// service's side, which may name {callback}. first are the kind's own
// settings, shown before the provider's, such as the cluster's address.
func (o *OIDC) Accounts(service, setup string, first ...channels.Field) Accounts {
	return NewAccounts(AccountsConfig{
		Service: service,
		HTTP:    o.hc,
		SignIn:  channels.SignIn{Fields: append(slices.Clone(first), OIDCFields()...), Setup: setup},
		App:     o.app,
		IDToken: true,
	})
}

func (o *OIDC) app(conn channels.Connection) (App, error) {
	issuer, ok := web.BaseURL(web.Value(conn, "issuer_url"))
	id, secret := web.Value(conn, "client_id"), web.Value(conn, "client_secret")
	switch {
	case !ok || issuer == "":
		return App{}, errors.New("the issuer URL must be https://, such as https://acme.okta.com")
	case id == "" || secret == "":
		return App{}, errors.New("the client ID or secret is missing")
	}
	// Signing in has no context of its own; the discovery document is
	// cached, so this waits at most once an hour.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := o.discover(ctx, issuer)
	if err != nil {
		return App{}, err
	}
	app := App{
		AuthURL: p.auth, TokenURL: p.token, ClientID: id, Secret: secret,
		Scopes: append([]string{"openid", "email"}, strings.FieldsFunc(web.Value(conn, "scopes"), func(r rune) bool {
			return r == ' ' || r == ','
		})...),
	}
	switch {
	case p.offline:
		app.Scopes = append(app.Scopes, "offline_access")
	case issuer == "https://accounts.google.com":
		// Google gives a refresh token for these instead.
		app.Params = url.Values{"access_type": {"offline"}, "prompt": {"consent"}}
	}
	app.Scopes = slices.Compact(app.Scopes)
	return app, nil
}

// discover reads the issuer's discovery document.
func (o *OIDC) discover(ctx context.Context, issuer string) (provider, error) {
	if p, ok := o.providers.Get(issuer, time.Now()); ok {
		return p, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return provider{}, err
	}
	b, err := web.Do(o.hc, req, "the identity provider", 1<<20, func(status int, _ []byte) error {
		return fmt.Errorf("found no OpenID Connect provider at %s (HTTP %d); check the issuer URL", issuer, status)
	})
	if err != nil {
		return provider{}, err
	}
	var d struct {
		Issuer string   `json:"issuer"`
		Auth   string   `json:"authorization_endpoint"`
		Token  string   `json:"token_endpoint"`
		Scopes []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return provider{}, fmt.Errorf("the identity provider's discovery document cannot be read: %w", err)
	}
	// The document must be the issuer's own, and send people and secrets
	// only over https.
	if strings.TrimRight(d.Issuer, "/") != issuer {
		return provider{}, fmt.Errorf("the identity provider names its issuer %s, not %s; enter that", web.Short(d.Issuer, 100), issuer)
	}
	for _, u := range []string{d.Auth, d.Token} {
		if !strings.HasPrefix(u, "https://") {
			return provider{}, errors.New("the identity provider's endpoints are not all https://")
		}
	}
	p := provider{auth: d.Auth, token: d.Token, offline: slices.Contains(d.Scopes, "offline_access")}
	o.providers.Put(issuer, p, time.Now())
	return p, nil
}
