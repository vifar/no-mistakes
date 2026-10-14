package git

type CommandCancellationError struct {
	cause        error
	cancellation error
}

func (e *CommandCancellationError) Error() string        { return e.cancellation.Error() }
func (e *CommandCancellationError) Unwrap() error        { return e.cause }
func (e *CommandCancellationError) Is(target error) bool { return target == e.cancellation }
