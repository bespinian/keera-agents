// Package oauth gets and caches the access tokens adapters call services
// with: client credentials for Microsoft and Salesforce, and a signed
// service account assertion for Google. App signs people in themselves,
// with an authorization code.
package oauth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

// Token is an access token and when it stops working. Refresh, IDToken and
// Instance are set only when the service sent them.
type Token struct {
	Access  string
	Expiry  time.Time
	Refresh string
	IDToken string
	// Instance is the server to call, as Salesforce names it.
	Instance string
}

// Error is a token endpoint's refusal. It holds the service's error code and
// description, which never contain the credentials sent.
type Error struct {
	Status      int
	Code        string
	Description string
}

func (e *Error) Error() string {
	s := e.Code
	if s == "" {
		s = fmt.Sprintf("HTTP %d", e.Status)
	}
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// ClientCredentials gets an app-only token, as Microsoft Entra issues them.
// An empty scope is left out, as Salesforce takes none.
func ClientCredentials(ctx context.Context, hc *http.Client, tokenURL, clientID, secret, scope string) (Token, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	if scope != "" {
		form.Set("scope", scope)
	}
	return post(ctx, hc, tokenURL, form)
}

// JWTBearer exchanges a signed assertion for a token, as Google does for
// service accounts.
func JWTBearer(ctx context.Context, hc *http.Client, tokenURL, assertion string) (Token, error) {
	return post(ctx, hc, tokenURL, url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	})
}

func post(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, err
	}
	body, err := web.ReadBody(resp, 1<<20)
	if err != nil {
		return Token{}, err
	}
	var r struct {
		AccessToken      string      `json:"access_token"`
		ExpiresIn        json.Number `json:"expires_in"`
		RefreshToken     string      `json:"refresh_token"`
		IDToken          string      `json:"id_token"`
		InstanceURL      string      `json:"instance_url"`
		Error            string      `json:"error"`
		ErrorDescription string      `json:"error_description"`
	}
	_ = json.Unmarshal(body, &r)
	if resp.StatusCode != http.StatusOK || r.AccessToken == "" {
		return Token{}, &Error{Status: resp.StatusCode, Code: r.Error, Description: web.Short(r.ErrorDescription, 300)}
	}
	secs, err := strconv.ParseInt(r.ExpiresIn.String(), 10, 64)
	if err != nil || secs <= 0 {
		secs = 3600
	}
	return Token{
		Access: r.AccessToken, Expiry: time.Now().Add(time.Duration(secs) * time.Second),
		Refresh: r.RefreshToken, IDToken: r.IDToken, Instance: r.InstanceURL,
	}, nil
}

// App is an OAuth client people sign in through themselves, with an
// authorization code and PKCE.
type App struct {
	AuthURL, TokenURL string
	ClientID, Secret  string
	Scopes            []string
	// Params go on the sign-in address too, such as Google's
	// access_type=offline.
	Params url.Values
}

// SignInURL is where the person's browser goes to sign in.
func (a App) SignInURL(redirect, state, challenge string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.ClientID},
		"redirect_uri":          {redirect},
		"scope":                 {strings.Join(a.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	maps.Copy(q, a.Params)
	sep := "?"
	if strings.Contains(a.AuthURL, "?") {
		sep = "&"
	}
	return a.AuthURL + sep + q.Encode()
}

// Exchange redeems the code the person came back with.
func (a App) Exchange(ctx context.Context, hc *http.Client, redirect, code, verifier string) (Token, error) {
	return post(ctx, hc, a.TokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {a.ClientID},
		"client_secret": {a.Secret},
		"code_verifier": {verifier},
	})
}

// Refresh gets a new access token with a refresh token.
func (a App) Refresh(ctx context.Context, hc *http.Client, refresh string) (Token, error) {
	return post(ctx, hc, a.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {a.ClientID},
		"client_secret": {a.Secret},
	})
}

// Check proves the service knows the client and its secret, before anyone
// has signed in, by refreshing a made-up token. A known client is told the
// token is wrong: invalid_grant, or GitHub's bad_refresh_token. An unknown
// one is refused itself.
func (a App) Check(ctx context.Context, hc *http.Client) error {
	_, err := a.Refresh(ctx, hc, "keera-agents-check")
	if oe, ok := errors.AsType[*Error](err); ok && (oe.Code == "invalid_grant" || oe.Code == "bad_refresh_token") {
		return nil
	}
	if err == nil {
		return errors.New("the service accepted a made-up refresh token")
	}
	return err
}

// idClaims reads an ID token's claims. The token came straight from the
// service's token endpoint over TLS, so it is not verified again, as OpenID
// Connect allows.
func idClaims(idToken string, out any) bool {
	if idToken == "" {
		return false
	}
	tok, err := jwt.ParseSigned(idToken, []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.PS256,
	})
	return err == nil && tok.UnsafeClaimsWithoutVerification(out) == nil
}

