//go:build !windows && !plan9

package git

import (
	"os/exec"
	"syscall"
)

// processWasCancelled reports whether a process exit is the exit of a
// subprocess THIS command's context stopped.
//
// The context stops its child with SIGTERM, so a signalled exit by that
// signal is the cancellation's doing. Any other exit - a child that died of
// SIGSEGV on its own, or one that was killed by something else - is not.
func processWasCancelled(err *exec.ExitError) bool {
	if err == nil {
		return false
	}
	status, ok := err.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGTERM
}
