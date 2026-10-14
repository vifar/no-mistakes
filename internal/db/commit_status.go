package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CommitStatusPublication binds reporting to a confirmed publication by this
// run. Host and Repo contain no credentials; LastState is the last successful POST.
type CommitStatusPublication struct {
	SHA, Host, Repo, LastState string
	Validated, CIEligible      bool
	// AdoptedOnly proves this run observed existing code without pushing it.
	// Legacy/unknown publications default to false. Actual publication is sticky.
	AdoptedOnly       bool
	StrictCertificate *string
	PreserveSuccess   *bool
}

// CommitStatusAttempt records a push or adoption target. An attempt is not
// publication proof until its exact target ref is observed at SHA.
type CommitStatusAttempt struct {
	CommitStatusPublication
	Target, Ref string
}

func (d *DB) PutCommitStatusAttempt(runID string, p CommitStatusAttempt) error {
	_, err := sqlitewrite.Exec(context.Background(), d.sql, "PutCommitStatusAttempt", `INSERT INTO commit_status_attempts (run_id,sha,host,repo,target,ref,validated,ci_eligible,adopted_only,retired)
		VALUES (?,?,?,?,?,?,?,?,?,EXISTS (
			SELECT 1 FROM commit_status_publications p JOIN runs owner ON owner.id=p.run_id JOIN runs current ON current.id=?
			WHERE p.host=? AND p.repo=? AND p.sha=? AND
			(owner.created_at>current.created_at OR (owner.created_at=current.created_at AND owner.id>current.id) OR (owner.id=current.id AND p.retired=1))
		)) ON CONFLICT(run_id) DO UPDATE SET sha=excluded.sha,host=excluded.host,repo=excluded.repo,target=excluded.target,ref=excluded.ref,validated=excluded.validated,ci_eligible=excluded.ci_eligible,adopted_only=commit_status_attempts.adopted_only AND excluded.adopted_only,
		retired=excluded.retired`, runID, p.SHA, p.Host, p.Repo, p.Target, p.Ref, p.Validated, p.CIEligible, p.AdoptedOnly, runID, p.Host, p.Repo, p.SHA)
	return err
}

