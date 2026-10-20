//go:build windows || plan9

package pipeline

import "os/exec"

// processWasSignaled reports whether a process exit was a process that was
// stopped by a signal. These platforms do not report a POSIX wait status, so
// no exit is process noise here and every exit keeps its own outcome.
func processWasSignaled(err *exec.ExitError) bool {
	return false
}
