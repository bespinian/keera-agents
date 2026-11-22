// Command keera-agents serves the control plane of Keera's agent platform:
// the panel, its API, connector webhooks and the runtime API. What runs
// inside agents' and workspaces' containers is cmd/keera-agents-runtime.
package main

import (
	"cmp"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	// Schedules run in people's time zones, and the image has no zone files.
	_ "time/tzdata"

	"github.com/bespinian/keera-agents/internal/config"
	"github.com/bespinian/keera-agents/internal/server"
	"github.com/bespinian/keera-agents/internal/version"
)

const usage = `usage: keera-agents [command]

  (none)    serve the control plane, configured by KEERA_AGENTS_* variables
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
