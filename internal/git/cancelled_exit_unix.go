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
// A stop reaches the child as SIGKILL: that is what the standard library's
// own kill does. The shell package terminates the child's whole PROCESS
// GROUP when the command returns, and it sends SIGTERM first, escalating to
// SIGKILL if the group has not gone within the grace period. Which of the
// two arrives is a consequence of that race, and it says nothing about WHO
// stopped the work - so both are the context's doing.
//
// Any other signal is the child's own. A child that SEGFAULTed or ABORTed
// kept its own outcome, and the cancellation did not cause it, so the exit
// is not the cancellation's and must not be marked as one. That is why the
// allow-list is exactly the two stop signals and not "any signalled exit".
//
// An exit that is not signalled at all is not the context's either: a child
// that exited 1 on its own kept its own outcome.
func processWasCancelled(err *exec.ExitError) bool {
	if err == nil {
		return false
	}
	status, ok := err.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && (status.Signal() == syscall.SIGTERM || status.Signal() == syscall.SIGKILL)
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
