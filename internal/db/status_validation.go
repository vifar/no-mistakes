package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

func invalidateStatusValidation(tx *sql.Tx, runID string) error {
	for _, query := range []string{
		`INSERT INTO status_validation_epochs(run_id,epoch) VALUES(?,1) ON CONFLICT(run_id) DO UPDATE SET epoch=epoch+1`,
		`UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=?`,
		`DELETE FROM status_validation_steps WHERE step_id IN (SELECT id FROM step_results WHERE run_id=?)`,
	} {
		if _, err := tx.Exec(query, runID); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) InvalidateStatusValidation(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := invalidateStatusValidation(tx, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) RecordStatusValidationStep(stepID, runID, head, configDigest string, epoch int64) error {
	_, err := d.sql.Exec(`INSERT INTO status_validation_steps(step_id,head_sha,epoch,config_digest)
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

func buildStatusCertificate(q statusQuery, runID, head, configDigest string) (*string, error) {
	if configDigest == "" {
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
		if core {
			if seen[p.Name] || executedHead == nil || *executedHead != head || stepEpoch == nil || *stepEpoch != epoch || digest == nil || *digest != configDigest {
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
	_, err := d.sql.ExecContext(ctx, `UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=? AND host=? AND repo=? AND sha=? AND retired=0`, runID, p.Host, p.Repo, p.SHA)
	return err
}

// TerminalizeCertifiedWatcher atomically binds the cancelled outcome and eligibility.
// A missing/stale certificate is an ordinary cancelled failure, never inferred proof.
type ControlledShutdownBinding struct{ Instance, Lifecycle, Reason string }

func (d *DB) TerminalizeCertifiedWatcher(runID, message, head, configDigest string, status types.RunStatus, bindings ...*ControlledShutdownBinding) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	run := &Run{}
	if err := scanRun(tx.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id=?`, runID), run); err != nil {
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
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM controlled_shutdown_runs WHERE run_id=? AND instance=? AND lifecycle=? AND reason=?)`, runID, b.Instance, b.Lifecycle, b.Reason).Scan(&matches); err != nil {
			return err
		}
		if !matches || run.Status != types.RunRunning {
			certificate = nil
		}
	}
	if certificate != nil {
		if _, err := tx.Exec(`UPDATE commit_status_publications SET preserve_success=1 WHERE run_id=? AND sha=? AND retired=0 AND last_state='success' AND strict_certificate=?`, runID, head, *certificate); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=?`, runID); err != nil {
			return err
		}
	}
	ts := now()
	if _, err := tx.Exec(`UPDATE runs SET error=?,status=?,head_sha=?,push_active=0,terminal_head_verified_at=?,updated_at=? WHERE id=?`, message, status, head, ts, ts, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) PrepareControlledShutdown(runIDs []string, instance, lifecycle, reason string) error {
	if instance == "" || lifecycle == "" || reason == "" {
		return fmt.Errorf("missing controlled shutdown identity")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, runID := range runIDs {
		if _, err := tx.Exec(`INSERT INTO controlled_shutdown_runs(run_id,instance,lifecycle,reason) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET instance=excluded.instance,lifecycle=excluded.lifecycle,reason=excluded.reason`, runID, instance, lifecycle, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) ControlledShutdownMatches(runID, instance, lifecycle, reason string) (bool, error) {
	var matches bool
	err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM controlled_shutdown_runs WHERE run_id=? AND instance=? AND lifecycle=? AND reason=?)`, runID, instance, lifecycle, reason).Scan(&matches)
	return matches, err
}

// RecordIneligibleTerminal commits terminal truth and revocation together.
func (d *DB) RecordIneligibleTerminal(runID, message string, status types.RunStatus, head *string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordIneligibleTerminal(tx, runID, message, status, head, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) SupersedeUncommittedCancellations(runIDs []string, instance string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, runID := range runIDs {
		if err := recordIneligibleTerminal(tx, runID, "daemon shutting down", types.RunFailed, nil, &instance); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func recordIneligibleTerminal(tx *sql.Tx, runID, message string, status types.RunStatus, head *string, rawInstance *string) error {
	var err error
	var current types.RunStatus
	if err := tx.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&current); err != nil {
		return err
	}
	if current != types.RunRunning && current != types.RunPending {
		return nil
	}
	if rawInstance != nil {
		if _, err := tx.Exec(`DELETE FROM controlled_shutdown_runs WHERE run_id=? AND instance=?`, runID, *rawInstance); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE commit_status_publications SET strict_certificate=NULL,preserve_success=NULL WHERE run_id=?`, runID); err != nil {
		return err
	}
	ts := now()
	if head != nil {
		_, err = tx.Exec(`UPDATE runs SET error=?,status=?,head_sha=?,push_active=0,terminal_head_verified_at=?,updated_at=? WHERE id=?`, message, status, *head, ts, ts, runID)
	} else {
		_, err = tx.Exec(`UPDATE runs SET error=?,status=?,push_active=0,terminal_head_verified_at=NULL,updated_at=? WHERE id=?`, message, status, ts, runID)
	}
	if err != nil {
		return err
	}
	return nil
}
