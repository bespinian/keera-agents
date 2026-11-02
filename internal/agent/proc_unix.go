//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The negative pid is the group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
