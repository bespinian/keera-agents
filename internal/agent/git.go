package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// serveGit lets git in the agent's commands reach its repositories. It
// listens on protocol.GitProxyURL, on the loopback only, and passes each
// request on to the control plane's git proxy with the runtime token, which
// commands never see. The control plane adds the code host's token and
// decides what may be fetched and pushed.
func (r *runner) serveGit(ctx context.Context) {
	addr, _ := url.Parse(protocol.GitProxyURL)
	base, err := url.Parse(r.cp.base)
	if err != nil {
		return
	}
	ln, err := net.Listen("tcp", addr.Host)
	if err != nil {
		r.log.Warn("agent: git cannot reach repositories, since its proxy could not listen", "error", err)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = base.Scheme, base.Host
			pr.Out.URL.Path = strings.TrimRight(base.Path, "/") + protocol.GitPath + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = base.Host
			pr.Out.Header.Set("Authorization", "Bearer "+r.cp.token)
		},
		Transport: r.cp.http.Transport,
		// Passed on as it comes, so git shows its progress.
		FlushInterval: -1,
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// Only git's paths: the token must not reach the rest of the
			// runtime API from here.
			if strings.Contains(req.URL.Path, "..") {
				http.Error(w, "not a repository", http.StatusBadRequest)
				return
			}
			proxy.ServeHTTP(w, req)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
}

// gitEnv makes the agent the author of its commits, and stops git from
// asking for a password nobody can type.
func gitEnv(a protocol.Agent) []string {
	name := a.Name
	if name == "" {
		name = "Keera agent"
	}
	// .invalid is never a real domain, so the address reaches nobody.
	email := a.ID + "@agents.keera.invalid"
	return []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
		"GIT_TERMINAL_PROMPT=0",
	}
}
