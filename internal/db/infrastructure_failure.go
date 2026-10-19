package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FailRunInfrastructure atomically settles active step state with the named run
// outcome. A failed completion/status write must not strand a running Push or
// gate after the run stops. verifiedHead is empty when HEAD could not be proved.
func (d *DB) FailRunInfrastructure(runID, message, verifiedHead string) error {
	if !sqlitewrite.RecordedContention(message) {
		return fmt.Errorf("infrastructure outcome requires a named SQLite write error")
	}
	return sqlitewrite.Transaction(context.Background(), d.sql, "settle infrastructure failure", func(ctx context.Context, tx *sql.Tx) error {
		ts := now()
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET error=?, status=?, push_active=0,
   parked_ms=COALESCE(parked_ms, 0) + CASE
     WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since
     THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
   awaiting_agent_since=NULL,
   head_sha=CASE WHEN ?='' THEN head_sha ELSE ? END,
   terminal_head_verified_at=CASE WHEN ?='' THEN NULL ELSE ? END, updated_at=? WHERE id=?`,
			message, types.RunFailed, ts, ts, verifiedHead, verifiedHead, verifiedHead, ts, ts, runID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE step_results SET status=?, error=?, completed_at=?,
   last_activity_at=?, last_activity=?, agent_pid=NULL, pending_response=NULL
   WHERE run_id=? AND status IN (?,?,?,?)`, types.StepStatusFailed, message, ts, ts,
			"infrastructure failure", runID, types.StepStatusRunning, types.StepStatusFixing,
			types.StepStatusAwaitingApproval, types.StepStatusFixReview)
		return err
	})
}
