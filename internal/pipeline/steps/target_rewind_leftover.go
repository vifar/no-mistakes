package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// commitLeftoverValidationEdits commits the uncommitted worktree edits a
// previous step left behind, so a target-advance rewind can integrate.
//
// A step's agent has free shell access, so it can leave edits in the worktree
// it was never asked to commit. Git then refuses the rewind integration
// ("cannot rebase: You have unstaged changes"), and the run dies on a base
// that merely moved - exactly the restart this path exists to prevent.
//
// The edits are COMMITTED, not discarded:
//
//   - Discarding them would destroy work the agent produced and validated. The
//     measured case was a real product change (a vitest route-wait fix) that
//     the Test agent validated but did not commit.
//   - Requiring the agent to leave the worktree clean is a promise the
//     pipeline cannot enforce, and enforcing it by failing is a false failure:
//     an evidence turn is allowed to write scratch (a live test database, for
//     example) and that scratch must not kill the run.
//
// The commit is a pipeline correction commit, attributed to the step that left
// the edits, and it goes through the normal revalidation: the rewind resets
// Review..Lint, so the committed edits are re-reviewed and re-tested on the
// new head before anything is published.
//
// The commit is scoped to what a correction commit can carry: tracked
// modifications and additions that survive the pipeline's scratch rules. Tool
// scratch is excluded under the automatic staging rules; staged scratch is
// unstaged without changing its worktree contents, even if the correction
// budget is spent, so it does not leave an index that blocks integration.
func commitLeftoverValidationEdits(c *pipeline.StepContext) (bool, error) {
	if c == nil || c.DB == nil || c.Run == nil {
		return false, nil
	}
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil {
		return false, err
	}
	if pending == nil {
		return false, nil
	}
	head, err := git.HeadSHA(c.Ctx, c.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head before committing leftover validation edits: %w", err)
	}
	if head != pending.LocalHeadSHA {
		return false, nil
	}
	producer, err := rewindProducerStep(c)
	if err != nil {
		return false, err
	}
	if producer == "" {
		producer = string(types.StepTest)
	}
	// Keep execution evidence BEFORE staging, unstaging scratch, or spending
	// the correction budget. A refusal must retain the same evidence as a
	// successful correction, independently of disposable-worktree cleanup.
	if err := snapshotLeftoverValidationEdits(c, producer, head); err != nil {
		// A terminal error would let cleanup delete the only copy of the
		// unsaved edits. Use the existing automatic-commit refusal gate.
		return false, &pipeline.ProtectedPathError{
			Rule:   "leftover validation evidence",
			Detail: fmt.Sprintf("save leftover validation evidence: %v; index and worktree preserved, restore the snapshot destination before retrying", err),
		}
	}
	if err := unstageIndexedScratch(c); err != nil {
		return false, err
	}
	allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID)
	if err != nil {
		return false, err
	}
	if !allowed {
		paths, err := leftoverDirtyPaths(c)
		if err != nil {
			return false, err
		}
		if len(paths) > 0 {
			return false, fmt.Errorf("leftover validation correction budget already spent")
		}
		return false, nil
	}
	committed, newHead, err := commitWorktreeEdits(c, producer, "before the target-advance rewind")
	if err != nil {
		return false, err
	}
	if !committed {
		return false, nil
	}
	if err := recordPipelineHeadRange(c, head, newHead); err != nil {
		return false, err
	}
	// The new head must be a descendant of the head the pending rewind was
	// classified against. Git is the only thing that can prove it.
	descendant := false
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", head, newHead); err == nil {
		descendant = true
	}
	if err := c.DB.AdvancePendingTargetRewindHead(c.Run.ID, head, newHead, descendant); err != nil {
		return false, err
	}
	if err := c.DB.RecordLeftoverValidationCommit(c.Run.ID, newHead); err != nil {
		return false, err
	}
	c.Run.HeadSHA = newHead
	// The receipt's local head follows the pipeline's own commit, exactly as
	// it does for any other forward commit.
	previous, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		return false, err
	}
	if previous != nil {
		own, err := prComparisonAtTarget(c, previous.TargetBranch, previous.TargetSHA, newHead)
		if err != nil {
			return false, err
		}
		own.PRURL, own.SourceRepo, own.SourceBranch, own.ForgeHeadSHA = previous.PRURL, previous.SourceRepo, previous.SourceBranch, previous.ForgeHeadSHA
		if _, err := c.DB.AdvanceRunPRContext(c.Run.ID, own); err != nil {
			return false, err
		}
	}
	if c.Log != nil {
		c.Log(fmt.Sprintf("the pending rewind now integrates head %s", shortObjectID(newHead)))
	}
	return true, nil
}

