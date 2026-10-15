package db

import (
	"context"
	"database/sql"
)

// CommitStatusPublication binds reporting to a confirmed publication by this
// run. Host and Repo contain no credentials; LastState is the last successful POST.
type CommitStatusPublication struct {
	SHA, Host, Repo, LastState string
	Validated, CIEligible      bool
}

// CommitStatusAttempt records a push or adoption target. An attempt is not
// publication proof until its exact target ref is observed at SHA.
type CommitStatusAttempt struct {
	CommitStatusPublication
	Target, Ref string
}

func (d *DB) PutCommitStatusAttempt(runID string, p CommitStatusAttempt) error {
	_, err := d.sql.Exec(`INSERT INTO commit_status_attempts (run_id,sha,host,repo,target,ref,validated,ci_eligible,retired)
		VALUES (?,?,?,?,?,?,?,?,EXISTS (
			SELECT 1 FROM commit_status_publications p JOIN runs owner ON owner.id=p.run_id JOIN runs current ON current.id=?
			WHERE p.host=? AND p.repo=? AND p.sha=? AND
			(owner.created_at>current.created_at OR (owner.created_at=current.created_at AND owner.id>current.id) OR (owner.id=current.id AND p.retired=1))
		)) ON CONFLICT(run_id) DO UPDATE SET sha=excluded.sha,host=excluded.host,repo=excluded.repo,target=excluded.target,ref=excluded.ref,validated=excluded.validated,ci_eligible=excluded.ci_eligible,
		retired=excluded.retired`, runID, p.SHA, p.Host, p.Repo, p.Target, p.Ref, p.Validated, p.CIEligible, runID, p.Host, p.Repo, p.SHA)
	return err
}

func (d *DB) GetCommitStatusAttempt(runID string) (*CommitStatusAttempt, error) {
	p := &CommitStatusAttempt{}
	err := d.sql.QueryRow(`SELECT sha,host,repo,target,ref,validated,ci_eligible FROM commit_status_attempts WHERE run_id=? AND retired=0`, runID).Scan(&p.SHA, &p.Host, &p.Repo, &p.Target, &p.Ref, &p.Validated, &p.CIEligible)
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
	tx, err := d.sql.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Reconfirmation updates validation evidence, not the last successful POST.
	// In particular, an up-to-date Push must preserve attach-time deduplication.
	if _, err := tx.Exec(`INSERT INTO commit_status_publications (run_id,sha,host,repo,last_state,validated,ci_eligible)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(host,repo,sha,run_id) DO UPDATE SET last_state=CASE WHEN ? THEN commit_status_publications.last_state ELSE excluded.last_state END,validated=excluded.validated,ci_eligible=excluded.ci_eligible`, runID, p.SHA, p.Host, p.Repo, p.LastState, p.Validated, p.CIEligible, confirm); err != nil {
		return false, err
	}
	for _, table := range []string{"commit_status_attempts", "commit_status_publications"} {
		if _, err = tx.Exec(`UPDATE `+table+` SET retired=1 WHERE host=? AND repo=? AND sha=? AND run_id IN (
			SELECT older.id FROM runs older JOIN runs newer
			ON newer.created_at>older.created_at OR (newer.created_at=older.created_at AND newer.id>older.id)
			JOIN commit_status_publications owner ON owner.run_id=newer.id
			WHERE owner.host=? AND owner.repo=? AND owner.sha=?
		)`, p.Host, p.Repo, p.SHA, p.Host, p.Repo, p.SHA); err != nil {
			return false, err
		}
	}
	if confirm {
		if _, err = tx.Exec(`DELETE FROM commit_status_attempts WHERE run_id=? AND retired=0`, runID); err != nil {
			return false, err
		}
	}
	var retired bool
	if err := tx.QueryRow(`SELECT retired FROM commit_status_publications WHERE run_id=? AND host=? AND repo=? AND sha=?`, runID, p.Host, p.Repo, p.SHA).Scan(&retired); err != nil {
		return false, err
	}
	return !retired, tx.Commit()
}

func (d *DB) GetCommitStatusPublication(ctx context.Context, runID string, p CommitStatusPublication) (*CommitStatusPublication, error) {
	current := &CommitStatusPublication{}
	err := d.sql.QueryRowContext(ctx, `SELECT sha,host,repo,last_state,validated,ci_eligible FROM commit_status_publications WHERE run_id=? AND host=? AND repo=? AND sha=? AND retired=0`, runID, p.Host, p.Repo, p.SHA).Scan(&current.SHA, &current.Host, &current.Repo, &current.LastState, &current.Validated, &current.CIEligible)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return current, err
}

func (d *DB) GetCommitStatusPublications(ctx context.Context, runID string) ([]CommitStatusPublication, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT sha,host,repo,last_state,validated,ci_eligible FROM commit_status_publications WHERE run_id=? AND retired=0 ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var publications []CommitStatusPublication
	for rows.Next() {
		var p CommitStatusPublication
		if err := rows.Scan(&p.SHA, &p.Host, &p.Repo, &p.LastState, &p.Validated, &p.CIEligible); err != nil {
			return nil, err
		}
		publications = append(publications, p)
	}
	return publications, rows.Err()
}

// CommitStatusRecoveryRunIDs includes terminal runs whose durable outcome has
// not reached the forge. Selection shares the reporting deadline.
func (d *DB) CommitStatusRecoveryRunIDs(ctx context.Context) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT r.id FROM runs r LEFT JOIN commit_status_publications p ON p.run_id=r.id AND p.retired=0 LEFT JOIN commit_status_attempts a ON a.run_id=r.id AND a.retired=0 WHERE r.status IN ('completed','failed','cancelled') AND (a.run_id IS NOT NULL OR (p.run_id IS NOT NULL AND p.last_state != CASE WHEN r.status='completed' AND p.validated=1 THEN 'success' ELSE 'failure' END)) ORDER BY r.created_at,r.id`)
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
