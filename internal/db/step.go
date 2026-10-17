package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepResult represents the result of a pipeline step execution.
type StepResult struct {
	ID             string
	RunID          string
	StepName       types.StepName
	StepOrder      int
	Status         types.StepStatus
	ExitCode       *int
	DurationMS     *int64
	LogPath        *string
	FindingsJSON   *string
	Error          *string
	StartedAt      *int64
	RoundStartedAt *int64
	CompletedAt    *int64
	LastActivityAt *int64
	LastActivity   *string
	AgentPID       *int
	AutoFixLimit   *int
	// OverrideReason is non-nil exactly when a human answered ActionApprove on
	// this step's gate despite an unresolved condition (currently: the CI
	// step's live checks were still failing, or the Test step's configured
	// commands.test exited non-zero). See
	// pipeline.ApprovalOverrideVerifier and Executor's two ActionApprove sites.
	OverrideReason *string
	// ApprovalReason records an explicit Test-gate approval independently of
	// the configured-command waiver used by PR enforcement. NULL means no
	// recorded approval; an empty string means approved without a reason.
	ApprovalReason *string
	// AcceptedGapsJSON is the audit record of an owner-approved Test gap
	// accepted under strict shipping (see AcceptedGaps). NULL means none.
	AcceptedGapsJSON *string
	// SkipReason records an automatic PR/CI skip, distinct from an explicit
	// per-run skip. Legacy rows have no recorded reason.
	SkipReason *string
	// PendingResponse is the accepted gate response (JSON) of a parked step
	// whose run is waiting for a run-concurrency slot. NULL means none.
	PendingResponse *string
	// GateNonce and GateRoundID identify the last durable park. A new park
	// always mints a nonce, including a repeated park of the same round.
	GateNonce   *string
	GateRoundID *string
}

// AnsweredWaitingForSlot reports whether the step is parked at a gate whose
// response was already accepted and is waiting for a run slot to dispatch. It
// is answered, not awaiting a decision, whatever the persisted parked status.
func (s *StepResult) AnsweredWaitingForSlot() bool {
	return s != nil && s.PendingResponse != nil &&
		(s.Status == types.StepStatusAwaitingApproval || s.Status == types.StepStatusFixReview)
}

const stepResultColumns = `id, run_id, step_name, step_order, status, exit_code, duration_ms, log_path, findings_json, error, started_at, completed_at`

// readableStepResultColumns tolerates databases that predate the optional
// columns. The ci_fix_attempts column is no longer read: the CI step's fix
// rounds are counted by the executor from the round history, exactly like
// every other step's, and the column only remains because migrations are
// append-only.
func (d *DB) readableStepResultColumns() string {
	columns := stepResultColumns
	for _, column := range []string{"last_activity_at", "last_activity", "agent_pid", "auto_fix_limit", "round_started_at", "override_reason", "skip_reason", "approval_reason", "gate_nonce", "gate_round_id"} {
		if d.hasColumn("step_results", column) {
			columns += ", " + column
		} else {
			columns += ", NULL AS " + column
		}
	}
	if d.hasColumn("step_results", "accepted_gaps_json") {
		columns += ", accepted_gaps_json"
	} else {
		columns += ", NULL AS accepted_gaps_json"
	}
	if d.hasColumn("step_results", "pending_response") {
		columns += ", pending_response"
	} else {
		columns += ", NULL AS pending_response"
	}
	return columns
}

// InsertStepResult creates a new step result record.
func (d *DB) InsertStepResult(runID string, stepName types.StepName) (*StepResult, error) {
	s := &StepResult{
		ID:        newID(),
		RunID:     runID,
		StepName:  stepName,
		StepOrder: stepName.Order(),
		Status:    types.StepStatusPending,
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.RunID, s.StepName, s.StepOrder, s.Status,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step result: %w", err)
	}
	return s, nil
}

// GetStepResult returns a step result by ID.
func (d *DB) GetStepResult(id string) (*StepResult, error) {
	s := &StepResult{}
	err := d.sql.QueryRow(
		`SELECT `+d.readableStepResultColumns()+` FROM step_results WHERE id = ?`, id,
	).Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.Error, &s.StartedAt, &s.CompletedAt, &s.LastActivityAt, &s.LastActivity, &s.AgentPID, &s.AutoFixLimit, &s.RoundStartedAt, &s.OverrideReason, &s.SkipReason, &s.ApprovalReason, &s.GateNonce, &s.GateRoundID, &s.AcceptedGapsJSON, &s.PendingResponse)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get step result: %w", err)
	}
	return s, nil
}

