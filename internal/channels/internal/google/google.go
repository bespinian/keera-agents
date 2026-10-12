// Package google signs in to Google APIs as a service account, or signs
// people in with their own accounts, and reads the APIs' errors, for the
// Google adapters.
package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// Auth gets access tokens for service account key files, and for people
// who sign in themselves.
type Auth struct {
	hc       *http.Client
	tokenURL string
	authURL  string
	cache    oauth.Cache
}

// NewAuth signs in at the "token" endpoint, by default
// https://oauth2.googleapis.com/token. People sign in at the "auth"
// endpoint, by default https://accounts.google.com/o/oauth2/v2/auth.
func NewAuth(opts channels.Options) *Auth {
	return &Auth{
		hc:       web.Client(opts),
		tokenURL: web.Endpoint(opts, "token", "https://oauth2.googleapis.com/token"),
		authURL:  web.Endpoint(opts, "auth", "https://accounts.google.com/o/oauth2/v2/auth"),
	}
}

// Accounts lets people sign in to a kind's connectors with their own Google
// accounts, for scope. apis names the APIs to enable, for the setup steps.
func (a *Auth) Accounts(scope, apis string) oauth.Accounts {
	return oauth.NewAccounts(oauth.AccountsConfig{
		Service: "Google",
		HTTP:    a.hc,
		SignIn: channels.SignIn{
			Fields: []channels.Field{
				{
					Name: "client_id", Label: "OAuth client ID", Required: true,
					Help: "The client ID of an OAuth client of type Web application.",
				},
				{Name: "client_secret", Label: "OAuth client secret", Secret: true, Required: true},
			},
			Setup: "1. In a Google Cloud project, enable the " + apis + ".\n" +
				"2. Under Google Auth Platform, set the audience to **Internal**, so only people in your Workspace can sign in.\n" +
				"3. Create an OAuth client of type **Web application**, with the authorized redirect URI {callback}.\n" +
				"4. Enter its client ID and secret here. Then each person connects their own Google account under Connectors.",
		},
		App: func(conn channels.Connection) (oauth.App, error) {
			id, secret := web.Value(conn, "client_id"), web.Value(conn, "client_secret")
			if id == "" || secret == "" {
				return oauth.App{}, errors.New("the OAuth client ID or secret is missing")
			}
			return oauth.App{
				AuthURL: a.authURL, TokenURL: a.tokenURL, ClientID: id, Secret: secret,
				// The email says who signed in.
				Scopes: append(strings.Fields(scope), "openid", "email"),
				// offline gives a refresh token, so agents keep working after
				// the first hour. consent asks for it again on every sign-in:
				// Google sends a refresh token only with consent.
				Params: url.Values{"access_type": {"offline"}, "prompt": {"consent"}},
			}, nil
		},
	})
}

// token returns a cached token for the key file, scope and subject.
func (a *Auth) token(ctx context.Context, keyJSON, scope, subject string) (string, error) {
	sa, err := oauth.ParseServiceAccount(keyJSON)
	if err != nil {
		return "", err
	}
	tok, err := a.cache.Get(ctx, web.Fingerprint(keyJSON, scope, subject), func(ctx context.Context) (oauth.Token, error) {
		return a.fetch(ctx, sa, scope, subject)
	})
	if err != nil {
		return "", oauth.Describe("Google", err)
	}
	return tok, nil
}

// Check gets a new token, bypassing the cache, to prove the key works.
func (a *Auth) Check(ctx context.Context, keyJSON, scope, subject string) error {
	sa, err := oauth.ParseServiceAccount(keyJSON)
	if err != nil {
		return err
	}
	if _, err := a.fetch(ctx, sa, scope, subject); err != nil {
		return oauth.Describe("Google", err)
	}
	return nil
}

// forget drops a token after Google refused it, so the next call gets a new
// one.
func (a *Auth) forget(keyJSON, scope, subject string) {
	a.cache.Forget(web.Fingerprint(keyJSON, scope, subject))
}

// Creds are what a call is made with: a person's access Token, or else a
// service account's key file, acting as Subject when it is set.
type Creds struct {
	Key, Subject, Token string
}

// Do sends req with c, a service account's token being for scope, and
// returns the body of a 2xx answer, of at most limit bytes. A refused
// service account's token is forgotten.
func (a *Auth) Do(req *http.Request, c Creds, scope string, limit int64) ([]byte, error) {
	token := c.Token
	if token == "" {
		var err error
		if token, err = a.token(req.Context(), c.Key, scope, c.Subject); err != nil {
			return nil, err
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return web.Do(a.hc, req, "Google", limit, func(status int, body []byte) error {
		if status == http.StatusUnauthorized && c.Token == "" {
			a.forget(c.Key, scope, c.Subject)
		}
		return apiError(status, body)
	})
}

func (a *Auth) fetch(ctx context.Context, sa *oauth.ServiceAccount, scope, subject string) (oauth.Token, error) {
	assertion, err := sa.Assertion(a.tokenURL, scope, subject, time.Now())
	if err != nil {
		return oauth.Token{}, err
	}
	return oauth.JWTBearer(ctx, a.hc, a.tokenURL, assertion)
}

// apiError describes a failed API call from its status and body.
func apiError(status int, body []byte) *web.APIError {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	code := e.Error.Status
	if code == "" && len(e.Error.Errors) > 0 {
		code = e.Error.Errors[0].Reason
	}
	return &web.APIError{Service: "Google", Status: status, Code: code, Message: e.Error.Message}
}
