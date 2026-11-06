package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/metrics"
	"github.com/bespinian/keera-agents/internal/store"
)

// The refusals answer before the database is read, so no store is needed.
func TestMetricsNeedsItsToken(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	reg := metrics.New("test")
	for name, tc := range map[string]struct {
		token, header string
		want          int
	}{
		"off":      {token: "", header: "Bearer ", want: http.StatusNotFound},
		"none":     {token: "0123456789abcdef", header: "", want: http.StatusUnauthorized},
		"wrong":    {token: "0123456789abcdef", header: "Bearer 0123456789abcdeX", want: http.StatusUnauthorized},
		"no kind":  {token: "0123456789abcdef", header: "0123456789abcdef", want: http.StatusUnauthorized},
		"too long": {token: "0123456789abcdef", header: "Bearer 0123456789abcdef0", want: http.StatusUnauthorized},
	} {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		w := httptest.NewRecorder()
		metricsHandler(nil, reg, tc.token, log)(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d", name, w.Code, tc.want)
		}
	}
}

func TestSnapshot(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-time.Minute)
	s := snapshot(store.Gauges{
		Machines: map[[2]string]int{{store.KindAgent, store.AgentRunning}: 4},
		Queued:   2, OldestQueued: &oldest,
	}, now)
	if len(s.Machines) != 10 {
		t.Errorf("every kind and state should be there: %v", s.Machines)
	}
	if s.Machines[[2]string{store.KindAgent, store.AgentRunning}] != 4 ||
		s.Machines[[2]string{store.KindWorkspace, store.AgentFailed}] != 0 {
		t.Errorf("machines: %v", s.Machines)
	}
	if s.Queued != 2 || s.OldestQueued != time.Minute {
		t.Errorf("queue: %+v", s)
	}
	if s := snapshot(store.Gauges{}, now); s.OldestQueued != 0 {
		t.Errorf("no queue, but the oldest waited %s", s.OldestQueued)
	}
}

// While it migrates, the control plane is alive but not ready.
func TestStartingIsAliveButNotReady(t *testing.T) {
	h := startingHandler()
	for path, want := range map[string]int{
		"/healthz": http.StatusOK, "/readyz": http.StatusServiceUnavailable,
		"/api/v1/agents": http.StatusServiceUnavailable,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s = %d, want %d", path, rec.Code, want)
		}
	}
}
