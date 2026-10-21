package db

import (
	"context"
	"database/sql"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const terminalStepInterruptedActivity = "terminal run: interrupted step"

// InterruptedByTerminalRun distinguishes an administratively closed row from a
// step that itself failed. In particular, interruption must not invent a
// Document refusal that changes the existing worktree preservation policy.
func (s *StepResult) InterruptedByTerminalRun() bool {
	return s != nil && s.Status == types.StepStatusFailed && s.LastActivity != nil && *s.LastActivity == terminalStepInterruptedActivity
}

// closeExecutingSteps is used only after the executor has stopped. Keep the
// run outcome and its executing rows atomic; pending and finished rows retain
// their evidence. Failed means interrupted, never successfully validated.
func closeExecutingSteps(ctx context.Context, tx *sql.Tx, runID, reason string, ts int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE step_results SET status='failed', error=?,
 completed_at=?, last_activity_at=?, last_activity=?, agent_pid=NULL, pending_response=NULL
 WHERE run_id=? AND status IN ('running','fixing','awaiting_approval','fix_review')`,
		reason, ts, ts, terminalStepInterruptedActivity, runID)
	return err
}

// TerminalExecutingSteps finds legacy rows left executing after their run ended.
// No active run is a candidate, including non-native steps with no agent PID.
// Certified watcher cancellation keeps its existing proof-owned step semantics.
func (d *DB) TerminalExecutingSteps() ([]*StepResult, error) {
	rows, err := d.sql.Query(`SELECT s.id FROM step_results s JOIN runs r ON r.id=s.run_id
 WHERE r.status IN ('completed','failed','cancelled','ci_monitor_interrupted')
 AND s.status IN ('running','fixing','awaiting_approval','fix_review')
 AND NOT EXISTS (SELECT 1 FROM commit_status_publications p WHERE p.run_id=r.id
 AND p.preserve_success=1 AND p.strict_certificate IS NOT NULL AND p.last_state='success')`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var steps []*StepResult
	for _, id := range ids {
		step, err := d.GetStepResult(id)
		if err != nil {
			return nil, err
		}
		if step != nil {
			steps = append(steps, step)
		}
	}
	return steps, nil
}

// CloseTerminalStep conditionally closes the inspected row. A restarted row,
// new activity, changed PID or revived run invalidates the observation.
// The caller must first exclude executor ownership and a live/unknown process.
func (d *DB) CloseTerminalStep(step *StepResult, ts int64) (bool, error) {
	changed := false
	err := sqlitewrite.Transaction(context.Background(), d.sql, "CloseTerminalStep", func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE step_results SET status='failed', error='terminal run: interrupted step',
 completed_at=?, last_activity_at=?, last_activity='terminal run: interrupted step', agent_pid=NULL, pending_response=NULL
 WHERE id=? AND run_id=? AND status=? AND status IN ('running','fixing','awaiting_approval','fix_review')
 AND agent_pid IS ? AND last_activity_at IS ? AND started_at IS ? AND completed_at IS ?
 AND EXISTS (SELECT 1 FROM runs WHERE id=? AND status IN ('completed','failed','cancelled','ci_monitor_interrupted'))
 AND NOT EXISTS (SELECT 1 FROM commit_status_publications p WHERE p.run_id=step_results.run_id
 AND p.preserve_success=1 AND p.strict_certificate IS NOT NULL AND p.last_state='success')`,
			ts, ts, step.ID, step.RunID, step.Status, step.AgentPID, step.LastActivityAt, step.StartedAt, step.CompletedAt, step.RunID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = n == 1
		if changed {
			_, err = tx.ExecContext(ctx, `UPDATE runs SET push_active=0 WHERE id=? AND NOT EXISTS
 (SELECT 1 FROM step_results WHERE run_id=? AND step_name='push' AND status IN ('running','fixing'))`, step.RunID, step.RunID)
		}
		return err
	})
	return changed, err
}
