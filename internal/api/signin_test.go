package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/bespinian/keera-agents/internal/secret"
)

// A silent sign-in only asks the gateway, once per browser session, and a no
// ends on the sign-in screen without an error.
func TestASilentSignInAsksTheGatewayOnce(t *testing.T) {
	box, err := secret.New("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, nil, nil, nil, box, Options{
		PublicURL: "https://agents.example.ch", GatewayURL: "https://gateway.example.ch",
	})

	rec := httptest.NewRecorder()
	s.login(rec, httptest.NewRequest(http.MethodGet, "/auth/login?silent=1&provider=example", nil))
	to, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if q := to.Query(); q.Get("silent") != "1" || q.Has("provider") {
		t.Errorf("went to the gateway with %v, want silent=1 and no provider", q)
	}
	cookies := rec.Result().Cookies()
	if !has(cookies, askedCookie) {
		t.Errorf("no %s cookie, so the gateway would be asked again", askedCookie)
	}

	// The gateway says this browser is not signed in there.
	back := httptest.NewRequest(http.MethodGet, callbackPath+"?"+url.Values{
		"error": {"login_required"}, "state": {to.Query().Get("cli_state")},
	}.Encode(), nil)
	for _, c := range cookies {
		back.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	s.callback(rec, back)
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("back from the gateway, went to %q, want /", got)
	}
	if has(rec.Result().Cookies(), errorCookie) {
		t.Error("a silent no was shown as a sign-in error")
	}

	// A login_required for a sign-in this browser did not start is an error.
	rec = httptest.NewRecorder()
	s.callback(rec, httptest.NewRequest(http.MethodGet, callbackPath+"?error=login_required&state=forged", nil))
	if !has(rec.Result().Cookies(), errorCookie) {
		t.Error("a forged login_required ended without an error")
	}

	// Signing out here keeps the screen from signing the person straight
	// back in.
	rec = httptest.NewRecorder()
	s.logout(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if !has(rec.Result().Cookies(), askedCookie) {
		t.Errorf("signing out left no %s cookie", askedCookie)
	}
}

func has(cookies []*http.Cookie, name string) bool {
	for _, c := range cookies {
		if c.Name == name && c.MaxAge >= 0 {
			return true
		}
	}
	return false
}
