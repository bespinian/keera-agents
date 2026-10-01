//go:build !unix

package agent

import "os/exec"

// setProcessGroup does nothing here: the runtime runs in a Linux container,
// and other systems only build it.
func setProcessGroup(*exec.Cmd) {}
