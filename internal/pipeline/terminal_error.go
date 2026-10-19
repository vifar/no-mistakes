package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Rendering stays redacted while cancellation provenance remains inspectable.
type stepExecutionError struct {
	step      types.StepName
	message   string
	cause     error
	watchOnly bool
}

type ControlledShutdownCause struct {
	Instance  string
	Lifecycle string
}

func (*ControlledShutdownCause) Error() string { return "cancelled: planned daemon shutdown" }

// Is lets planned shutdown reuse the daemon-shutdown recovery paths for parked
// gates and CI monitors. Certified watchers are routed separately to terminal
// cancellation before the CI-monitor recovery path is chosen.
func (*ControlledShutdownCause) Is(target error) bool { return target == ErrDaemonShutdown }

func (e *Executor) SetRuntimeInstance(instance string) { e.runtimeInstance = instance }

func (e *stepExecutionError) Error() string {
	return fmt.Sprintf("step %s failed: %s", e.step, e.message)
}
func (e *stepExecutionError) Unwrap() error { return e.cause }

func CancellationOnly(err, cause error) bool {
	if err == nil || cause == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !CancellationOnly(child, cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return CancellationOnly(wrapped.Unwrap(), cause)
	}
	return err == context.Canceled || err == cause
}

func shutdownCancellationOnly(err error, ctxs ...context.Context) bool {
	for _, cause := range []error{ErrDaemonShutdown, ErrParkedAtShutdown} {
		if errors.Is(err, cause) && CancellationOnly(err, cause) {
			return true
		}
	}
	for _, ctx := range ctxs {
		if ctx != nil {
			cause := context.Cause(ctx)
			if errors.Is(cause, ErrDaemonShutdown) && CancellationOnly(err, cause) {
				return true
			}
		}
	}
	return false
}

func watcherCancellation(err error) bool {
	var stepErr *stepExecutionError
	return errors.As(err, &stepErr) && stepErr.watchOnly
}

type terminalPreparationError struct{ cause error }

func (e *terminalPreparationError) Error() string { return safeurl.RedactText(e.cause.Error()) }
func (e *terminalPreparationError) Unwrap() error { return e.cause }