// commitWorktreeEdits stages and commits the worktree's uncommitted edits as
// a pipeline correction commit attributed to producer. It returns the new head
// when it committed, and false when there was nothing to commit.
//
// The commit is a pipeline correction commit: hook verification is bypassed
// (the run worktree is carved from the bare gate repo, so tracked hooks that
// need generated runtime files cannot run there) and the subject names the
// step that produced the edits.
func commitWorktreeEdits(c *pipeline.StepContext, producer, reason string) (bool, string, error) {
	status, err := git.Run(c.Ctx, c.WorkDir, "status", "--porcelain")
	if err != nil {
		return false, "", fmt.Errorf("check leftover validation edits: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return false, "", nil
	}
	head, err := git.HeadSHA(c.Ctx, c.WorkDir)
	if err != nil {
		return false, "", fmt.Errorf("resolve head before committing leftover validation edits: %w", err)
	}
	summary := fmt.Sprintf("keep %s validation edits", producer)
	message := fmt.Sprintf("no-mistakes(%s): %s", producer, summary)
	if c.Config != nil {
		rendered, renderErr := c.Config.Commit.RenderFixMessageForBranch(types.StepName(producer), summary, c.Run.Branch)
		if renderErr != nil {
			return false, "", fmt.Errorf("render leftover validation commit message: %w", renderErr)
		}
		message = rendered
	}
	if err := stagePipelineChanges(c); err != nil {
		return false, "", fmt.Errorf("stage leftover validation edits: %w", err)
	}
	if err := commitPipelineCorrection(c.Ctx, c.WorkDir, message, c.Log); err != nil {
		return false, "", fmt.Errorf("commit leftover validation edits: %w", err)
	}
	newHead, err := git.HeadSHA(c.Ctx, c.WorkDir)
	if err != nil {
		return false, "", fmt.Errorf("resolve head after committing leftover validation edits: %w", err)
	}
	if newHead == head {
		// Scratch-safe staging can leave nothing to commit. The worktree is
		// still dirty with untracked scratch, which never blocks the rebase.
		return false, "", nil
	}
	if c.Log != nil {
		c.Log(fmt.Sprintf("committed leftover %s validation edits as %s %s", producer, shortObjectID(newHead), reason))
	}
	return true, newHead, nil
}

// commitPredecessorValidationEdits commits the edits a completed predecessor
// step left in the worktree, and asks the caller to restart from Review.
//
// It is the step-boundary half of the same rule the rewind path uses: an
// agent's uncommitted edits are preserved as an attributed pipeline commit
// rather than killing the run or being discarded. The restart from Review is
// what keeps the safety property the clean-worktree check protects: the
// committed edits are re-reviewed before anything is published, so no
// unreviewed agent edit ever reaches the branch.
//
// It returns false without committing when only tool scratch remains. The
// caller excludes that scratch from its clean-entry check too.
func commitPredecessorValidationEdits(c *pipeline.StepContext) (bool, error) {
	if c == nil || c.DB == nil || c.Run == nil {
		return false, nil
	}
	allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID)
	if err != nil {
		return false, err
	}
	if !allowed {
		// The budget is spent: leave the edits for the caller's clean-entry
		// handling in documentDirtyEntryOutcome.
		return false, nil
	}
	head, err := git.HeadSHA(c.Ctx, c.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head before committing predecessor validation edits: %w", err)
	}
	if head != c.Run.HeadSHA {
		// The head already moved: another owner attributes that movement.
		return false, nil
	}
	producer, err := lastCompletedValidationStep(c, types.StepDocument)
	if err != nil {
		return false, err
	}
	if producer == "" {
		return false, nil
	}
	committed, newHead, err := commitWorktreeEdits(c, producer, "before the documentation pass")
	if err != nil {
		return false, leftoverPredecessorEditsRefusal(c, producer, err)
	}
	if !committed {
		return false, nil
	}
	if err := recordPipelineHeadRange(c, head, newHead); err != nil {
		return false, err
	}
	if err := c.DB.RecordLeftoverValidationCommit(c.Run.ID, newHead); err != nil {
		return false, err
	}
	c.Run.HeadSHA = newHead
	return true, nil
}