func (d *DB) stepsByRunQuery() string {
	return `SELECT ` + d.readableStepResultColumns() + ` FROM step_results WHERE run_id = ? ORDER BY step_order, id`
}

// GetStepsByRun returns all step results for a run, in execution order.
//
// The `id` tie-break is load-bearing: a custom gate shares its anchor's
// step_order, so a run can hold duplicate sort keys, and SQLite does not define
// the order of rows with equal keys. Executor.recoveredGate matches these rows
// to the executor's step list POSITIONALLY, so an unspecified tie order would
// make every parked run in a gates-configured repository unrecoverable. Step
// ids are monotonic ULIDs, so ordering by id reproduces insertion - that is,
// execution - order deterministically.
func (d *DB) GetStepsByRun(runID string) ([]*StepResult, error) {
	rows, err := d.sql.Query(d.stepsByRunQuery(), runID)
	if err != nil {
		return nil, fmt.Errorf("get steps by run: %w", err)
	}
	defer rows.Close()
	var steps []*StepResult
	for rows.Next() {
		s := &StepResult{}
		if err := rows.Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.Error, &s.StartedAt, &s.CompletedAt, &s.LastActivityAt, &s.LastActivity, &s.AgentPID, &s.AutoFixLimit, &s.RoundStartedAt, &s.OverrideReason, &s.SkipReason, &s.ApprovalReason, &s.GateNonce, &s.GateRoundID, &s.AcceptedGapsJSON, &s.PendingResponse); err != nil {
			return nil, fmt.Errorf("scan step result: %w", err)
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

func (d *DB) ResetStepsFrom(runID string, stepOrder int) error {
	if stepOrder <= types.StepReview.Order() {
		if _, err := d.sql.Exec(`UPDATE review_comparisons SET approved=0 WHERE run_id=?`, runID); err != nil {
			return err
		}
		if _, err := d.sql.Exec(`UPDATE runs SET ci_ready_at=NULL,ci_ready_no_ci=0 WHERE id=? AND shipping_options IS NOT NULL`, runID); err != nil {
			return err
		}
	}
	_, err := d.sql.Exec(`
		UPDATE step_results
		SET status = ?, exit_code = NULL, duration_ms = NULL, log_path = NULL,
			findings_json = NULL, error = NULL, started_at = NULL,
			round_started_at = NULL, completed_at = NULL, last_activity_at = NULL, last_activity = NULL,
			agent_pid = NULL, auto_fix_limit = NULL, override_reason = NULL, approval_reason = NULL, accepted_gaps_json = NULL
		WHERE run_id = ? AND step_order >= ? AND status != ?`, types.StepStatusPending, runID, stepOrder, types.StepStatusSkipped)
	if err != nil {
		return fmt.Errorf("reset steps for revalidation: %w", err)
	}
	return nil
}

// UpdateStepStatus updates a step's status.
func (d *DB) UpdateStepStatus(id string, status types.StepStatus) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`, status, now(), fmt.Sprintf("status: %s", status), id)
	if err != nil {
		return fmt.Errorf("update step status: %w", err)
	}
	return nil
}

// UpdateStepStatusWithDuration updates a step's status and execution duration together.
func (d *DB) UpdateStepStatusWithDuration(id string, status types.StepStatus, durationMS int64) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, duration_ms = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`, status, durationMS, now(), fmt.Sprintf("status: %s", status), id)
	if err != nil {
		return fmt.Errorf("update step status with duration: %w", err)
	}
	return nil
}

// ParkStepForApproval publishes one park and always mints a new nonce.
// Executors supply their exact recorded round ID; omitted round IDs select the
// latest durable round for callers restoring an already-recorded gate.
func (d *DB) ParkStepForApproval(runID, stepID string, status types.StepStatus, exitCode int, durationMS int64, findingsJSON *string, roundID ...string) error {
	var parkedRound any
	if len(roundID) > 0 {
		parkedRound = roundID[0]
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin approval park: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	stepResult, err := tx.Exec(
		`UPDATE step_results SET status = ?, exit_code = ?, duration_ms = ?, findings_json = ?, pending_response = NULL, gate_nonce = ?, gate_round_id = COALESCE(?, (SELECT id FROM step_rounds WHERE step_result_id = ? ORDER BY round DESC LIMIT 1)), last_activity_at = ?, last_activity = ? WHERE id = ?`,
		status, exitCode, durationMS, findingsJSON, newID(), parkedRound, stepID, ts, fmt.Sprintf("status: %s", status), stepID,
	)
	if err != nil {
		return fmt.Errorf("park step for approval: %w", err)
	}
	changed, err := stepResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("park step for approval rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("park step for approval: updated %d rows", changed)
	}
	runResult, err := tx.Exec(
		`UPDATE runs SET awaiting_agent_since = ?, updated_at = ? WHERE id = ?`,
		ts, ts, runID,
	)
	if err != nil {
		return fmt.Errorf("mark run awaiting approval: %w", err)
	}
	changed, err = runResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark run awaiting approval rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("mark run awaiting approval: updated %d rows", changed)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval park: %w", err)
	}
	return nil
}

