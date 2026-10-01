package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