func (d *DB) GetCommitStatusAttempt(runID string) (*CommitStatusAttempt, error) {
	p := &CommitStatusAttempt{}
	err := d.sql.QueryRow(`SELECT sha,host,repo,target,ref,validated,ci_eligible,adopted_only FROM commit_status_attempts WHERE run_id=? AND retired=0`, runID).Scan(&p.SHA, &p.Host, &p.Repo, &p.Target, &p.Ref, &p.Validated, &p.CIEligible, &p.AdoptedOnly)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (d *DB) ConfirmCommitStatusAttempt(runID string, p CommitStatusPublication) (bool, error) {
	return d.putCommitStatusPublication(runID, p, true)
}

func (d *DB) PutCommitStatusPublication(runID string, p CommitStatusPublication) error {
	_, err := d.putCommitStatusPublication(runID, p, false)
	return err
}

func (d *DB) putCommitStatusPublication(runID string, p CommitStatusPublication, confirm bool) (bool, error) {
	return sqlitewrite.Value(context.Background(), "putCommitStatusPublication", func(writeCtx context.Context) (bool, error) {
		return d.putCommitStatusPublicationWriteAttempt(writeCtx, runID, p, confirm)
	})
}

func (d *DB) putCommitStatusPublicationWriteAttempt(writeCtx context.Context, runID string, p CommitStatusPublication, confirm bool) (bool, error) {
	tx, err := d.sql.BeginTx(writeCtx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if p.AdoptedOnly {
		// An uncertain/actual push attempt cannot be relabeled by a fresh
		// attach observation, including one racing with confirmation.
		var pushed bool
		if err := tx.QueryRowContext(writeCtx, `SELECT EXISTS (SELECT 1 FROM commit_status_attempts WHERE run_id=? AND adopted_only IS NOT 1)`, runID).Scan(&pushed); err != nil {
			return false, err
		}
		p.AdoptedOnly = !pushed
	}
	var status types.RunStatus
	if err := tx.QueryRowContext(writeCtx, `SELECT status FROM runs WHERE id=?`, runID).Scan(&status); err != nil {
		return false, err
	}
	if status == types.RunCancelled {
		var preserved bool
		if err := tx.QueryRowContext(writeCtx, `SELECT EXISTS(SELECT 1 FROM commit_status_publications WHERE run_id=? AND host=? AND repo=? AND sha=? AND retired=0 AND preserve_success=1 AND strict_certificate IS NOT NULL AND last_state='success')`, runID, p.Host, p.Repo, p.SHA).Scan(&preserved); err != nil {
			return false, err
		}
		if preserved {
			if confirm {
				if _, err := tx.ExecContext(writeCtx, `DELETE FROM commit_status_attempts WHERE run_id=? AND retired=0`, runID); err != nil {
					return false, err
				}
			}
			return true, tx.Commit()
		}
	}
	revoked, err := rawShutdownSuperseded(tx, runID)
	if err != nil {
		return false, err
	}
	if revoked && (p.StrictCertificate != nil || (p.LastState != "" && p.LastState != "failure")) {
		return false, fmt.Errorf("raw shutdown forbids status promotion")
	}
	if (status == types.RunFailed || status == types.RunCancelled || status == types.RunCIMonitorInterrupted) && p.LastState != "" && p.LastState != "failure" {
		return false, fmt.Errorf("terminal outcome forbids %s status", p.LastState)
	}
	if status == types.RunCompleted && p.LastState != "" {
		expected := "failure"
		if p.Validated {
			expected = "success"
		}
		if p.LastState != expected {
			return false, fmt.Errorf("completed outcome requires %s status", expected)
		}
	}
	if p.StrictCertificate != nil {
		var claimed StatusCertificate
		if err := json.Unmarshal([]byte(*p.StrictCertificate), &claimed); err != nil {
			return false, err
		}
		actual, err := buildStatusCertificate(tx, runID, p.SHA, claimed.ConfigDigest)
		if err != nil {
			return false, err
		}
		if actual == nil || *actual != *p.StrictCertificate {
			p.StrictCertificate = nil
			p.PreserveSuccess = nil
		}
	}
	// Reconfirmation updates validation evidence, not the last successful POST.
	// In particular, an up-to-date Push must preserve attach-time deduplication.
	// Preservation proof is still replaced from the caller, so reconfirmation can
	// only revoke eligibility, never carry it to a publication it did not certify.
	if _, err := tx.ExecContext(writeCtx, `INSERT INTO commit_status_publications (run_id,sha,host,repo,last_state,validated,ci_eligible,adopted_only,strict_certificate,preserve_success)
		VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(host,repo,sha,run_id) DO UPDATE SET last_state=CASE WHEN ? THEN commit_status_publications.last_state ELSE excluded.last_state END,validated=excluded.validated,ci_eligible=excluded.ci_eligible,adopted_only=commit_status_publications.adopted_only AND excluded.adopted_only,strict_certificate=excluded.strict_certificate,preserve_success=excluded.preserve_success`, runID, p.SHA, p.Host, p.Repo, p.LastState, p.Validated, p.CIEligible, p.AdoptedOnly, p.StrictCertificate, p.PreserveSuccess, confirm); err != nil {
		return false, err
	}
	for _, table := range []string{"commit_status_attempts", "commit_status_publications"} {
		if _, err = tx.ExecContext(writeCtx, `UPDATE `+table+` SET retired=1 WHERE host=? AND repo=? AND sha=? AND run_id IN (
			SELECT older.id FROM runs older JOIN runs newer
			ON newer.created_at>older.created_at OR (newer.created_at=older.created_at AND newer.id>older.id)
			JOIN commit_status_publications owner ON owner.run_id=newer.id
			WHERE owner.host=? AND owner.repo=? AND owner.sha=?
		)`, p.Host, p.Repo, p.SHA, p.Host, p.Repo, p.SHA); err != nil {
			return false, err
		}
	}
	if confirm {
		if _, err = tx.ExecContext(writeCtx, `DELETE FROM commit_status_attempts WHERE run_id=? AND retired=0`, runID); err != nil {
			return false, err
		}
	}
	var retired bool
	if err := tx.QueryRowContext(writeCtx, `SELECT retired FROM commit_status_publications WHERE run_id=? AND host=? AND repo=? AND sha=?`, runID, p.Host, p.Repo, p.SHA).Scan(&retired); err != nil {
		return false, err
	}
	return !retired, tx.Commit()
}

func (d *DB) GetCommitStatusPublication(ctx context.Context, runID string, p CommitStatusPublication) (*CommitStatusPublication, error) {
	current := &CommitStatusPublication{}
	err := d.sql.QueryRowContext(ctx, `SELECT sha,host,repo,last_state,validated,ci_eligible,adopted_only,strict_certificate,preserve_success FROM commit_status_publications WHERE run_id=? AND host=? AND repo=? AND sha=? AND retired=0`, runID, p.Host, p.Repo, p.SHA).Scan(&current.SHA, &current.Host, &current.Repo, &current.LastState, &current.Validated, &current.CIEligible, &current.AdoptedOnly, &current.StrictCertificate, &current.PreserveSuccess)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return current, err
}

func (d *DB) GetCommitStatusPublications(ctx context.Context, runID string) ([]CommitStatusPublication, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT sha,host,repo,last_state,validated,ci_eligible,adopted_only,strict_certificate,preserve_success FROM commit_status_publications WHERE run_id=? AND retired=0 ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var publications []CommitStatusPublication
	for rows.Next() {
		var p CommitStatusPublication
		if err := rows.Scan(&p.SHA, &p.Host, &p.Repo, &p.LastState, &p.Validated, &p.CIEligible, &p.AdoptedOnly, &p.StrictCertificate, &p.PreserveSuccess); err != nil {
			return nil, err
		}
		publications = append(publications, p)
	}
	return publications, rows.Err()
}

// CommitStatusRecoveryRunIDs includes terminal runs whose durable outcome has
// not reached the forge. Selection shares the reporting deadline.
func (d *DB) CommitStatusRecoveryRunIDs(ctx context.Context) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT r.id FROM runs r LEFT JOIN commit_status_publications p ON p.run_id=r.id AND p.retired=0 LEFT JOIN commit_status_attempts a ON a.run_id=r.id AND a.retired=0 WHERE r.status IN ('completed','failed','cancelled','ci_monitor_interrupted') AND (a.run_id IS NOT NULL OR (p.run_id IS NOT NULL AND p.last_state != CASE WHEN r.status='completed' AND p.validated=1 THEN 'success' WHEN r.status='cancelled' AND p.preserve_success=1 AND p.strict_certificate IS NOT NULL THEN 'success' ELSE 'failure' END)) ORDER BY r.created_at,r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