// SetStepPendingResponse durably records the accepted response of a parked gate
// whose run must wait for a run slot. The step keeps its parked status and the
// run keeps its awaiting-agent marker, so the gate is still recovered as a gate
// if the daemon stops during the wait; activity is what status surfaces show.
func (d *DB) SetStepPendingResponse(stepID, response, activity string, gate types.GateIdentity) error {
	if !gate.SameGate(gate) {
		return d.gateSupersededError(stepID)
	}
	result, err := d.sql.Exec(`UPDATE step_results SET pending_response = ?, last_activity_at = ?, last_activity = ? WHERE id = ? AND status IN (?, ?) AND (gate_nonce = ? AND gate_round_id = ? AND run_id = ? AND step_name = ? AND id = ? AND pending_response IS NULL)`,
		response, now(), activity, stepID, types.StepStatusAwaitingApproval, types.StepStatusFixReview, gate.Nonce, gate.RoundID, gate.RunID, gate.Step, gate.StepResultID)
	if err != nil {
		return fmt.Errorf("record pending gate response: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("record pending gate response: %w", err)
	} else if n != 1 {
		return d.gateSupersededError(stepID)
	}
	return nil
}

// ResumeStepFromApproval moves a parked step to running when its response is
// dispatched, clearing any pending response and the run's awaiting-agent marker
// in one transaction.
func (d *DB) ResumeStepFromApproval(runID, stepID, activity string, parkedMS int64, gate types.GateIdentity) error {
	if !gate.SameGate(gate) {
		return d.gateSupersededError(stepID)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE step_results SET status = ?, pending_response = NULL, last_activity_at = ?, last_activity = ? WHERE id = ? AND run_id = ? AND (gate_nonce = ? AND gate_round_id = ? AND run_id = ? AND step_name = ? AND id = ? AND status IN (?, ?))`, types.StepStatusRunning, now(), activity, stepID, runID, gate.Nonce, gate.RoundID, gate.RunID, gate.Step, gate.StepResultID, types.StepStatusAwaitingApproval, types.StepStatusFixReview)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("resume gate rows affected: %w", err)
	} else if n != 1 {
		tx.Rollback()
		return d.gateSupersededError(stepID)
	}
	if parkedMS < 0 {
		parkedMS = 0
	}
	if _, err := tx.Exec(`UPDATE runs SET awaiting_agent_since = NULL,
		parked_ms = COALESCE(parked_ms, 0) + CASE WHEN awaiting_agent_since IS NOT NULL THEN ? ELSE 0 END,
		updated_at = ? WHERE id = ?`, parkedMS, now(), runID); err != nil {
		return err
	}
	return tx.Commit()
}

// StartStep marks a step as running with a started_at timestamp.
func (d *DB) StartStep(id string) error {
	return d.StartStepWithAutoFixLimit(id, 0)
}

// StartStepWithAutoFixLimit marks a step as running and records the effective
// auto-fix limit that status surfaces use while the step is active.
func (d *DB) StartStepWithAutoFixLimit(id string, autoFixLimit int) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, started_at = ?, round_started_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL, auto_fix_limit = ? WHERE id = ?`, types.StepStatusRunning, ts, ts, ts, "step started", autoFixLimitDBValue(autoFixLimit), id)
	if err != nil {
		return fmt.Errorf("start step: %w", err)
	}
	return nil
}

// StartStepFixRound marks the beginning of a distinct fix execution while
// preserving started_at as the clock for the enclosing step. Recovery can use
// a newly loaded trusted configuration, so its supplied limit replaces the
// one recorded by an earlier execution.
func (d *DB) StartStepFixRound(id string, autoFixLimit int) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, round_started_at = ?, last_activity_at = ?, last_activity = ?, auto_fix_limit = ?, override_reason = NULL, approval_reason = NULL, accepted_gaps_json = NULL WHERE id = ?`, types.StepStatusFixing, ts, ts, fmt.Sprintf("status: %s", types.StepStatusFixing), autoFixLimitDBValue(autoFixLimit), id)
	if err != nil {
		return fmt.Errorf("start step fix round: %w", err)
	}
	return nil
}

func (d *DB) SetStepAutoFixLimit(id string, autoFixLimit int) error {
	if _, err := d.sql.Exec(`UPDATE step_results SET auto_fix_limit = ? WHERE id = ?`, autoFixLimitDBValue(autoFixLimit), id); err != nil {
		return fmt.Errorf("set step auto-fix limit: %w", err)
	}
	return nil
}

// SetStepOverrideReason records that a human answered ActionApprove on this
// step's gate despite an unresolved external condition (see
// pipeline.ApprovalOverrideVerifier). Called alongside, not instead of, the
// normal step-completion write: it never changes Status, only annotates why
// the completion happened without the condition that raised the gate having
// cleared. reason must be non-empty; the column stays NULL for every
// ordinarily-resolved step, which is what marks a run "verified green".
func (d *DB) SetStepOverrideReason(id string, reason string) error {
	if reason == "" {
		return fmt.Errorf("set step override reason: reason must not be empty")
	}
	if _, err := d.sql.Exec(`UPDATE step_results SET override_reason = ? WHERE id = ?`, reason, id); err != nil {
		return fmt.Errorf("set step override reason: %w", err)
	}
	return nil
}

// SetTestApprovalReason preserves the operator's explanation without changing
// the command failure marker or its PR-enforcement policy.
func (d *DB) SetTestApprovalReason(id, reason string) error {
	result, err := d.sql.Exec(`UPDATE step_results SET approval_reason = ? WHERE id = ? AND step_name = ? AND status IN (?, ?, ?)`,
		reason, id, types.StepTest, types.StepStatusAwaitingApproval, types.StepStatusFixReview, types.StepStatusRunning)
	if err != nil {
		return fmt.Errorf("record Test approval reason: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("record Test approval reason: active Test step not found")
	}
	return nil
}

// TestOverrideReason qualifies completed Test exceptions on both snapshot and
// event paths. Only an approval past a failing configured command, a no-go
// or inconclusive verdict, or a Test-agent invocation-budget cut is an
// exception; approving a no-surface park keeps its recorded reason but
// completes normally. Older command overrides still qualify without a
// recorded reason.
func (s *StepResult) TestOverrideReason() string {
	if s.StepName != types.StepTest || s.Status != types.StepStatusCompleted {
		return ""
	}
	condition := s.testExceptionCondition()
	if condition == "" {
		return ""
	}
	if gaps := s.ValidAcceptedGaps(); gaps != nil {
		return strings.TrimSpace(condition + "\nTest gap accepted (" + strings.Join(gaps.FindingIDs, ", ") + "): " + gaps.Reason + "; covered by: " + gaps.CoveredBy)
	}
	if s.ApprovalReason == nil {
		return condition
	}
	reason := *s.ApprovalReason
	if strings.TrimSpace(reason) == "" {
		reason = "no operator reason supplied"
	}
	return strings.TrimSpace(condition + "\nTest exception approved: " + reason)
}

func (s *StepResult) testExceptionCondition() string {
	if s.OverrideReason != nil && strings.TrimSpace(*s.OverrideReason) != "" {
		return *s.OverrideReason
	}
	if s.FindingsJSON == nil {
		return ""
	}
	findings, err := types.ParseFindingsJSON(*s.FindingsJSON)
	if err != nil {
		if s.ApprovalReason != nil {
			return "approved Test evidence could not be read"
		}
		return ""
	}
	switch findings.Verdict {
	case types.TestVerdictNoGo, types.TestVerdictInconclusive:
		return "live validation verdict: " + findings.Verdict
	}
	for _, item := range findings.Items {
		if item.ID == types.FindingIDTestAgentTimeout {
			return "test agent invocation budget exhausted"
		}
	}
	return ""
}

func autoFixLimitDBValue(autoFixLimit int) any {
	if autoFixLimit <= 0 {
		return nil
	}
	return autoFixLimit
}

// CompleteStep marks a step as completed with timing and result info.
func (d *DB) CompleteStep(id string, exitCode int, durationMS int64, logPath string) error {
	return d.CompleteStepWithStatus(id, types.StepStatusCompleted, exitCode, durationMS, logPath)
}

// CompleteStepWithStatus marks a step as finished with timing and result info.
func (d *DB) CompleteStepWithStatus(id string, status types.StepStatus, exitCode int, durationMS int64, logPath string) error {
	return d.completeStep(id, status, exitCode, durationMS, logPath, "")
}

// CompleteSkippedStep atomically preserves the automatic skip cause with its status.
func (d *DB) CompleteSkippedStep(id string, exitCode int, durationMS int64, logPath, reason string) error {
	return d.completeStep(id, types.StepStatusSkipped, exitCode, durationMS, logPath, reason)
}

func (d *DB) completeStep(id string, status types.StepStatus, exitCode int, durationMS int64, logPath, skipReason string) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, exit_code = ?, duration_ms = ?, log_path = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL, skip_reason = NULLIF(?, ''), override_reason = CASE WHEN ? THEN NULL ELSE override_reason END, approval_reason = CASE WHEN ? THEN NULL ELSE approval_reason END, accepted_gaps_json = CASE WHEN ? THEN NULL ELSE accepted_gaps_json END WHERE id = ?`,
		status, exitCode, durationMS, logPath, now(), now(), fmt.Sprintf("status: %s", status), skipReason, status == types.StepStatusSkipped, status == types.StepStatusSkipped, status == types.StepStatusSkipped, id,
	)
	if err != nil {
		return fmt.Errorf("complete step: %w", err)
	}
	return nil
}

