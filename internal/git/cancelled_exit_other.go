//go:build windows || plan9

package git

import "os/exec"

// processWasCancelled reports whether a process exit is the exit of a
// subprocess THIS command's context stopped. These platforms do not report a
// POSIX wait status, so no exit qualifies and the provenance is never claimed.
func processWasCancelled(err *exec.ExitError) bool { return false }

// isCancelRefusal reports whether an error is the standard library's report
// that a command's stop was REFUSED. The refusal is the same report on every
// platform, so the provenance is recorded here too.
func isCancelRefusal(err error) bool {
	return err != nil && len(err.Error()) >= len(cancelRefusalPrefix) && err.Error()[:len(cancelRefusalPrefix)] == cancelRefusalPrefix
}
