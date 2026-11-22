// Command keera-agents-runtime runs inside each agent's and workspace's
// container. It is its own binary, not a subcommand of keera-agents, so the
// containers do not carry the control plane's code and modules: the runtime
// needs only the standard library. It is built from the same source as the
// control plane, so both speak the same protocol.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bespinian/keera-agents/internal/agent"
	"github.com/bespinian/keera-agents/internal/version"
)

const usage = `usage: keera-agents-runtime <command>

  agent     run an agent, inside its container
  workspace run a workspace, inside its container
  version   print the version
`

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "agent":
		err = run(agent.Run)
	case "workspace":
		err = run(agent.RunWorkspace)
	case "version", "--version":
		fmt.Println(version.String())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "keera-agents-runtime:", err)
		os.Exit(1)
	}
}

// run runs an agent's or a workspace's runtime until SIGTERM.
func run(fn func(context.Context, *slog.Logger) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var level slog.Level
	_ = level.UnmarshalText([]byte(os.Getenv("KEERA_AGENT_LOG_LEVEL")))
	return fn(ctx, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
