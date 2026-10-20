package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const RawShutdownReason = "daemon shutting down"

type StatusCertificate struct {
	SHA            string `json:"sha"`
	Epoch          int64  `json:"epoch"`
	PushGeneration int64  `json:"push_generation"`
	ConfigDigest   string `json:"config_digest"`
	ProofDigest    string `json:"proof_digest"`
}

type statusQuery interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func statusEpoch(q statusQuery, runID string) (int64, error) {
	var epoch int64
	err := q.QueryRow(`SELECT COALESCE((SELECT epoch FROM status_validation_epochs WHERE run_id=?),0)`, runID).Scan(&epoch)
	return epoch, err
}

func (d *DB) StatusValidationEpoch(runID string) (int64, error) { return statusEpoch(d.sql, runID) }

func invalidateStatusValidation(ctx context.Context, tx *sql.Tx, runID string) error {
	for _, query := range []string{
		`INSERT INTO status_validation_epochs(run_id,epoch) VALUES(?,1) ON CONFLICT(run_id) DO UPDATE SET epoch=epoch+1`,
		`UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=?`,
		`DELETE FROM status_validation_steps WHERE step_id IN (SELECT id FROM step_results WHERE run_id=?)`,
	} {
		if _, err := tx.ExecContext(ctx, query, runID); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) InvalidateStatusValidation(runID string) error {
	return sqlitewrite.Transaction(context.Background(), d.sql, "InvalidateStatusValidation", func(writeCtx context.Context, tx *sql.Tx) error {
		if err := invalidateStatusValidation(writeCtx, tx, runID); err != nil {
			return err
		}
		return nil
	})
}

func (d *DB) RecordStatusValidationStep(stepID, runID, head, configDigest string, epoch int64) error {
	_, err := sqlitewrite.Exec(context.Background(), d.sql, "RecordStatusValidationStep", `INSERT INTO status_validation_steps(step_id,head_sha,epoch,config_digest)
	 SELECT id,?,?,? FROM step_results WHERE id=? AND run_id=? AND status='completed'
	 AND COALESCE((SELECT epoch FROM status_validation_epochs WHERE run_id=?),0)=?
	 ON CONFLICT(step_id) DO UPDATE SET head_sha=excluded.head_sha,epoch=excluded.epoch,config_digest=excluded.config_digest`, head, epoch, configDigest, stepID, runID, runID, epoch)
	return err
}

type statusStepProof struct {
	ID          string
	Name        types.StepName
	Findings    *string
	CompletedAt *int64
}

func rawShutdownSuperseded(q statusQuery, runID string) (bool, error) {
	var revoked bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM controlled_shutdown_runs WHERE run_id=? AND reason=?)`, runID, RawShutdownReason).Scan(&revoked)
	return revoked, err
}

func (d *DB) RawShutdownSuperseded(runID string) (bool, error) {
	return rawShutdownSuperseded(d.sql, runID)
}

func buildStatusCertificate(q statusQuery, runID, head, configDigest string) (*string, error) {
	revoked, err := rawShutdownSuperseded(q, runID)
	if err != nil {
		return nil, err
	}
	if revoked || configDigest == "" {
		return nil, nil
	}
	run := &Run{}
	if err := scanRun(q.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id=?`, runID), run); err != nil {
		return nil, err
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != head || run.LastPushedSHA == nil || *run.LastPushedSHA != head || run.PushGeneration == nil {
		return nil, nil
	}
	epoch, err := statusEpoch(q, runID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(`SELECT s.id,s.step_name,s.status,s.exit_code,s.findings_json,s.completed_at,s.override_reason,s.approval_reason,v.head_sha,v.epoch,v.config_digest
	 FROM step_results s LEFT JOIN status_validation_steps v ON v.step_id=s.id WHERE s.run_id=? ORDER BY s.step_order,s.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var proof []statusStepProof
	seen := map[types.StepName]bool{}
	for rows.Next() {
		var p statusStepProof
		var status string
		var exit *int
		var override, approval, executedHead, digest *string
		var stepEpoch *int64
		if err := rows.Scan(&p.ID, &p.Name, &status, &exit, &p.Findings, &p.CompletedAt, &override, &approval, &executedHead, &stepEpoch, &digest); err != nil {
			return nil, err
		}
		core := p.Name == types.StepReview || p.Name == types.StepTest || p.Name == types.StepDocument || p.Name == types.StepLint
		if !core && !p.Name.IsCustomGate() {
			continue
		}
		if status != "completed" || exit == nil || *exit != 0 || override != nil || approval != nil {
			return nil, nil
		}
		if p.Name.IsCustomGate() && p.Findings != nil && *p.Findings != "" {
			findings, err := types.ParseFindingsJSON(*p.Findings)
			if err != nil {
				return nil, nil
			}
			for _, finding := range findings.Items {
				if finding.Severity == types.FindingSeverityError {
					return nil, nil
				}
			}
		}
		if executedHead == nil || *executedHead != head || stepEpoch == nil || *stepEpoch != epoch || digest == nil || *digest != configDigest {
			return nil, nil
		}
		if core {
			if seen[p.Name] {
				return nil, nil
			}
			seen[p.Name] = true
			if p.Name == types.StepTest {
				if p.Findings == nil {
					return nil, nil
				}
				f, err := types.ParseFindingsJSON(*p.Findings)
				if err != nil || f.TestedHeadSHA != head {
					return nil, nil
				}
			}
		}
		proof = append(proof, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(seen) != 4 {
		return nil, nil
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	encoded, err = json.Marshal(StatusCertificate{head, epoch, *run.PushGeneration, configDigest, hex.EncodeToString(sum[:])})
	if err != nil {
		return nil, err
	}
	certificate := string(encoded)
	return &certificate, nil
}

func (d *DB) BuildStatusCertificate(runID, head, configDigest string) (*string, error) {
	return buildStatusCertificate(d.sql, runID, head, configDigest)
}

// Revocation is durable before outbound I/O; an uncertain reply cannot reuse old proof.
func (d *DB) RevokeStatusCertificate(ctx context.Context, runID string, p CommitStatusPublication) error {
	_, err := sqlitewrite.Exec(ctx, d.sql, "RevokeStatusCertificate", `UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=? AND host=? AND repo=? AND sha=? AND retired=0`, runID, p.Host, p.Repo, p.SHA)
	return err
}

type ControlledShutdownBinding struct{ Instance, Lifecycle, Reason string }

// TerminalizeCertifiedWatcher atomically binds the cancelled outcome and eligibility.
// Missing or stale execution proof or certificates cannot preserve success.
func (d *DB) TerminalizeCertifiedWatcher(runID, message, head, configDigest string, status types.RunStatus, bindings ...*ControlledShutdownBinding) error {
	return sqlitewrite.Transaction(context.Background(), d.sql, "TerminalizeCertifiedWatcher", func(writeCtx context.Context, tx *sql.Tx) error {
		run := &Run{}
		if err := scanRun(tx.QueryRowContext(writeCtx, `SELECT `+runColumns+` FROM runs WHERE id=?`, runID), run); err != nil {
			return err
		}
		if run.Status != types.RunRunning && run.Status != types.RunPending {
			return nil
		}
		certificate, err := buildStatusCertificate(tx, runID, head, configDigest)
		if err != nil {
			return err
		}
		if run.Status != types.RunRunning || run.PushActive || run.HeadSHA != head || run.LastPushedSHA == nil || *run.LastPushedSHA != head {
			certificate = nil
		}
		if len(bindings) > 0 && bindings[0] != nil {
			b := bindings[0]
			var matches bool
			if err := tx.QueryRowContext(writeCtx, `SELECT EXISTS(SELECT 1 FROM controlled_shutdown_runs WHERE run_id=? AND instance=? AND lifecycle=? AND reason=?)`, runID, b.Instance, b.Lifecycle, b.Reason).Scan(&matches); err != nil {
				return err
			}
			if !matches || run.Status != types.RunRunning {
				certificate = nil
			}
		}
		if certificate != nil {
			if _, err := tx.ExecContext(writeCtx, `UPDATE commit_status_publications SET preserve_success=1 WHERE run_id=? AND sha=? AND retired=0 AND last_state='success' AND strict_certificate=?`, runID, head, *certificate); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(writeCtx, `UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=?`, runID); err != nil {
				return err
			}
		}
		ts := now()
		if _, err := tx.ExecContext(writeCtx, `UPDATE runs SET error=?,status=?,head_sha=?,push_active=0,terminal_head_verified_at=?,updated_at=? WHERE id=?`, message, status, head, ts, ts, runID); err != nil {
			return err
		}
		return nil
	})
}

func (d *DB) PrepareControlledShutdown(runIDs []string, instance, lifecycle, reason string) error {
	if instance == "" || lifecycle == "" || reason == "" {
		return fmt.Errorf("missing controlled shutdown identity")
	}
	return sqlitewrite.Transaction(context.Background(), d.sql, "PrepareControlledShutdown", func(writeCtx context.Context, tx *sql.Tx) error {
		for _, runID := range runIDs {
			result, err := tx.ExecContext(writeCtx, `INSERT INTO controlled_shutdown_runs(run_id,instance,lifecycle,reason) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET instance=excluded.instance,lifecycle=excluded.lifecycle,reason=excluded.reason WHERE controlled_shutdown_runs.reason!=?`, runID, instance, lifecycle, reason, RawShutdownReason)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil {
				return err
			} else if n != 1 {
				return fmt.Errorf("controlled shutdown superseded")
			}
		}
		return nil
	})
}

func (d *DB) ControlledShutdownMatches(runID, instance, lifecycle, reason string) (bool, error) {
	var matches bool
	err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM controlled_shutdown_runs WHERE run_id=? AND instance=? AND lifecycle=? AND reason=?)`, runID, instance, lifecycle, reason).Scan(&matches)
	return matches, err
}

// RecordIneligibleTerminal commits terminal truth and revocation together.
func (d *DB) RecordIneligibleTerminal(runID, message string, status types.RunStatus, head *string) error {
	return sqlitewrite.Transaction(context.Background(), d.sql, "RecordIneligibleTerminal", func(writeCtx context.Context, tx *sql.Tx) error {
		var current types.RunStatus
		if err := tx.QueryRowContext(writeCtx, `SELECT status FROM runs WHERE id=?`, runID).Scan(&current); err != nil {
			return err
		}
		if current == types.RunCancelled {
			var preserved bool
			if err := tx.QueryRowContext(writeCtx, `SELECT EXISTS(SELECT 1 FROM commit_status_publications WHERE run_id=? AND preserve_success=1 AND strict_certificate IS NOT NULL AND last_state='success')`, runID).Scan(&preserved); err != nil {
				return err
			}
			if preserved {
				return nil
			}
		}
		if _, err := tx.ExecContext(writeCtx, `UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=?`, runID); err != nil {
			return err
		}
		var err error
		ts := now()
		if head != nil {
			_, err = tx.ExecContext(writeCtx, `UPDATE runs SET error=?,status=?,head_sha=?,push_active=0,terminal_head_verified_at=?,updated_at=? WHERE id=?`, message, status, *head, ts, ts, runID)
		} else {
			_, err = tx.ExecContext(writeCtx, `UPDATE runs SET error=?,status=?,push_active=0,terminal_head_verified_at=NULL,updated_at=? WHERE id=?`, message, status, ts, runID)
		}
		if err != nil {
			return err
		}
		return nil
	})
}

func (d *DB) SupersedeUncommittedCancellations(runIDs []string, instance string) error {
	return sqlitewrite.Transaction(context.Background(), d.sql, "SupersedeUncommittedCancellations", func(writeCtx context.Context, tx *sql.Tx) error {
		for _, runID := range runIDs {
			var current types.RunStatus
			if err := tx.QueryRowContext(writeCtx, `SELECT status FROM runs WHERE id=?`, runID).Scan(&current); err != nil {
				return err
			}
			if current != types.RunRunning && current != types.RunPending {
				continue
			}
			if instance == "" {
				return fmt.Errorf("missing raw shutdown runtime identity")
			}
			_, err := tx.ExecContext(writeCtx, `INSERT INTO controlled_shutdown_runs(run_id,instance,lifecycle,reason) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET instance=excluded.instance,lifecycle=excluded.lifecycle,reason=excluded.reason`, runID, instance, instance, RawShutdownReason)
			if err != nil {
				return err
			}
			if err := invalidateStatusValidation(writeCtx, tx, runID); err != nil {
				return err
			}
		}
		return nil
	})
}

// HasStatusPreservationProof reports whether a run holds an acknowledged,
// certified success that a planned cancellation may preserve. It is only a
// routing hint: TerminalizeCertifiedWatcher re-verifies the certificate.
func (d *DB) HasStatusPreservationProof(runID string) (bool, error) {
	var held bool
	err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM commit_status_publications WHERE run_id=? AND retired=0 AND strict_certificate IS NOT NULL AND last_state='success')`, runID).Scan(&held)
	return held, err
}

// ReleaseShutdownForRecovery settles the shutdown record of a run that a
// shutdown left non-terminal for the next start's recovery (parked at an
// approval gate or monitoring CI). Its status proof is invalidated with it, so
// the resumed run must earn any preservation again.
func (d *DB) ReleaseShutdownForRecovery(runID string) error {
	return sqlitewrite.Transaction(context.Background(), d.sql, "ReleaseShutdownForRecovery", func(writeCtx context.Context, tx *sql.Tx) error {
		var status types.RunStatus
		if err := tx.QueryRowContext(writeCtx, `SELECT status FROM runs WHERE id=?`, runID).Scan(&status); err != nil {
			return err
		}
		if status.Terminal() {
			return nil
		}
		if _, err := tx.ExecContext(writeCtx, `DELETE FROM controlled_shutdown_runs WHERE run_id=?`, runID); err != nil {
			return err
		}
		if err := invalidateStatusValidation(writeCtx, tx, runID); err != nil {
			return err
		}
		return nil
	})
}

func (d *DB) ReconcileRecoveredRun(runID, instance string) (bool, error) {
	var obsolete bool
	err := sqlitewrite.Transaction(context.Background(), d.sql, "ReconcileRecoveredRun", func(writeCtx context.Context, tx *sql.Tx) error {
		obsolete = false
		var status types.RunStatus
		if err := tx.QueryRowContext(writeCtx, `SELECT status FROM runs WHERE id=?`, runID).Scan(&status); err != nil {
			return err
		}
		if status.Terminal() {
			return nil
		}
		if err := tx.QueryRowContext(writeCtx, `SELECT EXISTS(SELECT 1 FROM controlled_shutdown_runs WHERE run_id=? AND instance<>?)`, runID, instance).Scan(&obsolete); err != nil {
			return err
		}
		if err := invalidateStatusValidation(writeCtx, tx, runID); err != nil {
			return err
		}
		if obsolete {
			if _, err := tx.ExecContext(writeCtx, `DELETE FROM controlled_shutdown_runs WHERE run_id=? AND instance<>?`, runID, instance); err != nil {
				return err
			}
			ts := now()
			if _, err := tx.ExecContext(writeCtx, `UPDATE runs SET status=?,error=?,push_active=0,terminal_head_verified_at=NULL,awaiting_agent_since=NULL,updated_at=? WHERE id=?`, types.RunFailed, "daemon crashed during execution", ts, runID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(writeCtx, `UPDATE step_results SET status=?,error=?,completed_at=? WHERE run_id=? AND status IN ('running','awaiting_approval','fixing','fix_review')`, types.StepStatusFailed, "daemon crashed during execution", ts, runID); err != nil {
				return err
			}
		}
		return nil
	})
	return obsolete && err == nil, err
}
