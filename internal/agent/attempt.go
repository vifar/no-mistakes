package agent

import "errors"

// ErrAttemptEnded marks a failure where the invocation's own bound ended the
// attempt - the silent budget, a still-working cap, a caller's Review/Test
// budget, or the progress bound - rather than the agent reaching a verdict.
//
// It exists because a caller that owns the bound also owns recovery from it. A
// session wrapper, for instance, retries a failed RESUME cold; a turn cut by its
// budget would be retried the same way, which both repeats work already done and
// spends the invocation twice. The marker tells it to stop instead.
var ErrAttemptEnded = errors.New("attempt ended by its invocation bound")

// MarkAttemptEnded tags err as ended by the invocation's own bound. It is a
// no-op for a nil or already-tagged error.
func MarkAttemptEnded(err error) error {
	if err == nil || errors.Is(err, ErrAttemptEnded) {
		return err
	}
	return &attemptEndedError{err: err}
}

// AttemptEnded reports whether err came from an attempt the invocation bound
// ended.
func AttemptEnded(err error) bool {
	return errors.Is(err, ErrAttemptEnded)
}

type attemptEndedError struct{ err error }

func (e *attemptEndedError) Error() string { return e.err.Error() }

func (e *attemptEndedError) Unwrap() []error { return []error{e.err, ErrAttemptEnded} }
