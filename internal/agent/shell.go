package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

const (
	shellDefaultTimeout = 120 * time.Second
	shellMaxTimeout     = 600 * time.Second
	// maxShellOutput is the end of the output the model sees, where errors
	// usually are.
	maxShellOutput = 16 << 10
)

// shellTool runs commands in dir, with env added to the runtime's own.
func shellTool(dir string, env []string) tool {
	return tool{
		name: "shell",
		description: fmt.Sprintf("Run a shell command with bash in your workspace and return its exit code "+
			"and combined output (the last %d KiB). Times out after timeout_seconds (default %d, at most %d). "+
			"Each call is a new shell: use && to chain, and files to keep state.",
			maxShellOutput>>10, int(shellDefaultTimeout.Seconds()), int(shellMaxTimeout.Seconds())),
		parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"command":{"type":"string","description":"The command, run with bash -lc."},` +
			`"timeout_seconds":{"type":"integer"}},"required":["command"]}`),
		summary: summarise("shell", "command"),
		run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Command) == "" {
				return "", errors.New("a command is needed")
			}
			timeout := shellDefaultTimeout
			if a.TimeoutSeconds > 0 {
				timeout = min(time.Duration(a.TimeoutSeconds)*time.Second, shellMaxTimeout)
			}
			return runShell(ctx, dir, a.Command, timeout, env...)
		},
	}
}

func runShell(ctx context.Context, dir, command string, timeout time.Duration, env ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, flag := "bash", "-lc"
	if _, err := exec.LookPath("bash"); err != nil {
		name, flag = "sh", "-c"
	}
	cmd := exec.CommandContext(ctx, name, flag, command)
	cmd.Dir = dir
	cmd.Env = append(childEnv(), env...)
	out := &tailBuffer{max: maxShellOutput}
	cmd.Stdout, cmd.Stderr = out, out
	// Kill the whole process group, so a command's children do not outlive
	// its timeout, and stop waiting for pipes a stray child holds open.
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	err := cmd.Run()
	var b strings.Builder
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		fmt.Fprintf(&b, "timed out after %s and was killed\n", timeout)
	case err != nil && cmd.ProcessState == nil:
		return "", fmt.Errorf("starting %s: %v", name, err)
	default:
		fmt.Fprintf(&b, "exit code: %d (%.1fs)\n", cmd.ProcessState.ExitCode(), time.Since(start).Seconds())
	}
	if cut := out.cut(); cut > 0 {
		fmt.Fprintf(&b, "[the first %s of output were cut]\n", size(cut))
	}
	b.WriteString(out.String())
	return b.String(), nil
}

// childEnv is the runtime's environment without its token, which a command
// could otherwise print. The gateway key never is in the environment.
func childEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, protocol.EnvRuntimeToken+"=")
	})
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	max   int
	buf   []byte
	total int64
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += int64(len(p))
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return tail(string(bytes.ToValidUTF8(t.buf, []byte("?"))), t.max)
}

func (t *tailBuffer) cut() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(t.total-int64(t.max), 0)
}
