//go:build !windows && !plan9

package git

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// processWasCancelled reports whether a process exit is the exit of a
// subprocess THIS command's context stopped.
//
// The context stops its own child with SIGTERM, so a signalled exit by that
// signal is the cancellation's doing. Any other exit - a child that died of
// SIGSEGV on its own, or one that was killed by something else - is not.
//
// The signal matters, and not only because it names the killer. The shell
// package terminates the child's whole PROCESS GROUP when the command
// returns: SIGTERM first, and SIGKILL if the group has not gone within the
// grace period. A child killed by that escalation died of the package's
// cleanup, not of the context's stop, and the difference is the difference
// between the cancellation's failure and an independent one.
func processWasCancelled(err *exec.ExitError) bool {
	if err == nil {
		return false
	}
	status, ok := err.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGTERM
}

// cancelRefusalPrefix is the standard library's report of a REFUSED stop. It
// wraps the refusal the cancel hook returned, so the text is the only way to
// recognise it: the wrapper type is unexported.
const cancelRefusalPrefix = "exec: canceling Cmd"

// isCancelRefusal reports whether an error is the standard library's report
// that a command's stop was REFUSED.
//
// When the stop is refused - the process group is already gone, or the
// platform refuses the kill - the standard library reports the refusal
// instead of the child's exit status, so there is no exit status to
// inspect. The refusal is still the context's doing: the context is what
// interrupted the command, and a refused kill does not turn the
// interruption into an independent failure.
//
// The check is on the refusal itself, not on the mere presence of a
// cancellation: an error that is not the refusal is the command's own, and
// is never attributed to the cancellation.
func isCancelRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrProcessDone) {
		// The process had already finished. That is not a refusal and
		// not a cancellation's doing.
		return false
	}
	return strings.HasPrefix(err.Error(), cancelRefusalPrefix)
}
