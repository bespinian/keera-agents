package web

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
)

func TestEndpointAndValue(t *testing.T) {
	opts := connectors.Options{Endpoints: map[string]string{"api": "http://x/"}}
	if got := Endpoint(opts, "api", "https://real"); got != "http://x" {
		t.Errorf("Endpoint = %q", got)
	}
	if got := Endpoint(opts, "other", "https://real"); got != "https://real" {
		t.Errorf("Endpoint default = %q", got)
	}
	conn := connectors.Connection{
		Config:  map[string]string{"a": " plain ", "b": "config"},
		Secrets: map[string]string{"b": "secret"},
	}
	if Value(conn, "a") != "plain" || Value(conn, "b") != "secret" || Value(conn, "c") != "" {
		t.Errorf("Value: %q %q %q", Value(conn, "a"), Value(conn, "b"), Value(conn, "c"))
	}
	if Client(connectors.Options{}).Timeout != timeout {
		t.Error("default client has no timeout")
	}
}

func TestReadBody(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("hello"))}
	if b, err := ReadBody(resp, 5); err != nil || string(b) != "hello" {
		t.Errorf("ReadBody = %q, %v", b, err)
	}
	resp = &http.Response{Body: io.NopCloser(strings.NewReader("hello!"))}
	if _, err := ReadBody(resp, 5); err != ErrTooLarge {
		t.Errorf("ReadBody over limit: %v", err)
	}
}

func TestDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.Error(w, "no such thing", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	var failed int
	fail := func(status int, body []byte) error {
		failed = status
		return errors.New(strings.TrimSpace(string(body)))
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	if b, err := Do(srv.Client(), req, "Test", 10, fail); err != nil || string(b) != "ok" {
		t.Fatalf("Do = %q, %v", b, err)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/missing", nil)
	if _, err := Do(srv.Client(), req, "Test", 10, fail); failed != 404 || err == nil || err.Error() != "no such thing" {
		t.Fatalf("Do on 404 = %v, status %d", err, failed)
	}
}

func TestCallRetriesAThrottledRequest(t *testing.T) {
	var calls int
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		switch r.URL.Path {
		case "/busy":
			if calls == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, "ok")
		case "/later":
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/down":
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/busy", strings.NewReader("hello"))
	resp, err := Call(srv.Client(), req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Call = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
	if calls != 2 || bodies[1] != "hello" {
		t.Fatalf("calls = %d, bodies = %q: want the body sent again", calls, bodies)
	}

	calls = 0
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/later", nil)
	if resp, _ := Call(srv.Client(), req); resp.StatusCode != http.StatusServiceUnavailable || calls != 1 {
		t.Fatalf("an hour's wait was retried: %d calls", calls)
	}

	calls = 0
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/down", nil)
	if resp, _ := Call(srv.Client(), req); resp.StatusCode != http.StatusServiceUnavailable || calls != attempts {
		t.Fatalf("calls = %d, want %d", calls, attempts)
	}
}

func TestShortCutAndFingerprint(t *testing.T) {
	if got := Short("  first line\nsecond", 50); got != "first line" {
		t.Errorf("Short = %q", got)
	}
	if got := Short("abcdef", 3); got != "abc…" {
		t.Errorf("Short = %q", got)
	}
	if got := Cut("héllo", 2, "…"); got != "h…" {
		t.Errorf("Cut = %q", got)
	}
	if got := Cut("hi", 2, "…"); got != "hi" {
		t.Errorf("Cut = %q", got)
	}
	if Fingerprint("a", "b") == Fingerprint("ab") {
		t.Error("Fingerprint does not separate parts")
	}
	if strings.Contains(Fingerprint("secret"), "secret") {
		t.Error("Fingerprint leaks")
	}
}

func TestCache(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewCache[string](time.Minute, 2)
	c.Put("a", "1", now)
	if v, ok := c.Get("a", now.Add(30*time.Second)); !ok || v != "1" {
		t.Errorf("Get = %q, %v", v, ok)
	}
	if _, ok := c.Get("a", now.Add(2*time.Minute)); ok {
		t.Error("expired entry returned")
	}
	c.Put("b", "2", now)
	c.Put("c", "3", now)
	if len(c.m) > 2 {
		t.Errorf("cache grew to %d", len(c.m))
	}
	if v, ok := c.Get("c", now); !ok || v != "3" {
		t.Errorf("newest entry missing: %q %v", v, ok)
	}
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]bool{
		"":                     true,
		"https://git.x.ch/":    true,
		"http://git.x.ch":      false,
		"https://":             false,
		"git.x.ch":             false,
		"https://git.x.ch/sub": true,
	} {
		if got, ok := BaseURL(in); ok != want || strings.HasSuffix(got, "/") {
			t.Errorf("BaseURL(%q) = %q, %v", in, got, ok)
		}
	}
}

func TestAPIError(t *testing.T) {
	err := &APIError{Service: "Google", Status: 403, Code: "PERMISSION_DENIED", Message: "no\ntrace"}
	if err.Error() != "Google answered 403 Forbidden PERMISSION_DENIED: no" {
		t.Errorf("err = %v", err)
	}
	if err := (&APIError{Service: "GitHub", Status: 599}); err.Error() != "GitHub answered 599" {
		t.Errorf("err = %v", err)
	}
}
