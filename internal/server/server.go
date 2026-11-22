// Package server wires keera-agents' control plane together and runs it.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bespinian/keera-agents/internal/api"
	"github.com/bespinian/keera-agents/internal/billing"
	"github.com/bespinian/keera-agents/internal/config"
	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/all"
	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/fleet"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/metrics"
	"github.com/bespinian/keera-agents/internal/runner"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/secret"
	"github.com/bespinian/keera-agents/internal/store"
	"github.com/bespinian/keera-agents/internal/version"
	"github.com/bespinian/keera-agents/internal/webui"
)

// Run starts the control plane and blocks until SIGINT or SIGTERM.
func Run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel, cfg.LogFormat)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return err
	}

	// Listening starts first: migrating can wait for another replica, and a
	// liveness probe that gets no answer meanwhile restarts the pod. Until
	// the app is wired, /healthz answers and everything else says starting.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Addr, err)
	}
	var handler atomic.Pointer[http.Handler]
	starting := startingHandler()
	handler.Store(&starting)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			(*handler.Load()).ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		// Long polls from agents wait up to protocol.MaxWait; log streams
		// lift this themselves.
		WriteTimeout: 2 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	// Returning before the end, as when migrating fails, stops it too.
	defer func() { _ = srv.Close() }()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	defer st.Close()
	applied, err := st.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("migrating the database: %w", err)
	}
	for _, name := range applied {
		log.Info("applied a database migration", "migration", name)
	}

	gw := gateway.New(cfg.GatewayInternalURL, cfg.ServiceKey)
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	if _, err := gw.AuthConfig(check); err != nil {
		log.Warn("Keera Gateway does not answer yet; sign-ins and agents will fail until it does",
			"gateway", cfg.GatewayInternalURL, "error", err)
	}
	cancel()

	driver, err := runtime.New(ctx, cfg.Runtime)
	if err != nil {
		return fmt.Errorf("starting the %s runtime: %w", cfg.Runtime.Driver, err)
	}
	if driver == nil {
		log.Warn("KEERA_AGENTS_DRIVER is not set, so agents can be set up but not run")
	}

	all.Register(connectors.Options{
		HTTP: &http.Client{Timeout: 30 * time.Second, Transport: cfg.Egress.Transport()},
		Dial: cfg.Egress.Dial(),
	})
	app, err := New(cfg, st, gw, driver, box, log)
	if err != nil {
		return err
	}
	handler.Store(&app.Handler)

	background := make(chan struct{})
	go func() {
		app.Background(ctx)
		close(background)
	}()

	log.Info("keera-agents is ready", "addr", cfg.Addr, "public_url", cfg.PublicURL,
		"gateway", cfg.GatewayURL, "driver", cfg.Runtime.Driver, "version", version.String())
	if !strings.HasPrefix(cfg.PublicURL, "http://127.") && !strings.HasPrefix(cfg.PublicURL, "http://[::1]") {
		log.Info("the gateway must allow this address to sign in; set KEERA_AGENTS_URL=" +
			cfg.PublicURL + " on Keera Gateway")
	}

	select {
	case err := <-errc:
		return fmt.Errorf("listening on %s: %w", cfg.Addr, err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdown); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Warn("stopped before every request was answered")
			return nil
		}
		return err
	}
	// A result being sent and messages already answered with 200 would
	// otherwise be lost: nothing sends them again.
	select {
	case <-background:
	case <-shutdown.Done():
	}
	if err := app.disp.Wait(shutdown); err != nil {
		log.Warn("stopped before all accepted work was done", "error", err)
	}
	return nil
}

// healthz says the process is up, while it starts and after.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok\n"))
}

// startingHandler answers while the control plane starts: alive, not ready.
func startingHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "keera-agents is starting", http.StatusServiceUnavailable)
	})
	return mux
}

// App is the control plane, wired: its HTTP handler and its periodic work.
type App struct {
	Handler http.Handler
	st      *store.Store
	fleet   *fleet.Manager
	disp    *dispatch.Dispatcher
	biller  *billing.Biller
	metrics *metrics.Registry
	log     *slog.Logger
}

