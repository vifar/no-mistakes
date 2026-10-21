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

// cancellationAttributable reports whether a run's failure IS the
// cancellation, and so must be recorded as the cancellation's reason.
//
// The rule is deliberately narrow. A step that was cut off mid-flight
// reports the failure it was in the middle of - a terminated git command, a
// closed pipe - and that failure is the step's own: the operation it was
// doing did not succeed. A shutdown that arrives while a read is in flight
// does not make the read succeed, so the read keeps its own outcome.
//
// What IS decoration is a wrapper that adds a MESSAGE around the
// cancellation. A step that reports "step document failed: daemon shutting
// down" says the same thing the cancellation says, with a prefix. So the
// question is asked of the error's own cause: a wrapper is asked of what it
// wraps, a joined error is attributable only when EVERY part of it is, and a
// leaf is the cancellation only if it IS the cancellation.
//
// An error with no cancellation anywhere in its chain is an independent
// failure and keeps its own reason. An error that joins an independent
// failure to the cancellation is also independent: the failure would have
// happened anyway, and the cancellation only arrived alongside it.
func cancellationAttributable(err, cause error) bool {
	if err == nil || cause == nil {
		return false
	}
	if CancellationOnly(err, cause) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		// A joined error is attributable only when EVERY part of it is. One
		// independent part among the cancellations makes the whole thing an
		// independent failure.
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cancellationAttributable(child, cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		// A wrapper adds a message around the real cause. What matters is
		// whether the cause it wraps is the cancellation, so the question
		// is asked of the inner error.
		inner := wrapped.Unwrap()
		if inner == nil {
			return false
		}
		return cancellationAttributable(inner, cause)
	}
	// A leaf: it is the cancellation only if it IS the cancellation.
	return err == cause || err == context.Canceled
}

func watcherCancellation(err error) bool {
	var stepErr *stepExecutionError
	return errors.As(err, &stepErr) && stepErr.watchOnly
}

type terminalPreparationError struct{ cause error }

func (e *terminalPreparationError) Error() string { return safeurl.RedactText(e.cause.Error()) }
func (e *terminalPreparationError) Unwrap() error { return e.cause }
