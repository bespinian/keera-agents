package agent

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// The workspace runtime (`keera-agents workspace`) runs in a workspace: a
// machine an engineer works in, with a coding agent they drive. It takes no
// tasks. It keeps the same two credentials as an agent's runtime, out of
// reach of what runs beside it, and serves:
//
//	127.0.0.1:7417  the git proxy, as for an agent
//	127.0.0.1:7418  the bridge: the gateway's models and the workspace's
//	                tools, without a key, for the coding agent
//	:7420           the front, where keera-agents passes the owner's
//	                connections through to a port inside
//
// and keeps sshd and the experience (pi in ttyd, or code-server) running.

// BridgeURL is where the coding agent reaches models and tools.
const BridgeURL = "http://127.0.0.1:7418"

// RunWorkspace serves a workspace until ctx ends.
func RunWorkspace(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	// As for an agent: what the engineer and their coding agent run must
	// not read the token or the key.
	_ = os.Unsetenv(protocol.EnvRuntimeToken)
	if err := hideProcess(); err != nil {
		log.Warn("workspace: could not hide the runtime's memory from what runs beside it", "error", err)
	}
	r, err := newRunner(cfg, log)
	if err != nil {
		return err
	}
	r.log = log.With("workspace", cfg.AgentID)
	r.runWorkspace(ctx)
	return nil
}

func (r *runner) runWorkspace(ctx context.Context) {
	r.log.Info("workspace: starting", "workspace", r.cfg.Workspace)
	if !r.bootstrapUntilReady(ctx) {
		return
	}
	boot := r.snapshot()
	if boot.Workspace == nil {
		r.log.Error("workspace: keera-agents sees this container as an agent; run `keera-agents agent` in it")
		return
	}
	r.writeConfigs(ctx)
	go r.serveGit(ctx)
	go r.serveBridge(ctx)
	go r.serveFront(ctx)
	go r.supervise(ctx, sshdCommand(r.cfg.Workspace))
	go r.supervise(ctx, experienceCommand(boot.Workspace.Experience, r.cfg.Workspace))

	// SSH keys, connectors and models change while it runs.
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("workspace: stopping")
			return
		case <-t.C:
			if err := r.refresh(ctx); err != nil {
				if ctx.Err() == nil {
					r.log.Warn("workspace: fetching the bootstrap again failed; keeping the old one", "error", err)
				}
				continue
			}
			r.writeConfigs(ctx)
		}
	}
}