// New wires the control plane. Adapters must be registered with channels
// first. driver may be nil: agents can then be set up but not run.
func New(cfg config.Config, st *store.Store, gw *gateway.Client, driver runtime.Driver, box *secret.Box, log *slog.Logger) (*App, error) {
	reg := metrics.New(version.String())
	gw.Observe(reg.GatewayCall)
	limits, err := runtime.LimitsFrom(cfg.Runtime, driver)
	if err != nil {
		return nil, err
	}
	biller := billing.New(st, gw, billing.Prices{
		CPUHour: cfg.PriceCPUHour, MemoryGiBHour: cfg.PriceMemoryGiBHour, DiskGiBMonth: cfg.PriceDiskGiBMonth,
	}, log)
	fl := fleet.New(st, gw, driver, box, fleet.Options{
		Image: cfg.Runtime.Image, RuntimeURL: cfg.Runtime.URL, IdleSuspend: cfg.Runtime.IdleSuspend,
		WorkspaceImage: cfg.Runtime.WorkspaceImage, WorkspaceIdleSuspend: cfg.Runtime.WorkspaceIdleSuspend,
		Limits: limits, Credit: biller, Metrics: reg, Log: log,
	})
	var rn *runner.Runner
	disp := dispatch.New(st, fl, gw, box, dispatch.Options{
		PublicURL: cfg.PublicURL, WatchInterval: cfg.WatchInterval, Log: log,
		OnQueued: func(agentID string) { rn.Notify(agentID) },
		Metrics:  reg,
	})
	rn = runner.New(st, fl, disp, gw, runner.Options{
		GatewayURL: cfg.Runtime.GatewayURL, TaskTimeout: cfg.TaskTimeout, Egress: cfg.Egress, Log: log,
	})
	panel := api.New(st, gw, fl, disp, box, api.Options{
		PublicURL: cfg.PublicURL, GatewayURL: cfg.GatewayURL, SecureCookies: cfg.SecureCookies,
		Billing: biller, Log: log,
	})

	mux := http.NewServeMux()
	apiHandler := panel.Handler()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/auth/", apiHandler)
	mux.Handle("/runtime/", rn.Handler())
	mux.HandleFunc(dispatch.HookPath, disp.ServeHook)
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Ping(r.Context()); err != nil {
			http.Error(w, "the database does not answer", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", metricsHandler(st, reg, cfg.MetricsToken, log))
	mux.Handle("/", webui.Handler())
	return &App{Handler: mux, st: st, fleet: fl, disp: disp, biller: biller, metrics: reg, log: log}, nil
}

// metricsHandler serves the Prometheus exposition to whoever holds token. With
// no token, /metrics is off.
func metricsHandler(st *store.Store, reg *metrics.Registry, token string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.NotFound(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "the metrics token is required", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		g, err := st.Gauges(ctx)
		if err != nil {
			log.Warn("could not read the metrics' gauges", "error", err)
			http.Error(w, "the database does not answer", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		reg.Write(w, snapshot(g, time.Now()))
	}
}

// snapshot turns the store's gauges into what /metrics shows. Every kind and
// state is there, as 0 if need be, so a series never vanishes.
func snapshot(g store.Gauges, now time.Time) metrics.Snapshot {
	s := metrics.Snapshot{
		Machines: map[[2]string]int{}, Queued: g.Queued, Running: g.Running, Undelivered: g.Undelivered,
	}
	for _, kind := range []string{store.KindAgent, store.KindWorkspace} {
		for _, state := range []string{
			store.AgentStarting, store.AgentRunning, store.AgentSuspended, store.AgentStopped, store.AgentFailed,
		} {
			s.Machines[[2]string{kind, state}] = 0
		}
	}
	maps.Copy(s.Machines, g.Machines)
	if g.OldestQueued != nil {
		s.OldestQueued = max(now.Sub(*g.OldestQueued), 0)
	}
	return s
}

// leaderLock is the advisory lock the periodic work runs under, so one
// replica at a time starts schedules, picks up cards, reads watched folders,
// reconciles machines, bills them and prunes.
const leaderLock = 0x6A6_0002

// Background runs the periodic work until ctx ends. Each round takes the
// lock, does what is due and lets go, so another replica takes over when
// this one stops.
func (a *App) Background(ctx context.Context) {
	st, fl, disp, biller, log := a.st, a.fleet, a.disp, a.biller, a.log
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var lastReconcile, lastOwners, lastPrune, lastMeter time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		release, ok, err := st.TryLock(ctx, leaderLock)
		if err != nil {
			log.Warn("could not take the scheduler lock", "error", err)
			continue
		}
		if !ok {
			continue
		}
		func() {
			defer release()
			now := time.Now()
			defer func() { a.metrics.Round(time.Now(), time.Since(now)) }()
			step(ctx, func(ctx context.Context) { disp.ExpireTasks(ctx) })
			step(ctx, func(ctx context.Context) { disp.RunSchedules(ctx, now) })
			step(ctx, func(ctx context.Context) { disp.PickUpCards(ctx) })
			step(ctx, func(ctx context.Context) { disp.Deliver(ctx) })
			step(ctx, func(ctx context.Context) { disp.CheckWatches(ctx, now) })
			// The rarer steps count as done only if they finished in time;
			// otherwise the next round tries again.
			if now.Sub(lastReconcile) >= 15*time.Second && step(ctx, func(ctx context.Context) {
				fl.Reconcile(ctx)
				fl.RekeyStale(ctx)
			}) {
				lastReconcile = now
			}
			// Metering counts the time since its last round itself, so a
			// round that comes late loses nothing.
			if fl.Runs() && now.Sub(lastMeter) >= time.Minute && step(ctx, func(ctx context.Context) {
				biller.Meter(ctx, now)
				biller.Send(ctx, now)
			}) {
				lastMeter = now
			}
			if now.Sub(lastOwners) >= 10*time.Minute && step(ctx, func(ctx context.Context) {
				fl.RevokeDeletedKeys(ctx)
				fl.CheckOwners(ctx)
				disp.CheckAccounts(ctx)
			}) {
				lastOwners = now
			}
			var pruned bool
			if now.Sub(lastPrune) >= 24*time.Hour && step(ctx, func(ctx context.Context) {
				n, err := st.PruneTasks(ctx, now.Add(-store.TaskRetention))
				if err == nil {
					err = st.PruneWrites(ctx, now.AddDate(0, 0, -7))
				}
				if err == nil {
					err = st.PruneUsage(ctx, now.Add(-billing.Retention))
				}
				if err != nil {
					log.Warn("could not delete old rows", "error", err)
					return
				}
				if n > 0 {
					log.Info("deleted old tasks", "count", n)
				}
				pruned = true
			}) && pruned {
				lastPrune = now
			}
		}()
	}
}

// stepTimeout bounds one step of a round, so a slow one, such as a delivery
// waiting on a service, leaves the others their time. Tests shorten it.
var stepTimeout = time.Minute

// step runs f with its own deadline and reports whether it finished before
// the deadline.
func step(ctx context.Context, f func(context.Context)) bool {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	f(ctx)
	return ctx.Err() == nil
}

// newLogger writes to stderr, as text or JSON.
func newLogger(level, format string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: l}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
