package google

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/jwttest"
	"github.com/bespinian/keera-agents/internal/channels/internal/oauth"
)

func TestAuth(t *testing.T) {
	key := jwttest.NewKey(t, "k")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		c := jwttest.Claims(t, r.Form.Get("assertion"), &key.Private.PublicKey)
		if c["sub"] == "nobody@x.ch" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens using this method."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"g","expires_in":3600}`))
	}))
	defer srv.Close()
	a := NewAuth(channels.Options{HTTP: srv.Client(), Endpoints: map[string]string{"token": srv.URL}})
	sa := key.ServiceAccountJSON("sa@p.iam.gserviceaccount.com")
	for range 2 {
		if tok, err := a.token(context.Background(), sa, "s", ""); err != nil || tok != "g" {
			t.Fatalf("Token = %q, %v", tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("token fetched %d times", calls.Load())
	}
	a.forget(sa, "s", "")
	_, _ = a.token(context.Background(), sa, "s", "")
	if calls.Load() != 2 {
		t.Error("forgotten token reused")
	}
	if err := a.Check(context.Background(), Creds{Key: sa}, "s"); err != nil || calls.Load() != 3 {
		t.Errorf("Check = %v after %d calls", err, calls.Load())
	}
	err := a.Check(context.Background(), Creds{Key: sa, Subject: "nobody@x.ch"}, "s t")
	if err == nil || !strings.Contains(err.Error(), "Google refused the credentials (unauthorized_client") ||
		!strings.Contains(err.Error(), "domain-wide delegation for s,t") {
		t.Errorf("err = %v", err)
	}
	if _, err := a.token(context.Background(), "{}", "s", ""); err == nil {
		t.Error("empty key accepted")
	}
}

func TestError(t *testing.T) {
	err := apiError(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`))
	if err.Error() != "Google answered 403 Forbidden PERMISSION_DENIED: The caller does not have permission" {
		t.Errorf("err = %v", err)
	}
	err = apiError(http.StatusForbidden, []byte(`{"error":{"errors":[{"reason":"insufficientFilePermissions"}],"message":"no"}}`))
	if err.Error() != "Google answered 403 Forbidden insufficientFilePermissions: no" {
		t.Errorf("err = %v", err)
	}
}

func TestCredsOf(t *testing.T) {
	key := map[string]string{"service_account_json": "k"}
	// Without the person's token, a call must not fall back to the key.
	if _, err := CredsOf(channels.Connection{PerUser: true, Secrets: key}); !errors.Is(err, oauth.ErrNoToken) {
		t.Errorf("no token: %v", err)
	}
	if c, err := CredsOf(channels.Connection{PerUser: true, Token: "t", Secrets: key}); err != nil || c != (Creds{Token: "t"}) {
		t.Errorf("person = %+v, %v", c, err)
	}
	c, err := CredsOf(channels.Connection{Secrets: key, Config: map[string]string{"subject": "ada@x.ch"}})
	if err != nil || c != (Creds{Key: "k", Subject: "ada@x.ch"}) {
		t.Errorf("key = %+v, %v", c, err)
	}
	if _, err := CredsOf(channels.Connection{}); err == nil {
		t.Error("no key passed")
	}
}

func TestDoJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/empty" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"name":"n"}`))
	}))
	defer srv.Close()
	a := NewAuth(channels.Options{HTTP: srv.Client()})
	var v struct{ Name string }
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/empty", nil)
	if err := a.DoJSON(req, Creds{Token: "t"}, "s", 1<<10, &v); err != nil {
		t.Errorf("no content: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	if err := a.DoJSON(req, Creds{Token: "t"}, "s", 1<<10, &v); err != nil || v.Name != "n" {
		t.Errorf("DoJSON = %+v, %v", v, err)
	}
}
