package runner

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/egress"
)

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

const (
	zero = "0000000000000000000000000000000000000000"
	oid  = "1111111111111111111111111111111111111111"
)

func TestCheckPush(t *testing.T) {
	pack := "PACK\x00\x00\x00\x02rest of the pack"
	ok := pkt(zero+" "+oid+" refs/heads/keera/fix-login\x00report-status side-band-64k\n") +
		pkt(oid+" "+zero+" refs/heads/keera/old\n") + "0000" + pack
	body, err := checkPush(strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	// What goes on to the host is the push as it came.
	if got, _ := io.ReadAll(body); string(got) != ok {
		t.Fatalf("passed on %q", got)
	}
	shallow := pkt("shallow "+oid+"\n") + pkt(zero+" "+oid+" refs/heads/keera/x\x00report-status\n") + "0000"
	if _, err := checkPush(strings.NewReader(shallow)); err != nil {
		t.Fatalf("shallow push: %v", err)
	}

	for name, push := range map[string]string{
		"main":       pkt(zero+" "+oid+" refs/heads/main\x00report-status\n") + "0000",
		"second ref": pkt(zero+" "+oid+" refs/heads/keera/ok\x00x\n") + pkt(zero+" "+oid+" refs/heads/main\n") + "0000",
		"tag":        pkt(zero+" "+oid+" refs/tags/keera/v1\n") + "0000",
		"lookalike":  pkt(zero+" "+oid+" refs/heads/keera-x\n") + "0000",
		"cert":       pkt("push-cert\x00x\n") + "0000",
		"options": pkt(zero+" "+oid+" refs/heads/keera/x\x00report-status push-options\n") + "0000" +
			pkt("merge_request.create") + "0000",
		"options with a value": pkt(zero+" "+oid+" refs/heads/keera/x\x00report-status push-options=1\n") + "0000" +
			pkt("merge_request.create") + "0000",
		"cut short": pkt(zero + " " + oid + " refs/heads/keera/x\n"),
		"garbage":   "zzzz",
	} {
		if _, err := checkPush(strings.NewReader(push)); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

// TestGitThroughTheProxy clones and pushes with git itself, through the
// proxy, to git http-backend standing in for the code host.
func TestGitThroughTheProxy(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	env := []string{
		"HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Scout", "GIT_AUTHOR_EMAIL=scout@agents.keera.invalid",
		"GIT_COMMITTER_NAME=Scout", "GIT_COMMITTER_EMAIL=scout@agents.keera.invalid",
	}
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if out, err := run(dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	hosted := filepath.Join(root, "hosted")
	must(root, "init", "--bare", "-b", "main", filepath.Join(hosted, "web.git"))
	must(filepath.Join(hosted, "web.git"), "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	must(root, "init", "-b", "main", seed)
	must(seed, "commit", "--allow-empty", "-m", "first")
	must(seed, "push", filepath.Join(hosted, "web.git"), "main")

	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, _ := r.BasicAuth(); user != "x-access-token" || pass != "host-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		(&cgi.Handler{
			Path: gitBin, Args: []string{"http-backend"},
			Env: append(env, "GIT_PROJECT_ROOT="+hosted, "GIT_HTTP_EXPORT_ALL=1"),
		}).ServeHTTP(w, r)
	}))
	defer host.Close()
	remote := connectors.Remote{URL: host.URL + "/web.git", User: "x-access-token", Password: "host-token"}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rest, _ := strings.Cut(r.URL.Path, ".git/")
		forwardGit(w, r, gitClient(egress.Policy{}), slog.New(slog.DiscardHandler), rest, remote)
	}))
	defer proxy.Close()

	work := filepath.Join(root, "work")
	must(root, "clone", proxy.URL+"/bnd_1/web.git", work)
	must(work, "commit", "--allow-empty", "-m", "fix")
	must(work, "push", "origin", "HEAD:keera/fix")
	if out, err := run(filepath.Join(hosted, "web.git"), "rev-parse", "--verify", "keera/fix"); err != nil {
		t.Fatalf("the branch did not arrive: %s", out)
	}
	if out, err := run(work, "push", "origin", "HEAD:main"); err == nil {
		t.Fatalf("pushing main passed: %s", out)
	}
	if out, _ := run(filepath.Join(hosted, "web.git"), "log", "--format=%s", "main"); strings.TrimSpace(out) != "first" {
		t.Fatalf("main changed: %q", out)
	}
}
