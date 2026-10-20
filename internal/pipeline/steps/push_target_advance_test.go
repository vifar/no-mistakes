package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// publicationFixture builds a run that has already completed Review, Test,
// Document and Lint against a pinned target, with a live remote the target can
// advance on. It returns the step context and the selection the guard uses.
func publicationFixture(t *testing.T) (*pipeline.StepContext, pipeline.PRTargetSelection) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	// The actual remote is independent of the pipeline worktree.
	remote := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, dir, "clone", "--bare", dir, remote)
	gitCmd(t, dir, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, dir, "remote", "set-url", "origin", remote)
	c.Repo.UpstreamURL = remote
	c.Config.Rebase.MaxTargetAdvanceRewinds = 3
	selected := pipeline.PRTargetSelection{TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	// The run finished its ordinary validation at this head.
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		step, err := c.DB.InsertStepResult(c.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.DB.SetStepFindings(step.ID, `{"findings":[],"reviewed_paths":["feature.txt"]}`); err != nil {
			t.Fatal(err)
		}
		if name == types.StepReview {
			err = c.DB.CompleteReviewStep(step.ID, c.Run.ID, head, 0, 1, "review.log")
		} else {
			err = c.DB.CompleteStep(step.ID, 0, 1, "step.log")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	recordReviewApproval(t, c, head)
	return c, selected
}

// TestPushStep_TargetAdvanceBetweenLintAndPushRewindsInsteadOfFailing is the
// measured controller failure: the run completed Review, Test, Document and
// Lint at c05a30daff with no findings, dev moved under it, and the push failed
// with "live target commit, merge base, or diff differs from PR comparison
// receipt before update". The push must take the safe rewind path instead.
func TestPushStep_TargetAdvanceBetweenLintAndPushRewindsInsteadOfFailing(t *testing.T) {
	c, _ := publicationFixture(t)
	head := c.Run.HeadSHA

	// dev moves under the run, after the last step boundary.
	advanceRewindTarget(t, c)

	outcome, err := (&PushStep{}).Execute(c)
	if err != nil {
		t.Fatalf("push failed on a target advance: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepRebase {
		t.Fatalf("push did not request a rewind: %+v", outcome)
	}
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil || pending == nil {
		t.Fatalf("no pending rewind recorded: %+v %v", pending, err)
	}
	if pending.OldTargetSHA == pending.TargetSHA {
		t.Fatalf("rewind recorded no target advance: %+v", pending)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil || run.TargetAdvanceRewinds != 1 {
		t.Fatalf("rewind was not charged: %+v %v", run, err)
	}
	// Nothing was published: the rewind is the pre-publication path.
	if run.LastPushedSHA != nil {
		t.Fatalf("a rewound push published anyway: %+v", run.LastPushedSHA)
	}
	// The run's validation evidence is archived, not silently dropped.
	history, err := c.DB.GetTargetRewindHistory(c.Run.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("missing durable rewind history: %+v %v", history, err)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("push moved the head before the rewind: %s want %s", got, head)
	}
}

// TestPushStep_TargetAdvanceRewindReValidatesBeforePublishing proves the
// rewind is the SAME safe path the Review..Lint boundaries use: the run
// returns to Rebase, re-integrates the new target, and the validation steps
// run again on the new head before anything is pushed.
func TestPushStep_TargetAdvanceRewindReValidatesBeforePublishing(t *testing.T) {
	c, selected := publicationFixture(t)
	advanceRewindTarget(t, c)

	outcome, err := (&PushStep{}).Execute(c)
	if err != nil || outcome == nil || outcome.RestartFrom != types.StepRebase {
		t.Fatalf("push did not rewind: %+v %v", outcome, err)
	}
	// The executor restarts at Rebase. The pending rewind drives the checked
	// integration onto the advanced target.
	c.PRContextAfterStep, c.PRContextRebaseCompleted = false, false
	out, err := (&RebaseStep{}).Execute(c)
	if err != nil || out == nil || out.NeedsApproval {
		t.Fatalf("rewind integration failed: %+v %v", out, err)
	}
	c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	c.PRContextRebaseCompleted = false

	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil || pending != nil {
		t.Fatalf("pending rewind did not complete: %+v %v", pending, err)
	}
	// The new head contains the advanced target.
	rebased := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", pendingTargetSHA(t, c), rebased); err != nil {
		t.Fatalf("rewound head does not contain the advanced target: %v", err)
	}
	// The validation steps are pending again: they re-run on the new head.
	rows, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == types.StepReview || row.StepName == types.StepTest ||
			row.StepName == types.StepDocument || row.StepName == types.StepLint {
			if row.Status != types.StepStatusPending {
				t.Fatalf("%s kept its old-head evidence after the rewind: %+v", row.StepName, row)
			}
		}
	}
}

// TestPushStep_TargetAdvanceConflictStillParks proves a REAL conflict still
// parks at a gate instead of silently pushing.
func TestPushStep_TargetAdvanceConflictStillParks(t *testing.T) {
	c, _ := publicationFixture(t)
	// The advanced target touches the same file the branch changed.
	gitCmd(t, c.WorkDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("conflicting target content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "feature.txt")
	gitCmd(t, c.WorkDir, "commit", "-m", "conflicting target")
	gitCmd(t, c.WorkDir, "push", "origin", "main")
	gitCmd(t, c.WorkDir, "checkout", "feature")

	outcome, err := (&PushStep{}).Execute(c)
	if err != nil {
		t.Fatalf("push failed instead of rewinding: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepRebase {
		t.Fatalf("push did not request a rewind: %+v", outcome)
	}
	c.PRContextAfterStep = false
	out, err := (&RebaseStep{}).Execute(c)
	if err != nil {
		t.Fatalf("conflicting rewind failed hard instead of parking: %v", err)
	}
	if out == nil || !out.NeedsApproval || !strings.Contains(out.Findings, "conflict on rebase") {
		t.Fatalf("conflict did not park clearly: %+v", out)
	}
}

// TestPushStep_AlreadyPublishedRunCannotRewind proves the rewind is refused
// once the run published: a published head is not this run's to rewrite.
func TestPushStep_AlreadyPublishedRunCannotRewind(t *testing.T) {
	c, _ := publicationFixture(t)
	if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: c.Run.HeadSHA}); err != nil {
		t.Fatal(err)
	}
	advanceRewindTarget(t, c)

	outcome, err := (&PushStep{}).Execute(c)
	if err != nil {
		t.Fatalf("push failed on a published run: %v", err)
	}
	if outcome != nil && outcome.RestartFrom != "" {
		t.Fatalf("a published run rewound: %+v", outcome)
	}
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil || pending != nil {
		t.Fatalf("a published run recorded a pending rewind: %+v %v", pending, err)
	}
}

// pendingTargetSHA reads the advanced target the fixture's remote carries.
func pendingTargetSHA(t *testing.T, c *pipeline.StepContext) string {
	t.Helper()
	out, err := git.Run(c.Ctx, c.WorkDir, "rev-parse", "--verify", "refs/remotes/origin/main^{commit}")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}