// CompleteReviewStep atomically completes a successful review and replaces
// the run's exact review-approved head. Neither write survives if the other
// fails, so a failed completion cannot create approval authority and a
// completed review cannot lack it.
func (d *DB) CompleteReviewStep(id, runID, approvedHeadSHA string, exitCode int, durationMS int64, logPath string, generation ...int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin complete review step: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	var shipping *string
	if err := tx.QueryRow(`SELECT shipping_options FROM runs WHERE id=?`, runID).Scan(&shipping); err != nil {
		return fmt.Errorf("read review run: %w", err)
	}
	if shipping != nil {
		if len(generation) != 1 || generation[0] < 1 {
			return fmt.Errorf("strict review completion requires captured generation")
		}
		result, err := tx.Exec(`UPDATE review_comparisons SET approved=1 WHERE run_id=? AND head_sha=? AND generation=?`, runID, approvedHeadSHA, generation[0])
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("review completion lacks exact comparison")
		}
	}
	result, err := tx.Exec(
		`UPDATE step_results SET status = ?, exit_code = ?, duration_ms = ?, log_path = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL WHERE id = ? AND run_id = ?`,
		types.StepStatusCompleted, exitCode, durationMS, logPath, ts, ts, fmt.Sprintf("status: %s", types.StepStatusCompleted), id, runID,
	)
	if err != nil {
		return fmt.Errorf("complete review step: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("complete review step: step row not found")
	}
	result, err = tx.Exec(`UPDATE runs SET review_approved_head_sha = ?, updated_at = ? WHERE id = ?`, approvedHeadSHA, ts, runID)
	if err != nil {
		return fmt.Errorf("record review-approved head: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("record review-approved head: run row not found")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completed review: %w", err)
	}
	return nil
}

