package fleet

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/runtime"
)

// fakeDriver keeps containers in a map and records each call as "verb id".
type fakeDriver struct {
	mu     sync.Mutex
	status map[string]runtime.Status
	specs  map[string]runtime.Spec
	calls  []string
	tiers  []runtime.Isolation
	// createState is what a new container starts as; Pending when empty.
	createState  runtime.State
	createErr    error
	statusErr    error
	terminateErr error
	// onSuspend runs before a container is suspended.
	onSuspend func(agentID string)
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{status: map[string]runtime.Status{}, specs: map[string]runtime.Spec{}}
}

func (d *fakeDriver) record(verb, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, verb+" "+id)
}

// count is how often call was made, such as "create agt_1".
func (d *fakeDriver) count(call string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (d *fakeDriver) set(id string, st runtime.Status) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status[id] = st
}

func (d *fakeDriver) spec(id string) runtime.Spec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.specs[id]
}

func (d *fakeDriver) Name() string { return "fake" }

func (d *fakeDriver) Tiers() []runtime.Isolation { return d.tiers }

func (d *fakeDriver) Create(_ context.Context, spec runtime.Spec) (runtime.Status, error) {
	d.record("create", spec.AgentID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createErr != nil {
		return runtime.Status{}, d.createErr
	}
	st := runtime.Status{State: cmp.Or(d.createState, runtime.Pending), Token: spec.Env[protocol.EnvRuntimeToken]}
	d.status[spec.AgentID] = st
	d.specs[spec.AgentID] = spec
	return st, nil
}

func (d *fakeDriver) Status(_ context.Context, id string) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.statusErr != nil {
		return runtime.Status{}, d.statusErr
	}
	if st, ok := d.status[id]; ok {
		return st, nil
	}
	return runtime.Status{State: runtime.Gone}, nil
}

func (d *fakeDriver) Suspend(_ context.Context, id string) error {
	d.record("suspend", id)
	if d.onSuspend != nil {
		d.onSuspend(id)
	}
	return d.change(id, runtime.Suspended)
}

func (d *fakeDriver) Resume(_ context.Context, id string) error {
	d.record("resume", id)
	return d.change(id, runtime.Pending)
}

func (d *fakeDriver) change(id string, to runtime.State) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.status[id]; !ok {
		return runtime.ErrNotFound
	}
	d.status[id] = runtime.Status{State: to}
	return nil
}

func (d *fakeDriver) Terminate(_ context.Context, id string) error {
	d.record("terminate", id)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.terminateErr != nil {
		return d.terminateErr
	}
	delete(d.status, id)
	return nil
}

func (d *fakeDriver) Logs(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, runtime.ErrNotFound
}

func (d *fakeDriver) Address(_ context.Context, id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.status[id].State != runtime.Running {
		return "", runtime.ErrNotFound
	}
	return "10.0.0.7:7000", nil
}

var testServiceKey = strings.Repeat("s", 32)

// fakeGateway answers the service routes of the gateway's control API. Owners
// are looked up by id: usr_gone left, usr_off is disabled, usr_flaky gets a
// server error and usr_proxy a 404 from something in between.
type fakeGateway struct {
	mu        sync.Mutex
	mints     int
	mintFails bool
	lookups   int
	// revoked holds "org/key".
	revoked []string
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	refuse := func(status int, code, msg string) {
		reply(status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}
	if r.Header.Get("Authorization") != "Bearer "+testServiceKey {
		refuse(http.StatusUnauthorized, "unauthenticated", "wrong key")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == "/control/v1/service/keys" && r.Method == http.MethodPost:
		if g.mintFails {
			refuse(http.StatusServiceUnavailable, "unavailable", "the gateway is busy")
			return
		}
		g.mints++
		reply(http.StatusCreated, gateway.Key{ID: fmt.Sprintf("key_%d", g.mints), Secret: fmt.Sprintf("sk-%d", g.mints)})
	case strings.HasPrefix(r.URL.Path, "/control/v1/service/keys/") && r.Method == http.MethodDelete:
		g.revoked = append(g.revoked, r.URL.Query().Get("org_id")+"/"+strings.TrimPrefix(r.URL.Path, "/control/v1/service/keys/"))
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/control/v1/service/users":
		g.lookups++
		switch id := r.URL.Query().Get("id"); id {
		case "usr_gone":
			refuse(http.StatusNotFound, "not_found", "not a member")
		case "usr_off":
			reply(http.StatusOK, gateway.User{ID: id, Disabled: true})
		case "usr_flaky":
			refuse(http.StatusInternalServerError, "internal", "database down")
		case "usr_proxy":
			http.Error(w, "404 page not found", http.StatusNotFound)
		default:
			reply(http.StatusOK, gateway.User{ID: id})
		}
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func (g *fakeGateway) wasRevoked(org, key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Contains(g.revoked, org+"/"+key)
}

func (g *fakeGateway) counts() (mints, lookups int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mints, g.lookups
}
