package pipeline

import (
	"context"
	"errors"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RunCancelCause keeps the existing cancellation literal intact: classification,
// preservation and status policy still use that literal. Audit facts are separate.
type RunCancelCause struct {
	Reason string
	Event  *types.RunEndingEvent
}

func (c *RunCancelCause) Error() string { return c.Reason }

// RecordCancellationAttribution runs AFTER the stop signal, in the same retry
// loop as terminal settlement. Storage failure must never delay signalling work.
func RecordCancellationAttribution(d *db.DB, runID string, ctxs ...context.Context) error {
	for _, ctx := range ctxs {
		if ctx == nil {
			continue
		}
		var cause *RunCancelCause
		if errors.As(context.Cause(ctx), &cause) {
			return d.RecordRunEnding(runID, cause.Event)
		}
	}
	return nil
}
