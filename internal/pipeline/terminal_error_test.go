package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// A run's failure is the cancellation only when the cancellation IS the whole
// story. A step that was cut off mid-flight reports the failure it was in the
// middle of, and that failure is its own.
func TestCancellationAttributable(t *testing.T) {
	cause := errors.New("daemon shutting down")
	independent := errors.New("the ref was already deleted")

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the cancellation itself",
			err:  cause,
			want: true,
		},
		{
			name: "a plain context cancellation",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "a step that wraps the cancellation",
			// The step's message says the same thing the cancellation says,
			// with a prefix. That is decoration, not an independent failure.
			err:  &stepExecutionError{step: "document", message: "daemon shutting down", cause: cause},
			want: true,
		},
		{
			name: "a wrapper that adds its own message around the cancellation",
			err:  fmt.Errorf("fatal gate reconciliation: %w", cause),
			want: true,
		},
		{
			name: "a cleanup path that joins a cancelled command error onto a successful cleanup",
			// The join is still the cancellation, so the enclosing join must
			// see that rather than reject it.
			err:  errors.Join(&stepExecutionError{step: "document", message: "daemon shutting down", cause: cause}, nil),
			want: true,
		},
		{
			name: "a subprocess that was killed",
			// The command was doing real work when it was stopped, and that
			// work did not succeed. The read failed; the cancellation only
			// arrived alongside it.
			err:  errors.Join(killedProcessExit(t), context.Canceled),
			want: false,
		},
		{
			name: "a killed subprocess wrapped by a step",
			err: &stepExecutionError{step: "document", message: "git rev-parse: signal: terminated",
				cause: errors.Join(killedProcessExit(t), context.Canceled)},
			want: false,
		},
		{
			name: "a subprocess that failed on its own",
			err:  errors.Join(failedProcessExit(t), context.Canceled),
			want: false,
		},
		{
			name: "an independent failure",
			err:  independent,
			want: false,
		},
		{
			name: "an independent failure joined to the cancellation",
			// The failure would have happened anyway; the cancellation only
			// arrived alongside it.
			err:  errors.Join(independent, context.Canceled),
			want: false,
		},
		{
			name: "an independent failure wrapped by a step",
			err:  &stepExecutionError{step: "document", message: "the ref was already deleted", cause: independent},
			want: false,
		},
		{
			name: "no error",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cancellationAttributable(tc.err, cause); got != tc.want {
				t.Fatalf("cancellationAttributable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The reason a run records when a shutdown reaches it is the shutdown's own
// reason, not the step message that happened to be on top of it.
func TestCancellationAttributableIgnoresAWrapperAroundAnIndependentCause(t *testing.T) {
	cause := errors.New("daemon shutting down")
	inner := fmt.Errorf("wrapped: %w", errors.New("the ref was already deleted"))
	err := &stepExecutionError{step: "push", message: "push failed", cause: inner}
	if cancellationAttributable(err, cause) {
		t.Fatalf("an independent failure was attributed to the cancellation: %v", err)
	}
}

// killedProcessExit runs a process that is killed by a signal and returns its
// exit error, which is what a subprocess stopped mid-flight reports.
func killedProcessExit(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("killed process did not report an exit: %v", err)
	}
	return exit
}

// failedProcessExit runs a process that exits with a failure code of its own
// and returns its exit error.
func failedProcessExit(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 128")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("failed process did not report an exit: %v", err)
	}
	return exit
}
