package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels/internal/jwttest"
)

func TestClientCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "s" {
			t.Errorf("form = %v", r.Form)
		}
		if r.Form.Get("client_secret") != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.\r\nTrace ID: x"}`))
			return
		}
		// Older Entra endpoints send expires_in as a string.
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":"3599"}`))
	}))
	defer srv.Close()

	tok, err := ClientCredentials(context.Background(), srv.Client(), srv.URL, "id", "pw", "s")
	if err != nil || tok.Access != "tok" || time.Until(tok.Expiry) < 59*time.Minute {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	_, err = ClientCredentials(context.Background(), srv.Client(), srv.URL, "id", "wrong", "s")
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "invalid_client" || oe.Status != 401 {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), "Trace") {
		t.Errorf("error leaks or is long: %v", err)
	}
	if msg := Describe("Microsoft", err).Error(); !strings.Contains(msg, "Microsoft refused the credentials (invalid_client: AADSTS7000215") {
		t.Errorf("Describe = %q", msg)
	}
}

func TestServiceAccountAssertion(t *testing.T) {
	key := jwttest.NewKey(t, "kid1")
	sa, err := ParseServiceAccount(key.ServiceAccountJSON("bot@p.iam.gserviceaccount.com"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		c := jwttest.Claims(t, r.Form.Get("assertion"), &key.Private.PublicKey)
		if c["iss"] != "bot@p.iam.gserviceaccount.com" || c["scope"] != "scope" || c["sub"] != "user@x.ch" || c["aud"] != "http://"+r.Host {
			t.Errorf("claims = %v", c)
		}
		_, _ = w.Write([]byte(`{"access_token":"g","expires_in":3600}`))
	}))
	defer srv.Close()
	a, err := sa.Assertion("http://"+strings.TrimPrefix(srv.URL, "http://"), "scope", "user@x.ch", now)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := JWTBearer(context.Background(), srv.Client(), srv.URL, a)
	if err != nil || tok.Access != "g" {
		t.Fatalf("token = %+v, %v", tok, err)
	}
}

func TestParseServiceAccountErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"not json -----BEGIN PRIVATE KEY-----",
		`{"type":"authorized_user"}`,
		`{"type":"service_account","client_email":"a@b"}`,
		`{"client_email":"a@b","private_key":"nope"}`,
	} {
		_, err := ParseServiceAccount(in)
		if err == nil {
			t.Errorf("%q: no error", in)
			continue
		}
		if strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), "nope") {
			t.Errorf("error quotes the key: %v", err)
		}
	}
}

func TestCache(t *testing.T) {
	var c Cache
	var calls atomic.Int32
	fetch := func(context.Context) (Token, error) {
		n := calls.Add(1)
		exp := time.Now().Add(time.Hour)
		if n == 2 {
			exp = time.Now().Add(time.Minute) // inside the early margin
		}
		return Token{Access: "t", Expiry: exp}, nil
	}
	for range 3 {
		if _, err := c.Get(context.Background(), "k", fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetched %d times, want 1", calls.Load())
	}
	c.Forget("k")
	_, _ = c.Get(context.Background(), "k", fetch) // nearly expired
	_, _ = c.Get(context.Background(), "k", fetch)
	if calls.Load() != 3 {
		t.Fatalf("fetched %d times, want 3", calls.Load())
	}
	_, err := c.Get(context.Background(), "e", func(context.Context) (Token, error) { return Token{}, errors.New("boom") })
	if err == nil {
		t.Fatal("error not returned")
	}
}
