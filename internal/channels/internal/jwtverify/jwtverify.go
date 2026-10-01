// Package jwtverify checks the bearer tokens services sign their webhooks
// with, against the service's published keys.
package jwtverify

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	// refresh is how long keys are trusted without asking again. Services
	// announce new keys well before they sign with them.
	refresh = 24 * time.Hour
	// retry spaces out fetches for a key id we do not know, so forged
	// tokens with random ids cannot make us hammer the service.
	retry = 5 * time.Minute
	// retryEmpty is the same while we have no keys at all.
	retryEmpty = 10 * time.Second
)

// KeySet is a service's signing keys, fetched when needed and cached.
type KeySet struct {
	hc     *http.Client
	url    string
	openID bool

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	tried   time.Time
}

// NewJWKS reads keys from a JWKS document at url.
func NewJWKS(hc *http.Client, url string) *KeySet {
	return &KeySet{hc: hc, url: url}
}

// NewOpenID reads keys from the jwks_uri of an OpenID configuration.
func NewOpenID(hc *http.Client, metadataURL string) *KeySet {
	return &KeySet{hc: hc, url: metadataURL, openID: true}
}

// Key returns the key with the given id.
func (ks *KeySet) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	now := time.Now()
	k, known := ks.keys[kid]
	stale := now.Sub(ks.fetched) > refresh
	wait := retry
	if len(ks.keys) == 0 {
		wait = retryEmpty
	}
	if (stale || !known) && now.Sub(ks.tried) >= wait {
		ks.tried = now
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		keys, err := ks.fetch(ctx)
		cancel()
		switch {
		case err == nil:
			ks.keys, ks.fetched = keys, now
		case len(ks.keys) == 0:
			return nil, fmt.Errorf("fetching the signing keys: %w", err)
		}
		// On error with keys in hand, keep using them: a short outage of
		// the key endpoint should not stop every webhook.
		k, known = ks.keys[kid]
	}
	if !known {
		return nil, fmt.Errorf("no signing key %q", web.Short(kid, 60))
	}
	return k, nil
}

func (ks *KeySet) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	url := ks.url
	if ks.openID {
		var meta struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := getJSON(ctx, ks.hc, url, &meta); err != nil {
			return nil, err
		}
		if meta.JWKSURI == "" {
			return nil, errors.New("the OpenID configuration has no jwks_uri")
		}
		url = meta.JWKSURI
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := getJSON(ctx, ks.hc, url, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, raw := range set.Keys {
		// Parse one by one, so a key we cannot use does not hide the rest.
		var k jose.JSONWebKey
		if k.UnmarshalJSON(raw) != nil || k.KeyID == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		if pub, ok := k.Key.(*rsa.PublicKey); ok {
			keys[k.KeyID] = pub
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("the key set has no usable RSA keys")
	}
	return keys, nil
}

func getJSON(ctx context.Context, hc *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	b, err := web.Do(hc, req, url, 1<<20, func(status int, _ []byte) error {
		return fmt.Errorf("%s answered %d %s", url, status, http.StatusText(status))
	})
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Expected is what a token must say.
type Expected struct {
	// Issuers lists the accepted "iss" values.
	Issuers []string
	// Audience must be in "aud". It is required, since a token for another
	// app would otherwise pass.
	Audience string
	// Leeway allows for clocks that are a little apart.
	Leeway time.Duration
}

// Verify checks token's signature, issuer, audience and lifetime, and
// decodes its claims into extra when it is not nil.
func Verify(ctx context.Context, ks *KeySet, token string, want Expected, extra any) error {
	if want.Audience == "" || len(want.Issuers) == 0 {
		return errors.New("no audience or issuer to check the token against")
	}
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return fmt.Errorf("reading the token: %w", err)
	}
	if len(tok.Headers) != 1 || tok.Headers[0].KeyID == "" {
		return errors.New("the token names no signing key")
	}
	key, err := ks.Key(ctx, tok.Headers[0].KeyID)
	if err != nil {
		return err
	}
	var std jwt.Claims
	dest := []any{&std}
	if extra != nil {
		dest = append(dest, extra)
	}
	if err := tok.Claims(key, dest...); err != nil {
		return fmt.Errorf("checking the token: %w", err)
	}
	if std.Expiry == nil {
		return errors.New("the token does not expire")
	}
	if !slices.Contains(want.Issuers, std.Issuer) {
		return fmt.Errorf("the token is from %q", web.Short(std.Issuer, 80))
	}
	return std.ValidateWithLeeway(jwt.Expected{AnyAudience: jwt.Audience{want.Audience}}, want.Leeway)
}

// Bearer returns the token in an Authorization header, or "".
func Bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
