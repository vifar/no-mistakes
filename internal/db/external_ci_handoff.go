package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PendingExternalCISupport returns only pending CI claims from this run's
// completed Review under its current approved PR comparison. It is a request
// for external verification, never a result of that verification.
func (d *DB) PendingExternalCISupport(run *Run) ([]types.PendingCISupport, error) {
	if run == nil || run.ExternalCIOwner == "" {
		return nil, nil
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		return nil, err
	}
	var review *StepResult
	for _, step := range steps {
		if step.StepName == types.StepReview && step.Status == types.StepStatusCompleted {
			if review != nil {
				return nil, fmt.Errorf("multiple completed Review steps for external CI handoff")
			}
			review = step
		}
	}
	if review == nil || review.FindingsJSON == nil {
		return nil, nil
	}
	findings, err := types.ParseFindingsJSON(*review.FindingsJSON)
	if err != nil {
		return nil, fmt.Errorf("read Review support claims: %w", err)
	}
	var claims []types.Finding
	for _, finding := range findings.Items {
		if finding.Category != types.FindingCategoryReviewSupportPending || finding.Support == nil || finding.Support.ClaimType != types.FindingClaimCI {
			continue
		}
		if finding.ID == "" || finding.Support.OwnerResult != nil || finding.Support.CI == nil {
			return nil, fmt.Errorf("invalid pending Review CI support claim")
		}
		if err := finding.Support.Validate(); err != nil {
			return nil, fmt.Errorf("validate pending Review CI support claim: %w", err)
		}
		if len(finding.ID) > 1024 || len(finding.Support.CI.CheckID) > 1024 || strings.TrimSpace(finding.Support.CI.CheckID) != finding.Support.CI.CheckID || !validGitSHA(finding.Support.CI.HeadSHA) {
			return nil, fmt.Errorf("pending Review CI support claim exceeds handoff bounds or has invalid historical head")
		}
		claims = append(claims, finding)
	}
	if len(claims) == 0 {
		return nil, nil
	}
	if len(claims) > 128 {
		return nil, fmt.Errorf("pending Review CI support exceeds 128 handoff claims")
	}
	receipt, err := d.GetRunPRContext(run.ID)
	if err != nil {
		return nil, err
	}
	if receipt == nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA == "" ||
		run.HeadSHA != receipt.LocalHeadSHA ||
		strings.TrimSpace(receipt.SourceRepo) == "" || strings.TrimSpace(receipt.SourceBranch) == "" {
		return nil, fmt.Errorf("pending Review CI support has no current approved PR comparison")
	}
	rounds, err := d.GetRoundsByStep(review.ID)
	if err != nil {
		return nil, fmt.Errorf("read approved Review rounds: %w", err)
	}
	approvedRoundHead := ""
	for _, round := range rounds {
		if round.ReviewedHeadSHA != nil {
			approvedRoundHead = *round.ReviewedHeadSHA
		}
	}
	if approvedRoundHead == "" || approvedRoundHead != *run.ReviewApprovedHeadSHA {
		return nil, fmt.Errorf("pending Review CI support has no matching approved Review round")
	}
	if err := d.ValidatePipelineHeadForward(run.ID, approvedRoundHead, receipt.LocalHeadSHA); err != nil {
		return nil, err
	}
	result := make([]types.PendingCISupport, 0, len(claims))
	for _, claim := range claims {
		result = append(result, types.PendingCISupport{
			ClaimID: types.ReviewSupportClaimID(claim), OriginalFindingID: claim.ID,
			HistoricalCheckID: claim.Support.CI.CheckID, HistoricalHeadSHA: claim.Support.CI.HeadSHA,
			SourceHeadSHA: receipt.LocalHeadSHA, ForgeHeadSHA: receipt.ForgeHeadSHA,
			TargetBranch: receipt.TargetBranch, TargetSHA: receipt.TargetSHA,
			DiffDigest: receipt.DiffDigest, ReceiptGeneration: receipt.Generation,
			RunID: run.ID, PRURL: receipt.PRURL, Owner: run.ExternalCIOwner,
		})
	}
	return result, nil
}

func (d *DB) RecordPipelineCommitHead(runID, parentSHA, headSHA string) error {
	if !validGitSHA(parentSHA) || !validGitSHA(headSHA) || parentSHA == headSHA {
		return fmt.Errorf("invalid pipeline commit head proof")
	}
	_, err := sqlitewrite.Exec(context.Background(), d.sql, "RecordPipelineCommitHead", `INSERT INTO pipeline_commit_heads (run_id, head_sha, parent_sha) VALUES (?, ?, ?)`, runID, headSHA, parentSHA)
	return err
}

func (d *DB) ValidatePipelineHeadForward(runID, approvedSHA, headSHA string) error {
	seen := map[string]bool{}
	for headSHA != approvedSHA {
		if seen[headSHA] {
			return fmt.Errorf("cyclic pipeline commit head proof")
		}
		seen[headSHA] = true
		var parent string
		err := d.sql.QueryRow(`SELECT parent_sha FROM pipeline_commit_heads WHERE run_id = ? AND head_sha = ?`, runID, headSHA).Scan(&parent)
		if err == sql.ErrNoRows {
			return fmt.Errorf("external CI handoff head has no pipeline-authored path to approved Review")
		}
		if err != nil {
			return fmt.Errorf("read pipeline commit head proof: %w", err)
		}
		headSHA = parent
	}
	return nil
}
