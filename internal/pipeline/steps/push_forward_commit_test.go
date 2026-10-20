package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPushStep_FormatterCommitAtThePublicationBoundaryIsThePipelineOwn proves
// a commit the PUSH step itself made does not fail the run at the
// publication boundary.
//
// The measured failure: the repository's formatter (`gofmt -w .`) removed a
// trailing blank line, the Push step committed that change as "no-mistakes:
// apply agent fixes", and the after-push comparison guard then refused the
// moved head with "PR comparison changed after review; start a new run for
// the current target and head". The head advance was the pipeline's own, and
// the recorded commit lineage proves it.
//
// The step's own outcome asks for a re-review when the head moved, so the run
// restarts at Review instead of failing.
func TestPushStep_FormatterCommitAtThePublicationBoundaryIsThePipelineOwn(t *testing.T) {
	c, _ := publicationFixture(t)
	// The formatter changed a tracked file: the worktree holds the
	// formatter's output, and HEAD holds what it replaced.
	path := filepath.Join(c.WorkDir, "formatted.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "formatted.go")
	gitCmd(t, c.WorkDir, "commit", "-m", "add a file the formatter will change")
	// The run validated the committed shape, and the formatter now produces
	// the other one.
	headBefore := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	c.Run.HeadSHA = headBefore
	recordReviewApproval(t, c, headBefore)
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n\n\n"), 0600); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&PushStep{}).Execute(c)
	if err != nil {
		t.Fatalf("the pipeline's own formatter commit failed the run: %v", err)
	}
	if outcome == nil {
		t.Fatal("the push produced no outcome")
	}
	// The commit is a pipeline commit with the pipeline's own subject.
	subject := gitCmd(t, c.WorkDir, "log", "-1", "--format=%s")
	if !strings.Contains(subject, "apply agent fixes") {
		t.Fatalf("the formatter commit has an unexpected subject: %q", subject)
	}
	// The lineage is recorded, so the comparison guard can attribute the
	// advance to the pipeline.
	newHead := c.Run.HeadSHA
	if newHead == headBefore {
		t.Fatal("the formatter commit did not move the head")
	}
	if err := c.DB.ValidatePipelineHeadForward(c.Run.ID, headBefore, newHead); err != nil {
		t.Fatalf("the formatter commit's lineage is not recorded: %v", err)
	}
	// The run's head follows the commit.
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != newHead {
		t.Fatalf("the run head = %s, want %s", run.HeadSHA, newHead)
	}
	// The run published: the guard accepted the pipeline's own advance.
	if run.LastPushedSHA == nil || *run.LastPushedSHA != newHead {
		t.Fatalf("the run did not publish the head: %+v", run.LastPushedSHA)
	}
}

// TestPushStep_AnUnexplainedHeadMoveStillRefuses proves the new path does not
// weaken the guard: a head the pipeline did NOT move is still refused.
func TestPushStep_AnUnexplainedHeadMoveStillRefuses(t *testing.T) {
	c, _ := publicationFixture(t)
	// A commit the pipeline never made.
	gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "an out-of-band commit")
	c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	receipt, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil || receipt == nil {
		t.Fatal(err)
	}
	selection := pipeline.PRTargetSelection{
		PRURL: receipt.PRURL, SourceRepo: receipt.SourceRepo, SourceBranch: receipt.SourceBranch,
		ForgeHeadSHA: receipt.ForgeHeadSHA, TargetBranch: receipt.TargetBranch,
	}
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepPush, selection, false); err == nil {
		t.Fatal("an unexplained head move was accepted at the publication boundary")
	}
}

func TestPushStep_CorrectionAndTargetAdvanceRewind(t *testing.T) {
	for _, boundary := range []string{"post-step", "publication"} {
		t.Run(boundary, func(t *testing.T) {
			var c *pipeline.StepContext
			if boundary == "post-step" {
				c, _, _ = unpublishedAttestationFixture(t)
				c.Repo.URLsVerified = true
			} else {
				c, _ = publicationFixture(t)
			}
			oldHead := c.Run.HeadSHA
			target := advanceRewindTarget(t, c)
			if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("formatted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			outcome, err := (&PushStep{}).Execute(c)
			if err != nil || outcome == nil {
				t.Fatalf("push correction: %+v, %v", outcome, err)
			}
			if boundary == "post-step" {
				if outcome.RestartFrom != types.StepReview {
					t.Fatalf("correction did not request review: %+v", outcome)
				}
				receipt, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil || receipt == nil {
					t.Fatalf("receipt: %+v, %v", receipt, err)
				}
				selected := pipeline.PRTargetSelection{PRURL: receipt.PRURL, SourceRepo: receipt.SourceRepo, SourceBranch: receipt.SourceBranch, ForgeHeadSHA: receipt.ForgeHeadSHA, TargetBranch: receipt.TargetBranch}
				c.PRContextAfterStep = true
				decision, err := guardPRContextWithSelection(c, types.StepPush, selected, false)
				if err != nil || decision.RestartFrom != types.StepRebase {
					t.Fatalf("post-step rewind: %+v, %v", decision, err)
				}
			} else if outcome.RestartFrom != types.StepRebase {
				t.Fatalf("publication did not rewind: %+v", outcome)
			}
			newHead := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
			if newHead == oldHead || gitCmd(t, c.WorkDir, "show", newHead+":feature.txt") != "formatted feature" {
				t.Fatal("correction was not preserved")
			}
			pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
			if err != nil || pending == nil || pending.LocalHeadSHA != newHead || pending.TargetSHA != target {
				t.Fatalf("correction and target not bound to rewind: %+v, %v", pending, err)
			}
			run, err := c.DB.GetRun(c.Run.ID)
			if err != nil || run.LastPushedSHA != nil || run.TargetAdvanceRewinds != 1 {
				t.Fatalf("expected one unpublished rewind: %+v, %v", run, err)
			}
		})
	}
}

func TestPushStep_TargetAdvanceRejectsUnrecordedCorrection(t *testing.T) {
	for _, boundary := range []string{"post-step", "publication"} {
		t.Run(boundary, func(t *testing.T) {
			c, selected := publicationFixture(t)
			advanceRewindTarget(t, c)
			gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "unrecorded correction")
			c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
			if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
				t.Fatal(err)
			}
			var err error
			if boundary == "post-step" {
				c.PRContextAfterStep = true
				_, err = guardPRContextWithSelection(c, types.StepPush, selected, false)
			} else {
				_, err = detectTargetAdvanceAtPublication(c, types.StepPush)
			}
			if err == nil {
				t.Fatal("unrecorded correction accepted")
			}
			pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
			if err != nil || pending != nil {
				t.Fatalf("unrecorded correction created a rewind: %+v, %v", pending, err)
			}
		})
	}
}
