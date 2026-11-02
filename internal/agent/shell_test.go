package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

func TestShell(t *testing.T) {
	dir := t.TempDir()
	out, err := runShell(context.Background(), dir, "pwd; echo oops >&2; exit 3", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "exit code: 3") || !strings.Contains(out, dir) || !strings.Contains(out, "oops") {
		t.Errorf("shell = %q", out)
	}
}

func TestShellKeepsTheEndOfLongOutput(t *testing.T) {
	out, _ := runShell(context.Background(), t.TempDir(), "seq 1 20000; echo the-end", 10*time.Second)
	if !strings.HasSuffix(out, "the-end\n") || !strings.Contains(out, "were cut") || len(out) > maxShellOutput+200 {
		t.Errorf("shell output of %d bytes ends %q", len(out), tail(out, 40))
	}
}

func TestShellTimesOutAndKillsChildren(t *testing.T) {
	start := time.Now()
	out, _ := runShell(context.Background(), t.TempDir(), "sleep 30 & sleep 30; echo never", 300*time.Millisecond)
	if !strings.Contains(out, "timed out") || strings.Contains(out, "never") {
		t.Errorf("shell = %q", out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the timeout took %s", d)
	}
}

func TestShellHidesTheRuntimeToken(t *testing.T) {
	t.Setenv(protocol.EnvRuntimeToken, "rt_hidden")
	out, _ := runShell(context.Background(), t.TempDir(), "env", 10*time.Second)
	if strings.Contains(out, "rt_hidden") {
		t.Errorf("a command sees the runtime token: %s", out)
	}
}
