package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// A run's failure is the cancellation only when the cancellation is the whole
// story. A step that was cut off mid-flight reports the failure it was in the
// middle of, and that failure is its own UNLESS the cancellation is what
// stopped the work.
func TestCancellationAttributable(t *testing.T) {
	cause := errors.New("daemon shutting down")
	independent := errors.New("the ref was already deleted")
	// A wrapper that adds MEANING rather than a message: the read did not
	// produce a result, which is a failure in its own right.
	meaningful := fmt.Errorf("observe shipping ref: remote observation failed: %w", independent)

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
			name: "the pipeline's own report of a step that failed",
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
			name: "a wrapper that adds meaning",
			// The observation failed to produce a result. That is the
			// failure, whatever stopped the process.
			err:  meaningful,
			want: false,
		},
		{
			name: "a step whose meaning wrapped the cancellation",
			err:  &stepExecutionError{step: "ci", message: "observe shipping ref: remote observation failed", cause: meaningful},
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
			name: "a step's report joined to an independent failure",
			err: errors.Join(&stepExecutionError{step: "ci", message: "remote observation failed", cause: independent},
				errors.New("independent failure")),
			want: false,
		},
		{
			name: "an independent failure",
			err:  independent,
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

// A subprocess this command's own context stopped is the cancellation: the
// git package records that provenance at the execution boundary, where it is
// known, and the classifier reads the marker rather than guessing from the
// shape of the error.
//
// The marker is read through the git package's own transport wrapper, which
// adds the command and the captured stderr - a MESSAGE, not a meaning.
func TestCancellationAttributableAcceptsAStoppedSubprocess(t *testing.T) {
	cause := errors.New("daemon shutting down")
	stopped := &git.CancelledExitError{ExitError: killedProcessExit(t)}
	err := &stepExecutionError{step: "document", message: "git rev-parse: signal: terminated", cause: stopped}
	if !cancellationAttributable(err, cause) {
		t.Fatalf("a subprocess the context stopped was not attributed to the cancellation: %v", err)
	}
	transport := &git.CommandError{
		Command: "rev-parse refs/heads/main^{commit}",
		Stderr:  "signal: terminated",
		Err:     errors.Join(stopped, context.Canceled),
	}
	if !cancellationAttributable(transport, cause) {
		t.Fatalf("the marker was not read through git's transport wrapper: %v", transport)
	}
}

// killedProcessExit runs a process that is killed by a signal and returns its
// exit error, which is what a subprocess stopped mid-flight reports.
func killedProcessExit(t *testing.T) *exec.ExitError {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("killed process did not report an exit: %v", err)
	}
	return exit
}
