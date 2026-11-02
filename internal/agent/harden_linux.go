package agent

import "syscall"

// hideProcess marks the runtime not dumpable. Its /proc files then belong to
// root, so a command the model runs, as the same user, cannot read the
// runtime token from /proc/<pid>/environ or the gateway key from its memory.
// Children are dumpable again after exec, and hold neither.
func hideProcess() error {
	const prSetDumpable = 4
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
