package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
)

// Accounts lets people sign in to a kind's connectors with their own
// accounts. An adapter embeds it to implement channels.PerUser, and calls
// CheckClient from its Check for a connector people sign in to.
type Accounts struct {
	c AccountsConfig
}

// AccountsConfig is how one service signs people in.
type AccountsConfig struct {
	// Service names it in errors, such as "Google".
	Service string
	HTTP    *http.Client
	// SignIn is what the administrator sets up.
	SignIn channels.SignIn
	// App reads the OAuth client from a connection's settings.
	App func(conn channels.Connection) (App, error)
	// Who asks the service who signed in, for a service whose token answer
	// names nobody. Nil leaves it to the ID token.
	Who func(ctx context.Context, conn channels.Connection, access string) (string, error)
	// Lifetime caps how long an access token is used, for a service that
	// does not say when it expires. Zero trusts the service.
	Lifetime time.Duration
	// IDToken calls the service with the ID token instead of the access
	// token, as a Kubernetes cluster or AWS STS take one from an identity
	// provider they trust.
	IDToken bool
}

func NewAccounts(c AccountsConfig) Accounts { return Accounts{c: c} }

func (s Accounts) SignIn() channels.SignIn { return s.c.SignIn }

// WithFields adds a kind's own settings to the sign-in's, such as the drive
// to work in.
func (s Accounts) WithFields(fields ...channels.Field) Accounts {
	s.c.SignIn.Fields = append(slices.Clone(s.c.SignIn.Fields), fields...)
	return s
}

func (s Accounts) SignInURL(conn channels.Connection, redirect, state, challenge string) (string, error) {
	app, err := s.c.App(conn)
	if err != nil {
		return "", err
	}
	return app.SignInURL(redirect, state, challenge), nil
}

func (s Accounts) Exchange(ctx context.Context, conn channels.Connection, redirect, code, verifier string) (channels.Grant, error) {
	app, err := s.c.App(conn)
	if err != nil {
		return channels.Grant{}, err
	}
	tok, err := app.Exchange(ctx, s.c.HTTP, redirect, code, verifier)
	if err != nil {
		return channels.Grant{}, Describe(s.c.Service, err)
	}
	if tok.Refresh == "" {
		return channels.Grant{}, fmt.Errorf("no refresh token came from %s, so agents could not keep working as you; "+
			"check the app may keep access, and try again", s.c.Service)
	}
	g, err := s.grant(tok)
	if err != nil {
		return g, err
	}
	g.Account = Email(tok.IDToken)
	if g.Account == "" && s.c.Who != nil {
		// Only a label for the panel: signing in worked without it.
		g.Account, _ = s.c.Who(ctx, conn, tok.Access)
	}
	return g, nil
}

func (s Accounts) Refresh(ctx context.Context, conn channels.Connection, refresh string) (channels.Grant, error) {
	app, err := s.c.App(conn)
	if err != nil {
		return channels.Grant{}, err
	}
	tok, err := app.Refresh(ctx, s.c.HTTP, refresh)
	if err != nil {
		return channels.Grant{}, Describe(s.c.Service, err)
	}
	return s.grant(tok)
}

func (s Accounts) grant(tok Token) (channels.Grant, error) {
	if s.c.IDToken {
		if tok.IDToken == "" {
			return channels.Grant{}, fmt.Errorf("%s sent no ID token; check the client may ask for the scope openid", s.c.Service)
		}
		tok.Access = tok.IDToken
		if exp := expiry(tok.IDToken); !exp.IsZero() {
			tok.Expiry = exp
		}
	}
	if limit := time.Now().Add(s.c.Lifetime); s.c.Lifetime > 0 && tok.Expiry.After(limit) {
		tok.Expiry = limit
	}
	return channels.Grant{Refresh: tok.Refresh, Access: tok.Access, Expiry: tok.Expiry}, nil
}

// CheckClient proves the service knows the OAuth client, as Check does for
// a connector people sign in to: nobody has signed in yet.
func (s Accounts) CheckClient(ctx context.Context, conn channels.Connection) error {
	app, err := s.c.App(conn)
	if err != nil {
		return err
	}
	if err := app.Check(ctx, s.c.HTTP); err != nil {
		return fmt.Errorf("%w; check the OAuth client's ID and secret", Describe(s.c.Service, err))
	}
	return nil
}

// ErrNoToken is a call on a connector people sign in to that is for nobody.
// Dispatch always gives one, so it means a call it did not make.
var ErrNoToken = errors.New("nobody is signed in to this connector for the call")
