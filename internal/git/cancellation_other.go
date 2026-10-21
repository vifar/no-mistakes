//go:build !unix

package git

// Without an identifiable termination signal, retain the original failure.
func cancellationExit(error) bool { return false }