// expiry is when an ID token stops working, or zero.
func expiry(idToken string) time.Time {
	var c struct {
		Exp int64 `json:"exp"`
	}
	if !idClaims(idToken, &c) || c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

// Email reads the email in an ID token, which says who signed in, or else
// the user name, as Microsoft gives when the account has no email.
func Email(idToken string) string {
	var c struct {
		Email    string `json:"email"`
		Username string `json:"preferred_username"`
	}
	if !idClaims(idToken, &c) {
		return ""
	}
	if c.Email == "" {
		c.Email = c.Username
	}
	return web.Short(c.Email, 200)
}

// Cache keeps tokens until shortly before they expire. Each key is fetched
// by one caller at a time, so a burst of messages costs one token request.
type Cache struct {
	mu sync.Mutex
	m  map[string]*slot
}

type slot struct {
	mu  sync.Mutex
	tok Token
}

// early is how long before expiry a token is replaced, so it does not run
// out in the middle of a call.
const early = 2 * time.Minute

// Get returns the cached token for key, or fetches a new one. key should
// change when the credentials do, such as by including web.Fingerprint.
func (c *Cache) Get(ctx context.Context, key string, fetch func(context.Context) (Token, error)) (Token, error) {
	c.mu.Lock()
	if c.m == nil || len(c.m) > 1000 {
		// Many keys means many connections came and went; starting over only
		// costs each live one a token request.
		c.m = map[string]*slot{}
	}
	s := c.m[key]
	if s == nil {
		s = &slot{}
		c.m[key] = s
	}
	c.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok.Access != "" && time.Now().Before(s.tok.Expiry.Add(-early)) {
		return s.tok, nil
	}
	tok, err := fetch(ctx)
	if err != nil {
		return Token{}, err
	}
	s.tok = tok
	return tok, nil
}

// Forget drops a token the service refused, so the next call gets a new one.
func (c *Cache) Forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// Bearer is what a call signs in with: a person's own access Token, or else
// the app's token, which Fetch gets and the cache keeps under Key.
type Bearer struct {
	Token string
	Key   string
	Fetch func(context.Context) (Token, error)
}

// Do sends req with b's token and returns the body of a 2xx answer, of at
// most limit bytes. A refused app token is forgotten, so the next call gets
// a new one. who names whose sign-in it is, such as "Microsoft", service who
// answers, and fail reads the service's error.
func (c *Cache) Do(hc *http.Client, req *http.Request, b Bearer, who, service string, limit int64, fail func(status int, body []byte) error) ([]byte, error) {
	token := b.Token
	if token == "" {
		tok, err := c.Get(req.Context(), b.Key, b.Fetch)
		if err != nil {
			return nil, Describe(who, err)
		}
		token = tok.Access
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return web.Do(hc, req, service, limit, func(status int, body []byte) error {
		if status == http.StatusUnauthorized && b.Token == "" {
			c.Forget(b.Key)
		}
		return fail(status, body)
	})
}

// ServiceAccount is a Google service account key file.
type ServiceAccount struct {
	Email string
	KeyID string
	key   *rsa.PrivateKey
}

// ParseServiceAccount reads a key file as Google Cloud downloads it. Its
// errors never quote the file, which holds the private key.
func ParseServiceAccount(s string) (*ServiceAccount, error) {
	var raw struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
	}
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("the service account key is missing")
	}
	if json.Unmarshal([]byte(s), &raw) != nil {
		return nil, errors.New("the service account key is not valid JSON; paste the whole key file")
	}
	if raw.Type != "" && raw.Type != "service_account" {
		return nil, fmt.Errorf("the key file is not a service account key (its type is %q)", web.Short(raw.Type, 40))
	}
	if raw.ClientEmail == "" || raw.PrivateKey == "" {
		return nil, errors.New("the key file has no client_email or private_key; paste the whole JSON key file")
	}
	block, _ := pem.Decode([]byte(raw.PrivateKey))
	if block == nil {
		return nil, errors.New("the key file's private_key is not a PEM key")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("the key file's private_key is not an RSA key")
		}
		key = rk
	} else if rk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = rk
	} else {
		return nil, errors.New("the key file's private_key cannot be read")
	}
	return &ServiceAccount{Email: raw.ClientEmail, KeyID: raw.PrivateKeyID, key: key}, nil
}

// Assertion signs the JWT that JWTBearer exchanges for a token. subject is
// the user to act as through domain-wide delegation, or empty for the
// service account itself.
func (sa *ServiceAccount) Assertion(aud, scope, subject string, now time.Time) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: sa.key, KeyID: sa.KeyID}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", err
	}
	claims := map[string]any{
		"iss":   sa.Email,
		"scope": scope,
		"aud":   aud,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	if subject != "" {
		claims["sub"] = subject
	}
	return jwt.Signed(signer).Claims(claims).Serialize()
}

// Describe turns a token error into words an administrator can act on. who
// names the service, such as "Microsoft".
func Describe(who string, err error) error {
	if oe, ok := errors.AsType[*Error](err); ok {
		return fmt.Errorf("%s refused the credentials (%w)", who, oe)
	}
	return fmt.Errorf("could not reach %s to sign in: %w", who, err)
}
