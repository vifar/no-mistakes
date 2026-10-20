//go:build !windows && !plan9

package pipeline

import (
	"os/exec"
	"syscall"
)

// processWasSignaled reports whether a process exit was a process that was
// stopped by a signal, as opposed to one that exited on its own with a
// failure code.
//
// Only a signaled process is process noise for a cancellation: it says what
// the process was doing when it was stopped, not that it failed for its own
// reasons. A command that exited 128 because the ref was rejected failed on
// its own, and a cancellation observed after that does not change it.
func processWasSignaled(err *exec.ExitError) bool {
	if err == nil {
		return false
	}
	status, ok := err.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}
