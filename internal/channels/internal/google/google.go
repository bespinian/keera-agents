// Package google signs in to Google APIs as a service account and reads
// their errors, for the Google Chat and Google Drive adapters.
package google

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

// Auth gets access tokens for service account key files.
type Auth struct {
	hc       *http.Client
	tokenURL string
	cache    oauth.Cache
}

// NewAuth signs in at the "token" endpoint, by default
// https://oauth2.googleapis.com/token.
func NewAuth(opts channels.Options) *Auth {
	return &Auth{hc: web.Client(opts), tokenURL: web.Endpoint(opts, "token", "https://oauth2.googleapis.com/token")}
}

// Token returns a cached token for the key file, scope and subject.
func (a *Auth) Token(ctx context.Context, keyJSON, scope, subject string) (string, error) {
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

// Forget drops a token after Google refused it, so the next call gets a new
// one.
func (a *Auth) Forget(keyJSON, scope, subject string) {
	a.cache.Forget(web.Fingerprint(keyJSON, scope, subject))
}

// Do sends req with a token for the key file, scope and subject, and returns
// the body of a 2xx answer, of at most limit bytes. A refused token is
// forgotten.
func (a *Auth) Do(req *http.Request, keyJSON, scope, subject string, limit int64) ([]byte, error) {
	token, err := a.Token(req.Context(), keyJSON, scope, subject)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return web.Do(a.hc, req, "Google", limit, func(status int, body []byte) error {
		if status == http.StatusUnauthorized {
			a.Forget(keyJSON, scope, subject)
		}
		return Error(status, body)
	})
}

func (a *Auth) fetch(ctx context.Context, sa *oauth.ServiceAccount, scope, subject string) (oauth.Token, error) {
	assertion, err := sa.Assertion(a.tokenURL, scope, subject, time.Now())
	if err != nil {
		return oauth.Token{}, err
	}
	return oauth.JWTBearer(ctx, a.hc, a.tokenURL, assertion)
}

// Error describes a failed API call from its status and body.
func Error(status int, body []byte) *web.APIError {
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