// rewindProducerStep names the step whose agent left the edits a pending
// rewind is about to preserve.
//
// The LIVE step rows are useless here: the rewind's own classification
// transaction reset every step from Rebase to pending, so a lookup over them
// finds no completed validation step and blames the wrong one. The ARCHIVED
// rewind history is the evidence. At a post-step boundary, BoundaryStep names
// the producer even though its archived status can still be running. At an
// entry boundary, the last completed archived step supplies the producer.
// Repository gates are attributed to their core anchor in either case.
func rewindProducerStep(c *pipeline.StepContext) (string, error) {
	history, err := c.DB.GetTargetRewindHistory(c.Run.ID)
	if err != nil {
		return "", err
	}
	if len(history) == 0 {
		return "", nil
	}
	latest := history[len(history)-1]
	if latest.BoundaryStep != nil {
		name := types.StepName(*latest.BoundaryStep)
		if anchor, ok := name.CustomGateAnchor(); ok {
			name = anchor
		}
		return string(name), nil
	}
	producer := ""
	for _, entry := range latest.Steps {
		if entry["status"] != string(types.StepStatusCompleted) {
			continue
		}
		rawName, _ := entry["step_name"].(string)
		name := types.StepName(rawName)
		if anchor, ok := name.CustomGateAnchor(); ok {
			name = anchor
		}
		switch string(name) {
		case string(types.StepRebase), string(types.StepReview), string(types.StepTest), string(types.StepDocument), string(types.StepLint):
			producer = string(name)
		}
	}
	return producer, nil
}

// lastCompletedValidationStep names the last core step that COMPLETED in this
// run, which is the step whose agent could have left the current worktree
// edits. It is execution evidence, not the name of the boundary that
// happened to notice them: a target that advances after Test but before
// Document still attributes Test's edits to Test.
//
// A repository gate is a possible producer too, so the gate's own anchor
// step counts. The step that is CURRENTLY running is never the producer: it
// has not left anything behind yet.
func lastCompletedValidationStep(c *pipeline.StepContext, current types.StepName) (string, error) {
	rows, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		return "", err
	}
	producer := ""
	for _, row := range rows {
		if row.Status != types.StepStatusCompleted {
			continue
		}
		name := row.StepName
		if anchor, ok := name.CustomGateAnchor(); ok {
			name = anchor
		}
		if name == current {
			continue
		}
		switch name {
		case types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint:
			producer = string(name)
		}
	}
	return producer, nil
}

// leftoverPredecessorEditsRefusal names the step and the dirty paths when the
// predecessor's edits cannot be committed.
func leftoverPredecessorEditsRefusal(c *pipeline.StepContext, producer string, err error) error {
	paths, pathsErr := leftoverDirtyPaths(c)
	if pathsErr != nil || len(paths) == 0 {
		return fmt.Errorf("%w (the %s step left uncommitted edits in the run worktree)", err, producer)
	}
	return fmt.Errorf("%w (the %s step left uncommitted edits in the run worktree: %s)", err, producer, strings.Join(paths, ", "))
}

// pendingStepName supplies a diagnostic fallback when archived producer
// evidence is unavailable. The detecting boundary alone does not prove who
// left the edits.
func pendingStepName(c *pipeline.StepContext, detected types.StepName) string {
	detected = types.StepName(strings.TrimSpace(string(detected)))
	switch detected {
	case types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush:
		return string(detected)
	}
	return string(types.StepTest)
}

// leftoverValidationEditsRefusal names the step and the dirty paths when the
// leftover edits cannot be committed, so the failure says which step left
// them instead of git's generic message.
func leftoverValidationEditsRefusal(c *pipeline.StepContext, err error) error {
	pending, pendingErr := c.DB.GetPendingTargetRewind(c.Run.ID)
	if pendingErr != nil {
		return err
	}
	if pending == nil {
		return err
	}
	producer, producerErr := rewindProducerStep(c)
	if producerErr != nil || producer == "" {
		producer = pendingStepName(c, pending.Step)
	}
	paths, pathsErr := leftoverDirtyPaths(c)
	if pathsErr != nil || len(paths) == 0 {
		return fmt.Errorf("%w (the %s step left uncommitted edits in the run worktree)", err, producer)
	}
	return fmt.Errorf("%w (the %s step left uncommitted edits in the run worktree: %s)", err, producer, strings.Join(paths, ", "))
}

// leftoverDirtyPaths lists the paths that made the worktree dirty, excluding
// recognized tool scratch. Scratch is not an edit, so naming it in a refusal
// would point the operator at the wrong thing.
func leftoverDirtyPaths(c *pipeline.StepContext) ([]string, error) {
	paths, err := worktreeDirtyPathsOutsideScratch(c)
	if err != nil {
		return nil, err
	}
	return paths, nil
}
