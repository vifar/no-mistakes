//go:build unix

package git

import (
	"os/exec"
	"syscall"
)

func cancellationExit(err error) bool {
	// A joined error may include an independent output/drain failure.
	exit, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGTERM
}
