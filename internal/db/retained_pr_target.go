package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// RecordRetainedPRTarget records the live target verified for a qualified
// published head. The immutable comparison and its generation do not move.
func (d *DB) RecordRetainedPRTarget(runID string, receipt *PRContext, previousTarget, target string) error {
	if receipt == nil || !validGitSHA(target) || previousTarget != "" && !validGitSHA(previousTarget) {
		return fmt.Errorf("retain qualified PR target: invalid observation")
	}
	result, err := d.sql.Exec(`UPDATE run_pr_contexts SET retained_target_sha=?
		WHERE run_id=? AND generation=? AND local_head_sha=? AND target_sha=? AND COALESCE(retained_target_sha, '')=?
		AND EXISTS (SELECT 1 FROM runs r WHERE r.id=run_id
		 AND r.head_sha=local_head_sha AND r.last_pushed_sha=local_head_sha
		 AND r.review_approved_head_sha=local_head_sha
		 AND (ci_qualified=1 OR r.ci_ready_at IS NOT NULL OR EXISTS
		  (SELECT 1 FROM commit_status_publications p WHERE p.run_id=r.id
		   AND p.sha=local_head_sha AND p.validated=1 AND p.last_state='success')))`,
		target, runID, receipt.Generation, receipt.LocalHeadSHA, receipt.TargetSHA, previousTarget)
	if err != nil {
		return fmt.Errorf("retain qualified PR target: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("retain qualified PR target: comparison, retained target, or publication binding changed")
	}
	return nil
}

func (d *DB) GetRetainedPRTarget(runID string, receipt *PRContext) (string, error) {
	if receipt == nil {
		return "", nil
	}
	var target sql.NullString
	err := d.sql.QueryRow(`SELECT retained_target_sha FROM run_pr_contexts
		WHERE run_id=? AND generation=? AND local_head_sha=? AND target_sha=?`,
		runID, receipt.Generation, receipt.LocalHeadSHA, receipt.TargetSHA).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read retained PR target: %w", err)
	}
	return target.String, nil
}
