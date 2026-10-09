// Package microsoft signs in to Microsoft APIs as an app with client
// credentials, or signs people in with their own accounts, and reads the
// APIs' errors, for the Microsoft adapters.
package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// App is an app registration's credentials and the scope to ask for. With
// Token, a person's access token, calls are made as that person instead.
type App struct {
	Tenant, ClientID, Secret, Scope string
	Token                           string
}

// AppOf reads an app registration from a connection's settings, for scope.
// A connection people sign in to carries the token of the person a call is
// for.
func AppOf(conn channels.Connection, scope string) (App, error) {
	app, err := clientOf(conn)
	app.Scope, app.Token = scope, conn.Token
	if err == nil && conn.PerUser && app.Token == "" {
		err = oauth.ErrNoToken
	}
	return app, err
}

// clientOf reads only the app registration, as signing someone in needs.
func clientOf(conn channels.Connection) (App, error) {
	app := App{
		Tenant:   web.Value(conn, "tenant_id"),
		ClientID: web.Value(conn, "client_id"),
		Secret:   web.Value(conn, "client_secret"),
	}
	if app.Tenant == "" || app.ClientID == "" || app.Secret == "" {
		return app, errors.New("the tenant ID, client ID or client secret is missing")
	}
	return app, nil
}

// Fields are an app registration's settings.
func Fields() []channels.Field {
	return []channels.Field{
		{Name: "tenant_id", Label: "Tenant ID", Required: true, Help: "Your Microsoft Entra tenant ID."},
		{Name: "client_id", Label: "Client ID", Required: true, Help: "The application (client) ID of the app registration."},
		{
			Name: "client_secret", Label: "Client secret", Secret: true, Required: true,
			Help: "A client secret of the app registration, from Certificates & secrets.",
		},
	}
}

func (app App) key() string {
	return web.Fingerprint(app.Tenant, app.ClientID, app.Secret, app.Scope)
}

// Auth gets app-only tokens from Microsoft Entra.
type Auth struct {
	hc    *http.Client
	login string
	cache oauth.Cache
}

// NewAuth signs in at the "login" endpoint, by default
// https://login.microsoftonline.com.
func NewAuth(opts channels.Options) *Auth {
	return &Auth{hc: web.Client(opts), login: web.Endpoint(opts, "login", "https://login.microsoftonline.com")}
}

// token returns a cached token for the app.
func (a *Auth) token(ctx context.Context, app App) (string, error) {
	tok, err := a.cache.Get(ctx, app.key(), func(ctx context.Context) (oauth.Token, error) {
		return a.fetch(ctx, app)
	})
	if err != nil {
		return "", oauth.Describe("Microsoft", err)
	}
	return tok, nil
}

// Check gets a new token, bypassing the cache, to prove the credentials
// work.
func (a *Auth) Check(ctx context.Context, app App) error {
	if _, err := a.fetch(ctx, app); err != nil {
		return oauth.Describe("Microsoft", err)
	}
	return nil
}

// forget drops the app's token after a service refused it, so the next call
// gets a new one.
func (a *Auth) forget(app App) {
	a.cache.Forget(app.key())
}

// Do sends req with the app's token, or the person's, and returns the body
// of a 2xx answer, of at most limit bytes. A refused token is forgotten.
// service names who is called, such as "Microsoft Graph".
func (a *Auth) Do(req *http.Request, app App, service string, limit int64) ([]byte, error) {
	token := app.Token
	if token == "" {
		var err error
		if token, err = a.token(req.Context(), app); err != nil {
			return nil, err
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return web.Do(a.hc, req, service, limit, func(status int, body []byte) error {
		if status == http.StatusUnauthorized {
			a.forget(app)
		}
		return apiError(service, status, body)
	})
}

// Accounts lets people sign in to a kind's connectors with their own
// Microsoft accounts. scopes are the delegated permissions to ask for, and
// permissions how the setup steps name them.
func (a *Auth) Accounts(scopes []string, permissions string) oauth.Accounts {
	return oauth.NewAccounts(oauth.AccountsConfig{
		Service: "Microsoft",
		HTTP:    a.hc,
		SignIn: channels.SignIn{
			Fields: Fields(),
			Setup: "1. In Microsoft Entra, register an app for your tenant only, and create a client secret.\n" +
				"2. Under Authentication, add the platform **Web** with the redirect URI {callback}.\n" +
				"3. Under API permissions, add the **delegated** permissions " + permissions + ". " +
				"Grant admin consent, unless people may consent for themselves.\n" +
				"4. Enter the tenant ID, client ID and secret here. Then each person connects their own account under Connectors.",
		},
		App: func(conn channels.Connection) (oauth.App, error) {
			app, err := clientOf(conn)
			if err != nil {
				return oauth.App{}, err
			}
			base := a.login + "/" + url.PathEscape(app.Tenant) + "/oauth2/v2.0"
			return oauth.App{
				AuthURL: base + "/authorize", TokenURL: base + "/token", ClientID: app.ClientID, Secret: app.Secret,
				// offline_access gives a refresh token; the rest names who signed in.
				Scopes: append(slices.Clone(scopes), "offline_access", "openid", "email", "profile"),
			}, nil
		},
	})
}

func (a *Auth) fetch(ctx context.Context, app App) (oauth.Token, error) {
	tokenURL := a.login + "/" + url.PathEscape(app.Tenant) + "/oauth2/v2.0/token"
	return oauth.ClientCredentials(ctx, a.hc, tokenURL, app.ClientID, app.Secret, app.Scope)
}

// apiError describes a failed call from its status and body, which Microsoft's
// APIs shape as {"error":{"code","message"}}. service names who answered,
// such as "Microsoft Graph".
func apiError(service string, status int, body []byte) *web.APIError {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return &web.APIError{Service: service, Status: status, Code: e.Error.Code, Message: e.Error.Message}
}
