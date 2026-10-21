package pipeline

import (
	"context"
	"errors"
	"fmt"

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

// MeaningfulFailure marks an error that adds MEANING around a cause, not just
// a message. A wrapper that only adds a message - which command ran, what
// its stderr said - is transparent: the question is asked of the cause it
// wraps. A wrapper that adds meaning says something the cause does not, so it
// IS the failure: the shipping layer's "the remote observation failed" says
// the read produced no result, which is a failure in its own right even when
// the cancellation stopped the process.
//
// The type is declared here and implemented by the packages that own such a
// wrapper, so the pipeline does not have to know them.
type MeaningfulFailure interface {
	error
	// AddsMeaning reports that this error is more than a message about the
	// cause it wraps.
	AddsMeaning() bool
}

// CancellationAttributable reports whether a run's failure IS the
// cancellation, and so must be recorded as the cancellation's reason.
//
// The rule is deliberately narrow. A step that was cut off mid-flight
// reports the failure it was in the middle of - a terminated git command, a
// closed pipe - and that failure is the step's own UNLESS the cancellation
// is what stopped the work. A shutdown that arrives while a read is in
// flight does not make the read succeed, so the read keeps its own
// outcome; a read whose subprocess this command's own context stopped IS the
// cancellation, and the git package records that provenance at the
// execution boundary, where it is known.
//
// What is otherwise decoration is a wrapper that adds a MESSAGE around the
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
func CancellationAttributable(err, cause error) bool {
	if err == nil || cause == nil {
		return false
	}
	if CancellationOnly(err, cause) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		// A joined error is attributable only when EVERY part of it is. One
		// independent part among the cancellations makes the whole thing an
		// independent failure, so the join is inspected BEFORE anything is
		// looked for through it.
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			// A stopped child's exit is the cancellation's own report of
			// the stop, and the report is a MESSAGE: it says the child
			// died of a signal, and the context's own error already says
			// the cancellation. It is not a part of its own that has to
			// BE the cancellation.
			if marked, ok := child.(*git.CancelledExitError); ok {
				if marked.ExitError == nil && marked.Refused == nil {
					return false
				}
				continue
			}
			if !CancellationAttributable(child, cause) {
				return false
			}
		}
		return true
	}
	// The pipeline's own report of a step that failed is a MESSAGE around the
	// step's error, so the question is asked of what it wraps. Every OTHER
	// wrapper is a MEANING: the shipping layer's "the remote observation
	// failed" says the read did not produce a result, which is a failure in
	// its own right even when the cancellation stopped the process. A wrapper
	// that adds meaning is never transparent.
	var stepErr *stepExecutionError
	if errors.As(err, &stepErr) {
		inner := stepErr.Unwrap()
		if inner == nil {
			return false
		}
		return CancellationAttributable(inner, cause)
	}
	// A wrapper that says something the cause does not is the failure, and
	// it is decided FIRST: a meaningful wrapper in between means the failure
	// is that wrapper's, so nothing underneath it speaks for the whole error.
	var meaningful MeaningfulFailure
	if errors.As(err, &meaningful) && meaningful.AddsMeaning() {
		return false
	}
	// A subprocess this command's own context stopped is the cancellation's
	// doing, and the git package records that provenance at the execution
	// boundary, where it is known. The marker is the AUDIT of who stopped
	// the work.
	//
	// The stopped child's exit is a MESSAGE about the stop, not a
	// MEANING: the child reports the signal it died of, and that report
	// says nothing the context's own error does not already say. So the
	// marker is read THROUGH, and the question is asked of what it carries.
	// A joined error is attributable only when EVERY part of it is, and
	// the stopped child's exit is not the cancellation - it is the
	// cancellation's own report of the stop.
	//
	// The marker is read where it IS, not wherever it is found. The git
	// package's own transport wrapper is the exception: it adds the command
	// and the captured stderr, which is a MESSAGE, so the marker is read
	// through it.
	var commandErr *git.CommandError
	if errors.As(err, &commandErr) {
		inner := commandErr.Unwrap()
		if inner == nil {
			return false
		}
		return CancellationAttributable(inner, cause)
	}
	if _, ok := err.(*git.CancelledExitError); ok {
		// The marker on its own, with nothing around it. It says the
		// context stopped the command; it does not say the failure IS
		// the cancellation.
		return err == cause || err == context.Canceled
	}
	if _, ok := err.(interface{ Unwrap() error }); ok {
		// Any other wrapper adds MEANING around the real cause. So the
		// wrapper is the answer, not a message to look through.
		return false
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
