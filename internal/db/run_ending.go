package db

import (
	"context"
	"encoding/json"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RecordRunEnding preserves the first effective request; retries and duplicate
// cancels must never replace the actor that actually signalled the context.
func (d *DB) RecordRunEnding(runID string, event *types.RunEndingEvent) error {
	if event == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = sqlitewrite.Exec(context.Background(), d.sql, "RecordRunEnding", `UPDATE runs SET ending_event=COALESCE(ending_event,?) WHERE id=?`, string(payload), runID)
	return err
}
