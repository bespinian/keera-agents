package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// Only the gateway's own "not found" means a person is gone. A proxy's 404,
// or one from a gateway without the route, must not stop their agents.
func TestIsNotFound(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   bool
	}{
		"gateway":     {404, `{"error":{"type":"invalid_request_error","code":"not_found","message":"that person is not in this organisation"}}`, true},
		"proxy":       {404, `<html>Not Found</html>`, false},
		"other code":  {404, `{"error":{"code":"model_not_found","message":"no"}}`, false},
		"other error": {500, `{"error":{"code":"not_found","message":"no"}}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(srv.URL, "key").UserByID(context.Background(), "org_1", "usr_1")
		srv.Close()
		if got := IsNotFound(err); got != tc.want {
			t.Errorf("%s: IsNotFound(%v) = %v, want %v", name, err, got, tc.want)
		}
	}
}

func TestObserveHearsOfEveryCall(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	var heard []string
	c := New(srv.URL, "key")
	c.Observe(func(op, outcome string, _ time.Duration) { heard = append(heard, op+" "+outcome) })
	ctx := context.Background()

	_, _ = c.UserByID(ctx, "org_1", "usr_1")
	status = http.StatusUnauthorized
	_ = c.RevokeKey(ctx, "org_1", "key_1")
	status = http.StatusBadGateway
	_, _ = c.AuthConfig(ctx)
	srv.Close()
	_, _ = c.AuthConfig(ctx)
	// A call its caller gave up on says nothing about the gateway.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = c.AuthConfig(cancelled)

	want := []string{
		"GET /v1/service/users ok",
		"DELETE /v1/service/keys/{id} 4xx",
		"GET /auth/config 5xx",
		"GET /auth/config unreachable",
	}
	if !slices.Equal(heard, want) {
		t.Fatalf("heard %q, want %q", heard, want)
	}
}

// Members calls with the person's own token, and leaves out disabled people:
// an agent cannot be created for them.
func TestMembersUsesTheCallersTokenAndSkipsDisabled(t *testing.T) {
	var auth, org string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, org = r.Header.Get("Authorization"), r.URL.Query().Get("org_id")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"usr_1","email":"anna@example.ch","name":"Anna","role":"admin"},
			{"id":"usr_2","email":"marc@example.ch","disabled_at":"2026-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, "service").Members(context.Background(), "tok", "org_1")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || org != "org_1" {
		t.Errorf("sent %q for %q", auth, org)
	}
	if want := []User{{ID: "usr_1", Email: "anna@example.ch", Name: "Anna"}}; !slices.Equal(got, want) {
		t.Errorf("members = %+v, want %+v", got, want)
	}
}

// A gateway too old to list capabilities sends none; its models are taken to
// call tools, so updating keera-agents first does not grey out every one.
func TestCallsTools(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"tools":     {`{"alias":"a","capabilities":["tools","vision"]}`, true},
		"no tools":  {`{"alias":"a","capabilities":["vision"]}`, false},
		"none":      {`{"alias":"a","capabilities":[]}`, false},
		"older one": {`{"alias":"a"}`, true},
	} {
		var m CatalogRef
		if err := json.Unmarshal([]byte(tc.body), &m); err != nil {
			t.Fatal(err)
		}
		if got := m.CallsTools(); got != tc.want {
			t.Errorf("%s: CallsTools() = %v, want %v", name, got, tc.want)
		}
	}
}

// fakeOperatorGateway signs the key "op-key" in, and hands a signed-in
// browser's sign-in over when handOver is set. ended says whether its
// session was signed out.
func fakeOperatorGateway(t *testing.T, handOver bool, ended *bool) *httptest.Server {
	t.Helper()
	signedIn := func(r *http.Request) bool {
		c, err := r.Cookie("keera_session")
		return err == nil && c.Value == "sess"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /control/auth/local", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Key string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Key != "op-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"that operator key is not valid"}}`))
			return
		}
		// Secure, as behind HTTPS: it must still come back over plain HTTP.
		http.SetCookie(w, &http.Cookie{Name: "keera_session", Value: "sess", Secure: true, HttpOnly: true})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /control/auth/login", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !handOver || !signedIn(r) || q.Get("cli_challenge") != "chal" {
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"error":{"code":"sso_disabled","message":"no identity provider is configured"}}`))
			return
		}
		http.Redirect(w, r, q.Get("cli_redirect")+"?code=the-code&state="+q.Get("cli_state"), http.StatusFound)
	})
	mux.HandleFunc("GET /control/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if !signedIn(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"csrf":"tok"}`))
	})
	mux.HandleFunc("POST /control/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if signedIn(r) && r.Header.Get("X-CSRF-Token") == "tok" {
			*ended = true
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOperatorSignIn(t *testing.T) {
	ctx := context.Background()
	const back = "https://agents.example.ch/auth/callback"

	var ended bool
	c := New(fakeOperatorGateway(t, true, &ended).URL, "")
	code, err := c.OperatorSignIn(ctx, "op-key", back, "chal", "st")
	if err != nil || code != "the-code" {
		t.Fatalf("OperatorSignIn = %q, %v", code, err)
	}
	if !ended {
		t.Error("the gateway's own session was left signed in")
	}

	if _, err := c.OperatorSignIn(ctx, "wrong", back, "chal", "st"); err == nil || err.Error() != "that operator key is not valid" {
		t.Errorf("a wrong key: %v", err)
	}

	ended = false
	c = New(fakeOperatorGateway(t, false, &ended).URL, "")
	_, err = c.OperatorSignIn(ctx, "op-key", back, "chal", "st")
	if e, ok := errors.AsType[*Error](err); !ok || e.Code != "no_hand_over" {
		t.Errorf("without a hand-over: %v", err)
	}
	if !ended {
		t.Error("the gateway's own session was left signed in after a failure")
	}
}
