package runner

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/dispatch"
	"github.com/bespinian/keera-agents/internal/egress"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// The git proxy passes git's smart HTTP protocol on to a repository an agent
// is bound to, adding the code host's credentials. It allows fetching, and
// pushing to branches that start with protocol.BranchPrefix: the commands at
// the start of a push name every ref it changes, and are checked before any
// byte reaches the host.

// gitClient has no overall timeout: a clone of a large repository takes as
// long as it takes. Its context ends with the agent's request.
func gitClient(p egress.Policy) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           p.Dial(),
			ResponseHeaderTimeout: 2 * time.Minute,
			IdleConnTimeout:       time.Minute,
		},
		CheckRedirect: channels.SameServer,
	}
}

func (rn *Runner) git(w http.ResponseWriter, r *http.Request, a store.Agent) {
	ctx := r.Context()
	// An agent works on a repository only during a task, and a workspace's
	// owner whenever they are connected. On a connector people sign in to,
	// git works as the person the task is for.
	t := dispatch.WorkspaceTask(a)
	if a.Kind != store.KindWorkspace {
		var err error
		if t, err = rn.st.RunningTask(ctx, a.ID); err != nil {
			gitError(w, http.StatusConflict, "git works only during a task")
			return
		}
	}
	// The clone URL names the repository, as web.git, for git's sake; only
	// what follows it matters here.
	rest := r.PathValue("path")
	if _, after, ok := strings.Cut(rest, ".git/"); ok {
		rest = after
	}
	service := r.URL.Query().Get("service")
	switch {
	case r.Method == http.MethodGet && rest == "info/refs" &&
		(service == "git-upload-pack" || service == "git-receive-pack"):
	case r.Method == http.MethodPost && (rest == "git-upload-pack" || rest == "git-receive-pack"):
	default:
		gitError(w, http.StatusNotFound, "only git's smart HTTP protocol is served here")
		return
	}
	remote, err := rn.disp.GitRemote(ctx, a, t, r.PathValue("binding"))
	if err != nil {
		gitError(w, http.StatusNotFound, err.Error())
		return
	}
	forwardGit(w, r, rn.gitHTTP, rn.log.With("agent", a.ID), rest, remote)
}

// forwardGit passes one request on to remote, checking a push first.
func forwardGit(w http.ResponseWriter, r *http.Request, hc *http.Client, log *slog.Logger, rest string,
	remote channels.Remote,
) {
	ctx := r.Context()
	// A clone or push may take longer than the server allows other requests.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	body := io.Reader(r.Body)
	encoding := r.Header.Get("Content-Encoding")
	if rest == "git-receive-pack" {
		if encoding == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				gitError(w, http.StatusBadRequest, "the push is not valid gzip")
				return
			}
			defer func() { _ = zr.Close() }()
			body, encoding = zr, ""
		}
		var err error
		if body, err = checkPush(body); err != nil {
			log.Info("git: refused a push", "error", err)
			gitError(w, http.StatusForbidden, err.Error())
			return
		}
	}

	target := remote.URL + "/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, body)
	if err != nil {
		gitError(w, http.StatusInternalServerError, "could not build the request")
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Git-Protocol"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	// Checking a push keeps its bytes, so only unzipping changes the length.
	// Without it the body goes chunked, which not every host takes.
	if encoding == r.Header.Get("Content-Encoding") {
		req.ContentLength = r.ContentLength
	}
	req.Header.Set("User-Agent", "git/keera-agents")
	req.SetBasicAuth(remote.User, remote.Password)
	resp, err := hc.Do(req)
	if err != nil {
		gitError(w, http.StatusBadGateway, "could not reach the code host: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// 401 would make git ask for a password it does not have.
	if resp.StatusCode == http.StatusUnauthorized {
		gitError(w, http.StatusForbidden, "the code host refused the connector's token")
		return
	}
	for _, h := range []string{"Content-Type", "Cache-Control", "Expires", "Pragma"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Flushed as it comes, so git shows progress and does not time out.
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

func gitError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// maxCommands bounds the commands of one push, so a hostile one cannot make
// the proxy buffer without end.
const maxCommands = 1 << 20

// checkPush reads the commands at the start of a receive-pack request, one
// pkt-line "old new ref" per ref ending in a flush, and refuses refs outside
// the agent's branches. It returns the whole request, to pass on.
func checkPush(body io.Reader) (io.Reader, error) {
	br := bufio.NewReader(body)
	var read bytes.Buffer
	for {
		if read.Len() > maxCommands {
			return nil, errors.New("the push names too many refs")
		}
		var size [4]byte
		if _, err := io.ReadFull(br, size[:]); err != nil {
			return nil, errors.New("the push ended before its commands did")
		}
		read.Write(size[:])
		n, err := strconv.ParseUint(string(size[:]), 16, 16)
		if err != nil {
			return nil, errors.New("the push is not in git's format")
		}
		if n == 0 {
			break
		}
		if n < 4 {
			return nil, errors.New("the push is not in git's format")
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return nil, errors.New("the push ended before its commands did")
		}
		read.Write(line)
		cmd, _, _ := strings.Cut(strings.TrimSuffix(string(line), "\n"), "\x00")
		// A shallow clone names its boundary first; those lines change
		// nothing.
		if strings.HasPrefix(cmd, "shallow ") {
			continue
		}
		fields := strings.Fields(cmd)
		if len(fields) != 3 {
			return nil, fmt.Errorf("the push holds something other than ref updates: %.40q", cmd)
		}
		if !strings.HasPrefix(fields[2], "refs/heads/"+protocol.BranchPrefix) {
			return nil, fmt.Errorf("agents may push only to branches starting with %s, not to %s",
				protocol.BranchPrefix, strings.TrimPrefix(fields[2], "refs/heads/"))
		}
	}
	return io.MultiReader(&read, br), nil
}
