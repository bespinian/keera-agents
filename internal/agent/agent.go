// Package agent is the runtime inside each agent's container
// (`keera-agents agent`). It asks the control plane for tasks, runs each one
// as a loop of model calls and tool calls through Keera Gateway, and reports
// what it did. See internal/protocol for the API it speaks.
//
// The container holds two credentials: the runtime token, from its
// environment, and the agent's gateway key, from the bootstrap. Neither goes
// into a log, an event, a prompt, or the environment of a command it runs.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// config is what the runtime reads from its environment.
type config struct {
	RuntimeURL string
	Token      string
	AgentID    string
	Workspace  string
}

func configFromEnv(getenv func(string) string) (config, error) {
	c := config{
		RuntimeURL: strings.TrimRight(strings.TrimSpace(getenv(protocol.EnvRuntimeURL)), "/"),
		Token:      strings.TrimSpace(getenv(protocol.EnvRuntimeToken)),
		AgentID:    strings.TrimSpace(getenv(protocol.EnvAgentID)),
		Workspace:  strings.TrimSpace(getenv(protocol.EnvWorkspace)),
	}
	if c.Workspace == "" {
		c.Workspace = protocol.Workspace
	}
	if u, err := url.Parse(c.RuntimeURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("%s must be keera-agents' address as this container reaches it, "+
			"such as http://keera-agents:8070, not %q", protocol.EnvRuntimeURL, c.RuntimeURL)
	}
	if c.Token == "" {
		return c, fmt.Errorf("%s is not set; the control plane sets it when it creates the container",
			protocol.EnvRuntimeToken)
	}
	return c, nil
}

// Run serves tasks until ctx ends, and then returns nil. It fails only on a
// configuration that cannot work. Cancel ctx on SIGTERM: the runtime then
// reports the task in hand as failed and exits cleanly, which the drivers
// read as suspended.
func Run(ctx context.Context, log *slog.Logger) error {
	return start(ctx, log, os.Getenv, true)
}

// Serve is Run with the environment read from getenv and without hardening
// the process, so tests can run agents inside the test binary.
func Serve(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	return start(ctx, log, getenv, false)
}

func start(ctx context.Context, log *slog.Logger, getenv func(string) string, harden bool) error {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := configFromEnv(getenv)
	if err != nil {
		return err
	}
	if harden {
		// Commands the model runs inherit the environment; the token stays out.
		_ = os.Unsetenv(protocol.EnvRuntimeToken)
		if err := hideProcess(); err != nil {
			log.Warn("agent: could not hide the runtime's memory from its commands", "error", err)
		}
	}
	r, err := newRunner(cfg, log)
	if err != nil {
		return err
	}
	r.run(ctx)
	return nil
}

type runner struct {
	cfg config
	log *slog.Logger
	cp  *controlPlane
	gw  *gateway
	web *http.Client
	ws  *workspace

	mu     sync.RWMutex
	boot   protocol.Bootstrap
	bootAt time.Time
	mcp    []mcpConn

	// Timings, shortened by tests.
	wait         time.Duration
	backoffMin   time.Duration
	backoffMax   time.Duration
	refreshEvery time.Duration
	flushEvery   time.Duration
	now          func() time.Time
}

type mcpConn struct {
	client *mcpClient
	tools  []mcpTool
}

func newRunner(cfg config, log *slog.Logger) (*runner, error) {
	ws, err := openWorkspace(cfg.Workspace)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &runner{
		cfg: cfg,
		log: log.With("agent", cfg.AgentID),
		cp:  &controlPlane{base: cfg.RuntimeURL, token: cfg.Token, http: &http.Client{Transport: transport}},
		gw:  &gateway{http: &http.Client{Transport: transport}, retryWait: 2 * time.Second},
		// Its own transport: a page may be slow or hostile, and must not hold
		// connections the gateway needs.
		web:          &http.Client{Timeout: webTimeout},
		ws:           ws,
		wait:         30 * time.Second,
		backoffMin:   time.Second,
		backoffMax:   30 * time.Second,
		refreshEvery: 10 * time.Minute,
		flushEvery:   time.Second,
		now:          time.Now,
	}, nil
}

// run takes tasks until ctx ends.
func (r *runner) run(ctx context.Context) {
	r.log.Info("agent: starting", "workspace", r.cfg.Workspace)
	if !r.bootstrapUntilReady(ctx) {
		return
	}
	go r.serveGit(ctx)
	backoff := r.backoffMin
	for ctx.Err() == nil {
		if r.now().Sub(r.bootstrappedAt()) > r.refreshEvery {
			// Each task's version brings changes at once; this is a safety
			// net, as for an MCP server that failed to connect.
			if err := r.refresh(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("agent: fetching the bootstrap again failed; keeping the old one", "error", err)
			}
		}
		task, err := r.cp.next(ctx, r.wait)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.log.Warn("agent: asking for the next task failed", "error", err, "retry_in", backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, r.backoffMax)
			continue
		}
		backoff = r.backoffMin
		if task == nil {
			continue
		}
		if task.Version != "" && task.Version != r.snapshot().Version {
			if err := r.refresh(ctx); err != nil {
				r.log.Warn("agent: the agent's settings changed but fetching them failed; "+
					"running the task with the old ones", "error", err)
			}
		}
		r.runTask(ctx, *task)
	}
	r.log.Info("agent: stopping")
}

// bootstrapUntilReady waits for the control plane, which may start after
// the container, or be restarting. It is false when ctx ended first.
func (r *runner) bootstrapUntilReady(ctx context.Context) bool {
	backoff := r.backoffMin
	for {
		err := r.refresh(ctx)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		r.log.Warn("agent: waiting for the control plane", "error", err, "retry_in", backoff)
		sleep(ctx, backoff)
		backoff = min(backoff*2, r.backoffMax)
	}
}

// refresh fetches the bootstrap and connects to its MCP servers. A server
// that fails is logged and left out.
func (r *runner) refresh(ctx context.Context) error {
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	boot, err := r.cp.bootstrap(bctx)
	cancel()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.boot, r.bootAt = boot, r.now()
	r.mu.Unlock()

	var conns []mcpConn
	for _, s := range boot.Gateway.MCP {
		c := &mcpClient{
			alias: s.Alias,
			url:   s.URL,
			http:  r.gw.http,
			key:   r.apiKey,
		}
		tools, err := c.connect(ctx)
		if err != nil {
			r.log.Warn("agent: skipping an MCP server that failed to connect", "mcp", s.Alias, "error", err)
			continue
		}
		conns = append(conns, mcpConn{client: c, tools: tools})
	}
	r.mu.Lock()
	r.mcp = conns
	r.mu.Unlock()
	r.log.Info("agent: bootstrapped", "name", boot.Agent.Name, "model", boot.Agent.Model,
		"version", boot.Version, "builtins", boot.Agent.Builtins, "platform_tools", len(boot.Tools),
		"mcp_servers", len(conns))
	return nil
}

func (r *runner) snapshot() protocol.Bootstrap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.boot
}

func (r *runner) bootstrappedAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bootAt
}

func (r *runner) apiKey() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.boot.Gateway.APIKey
}

func (r *runner) mcpConns() []mcpConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mcp
}

// sleep waits d, with some jitter so many agents do not retry in step.
func sleep(ctx context.Context, d time.Duration) {
	d += time.Duration(rand.Int64N(int64(d)/5 + 1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

var (
	errTaskGone = errors.New("the task was cancelled")
	errStopped  = errors.New("the agent's runtime was stopped")
)
