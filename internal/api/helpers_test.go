package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	box, err := secret.New("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{box: box, log: slog.New(slog.DiscardHandler)}
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer tok":    "tok",
		"bearer  tok ":  "tok",
		"Basic dXNlcjo": "",
		"Bearer ":       "",
		"tok":           "",
		"":              "",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", header)
		got, ok := bearerToken(r)
		if ok != (want != "") || (ok && got != want) {
			t.Errorf("%q: %q, %v; want %q", header, got, ok, want)
		}
	}
}

func TestTrimDoesNotSplitACharacter(t *testing.T) {
	if got := trim("  héllo  ", 2); got != "h" {
		t.Errorf("trim = %q, want %q", got, "h")
	}
	if got := trim(" ok ", 10); got != "ok" {
		t.Errorf("trim = %q", got)
	}
}

func TestReadJSONRefusesUnknownFieldsAndLargeBodies(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"unknown field", `{"name":"x","admin":true}`, http.StatusBadRequest},
		{"not JSON", `name=x`, http.StatusBadRequest},
		{"too large", `{"name":"` + strings.Repeat("x", 1<<20) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		w := httptest.NewRecorder()
		var in struct {
			Name string `json:"name"`
		}
		if readJSON(w, httptest.NewRequest("POST", "/", strings.NewReader(tc.body)), &in) || w.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}

func TestErrorsSayTheirType(t *testing.T) {
	for status, typ := range map[int]string{
		http.StatusBadRequest:          "invalid_request_error",
		http.StatusUnauthorized:        "authentication_error",
		http.StatusForbidden:           "permission_error",
		http.StatusNotFound:            "not_found_error",
		http.StatusConflict:            "invalid_request_error",
		http.StatusInternalServerError: "api_error",
		http.StatusBadGateway:          "api_error",
	} {
		w := httptest.NewRecorder()
		writeError(w, status, "c", "m")
		if w.Code != status || !strings.Contains(w.Body.String(), `"type":"`+typ+`"`) {
			t.Errorf("%d: %d %s", status, w.Code, w.Body)
		}
	}
}

// Another organisation's row answers as a missing one does, so ids say
// nothing.
func TestFoundHidesOtherOrganisations(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		name    string
		err     error
		sameOrg bool
		ok      bool
		want    int
	}{
		{"own row", nil, true, true, 200},
		{"another organisation's", nil, false, false, http.StatusNotFound},
		{"missing", store.ErrNotFound, false, false, http.StatusNotFound},
		{"missing, wrapped", fmt.Errorf("load: %w", store.ErrNotFound), true, false, http.StatusNotFound},
		{"the database failed", errors.New("connection reset"), false, false, http.StatusInternalServerError},
	} {
		w := httptest.NewRecorder()
		if ok := s.found(w, tc.err, tc.sameOrg, "agent"); ok != tc.ok || w.Code != tc.want {
			t.Errorf("%s: ok %v, %d; want %v, %d", tc.name, ok, w.Code, tc.ok, tc.want)
		}
		if w.Code == http.StatusInternalServerError && strings.Contains(w.Body.String(), "connection reset") {
			t.Errorf("%s: the answer shows our error: %s", tc.name, w.Body)
		}
	}
}

func TestSavedAnswersAConflict(t *testing.T) {
	s := testServer(t)
	w := httptest.NewRecorder()
	if s.saved(w, fmt.Errorf("insert: %w", store.ErrConflict), "name_taken", "taken") ||
		w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "name_taken") {
		t.Errorf("conflict: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	if !s.saved(w, nil, "", "") || w.Body.Len() != 0 {
		t.Errorf("no error: %d %s", w.Code, w.Body)
	}
}

func TestGatewayFailedPassesOnTheGatewaysWords(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		name     string
		err      error
		want     int
		wantCode string
	}{
		{"a refusal", &gateway.Error{Status: 403, Code: "forbidden", Message: "no"}, 403, "forbidden"},
		{"no code", &gateway.Error{Status: 404, Message: "gone"}, 404, "gateway_error"},
		// Our service key failing is not the caller's sign-in ending.
		{"unauthorised", &gateway.Error{Status: 401, Code: "unauthenticated"}, 502, "unauthenticated"},
		{"no answer", errors.New("dial tcp: refused"), 500, "internal"},
	} {
		w := httptest.NewRecorder()
		s.gatewayFailed(w, tc.err)
		if w.Code != tc.want || !strings.Contains(w.Body.String(), `"code":"`+tc.wantCode+`"`) {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
	}
}

func TestStrongestTier(t *testing.T) {
	for _, tc := range []struct {
		tiers []runtime.Isolation
		want  runtime.Isolation
	}{
		{[]runtime.Isolation{runtime.Standard, runtime.Isolated, runtime.VM}, runtime.VM},
		{[]runtime.Isolation{runtime.Standard, runtime.Isolated}, runtime.Isolated},
		{[]runtime.Isolation{runtime.Standard}, runtime.Standard},
		{nil, runtime.DefaultClass.Isolation},
	} {
		if got := strongestTier(tc.tiers); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.tiers, got, tc.want)
		}
	}
}

func TestPendingSignInChecksStateAndAge(t *testing.T) {
	p := newPending()
	good := url.Values{"state": {p.State}, "code": {"c"}}
	if msg, started := p.check(true, good); msg != "" || !started {
		t.Fatalf("a good return: %q, %v", msg, started)
	}
	for _, tc := range []struct {
		name    string
		ok      bool
		p       pending
		q       url.Values
		started bool
	}{
		{"no cookie", false, p, good, false},
		{"expired", true, pending{State: p.State, Expires: time.Now().Add(-time.Second).Unix()}, good, false},
		{"another state", true, p, url.Values{"state": {"forged"}, "code": {"c"}}, false},
		{"no code", true, p, url.Values{"state": {p.State}}, true},
	} {
		if msg, started := tc.p.check(tc.ok, tc.q); msg == "" || started != tc.started {
			t.Errorf("%s: %q, started %v", tc.name, msg, started)
		}
	}
}

// A sealed cookie names itself, so one cookie's value cannot pass as
// another's, such as a connector sign-in's as a session.
func TestSealedCookiesCannotBeSwapped(t *testing.T) {
	s := testServer(t)
	w := httptest.NewRecorder()
	if err := s.setSealed(w, signInCookie, "/", session{Token: "tok", Expires: time.Now().Add(time.Hour).Unix()}, time.Hour); err != nil {
		t.Fatal(err)
	}
	sealed := w.Result().Cookies()[0]
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sealed.Value})
	if _, ok := s.readSession(r); ok {
		t.Fatal("a connector's sign-in cookie passed as a session")
	}

	w = httptest.NewRecorder()
	_ = s.setSealed(w, sessionCookie, "/", session{Token: "tok", Expires: time.Now().Add(-time.Second).Unix()}, time.Hour)
	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(w.Result().Cookies()[0])
	if _, ok := s.readSession(r); ok {
		t.Fatal("an expired session was read")
	}
	if c := w.Result().Cookies()[0]; !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("the cookie is readable by scripts or sent cross-site: %+v", c)
	}
}

func TestCSRFTokenIsPerSignIn(t *testing.T) {
	if csrfFor("a") == csrfFor("b") || len(csrfFor("a")) != 32 {
		t.Error("the CSRF token is not a digest of the sign-in")
	}
}
