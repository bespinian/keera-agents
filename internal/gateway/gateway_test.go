package gateway

import (
	"context"
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