// FailStep marks a step as failed with an error message and duration.
func (d *DB) FailStep(id string, errMsg string, durationMS int64) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, error = ?, duration_ms = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL, pending_response = NULL WHERE id = ?`,
		types.StepStatusFailed, errMsg, durationMS, now(), now(), "step failed: "+errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("fail step: %w", err)
	}
	return nil
}

// TouchStepActivity records the latest meaningful activity for an active step
// without changing its status or current agent pid.
func (d *DB) TouchStepActivity(id string, text string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET last_activity_at = ?, last_activity = ? WHERE id = ?`, now(), text, id)
	if err != nil {
		return fmt.Errorf("touch step activity: %w", err)
	}
	return nil
}

// SetStepAgentActivity records an agent lifecycle activity and replaces the
// active agent pid. Passing nil clears the pid after the process exits.
func (d *DB) SetStepAgentActivity(id string, text string, agentPID *int) error {
	_, err := d.sql.Exec(`UPDATE step_results SET last_activity_at = ?, last_activity = ?, agent_pid = ? WHERE id = ?`, now(), text, agentPID, id)
	if err != nil {
		return fmt.Errorf("set step agent activity: %w", err)
	}
	return nil
}

// SetStepDuration sets the execution-only duration on a step result.
func (d *DB) SetStepDuration(id string, durationMS int64) error {
	_, err := d.sql.Exec(`UPDATE step_results SET duration_ms = ? WHERE id = ?`, durationMS, id)
	if err != nil {
		return fmt.Errorf("set step duration: %w", err)
	}
	return nil
}

