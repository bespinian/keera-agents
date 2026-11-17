package jwtverify

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors/internal/jwttest"
)

func TestVerify(t *testing.T) {
	key := jwttest.NewKey(t, "k1")
	other := jwttest.NewKey(t, "k1") // same id, different key: a forgery
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jwks_uri":"http://` + r.Host + `/keys"}`))
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// A key we cannot parse must not hide the good one.
		_, _ = w.Write([]byte(`{"keys":[{"kty":"bogus","kid":"x"},` + string(key.JWKS()[len(`{"keys":[`):])))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ks := NewOpenID(srv.Client(), srv.URL+"/meta")

	now := time.Now()
	good := map[string]any{"iss": "iss", "aud": "app", "exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix(), "serviceurl": "https://x"}
	want := Expected{Issuers: []string{"iss"}, Audience: "app", Leeway: time.Minute}

	var extra struct {
		ServiceURL string `json:"serviceurl"`
	}
	if err := Verify(context.Background(), ks, key.Sign(t, good), want, &extra); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if extra.ServiceURL != "https://x" {
		t.Errorf("extra = %+v", extra)
	}

	with := func(k, v any) map[string]any {
		c := map[string]any{}
		maps.Copy(c, good)
		if v == nil {
			delete(c, k.(string))
		} else {
			c[k.(string)] = v
		}
		return c
	}
	bad := map[string]string{
		"other issuer":   key.Sign(t, with("iss", "evil")),
		"other audience": key.Sign(t, with("aud", "someone-else")),
		"expired":        key.Sign(t, with("exp", now.Add(-5*time.Minute).Unix())),
		"not yet valid":  key.Sign(t, with("nbf", now.Add(5*time.Minute).Unix())),
		"no expiry":      key.Sign(t, with("exp", nil)),
		"forged":         other.Sign(t, good),
		"garbage":        "a.b.c",
	}
	for name, tok := range bad {
		if err := Verify(context.Background(), ks, tok, want, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("fetched keys %d times, want 1", hits.Load())
	}

	// An unknown key id does not refetch within the retry window.
	unknown := jwttest.NewKey(t, "k2")
	for range 3 {
		if err := Verify(context.Background(), ks, unknown.Sign(t, good), want, nil); err == nil {
			t.Error("unknown key accepted")
		}
	}
	if hits.Load() != 1 {
		t.Errorf("fetched keys %d times after unknown ids, want 1", hits.Load())
	}

	if err := Verify(context.Background(), ks, key.Sign(t, good), Expected{Issuers: []string{"iss"}}, nil); err == nil {
		t.Error("empty audience accepted")
	}
}

func TestKeySetOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	ks := NewJWKS(srv.Client(), srv.URL)
	if _, err := ks.Key(context.Background(), "k"); err == nil {
		t.Fatal("no error while the key endpoint is down")
	}
}

func TestBearer(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "bearer abc ")
	if Bearer(r) != "abc" {
		t.Errorf("Bearer = %q", Bearer(r))
	}
	r.Header.Set("Authorization", "Basic abc")
	if Bearer(r) != "" {
		t.Errorf("Bearer = %q", Bearer(r))
	}
}
