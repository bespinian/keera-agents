package brave

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
)

// newFake serves Brave's two endpoints, and records each request's query.
func newFake(t *testing.T) (*url.Values, *Adapter) {
	var last url.Values
	mux := http.NewServeMux()
	reply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Subscription-Token") != "key" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"type":"ErrorResponse","error":{"status":422,"code":"SUBSCRIPTION_TOKEN_INVALID","detail":"The provided subscription token is invalid."}}`))
				return
			}
			last = r.URL.Query()
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("GET /web/search", reply(`{"query":{"original":"keera agnets","altered":"keera agents","more_results_available":true},
		"web":{"results":[{"title":"Keera <strong>Agents</strong>","url":"https://example.com/a",
		"description":"Agents that work &amp; <strong>talk</strong>.","age":"2 days ago","meta_url":{"hostname":"example.com"}}]}}`))
	mux.HandleFunc("GET /news/search", reply(`{"query":{"original":"keera"},
		"results":[{"title":"News","url":"https://news.example.com/1","description":"`+strings.Repeat("x", 2000)+`",
		"age":"1 hour ago","meta_url":{"hostname":"news.example.com"}}]}`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &last, New(connectors.Options{Endpoints: map[string]string{"api": srv.URL}})
}

func conn(key string) connectors.Connection {
	return connectors.Connection{Kind: connectors.Brave, Secrets: map[string]string{"api_key": key}}
}

func call(a *Adapter, key, tool, args string) (any, error) {
	return a.Call(context.Background(), conn(key), connectors.AnyTarget, tool, json.RawMessage(args))
}

func TestDescriptor(t *testing.T) {
	a := New(connectors.Options{})
	if d := a.Descriptor(); d.Kind != connectors.Brave || d.TargetLabel != "" || d.Fields[0].Name != "api_key" || !d.Fields[0].Secret {
		t.Fatalf("descriptor = %+v", d)
	}
	for _, tool := range a.Tools() {
		if tool.Write {
			t.Errorf("%s writes", tool.Name)
		}
	}
	if got, err := a.Target(context.Background(), conn("key"), "anything"); err != nil || got != connectors.AnyTarget {
		t.Errorf("Target = %q, %v", got, err)
	}
}

func TestCheck(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, conn("key")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, conn("wrong")); err == nil || !strings.Contains(err.Error(), "does not work") {
		t.Fatalf("wrong key: %v", err)
	}
	if err := a.Check(ctx, conn("")); err == nil {
		t.Fatal("no key passed")
	}
}

func TestSearch(t *testing.T) {
	last, a := newFake(t)
	out, err := call(a, "key", "search", `{"query":" keera agnets ","count":50,"page":3,"country":"ch","language":"DE","freshness":"week"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"q": {"keera agnets"}, "count": {"20"}, "offset": {"2"}, "country": {"CH"}, "search_lang": {"de"}, "freshness": {"pw"},
	}
	if last.Encode() != want.Encode() {
		t.Errorf("query = %v, want %v", *last, want)
	}
	res := out.(Results)
	if res.SearchedFor != "keera agents" || !res.More || len(res.Results) != 1 {
		t.Fatalf("results = %+v", res)
	}
	if r := res.Results[0]; r.Title != "Keera Agents" || r.Description != "Agents that work & talk." ||
		r.URL != "https://example.com/a" || r.Age != "2 days ago" {
		t.Errorf("result = %+v", r)
	}

	if _, err := call(a, "key", "search", `{"query":"x"}`); err != nil {
		t.Fatal(err)
	}
	if last.Get("count") != "10" || last.Get("offset") != "0" || last.Has("country") || last.Has("freshness") {
		t.Errorf("defaults = %v", *last)
	}
	if _, err := call(a, "key", "search", `{"query":"x","freshness":"2026-01-01to2026-03-31"}`); err != nil ||
		last.Get("freshness") != "2026-01-01to2026-03-31" {
		t.Errorf("date range: %v, %v", *last, err)
	}
}

func TestNews(t *testing.T) {
	_, a := newFake(t)
	out, err := call(a, "key", "news", `{"query":"keera"}`)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(Results)
	if len(res.Results) != 1 || res.Results[0].URL != "https://news.example.com/1" ||
		len(res.Results[0].Description) > maxDescription+len("…") {
		t.Errorf("results = %+v", res)
	}
}

func TestBadArguments(t *testing.T) {
	_, a := newFake(t)
	for _, args := range []string{
		`{}`, `{"query":" "}`, `{"query":"x","country":"Switzerland"}`, `{"query":"x","language":"de&x=1"}`,
		`{"query":"x","freshness":"fortnight"}`, `{"query":"x","freshness":"2026-01-01..2026-02-01"}`,
	} {
		if _, err := call(a, "key", "search", args); err == nil {
			t.Errorf("%s passed", args)
		}
	}
	if _, err := call(a, "", "search", `{"query":"x"}`); err == nil {
		t.Error("no key passed")
	}
	if _, err := call(a, "wrong", "news", `{"query":"x"}`); err == nil || !strings.Contains(err.Error(), "SUBSCRIPTION_TOKEN_INVALID") {
		t.Errorf("wrong key: %v", err)
	}
}
