package microsoft

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

func TestAuth(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			if r.Header.Get("Authorization") != "Bearer m" {
				w.WriteHeader(http.StatusUnauthorized)
			}
			return
		}
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
	a := NewAuth(connectors.Options{HTTP: srv.Client(), Endpoints: map[string]string{"login": srv.URL + "/login"}})
	app := App{Tenant: "tenant-1", ClientID: "c", Secret: "pw", Scope: "s"}
	call := func(app App) error {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api", nil)
		_, err := a.Do(req, app, "Microsoft Graph", 1<<10)
		return err
	}
	for range 2 {
		if err := call(app); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("token fetched %d times", calls.Load())
	}
	// A person's refused token leaves the app's be.
	if err := call(App{Token: "theirs"}); web.Status(err) != http.StatusUnauthorized {
		t.Errorf("err = %v", err)
	}
	_ = call(app)
	if calls.Load() != 1 {
		t.Errorf("token fetched %d times, want 1", calls.Load())
	}
	app.Secret = "wrong"
	err := a.Check(context.Background(), app)
	if err == nil || !strings.Contains(err.Error(), "Microsoft refused the credentials (invalid_client") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("err = %v", err)
	}
}

func TestError(t *testing.T) {
	err := apiError("Microsoft Graph", 404, []byte(`{"error":{"code":"itemNotFound","message":"The resource could not be found."}}`))
	if err.Error() != "Microsoft Graph answered 404 Not Found itemNotFound: The resource could not be found." {
		t.Errorf("err = %v", err)
	}
}

func TestDoJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	a := NewAuth(connectors.Options{HTTP: srv.Client()})
	var v struct{}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err := a.DoJSON(req, App{Token: "t"}, "Azure", 1<<10, &v); err != nil {
		t.Errorf("no body: %v", err)
	}
}

func TestQuery(t *testing.T) {
	if got := Query(map[string]string{"$top": "5", "$filter": "a eq 'b'"}); got != "?$filter=a%20eq%20%27b%27&$top=5" {
		t.Errorf("Query = %q", got)
	}
	if Query(nil) != "" {
		t.Error("no options gave a query")
	}
}
