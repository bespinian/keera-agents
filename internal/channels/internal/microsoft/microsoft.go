// Package microsoft signs in to Microsoft APIs as an app with client
// credentials and reads their errors, for the Teams and OneDrive adapters.
package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// App is an app registration's credentials and the scope to ask for.
type App struct {
	Tenant, ClientID, Secret, Scope string
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

// Token returns a cached token for the app.
func (a *Auth) Token(ctx context.Context, app App) (string, error) {
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

// Forget drops the app's token after a service refused it, so the next call
// gets a new one.
func (a *Auth) Forget(app App) {
	a.cache.Forget(app.key())
}

// Do sends req with the app's token and returns the body of a 2xx answer,
// of at most limit bytes. A refused token is forgotten. service names who
// is called, such as "Microsoft Graph".
func (a *Auth) Do(req *http.Request, app App, service string, limit int64) ([]byte, error) {
	token, err := a.Token(req.Context(), app)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return web.Do(a.hc, req, service, limit, func(status int, body []byte) error {
		if status == http.StatusUnauthorized {
			a.Forget(app)
		}
		return Error(service, status, body)
	})
}

func (a *Auth) fetch(ctx context.Context, app App) (oauth.Token, error) {
	tokenURL := a.login + "/" + url.PathEscape(app.Tenant) + "/oauth2/v2.0/token"
	return oauth.ClientCredentials(ctx, a.hc, tokenURL, app.ClientID, app.Secret, app.Scope)
}

// Error describes a failed call from its status and body, which Microsoft's
// APIs shape as {"error":{"code","message"}}. service names who answered,
// such as "Microsoft Graph".
func Error(service string, status int, body []byte) *web.APIError {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return &web.APIError{Service: service, Status: status, Code: e.Error.Code, Message: e.Error.Message}
}
