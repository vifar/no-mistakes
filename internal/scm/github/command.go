package github

import (
	"context"
	"fmt"
	"os/exec"
	"sync/atomic"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

type ghCancellationError struct {
	cause error
	exit  error
}

func (e *ghCancellationError) Error() string { return fmt.Sprintf("%v: %v", e.exit, e.cause) }
func (e *ghCancellationError) Unwrap() error { return e.cause }

func runGHCommand(ctx context.Context, cmd *exec.Cmd, run func() error) error {
	shellenv.ConfigureShellCommand(cmd)
	cancel := cmd.Cancel
	var stopped atomic.Bool
	cmd.Cancel = func() error {
		err := cancel()
		if err == nil {
			stopped.Store(true)
		}
		return err
	}
	err := run()
	if exit, ok := err.(*exec.ExitError); ok && stopped.Load() && ghCommandWasStopped(exit) && ctx.Err() != nil {
		return &ghCancellationError{cause: ctx.Err(), exit: err}
	}
	return err
}
