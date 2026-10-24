package steps

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FindingIDTestLeftoverEdits is the auto-fixable Test finding for uncommitted
// edits the Test agents left in the run worktree after the run's one
// automatic leftover correction was already spent. The repair turn commits
// them (validation then re-runs on the new head) or reverts them.
const FindingIDTestLeftoverEdits = "test-leftover-edits"

// settleTestLeftoverEdits makes the Test step own the uncommitted edits its
// agents left in the run worktree before the worktree reaches the next step.
//
// The evidence turn may write or improve a focused test, but nothing commits
// what it writes. Left alone, the edits reach Document, which refuses a dirty
// worktree. Document's boundary commit absorbs the first set, but its budget
// is one per run: on the re-validation that follows, the Test agent wrote the
// same file again and Document killed the run (vifar/controller#8241).
//
// The step that made the edits settles them itself:
//
//   - While the run's leftover budget allows, a passing Test commits them as
//     an attributed "no-mistakes(test): keep test validation edits" commit and
//     sends the run back to Review, so they are reviewed and re-validated
//     before anything is published.
//   - Once the budget is spent, Test does not complete. It reports the edits
//     as an auto-fixable finding, so a repair turn commits them through the
//     normal Test fix path (validation re-runs on that head) or reverts them,
//     and it refuses Approve, which would carry them on unreviewed.
//
// A gate that already blocks with budget left is unchanged: its fix round
// commits the edits, and an approval still meets Document's boundary commit.
//
// Test claims the worktree only when its evidence turn dirtied a path that was
// clean before the turn (dirtyBefore). Output from a configured command and
// work already present when Test started are not its edits, and stay with
// Document's boundary as before.
func settleTestLeftoverEdits(sctx *pipeline.StepContext, findings *Findings, needsApproval bool, dirtyBefore []string) (*pipeline.StepOutcome, bool, error) {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil {
		return nil, false, nil
	}
	dirty, err := worktreeDirtyPathsOutsideScratch(sctx)
	if err != nil {
		return nil, false, err
	}
	if !slices.ContainsFunc(dirty, func(path string) bool { return !slices.Contains(dirtyBefore, path) }) {
		return nil, false, nil
	}
	allowed, err := sctx.DB.CanCommitLeftoverValidationEdits(sctx.Run.ID)
	if err != nil {
		return nil, false, err
	}
	if allowed {
		if needsApproval {
			return nil, false, nil
		}
		head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return nil, false, fmt.Errorf("resolve head before committing test validation edits: %w", err)
		}
		if head != sctx.Run.HeadSHA {
			// Another owner attributes a head that already moved.
			return nil, false, nil
		}
		committed, newHead, err := commitWorktreeEdits(sctx, string(types.StepTest), "at the end of the Test step")
		if err != nil {
			return nil, false, leftoverPredecessorEditsRefusal(sctx, string(types.StepTest), err)
		}
		if !committed {
			return nil, false, nil
		}
		if err := recordPipelineHeadRange(sctx, head, newHead); err != nil {
			return nil, false, err
		}
		if err := sctx.DB.RecordLeftoverValidationCommit(sctx.Run.ID, newHead); err != nil {
			return nil, false, err
		}
		sctx.Run.HeadSHA = newHead
		sctx.Log("returning to Review so the committed test validation edits are reviewed and re-validated")
		return &pipeline.StepOutcome{RestartFrom: types.StepReview}, false, nil
	}
	named := namedPaths(dirty)
	sctx.Log(fmt.Sprintf("the Test agents left uncommitted edits to %s and this run's leftover correction is spent; reporting them", named))
	findings.Items = append(findings.Items,
		Finding{
			ID:       FindingIDTestLeftoverEdits,
			Severity: types.FindingSeverityError,
			Action:   types.ActionAutoFix,
			File:     dirty[0],
			Description: fmt.Sprintf(
				"The Test agents left uncommitted edits to %s, and this run already committed one set of leftover Test edits, so they are not committed again automatically. "+
					"If validation needs them, keep them so this repair commits them and Test re-validates the new head; otherwise revert them.",
				named),
		},
		Finding{
			ID:       types.FindingIDTestAgentUnvalidatedWork,
			Severity: types.FindingSeverityError,
			Action:   types.ActionAskUser,
			Description: fmt.Sprintf(
				"Approval is refused: the run worktree at %s holds uncommitted changes to %s that no Test turn committed, and the steps after Test would carry them on unreviewed (inspect with `git -C %s status` and `git -C %s diff`). Respond with fix to commit and re-validate them, or abort.",
				sctx.WorkDir, named, sctx.WorkDir, sctx.WorkDir),
		},
	)
	return nil, true, nil
}

func namedPaths(paths []string) string {
	const maxNamed = 10
	named := strings.Join(paths[:min(len(paths), maxNamed)], ", ")
	if len(paths) > maxNamed {
		named += fmt.Sprintf(" and %d more", len(paths)-maxNamed)
	}
	return named
}

// documentDirtyEntryOutcome is Document's answer when the predecessor's edits
// could not be preserved at its clean-entry check. Edits the Test step left
// after the run's leftover budget is spent go back to Test, which owns them:
// it reports them for a repair turn instead of completing, so the run is not
// killed over work a fix round can commit. Anything else still fails closed,
// naming the step that left the edits and the way forward.
func documentDirtyEntryOutcome(sctx *pipeline.StepContext, dirtyPaths []string) (*pipeline.StepOutcome, error) {
	producer, err := lastCompletedValidationStep(sctx, types.StepDocument)
	if err != nil {
		return nil, err
	}
	spent := false
	if sctx.DB != nil && sctx.Run != nil {
		allowed, err := sctx.DB.CanCommitLeftoverValidationEdits(sctx.Run.ID)
		if err != nil {
			return nil, err
		}
		spent = !allowed
	}
	if producer == string(types.StepTest) && spent {
		sctx.Log(fmt.Sprintf("the test step left uncommitted edits to %s after this run's leftover correction was spent; returning to Test, which reports them for a repair", namedPaths(dirtyPaths)))
		return &pipeline.StepOutcome{RestartFrom: types.StepTest}, nil
	}
	return nil, documentDirtyEntryError(sctx, producer, spent, dirtyPaths)
}

// documentDirtyEntryError is the clean-entry refusal. It keeps the original
// wording and adds which step left the edits and how to go on.
func documentDirtyEntryError(sctx *pipeline.StepContext, producer string, spent bool, dirtyPaths []string) error {
	message := fmt.Sprintf("Document requires a clean worktree so convergence can safely discard only its own edits: %s", strings.Join(dirtyPaths, ", "))
	var detail []string
	if producer != "" {
		detail = append(detail, fmt.Sprintf("the %s step left them uncommitted", producer))
	}
	if spent {
		detail = append(detail, "this run already committed one set of leftover edits and sent it through re-review, so another is not committed unreviewed")
	}
	detail = append(detail, fmt.Sprintf("inspect them with `git -C %s status` and `git -C %s diff`; to keep them, commit them on the branch and push it again, otherwise abort this run", sctx.WorkDir, sctx.WorkDir))
	return fmt.Errorf("%s (%s)", message, strings.Join(detail, "; "))
}
