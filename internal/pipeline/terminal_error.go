package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/git"
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

// cancellationAttributable reports whether a run's failure is the
// cancellation itself, and so must be recorded as the cancellation's reason.
//
// A step that was cut off mid-flight reports the failure it was in the middle
// of - a terminated git command, a closed pipe - and that message is
// decoration around the cancellation, not an independent failure. The
// cancellation is the cause; the step's text only says what the process
// happened to be doing when it was stopped. A cancelled subprocess reports
// context.Canceled in its own chain, so the chain still carries the
// cancellation even though the step added its own message on top.
//
// An error with NO cancellation anywhere in its chain is an independent
// failure and keeps its own reason: a shutdown must never overwrite a failure
// that would have happened anyway. An error that joins an independent failure
// to the cancellation is also independent: the failure would have happened
// anyway, and the cancellation only arrived alongside it.
func cancellationAttributable(err, cause error) bool {
	if err == nil || cause == nil {
		return false
	}
	if CancellationOnly(err, cause) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		// A joined error is attributable when the cancellation is among its
		// parts and every OTHER part is process noise rather than an
		// independent failure. A subprocess that was killed by the
		// cancellation reports its exit alongside the cancellation, and that
		// exit is what the cancellation caused, not a failure that would have
		// happened anyway.
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		cancelled := false
		for _, child := range children {
			if isCancellationLeaf(child, cause) {
				cancelled = true
				continue
			}
			if isProcessNoise(child) {
				continue
			}
			// A child that is ITSELF attributable carries the cancellation
			// too, even when the cancellation is deeper in its chain - a
			// cleanup path that joins a cancelled command error onto a
			// successful cleanup still reports the cancellation. The join
			// must see that, or the enclosing join would reject an error
			// that is purely the cancellation.
			if cancellationAttributable(child, cause) {
				cancelled = true
				continue
			}
			return false
		}
		return cancelled
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

// isCancellationLeaf reports whether an error is the cancellation itself.
func isCancellationLeaf(err, cause error) bool {
	if err == nil {
		return false
	}
	return err == cause || err == context.Canceled
}

// isProcessNoise reports whether an error is a subprocess's exit rather than a
// failure of its own. A command CANCELLED by the context reports its exit
// alongside the cancellation, and that exit says what the process was doing
// when it was stopped, not that it failed on its own.
//
// The provenance comes from the execution boundary, not from the shape of the
// error: a git command records that its own context cancelled it. A signal
// alone does not prove it - a child can die of SIGSEGV on its own and then
// race a shutdown that happened to observe the context afterwards - so a
// signalled exit whose provenance is unknown keeps its own outcome.
func isProcessNoise(err error) bool {
	if err == nil {
		return false
	}
	if git.CancelledByContext(err) {
		return true
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return processWasSignaled(exit)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		// Every part must be noise: one independent part among the exits
		// makes the whole thing an independent failure.
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isProcessNoise(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		inner := wrapped.Unwrap()
		if inner == nil {
			return false
		}
		return isProcessNoise(inner)
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
