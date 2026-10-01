package microsoft

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestAuth(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.URL.Path != "/login/tenant-1/oauth2/v2.0/token" || r.Form.Get("scope") != "s" {
			t.Errorf("path %q scope %q", r.URL.Path, r.Form.Get("scope"))
		}
		if r.Form.Get("client_secret") != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"m","expires_in":3600}`))
	}))
	defer srv.Close()
	a := NewAuth(channels.Options{HTTP: srv.Client(), Endpoints: map[string]string{"login": srv.URL + "/login"}})
	app := App{Tenant: "tenant-1", ClientID: "c", Secret: "pw", Scope: "s"}
	for range 2 {
		if tok, err := a.Token(context.Background(), app); err != nil || tok != "m" {
			t.Fatalf("Token = %q, %v", tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("token fetched %d times", calls.Load())
	}
	a.Forget(app)
	_, _ = a.Token(context.Background(), app)
	if calls.Load() != 2 {
		t.Errorf("forgotten token reused")
	}
	app.Secret = "wrong"
	err := a.Check(context.Background(), app)
	if err == nil || !strings.Contains(err.Error(), "Microsoft refused the credentials (invalid_client") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("err = %v", err)
	}
}

func TestError(t *testing.T) {
	err := Error("Microsoft Graph", 404, []byte(`{"error":{"code":"itemNotFound","message":"The resource could not be found."}}`))
	if err.Error() != "Microsoft Graph answered 404 Not Found itemNotFound: The resource could not be found." {
		t.Errorf("err = %v", err)
	}
}
