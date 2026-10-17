package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

var (
	ErrTargetAdvanceIneligible = errors.New("run is not eligible for a target advance rewind")
	ErrTargetAdvanceCap        = errors.New("target advance rewind cap reached")
)

// TargetAdvanceRewind is diagnostic history. SourceSHA is the ORIGINAL source
// observation for the pending integration; nil means confirmed remote absence.
// ForgeHeadSHA pins the attached PR head; nil pins continued PR absence.
// Runs point at the latest row only while that integration remains pending.
type TargetAdvanceRewind struct {
	ID, RunID, OldTargetSHA, TargetSHA, LocalHeadSHA string
	SourceSHA, ForgeHeadSHA                          *string
	Step                                             types.StepName
	Generation, At                                   int64
}

// TargetAdvanceRewindRequest carries the guard's freshly classified external
// facts. Git ancestry is checked by the guard; all durable eligibility and the
// receipt generation are checked again by the binding transaction.
type TargetAdvanceRewindRequest struct {
	ExpectedGeneration int64
	Step               types.StepName
	MaxRewinds         int
	SourceSHA          *string
	HasPR              bool
}

type prContextBindOptions struct {
	rewind             *TargetAdvanceRewindRequest
	completeGeneration *int64
}

func (d *DB) RewindRunPRContext(runID string, candidate PRContextCandidate, request TargetAdvanceRewindRequest) (PRContextBindResult, error) {
	return d.bindRunPRContext(runID, candidate, types.StepRebase, false, &prContextBindOptions{rewind: &request})
}

// CompleteRebasePRContext consumes pending integration in the receipt bind,
// including an identical receipt. Only the executor's successful Rebase path
// may call this; parking and recovery are not successful integration.
func (d *DB) CompleteRebasePRContext(runID string, candidate PRContextCandidate, expectedGeneration int64) (PRContextBindResult, error) {
	return d.bindRunPRContext(runID, candidate, types.StepReview, false, &prContextBindOptions{completeGeneration: &expectedGeneration})
}

type rewindQuery interface{ QueryRow(string, ...any) *sql.Row }

func pendingTargetRewind(q rewindQuery, runID string) (*TargetAdvanceRewind, error) {
	p := &TargetAdvanceRewind{}
	err := q.QueryRow(`SELECT w.id,w.run_id,w.old_target_sha,w.target_sha,w.local_head_sha,w.source_sha,w.forge_head_sha,w.step,w.generation,w.at
 FROM run_target_rewinds w JOIN runs r ON r.pending_target_rewind_id=w.id AND r.id=w.run_id WHERE r.id=?`, runID).Scan(
		&p.ID, &p.RunID, &p.OldTargetSHA, &p.TargetSHA, &p.LocalHeadSHA, &p.SourceSHA, &p.ForgeHeadSHA, &p.Step, &p.Generation, &p.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pending target rewind: %w", err)
	}
	return p, nil
}

func (d *DB) GetPendingTargetRewind(runID string) (*TargetAdvanceRewind, error) {
	return pendingTargetRewind(d.sql, runID)
}

// prHeadObservation is nullable so absence cannot be confused with an
// unattached PR. Rewind admission rejects unattached PRs before this is stored.
func prHeadObservation(c PRContextCandidate) *string {
	if c.PRURL == "" {
		return nil
	}
	return &c.ForgeHeadSHA
}

func sameOptionalSHA(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// CanRewindTarget avoids adding source observations to excluded boundaries.
// The transaction repeats the same checks before it binds anything.
func (d *DB) CanRewindTarget(runID string, step types.StepName) (bool, error) {
	_, err := targetRewindRun(d.sql, runID, step)
	if errors.Is(err, ErrTargetAdvanceIneligible) {
		return false, nil
	}
	return err == nil, err
}

func targetRewindRun(q rewindQuery, runID string, step types.StepName) (*Run, error) {
	r := &Run{}
	if err := scanRun(q.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id=?`, runID), r); err != nil {
		return nil, err
	}
	var prohibited bool
	if err := q.QueryRow(`SELECT retarget_attempted IS NOT 0 OR
 EXISTS (SELECT 1 FROM commit_status_attempts WHERE run_id=?) OR
 EXISTS (SELECT 1 FROM commit_status_publications WHERE run_id=?) OR
 EXISTS (SELECT 1 FROM step_results WHERE run_id=? AND step_name='pr' AND status='completed') OR
 EXISTS (SELECT 1 FROM step_results WHERE run_id=? AND step_name='rebase' AND status='skipped')
 FROM runs WHERE id=?`, runID, runID, runID, runID, runID).Scan(&prohibited); err != nil {
		return nil, err
	}
	published := r.LastPushedSHA != nil || r.PushGeneration != nil && *r.PushGeneration != 0 || prohibited

	if r.Shipping != nil || published || step.Order() < types.StepRebase.Order() || step.Order() > types.StepLint.Order() ||
		(r.Status != types.RunRunning && r.Status != types.RunPending) {
		return nil, ErrTargetAdvanceIneligible
	}
	return r, nil
}

func checkTargetRewindEligibility(tx *sql.Tx, runID string, previous *PRContext, candidate PRContextCandidate, req *TargetAdvanceRewindRequest) (*Run, *TargetAdvanceRewind, error) {
	r, err := targetRewindRun(tx, runID, req.Step)
	if err != nil {
		return nil, nil, err
	}
	pending, err := pendingTargetRewind(tx, runID)
	if err != nil {
		return nil, nil, err
	}
	if pending != nil && (!sameOptionalSHA(pending.SourceSHA, req.SourceSHA) || !sameOptionalSHA(pending.ForgeHeadSHA, prHeadObservation(candidate))) {
		return nil, nil, fmt.Errorf("%w: remote source or PR observation changed during pending rewind", ErrTargetAdvanceIneligible)
	}
	identity := previous.PRURL == candidate.PRURL && previous.SourceRepo == candidate.SourceRepo && previous.SourceBranch == candidate.SourceBranch && previous.TargetBranch == candidate.TargetBranch && previous.ForgeHeadSHA == candidate.ForgeHeadSHA
	pinnedPR := !req.HasPR && previous.PRURL == "" || req.HasPR && previous.PRURL != "" && previous.ForgeHeadSHA != "" && previous.ForgeHeadSHA == candidate.ForgeHeadSHA
	if !identity || !pinnedPR ||
		previous.LocalHeadSHA != candidate.LocalHeadSHA || r.HeadSHA != candidate.LocalHeadSHA || previous.TargetSHA == candidate.TargetSHA || previous.Generation != req.ExpectedGeneration {
		return nil, nil, ErrTargetAdvanceIneligible
	}
	if req.SourceSHA != nil && !validGitSHA(*req.SourceSHA) {
		return nil, nil, fmt.Errorf("invalid remote source SHA")
	}
	if r.TargetAdvanceRewinds >= int64(req.MaxRewinds) {
		return nil, nil, fmt.Errorf("%w (rebase.max_target_advance_rewinds=%d)", ErrTargetAdvanceCap, req.MaxRewinds)
	}
	return r, pending, nil
}

// MarkRunRetargetAttempted is monotonic. An uncertain forge call counts as an
// attempt, so this write must succeed BEFORE calling the forge.
func (d *DB) MarkRunRetargetAttempted(runID string) error {
	result, err := d.sql.Exec(`UPDATE runs SET retarget_attempted=1,updated_at=? WHERE id=?`, now(), runID)
	if err != nil {
		return fmt.Errorf("record PR retarget attempt: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("record PR retarget attempt: run not found")
	}
	return nil
}
