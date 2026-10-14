//go:build unix

package shellenv

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

type failedOutputWriter struct{ err error }

func (w failedOutputWriter) Write([]byte) (int, error) { return 0, w.err }

func TestShellCommandPreservesCommandAndOutputCopyErrors(t *testing.T) {
	copyFailure := errors.New("fixture output writer failure")
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf 'failure' >&2; exit 128")
	cmd.Stderr = failedOutputWriter{copyFailure}
	ConfigureShellCommand(cmd)
	err := RunShellCommand(cmd)
	var exit *exec.ExitError
	if !errors.Is(err, copyFailure) || !errors.As(err, &exit) {
		t.Fatalf("lost command or output-copy failure: %v", err)
	}
}
