// Command keera-agents runs Keera's agent platform. With no arguments it
// serves the control plane: the panel, its API, channel webhooks and the
// runtime API. `keera-agents agent` is the runtime inside each agent's
// container. One binary does both, so the image an agent runs is built from
// the same source as the server that runs it.
package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// Schedules run in people's time zones, and the image has no zone files.
	_ "time/tzdata"

	"github.com/bespinian/keera-agents/internal/agent"
	"github.com/bespinian/keera-agents/internal/config"
	"github.com/bespinian/keera-agents/internal/server"
	"github.com/bespinian/keera-agents/internal/version"
)

const usage = `usage: keera-agents [command]

  (none)    serve the control plane, configured by KEERA_AGENTS_* variables
  agent     run an agent, inside its container
  health    check that the control plane on this machine is ready
  version   print the version
`

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "", "serve":
		err = server.Run()
	case "agent":
		err = runAgent()
	case "health":
		err = health()
	case "version", "--version":
		fmt.Println(version.String())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "keera-agents:", err)
		os.Exit(1)
	}
}

func runAgent() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agent.Run(ctx, server.NewLogger(os.Getenv("KEERA_AGENT_LOG_LEVEL"), "text"))
}

// health asks this machine's /readyz, for a container healthcheck in an
// image that has no curl.
func health() error {
	addr := cmp.Or(os.Getenv("KEERA_AGENTS_ADDR"), config.DefaultAddr)
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 5 * time.Second}
	res, err := c.Get("http://" + addr + "/readyz")
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: %s", res.Status)
	}
	return nil
}
