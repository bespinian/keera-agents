package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// child is a program the workspace runtime keeps running.
type child struct {
	name string
	path string
	args []string
	// prepare runs once before the first start.
	prepare func() error
}

// vscodeExtensions holds the extensions the image ships, such as Cline.
// They are installed into the workspace at the first start, since anything
// the image put in the home directory would be hidden by the volume.
const vscodeExtensions = "/opt/keera/vscode-extensions"

// projectsDir is the folder in the home directory VS Code opens.
const projectsDir = "Projects"

func sshdCommand(workspace string) child {
	// sshd must be started by its full path: it runs itself again for each
	// connection.
	return child{name: "sshd", path: "/usr/sbin/sshd", args: []string{
		"-D", "-e", "-f", filepath.Join(workspace, ".keera", "ssh", "sshd_config"),
	}}
}

// experienceCommand is what the browser opens. The terminal is pi in tmux,
// so the browser and ssh with `tmux attach` share one session, and a shell
// is left when pi exits.
func experienceCommand(experience, workspace string) child {
	if experience == "vscode" {
		// Not the home directory, which holds the runtime's and the tools'
		// own files, but a folder of the owner's.
		folder := filepath.Join(workspace, projectsDir)
		return child{name: "code-server", path: "code-server", args: []string{
			"--bind-addr", "127.0.0.1:" + strconv.Itoa(vscodePort),
			// The front lets only the owner in, through keera-agents.
			"--auth", "none",
			// Its /proxy/<port>/ would let any web page the owner visits
			// reach the bridge and the git proxy. keera workspace forward
			// does the same job safely.
			"--disable-proxy",
			"--disable-telemetry", "--disable-update-check",
			// Coder's own advert in Help: Getting Started.
			"--disable-getting-started-override",
			folder,
		}, prepare: func() error {
			return errors.Join(os.MkdirAll(folder, 0o755), installExtensions(workspace))
		}}
	}
	return child{name: "ttyd", path: "ttyd", args: []string{
		"--interface", "127.0.0.1", "--port", strconv.Itoa(terminalPort), "--writable",
		// Without it, any page open in the owner's browser could reach the
		// shell through the port `keera workspace open` listens on.
		"--check-origin",
		"tmux", "new-session", "-A", "-s", "pi", "-c", workspace, "pi; exec bash -l",
	}}
}

// supervise runs c until ctx ends, starting it again when it exits, less
// and less often while it keeps failing.
func (r *runner) supervise(ctx context.Context, c child) {
	path, err := exec.LookPath(c.path)
	if err != nil {
		r.log.Warn("workspace: not in this image, so not started", "program", c.name, "error", err)
		return
	}
	if c.prepare != nil {
		if err := c.prepare(); err != nil {
			r.log.Warn("workspace: preparing a program failed; starting it anyway", "program", c.name, "error", err)
		}
	}
	env := append(childEnv(), r.workspaceEnv()...)
	backoff := time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, path, c.args...)
		cmd.Dir = r.cfg.Workspace
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		// A stop gives it time to save, as code-server does its state.
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
		started := time.Now()
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		r.log.Warn("workspace: a program exited; starting it again", "program", c.name, "error", err,
			"retry_in", backoff)
		sleep(ctx, backoff)
		backoff = min(backoff*2, time.Minute)
	}
}

// installExtensions installs each extension the image ships once. A mark in
// the workspace says it was, so one the owner removed stays removed.
func installExtensions(workspace string) error {
	files, _ := filepath.Glob(filepath.Join(vscodeExtensions, "*.vsix"))
	marks := filepath.Join(workspace, ".keera", "vscode")
	if err := os.MkdirAll(marks, 0o755); err != nil {
		return err
	}
	var failed error
	for _, f := range files {
		mark := filepath.Join(marks, filepath.Base(f)+".installed")
		if _, err := os.Stat(mark); err == nil {
			continue
		}
		cmd := exec.Command("code-server", "--install-extension", f)
		cmd.Env = childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			failed = fmt.Errorf("installing %s: %w: %s", filepath.Base(f), err, strings.TrimSpace(string(out)))
			continue
		}
		_ = os.WriteFile(mark, nil, 0o644)
	}
	return failed
}

// workspaceEnv points what runs in the workspace at the bridge, and makes
// the owner the author of commits.
func (r *runner) workspaceEnv() []string {
	boot := r.snapshot()
	env := []string{
		// Read by many OpenAI-compatible tools. The bridge adds the real key.
		"OPENAI_BASE_URL=" + bridgeURL + "/v1",
		"OPENAI_API_KEY=keera",
		"TERM=xterm-256color",
	}
	if email := boot.Agent.Owner.Email; email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
	}
	return env
}
