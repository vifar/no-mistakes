package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// retainPublishedComparison deliberately leaves the qualification receipt at
// its original comparison. It never restamps old evidence against a new base.
func retainPublishedComparison(c *pipeline.StepContext, host scm.Host, previous *db.PRContext, candidate db.PRContextCandidate) (bool, error) {
	if c.Run.Shipping != nil || previous == nil || !sameRewindIdentity(previous, candidate) || previous.PRURL == "" ||
		previous.LocalHeadSHA != candidate.LocalHeadSHA || candidate.ForgeHeadSHA != candidate.LocalHeadSHA || previous.TargetSHA == candidate.TargetSHA {
		return false, nil
	}
	current, err := c.DB.GetRun(c.Run.ID)
	if err != nil {
		return false, err
	}
	if current == nil || current.HeadSHA != candidate.LocalHeadSHA || current.LastPushedSHA == nil || *current.LastPushedSHA != candidate.LocalHeadSHA || current.ReviewApprovedHeadSHA == nil || *current.ReviewApprovedHeadSHA != candidate.LocalHeadSHA {
		return false, nil
	}
	qualified := previous.CIQualified || current.CIReadyAt != nil
	if !qualified {
		publications, err := c.DB.GetCommitStatusPublications(c.Ctx, c.Run.ID)
		if err != nil {
			return false, err
		}
		for _, publication := range publications {
			qualified = qualified || publication.SHA == candidate.LocalHeadSHA && publication.Validated && publication.LastState == "success"
		}
	}
	if !qualified {
		return false, nil
	}
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", previous.TargetSHA, candidate.TargetSHA); err != nil {
		return false, fmt.Errorf("published target did not fast-forward: %w", err)
	}
	policy, ok := host.(scm.BranchUpdatePolicyReader)
	if !ok {
		return false, nil
	}
	strict, err := policy.RequiresUpToDate(c.Ctx, prFromOwnedURL(previous.PRURL), previous.TargetBranch)
	if err != nil {
		return false, fmt.Errorf("verify qualified head protection: %w", err)
	}
	if strict {
		return false, nil
	}
	if !host.Capabilities().MergeableState {
		return false, nil
	}
	state, err := host.GetMergeableState(c.Ctx, prFromOwnedURL(previous.PRURL))
	if err != nil {
		return false, err
	}
	if state != scm.MergeableOK {
		return false, fmt.Errorf("qualified head mergeability is %s after target advance", state)
	}
	return true, nil
}

// reusableRewindReview requires both an identical canonical PR patch and a
// conflict-free three-way merge that produces exactly the integrated tree.
// Tests and lint still run on the new head; old gate answers remain history.
func reusableRewindReview(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if c.DB == nil || c.Run.Shipping != nil || c.Fixing {
		return nil, nil
	}
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, nil
	}
	history, err := c.DB.GetTargetRewindHistory(c.Run.ID)
	if err != nil {
		return nil, err
	}
	if len(history) == 0 {
		return nil, nil
	}
	h := history[len(history)-1]
	// Consecutive advances before integration archive only reset steps. Walk
	// those empty generations back, but never cross a newer review attempt.
	for i := len(history) - 1; i > 0; i-- {
		attempted := h.ReviewHeadSHA != nil || h.BoundaryStep != nil && *h.BoundaryStep == string(types.StepReview)
		for _, step := range h.Steps {
			attempted = attempted || step["step_name"] == string(types.StepReview) && step["status"] != string(types.StepStatusPending)
		}
		if attempted {
			break
		}
		h = history[i-1]
	}
	var boundary *pipeline.StepOutcome
	if h.BoundaryStep != nil && *h.BoundaryStep == string(types.StepReview) && h.BoundaryJSON != nil {
		var out pipeline.StepOutcome
		if json.Unmarshal([]byte(*h.BoundaryJSON), &out) == nil && !out.NeedsApproval && !out.Skipped && !out.SkipRemaining && out.RestartFrom == "" && out.ReviewApprovedHeadSHA == h.Comparison.LocalHeadSHA && reviewedPathsCoverReviewable(out.ReviewedPaths, out.ReviewablePaths) {
			boundary = &out
		}
	}
	if boundary == nil && (h.ReviewHeadSHA == nil || *h.ReviewHeadSHA != h.Comparison.LocalHeadSHA) {
		return nil, nil
	}
	current, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		return nil, err
	}
	if current == nil || current.LocalHeadSHA != c.Run.HeadSHA || current.TargetBranch != h.Comparison.TargetBranch || current.DiffDigest != h.Comparison.DiffDigest {
		return nil, nil
	}
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", h.Comparison.TargetSHA, current.TargetSHA); err != nil {
		return nil, nil
	}
	tree, err := git.Run(c.Ctx, c.WorkDir, "merge-tree", "--write-tree", current.TargetSHA, h.Comparison.LocalHeadSHA)
	if err != nil {
		return nil, nil
	} // Conflicts never carry credit.
	headTree, err := git.Run(c.Ctx, c.WorkDir, "rev-parse", current.LocalHeadSHA+"^{tree}")
	if err != nil {
		return nil, err
	}
	if tree != headTree {
		return nil, nil
	}
	if boundary != nil {
		findings, err := types.ParseFindingsJSON(boundary.Findings)
		if err != nil || len(findings.Items) != 0 {
			return nil, nil
		}
		boundary.ReviewApprovedHeadSHA = current.LocalHeadSHA
		return boundary, nil
	}
	for _, step := range h.Steps {
		if step["step_name"] != string(types.StepReview) || step["status"] != string(types.StepStatusCompleted) || step["override_reason"] != nil {
			continue
		}
		raw, _ := step["findings_json"].(string)
		findings, err := types.ParseFindingsJSON(raw)
		if err != nil || len(findings.Items) != 0 {
			return nil, nil
		}
		if c.Log != nil {
			c.Log("retaining clean review after conflict-free target integration with identical PR diff")
		}
		return approvedReviewOutcome(current.LocalHeadSHA, &pipeline.StepOutcome{Findings: raw, ReviewedPaths: findings.ReviewedPaths, ReviewablePaths: findings.ReviewedPaths})
	}
	return nil, nil
}