// SetStepFindings sets the findings JSON on a step result.
func (d *DB) SetStepFindings(id string, findingsJSON string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET findings_json = ? WHERE id = ?`, findingsJSON, id)
	if err != nil {
		return fmt.Errorf("set step findings: %w", err)
	}
	return nil
}

// ClearStepFindings removes any stored findings JSON from a step result.
func (d *DB) ClearStepFindings(id string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET findings_json = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("clear step findings: %w", err)
	}
	return nil
}

func (s *StepResult) NoSurfaceTestAcknowledgmentEligible() bool {
	if s.StepName != types.StepTest || s.ExitCode == nil || *s.ExitCode != 0 || s.OverrideReason != nil || s.SkipReason != nil || s.FindingsJSON == nil {
		return false
	}
	findings, err := types.ParseFindingsJSON(*s.FindingsJSON)
	return err == nil && types.IsNoSurfaceTestAcknowledgment(findings)
}

// StrictApprovalRefusalWithGaps is StrictApprovalRefusal for an approval that
// may carry an accepted-gap request. With a request, AcceptedGapRefusal alone
// decides; without one it is exactly StrictApprovalRefusal.
func (s *StepResult) StrictApprovalRefusalWithGaps(reason string, gaps *AcceptedGaps) string {
	if gaps == nil {
		return s.StrictApprovalRefusal(reason)
	}
	return s.AcceptedGapRefusal(true, *gaps)
}

func (s *StepResult) StrictApprovalRefusal(reason string) string {
	if s.StepName == types.StepReview {
		return "strict Review requires a complete independent review; fix or rerun the review"
	}
	if s.ExitCode != nil && *s.ExitCode != 0 {
		return "strict shipping forbids passing command overrides"
	}
	if s.OverrideReason != nil || s.SkipReason != nil {
		return "strict shipping forbids recorded overrides or skips"
	}
	if reason == "" && s.ValidAcceptedGaps() != nil {
		return ""
	}
	if s.NoSurfaceTestAcknowledgmentEligible() {
		return ""
	}
	if reason != "" || s.ApprovalReason != nil {
		return "strict shipping forbids Test exceptions" + s.acceptedGapHint()
	}
	if s.FindingsJSON != nil {
		findings, err := types.ParseFindingsJSON(*s.FindingsJSON)
		if s.StepName == types.StepTest {
			for _, item := range findings.Items {
				if item.ID == types.FindingIDTestAgentTimeout || item.ID == types.FindingIDTestAgentUnvalidatedWork || item.Category == types.FindingCategoryTestCommand {
					return "strict shipping forbids Test exceptions"
				}
			}
		}
		if err != nil || types.HasActionableFindings(findings) || s.StepName == types.StepTest && (findings.Verdict == types.TestVerdictNoSurface || findings.Verdict == types.TestVerdictNoGo || findings.Verdict == types.TestVerdictInconclusive) {
			return "strict shipping cannot approve unresolved actionable findings" + s.acceptedGapHint()
		}
	}
	return ""
}

// GateIdentity returns the durable identity of the last park. Status tells the
// caller whether it is still waiting, or already running/completed.
func (s *StepResult) GateIdentity() types.GateIdentity {
	g := types.GateIdentity{RunID: s.RunID, StepResultID: s.ID, Step: s.StepName, Status: s.Status}
	if s.AnsweredWaitingForSlot() {
		g.Status = types.StepStatusRunning
	}
	if s.GateNonce != nil {
		g.Nonce = *s.GateNonce
	}
	if s.GateRoundID != nil {
		g.RoundID = *s.GateRoundID
	}
	return g
}

func (d *DB) gateSupersededError(stepID string) error {
	row, err := d.GetStepResult(stepID)
	if err != nil {
		return err
	}
	current := types.GateIdentity{StepResultID: stepID}
	if row != nil {
		current = row.GateIdentity()
	}
	return &types.GateSupersededError{Current: current}
}
