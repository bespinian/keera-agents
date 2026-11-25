// Package web holds the HTTP plumbing the adapters share.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bespinian/keera-agents/internal/connectors"
)

// timeout bounds a call when the server passes no client of its own, so a
// hung service cannot hold a task forever.
const timeout = 30 * time.Second

// Client returns the client to call services with.
func Client(opts connectors.Options) *http.Client {
	if opts.HTTP != nil {
		return opts.HTTP
	}
	return &http.Client{Timeout: timeout}
}

// SameServerClient is Client following redirects only on the server first
// asked, for services whose address a connection sets.
func SameServerClient(opts connectors.Options) *http.Client {
	c := *Client(opts)
	c.CheckRedirect = connectors.SameServer
	return &c
}

// Endpoint returns the base URL called name, or def when it is not
// overridden. It never ends in a slash, so paths can be appended.
func Endpoint(opts connectors.Options, name, def string) string {
	if v := opts.Endpoints[name]; v != "" {
		return strings.TrimRight(v, "/")
	}
	return def
}

// Value returns a connection setting. It looks in the secrets first and the
// plain settings second, so a field moved between the two keeps working.
func Value(conn connectors.Connection, name string) string {
	if v := strings.TrimSpace(conn.Secrets[name]); v != "" {
		return v
	}
	return strings.TrimSpace(conn.Config[name])
}

// BaseURL checks the base URL a connection names, such as a GitLab on the
// business's own servers, and trims its trailing slash. Empty is fine;
// otherwise it must be https://, since the token is sent there.
func BaseURL(s string) (string, bool) {
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", true
	}
	u, err := url.Parse(s)
	return s, err == nil && u.Scheme == "https" && u.Host != ""
}

// RepoPath trims a repository's path as people paste it, with slashes or
// .git around it, and says whether it matches valid. It goes into URLs, so
// ".." is never valid.
func RepoPath(path string, valid *regexp.Regexp) (string, bool) {
	path = strings.TrimSuffix(strings.Trim(strings.TrimSpace(path), "/"), ".git")
	return path, valid.MatchString(path) && !strings.Contains(path, "..")
}

// NewJSONRequest makes a request with in as its JSON body, or with no body
// when in is nil.
func NewJSONRequest(ctx context.Context, method, u string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err == nil && in != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	return req, err
}

// APIError is a call a service refused. It reads as a sentence for the
// administrator, such as "GitHub answered 404 Not Found: Not Found".
type APIError struct {
	// Service names who answered, such as "GitHub".
	Service string
	Status  int
	// Code is the service's own error code, if it gives one.
	Code    string
	Message string
}

func (e *APIError) Error() string {
	s := strings.TrimSpace(fmt.Sprintf("%s answered %d %s", e.Service, e.Status, http.StatusText(e.Status)))
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + Short(e.Message, 300)
	}
	return s
}

// Status returns the HTTP status of a call a service refused, or 0.
func Status(err error) int {
	if e, ok := errors.AsType[*APIError](err); ok {
		return e.Status
	}
	return 0
}

// ErrTooLarge is a response bigger than the caller allows.
var ErrTooLarge = errors.New("the response is too large")

// ReadBody reads and closes a response body of at most limit bytes.
func ReadBody(resp *http.Response, limit int64) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}

// Retries and the longest wait between them. A service that asks for a
// longer pause gets its refusal back instead: a result waits in the delivery
// loop, and a tool call in a running task.
const (
	attempts = 3
	maxWait  = 30 * time.Second
)

// Call sends req, and sends it again when the service answers 429 or 503.
// Both say the request was not handled, so a message is not posted twice.
// It waits as Retry-After asks, or a second and then two.
func Call(hc *http.Client, req *http.Request) (*http.Response, error) {
	for try := 1; ; try++ {
		resp, err := hc.Do(req)
		if err != nil || try == attempts ||
			resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
			return resp, err
		}
		wait, ok := retryAfter(resp.Header.Get("Retry-After"), time.Duration(try)*time.Second)
		if !ok || (req.Body != nil && req.GetBody == nil) {
			return resp, nil
		}
		_ = resp.Body.Close()
		next := req.Clone(req.Context())
		if req.GetBody != nil {
			if next.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		req = next
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
	}
}

// retryAfter reads a Retry-After header, in seconds or as a date, and says
// whether it is short enough to wait for. Without one it is def.
func retryAfter(h string, def time.Duration) (time.Duration, bool) {
	wait := def
	if s, err := strconv.Atoi(h); err == nil {
		wait = time.Duration(max(s, 0)) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		wait = max(time.Until(t), 0)
	}
	return wait, wait <= maxWait
}

// Do sends req with Call and returns the body of a 2xx answer, of at most
// limit bytes. Another status is fail's error, made from the first MiB of
// the body. service names who is called, for when it cannot be reached.
func Do(hc *http.Client, req *http.Request, service string, limit int64, fail func(status int, body []byte) error) ([]byte, error) {
	resp, err := Call(hc, req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", service, err)
	}
	if resp.StatusCode/100 != 2 {
		b, _ := ReadBody(resp, 1<<20)
		return nil, fail(resp.StatusCode, b)
	}
	return ReadBody(resp, limit)
}

// DoJSON is Do, then decodes the body into out unless out is nil.
func DoJSON(hc *http.Client, req *http.Request, service string, limit int64, fail func(status int, body []byte) error, out any) error {
	b, err := Do(hc, req, service, limit, fail)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// Decode reads an answer of service into out, unless out is nil or the body
// is empty, as a 202 Accepted's or a 204 No Content's is.
func Decode(b []byte, service string, out any) error {
	if out == nil || len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("could not read the answer of %s: %w", service, err)
	}
	return nil
}

// FileTooLarge is the error for a file bigger than limit bytes. prefix names
// the adapter, such as "gdrive".
func FileTooLarge(prefix, name string, limit int64) error {
	return fmt.Errorf("%s: %q is larger than %d MiB", prefix, name, limit>>20)
}

// Fingerprint names a set of credentials in a cache key without keeping
// the secret itself, and changes when the secret does.
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// Short trims a service's error text to its first line and n characters, so
// an administrator sees the gist and not a trace dump.
func Short(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// Cut trims s to at most n bytes, on a rune boundary, and adds end when it
// did. It bounds a value a tool returns, so one long text does not fill the
// model's context or the 64 KiB a tool may answer.
func Cut(s string, n int, end string) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + end
}

// Cache keeps small lookups, such as a user's email, for a while, so a busy
// conversation does not ask the service on every message.
type Cache[V any] struct {
	mu  sync.Mutex
	ttl time.Duration
	max int
	m   map[string]cached[V]
}

type cached[V any] struct {
	v   V
	exp time.Time
}

func NewCache[V any](ttl time.Duration, max int) *Cache[V] {
	return &Cache[V]{ttl: ttl, max: max, m: map[string]cached[V]{}}
}

func (c *Cache[V]) Get(key string, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || now.After(e.exp) {
		var zero V
		return zero, false
	}
	return e.v, true
}

func (c *Cache[V]) Put(key string, v V, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		for k, e := range c.m {
			if now.After(e.exp) {
				delete(c.m, k)
			}
		}
		// Still full: drop any entry, which only costs one lookup later.
		for k := range c.m {
			if len(c.m) < c.max {
				break
			}
			delete(c.m, k)
		}
	}
	c.m[key] = cached[V]{v: v, exp: now.Add(c.ttl)}
}
