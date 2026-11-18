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
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			forward(pr, base, protocol.GitPath+pr.In.URL.Path, r.cp.token)
		},
		Transport: r.cp.http.Transport,
		// Passed on as it comes, so git shows its progress.
		FlushInterval: -1,
	}
	err = serve(ctx, addr.Host, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Only git's paths: the token must not reach the rest of the
		// runtime API from here.
		if strings.Contains(req.URL.Path, "..") {
			http.Error(w, "not a repository", http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, req)
	}))
	if err != nil {
		r.log.Warn("agent: git cannot reach repositories, since its proxy could not listen", "error", err)
	}
}

// serve answers on addr with h until ctx ends. It returns at once if it
// cannot listen.
func serve(ctx context.Context, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: notFromBrowsers(h), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
	return nil
}

// notFromBrowsers refuses requests a browser sent. Git, pi and Cline send
// neither header; a web page that reaches the loopback somehow, such as
// through a proxy in the workspace, sends at least one.
func notFromBrowsers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "not for browsers", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// forward points a proxied request at path under base, with token as its
// bearer token.
func forward(pr *httputil.ProxyRequest, base *url.URL, path, token string) {
	pr.Out.URL.Scheme, pr.Out.URL.Host = base.Scheme, base.Host
	pr.Out.URL.Path = strings.TrimRight(base.Path, "/") + path
	pr.Out.URL.RawPath = ""
	pr.Out.Host = base.Host
	pr.Out.Header.Set("Authorization", "Bearer "+token)
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
