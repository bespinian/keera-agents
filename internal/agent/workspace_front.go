package agent

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/stream"
)

// The ports inside a workspace that its front names.
const (
	sshPort      = 2222
	terminalPort = 7681
	vscodePort   = 8080
)

// serveFront takes connections keera-agents passes through from the
// workspace's owner, and joins each to a port on the loopback. keera-agents
// proves itself with the ingress secret, which changes at each start; the
// namespace's NetworkPolicy keeps everyone else from the port in the first
// place.
func (r *runner) serveFront(ctx context.Context) {
	if err := serve(ctx, ":"+strconv.Itoa(protocol.FrontPort), http.HandlerFunc(r.front)); err != nil {
		r.log.Error("workspace: nobody can connect, since the front could not listen", "error", err)
	}
}

func (r *runner) front(w http.ResponseWriter, req *http.Request) {
	boot := r.snapshot()
	want := ""
	if boot.Workspace != nil {
		want = boot.Workspace.IngressSecret
	}
	got, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		http.Error(w, "not keera-agents", http.StatusUnauthorized)
		return
	}
	port, ok := frontPort(req.URL.Query().Get("port"), boot.Workspace.Experience)
	if !ok {
		http.Error(w, "port must be web, ssh or a number", http.StatusBadRequest)
		return
	}
	// Dialled before the upgrade, so a port nothing listens on is an answer
	// keera-agents can pass on.
	inside, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		http.Error(w, "nothing listens on port "+strconv.Itoa(port)+" in the workspace", http.StatusBadGateway)
		return
	}
	conn, err := stream.Accept(w, req)
	if err != nil {
		_ = inside.Close()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stream.Join(conn, inside)
}

// frontPort resolves the port a connection names.
func frontPort(name, experience string) (int, bool) {
	switch name {
	case protocol.PortSSH:
		return sshPort, true
	case protocol.PortWeb:
		if experience == "vscode" {
			return vscodePort, true
		}
		return terminalPort, true
	}
	return protocol.Port(name)
}
