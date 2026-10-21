//go:build windows || plan9

package git

import "os/exec"

// processWasCancelled reports whether a process exit is the exit of a
// subprocess THIS command's context stopped. These platforms do not report a
// POSIX wait status, so no exit qualifies and the provenance is never claimed.
func processWasCancelled(err *exec.ExitError) bool { return false }
