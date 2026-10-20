package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// unpublishedAttestationFixture is a run that has completed its ordinary
// validation and is about to publish for the FIRST time: no push binding, no
// commit-status attempt, no publication. A target advance detected here is
// still recoverable, which is exactly the measured controller failure.
func unpublishedAttestationFixture(t *testing.T) (*pipeline.StepContext, string, string) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"summary":"no changes"}`)}, nil
	}}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`),
		"FAKE_CLI_PR_HEAD_SHA="+headSHA,
		`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`,
	)
	sctx.Run.PRURL = &prURL
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Config.Rebase.MaxTargetAdvanceRewinds = 3
	// The push must reach the real bare upstream, not the GitHub-shaped URL.
	gitCmd(t, dir, "config", "url."+upstream+".insteadOf", "https://github.com/test/repo")
	// The run's ordinary validation completed at this head.
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		step, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.SetStepFindings(step.ID, `{"findings":[],"reviewed_paths":["feature.txt"]}`); err != nil {
			t.Fatal(err)
		}
		if name == types.StepReview {
			err = sctx.DB.CompleteReviewStep(step.ID, sctx.Run.ID, headSHA, 0, 1, "review.log")
		} else {
			err = sctx.DB.CompleteStep(step.ID, 0, 1, "step.log")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	recordReviewApproval(t, sctx, headSHA)
	// The comparison receipt pins the target the run validated against.
	selection := pipeline.PRTargetSelection{
		PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature",
		ForgeHeadSHA: headSHA, TargetBranch: "main",
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	// The PR carries an attestation for this head, so the push step's
	// attestation write is a real rebind and its comparison is real.
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(compliantPipelineBody(t, headSHA)), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = append(sctx.Env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	return sctx, dir, upstream
}

// TestPushStep_TargetAdvanceAtTheAttestationBoundaryRewinds is the exact
// measured controller failure: the run completed Review, Test, Document and
// Lint with no findings, the target moved under it, and the push died with
// "pipeline attestation write failed: live target commit, merge base, or diff
// differs from PR comparison receipt before update".
//
// The push must take the same safe rewind the Review..Lint boundaries take:
// rewind to Rebase, re-integrate, re-validate, and push.
func TestPushStep_TargetAdvanceAtTheAttestationBoundaryRewinds(t *testing.T) {
	sctx, dir, upstream := unpublishedAttestationFixture(t)
	head := sctx.Run.HeadSHA

	// The target advances under the run, after the last step boundary.
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "advance.md"), []byte("target moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "advance target")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	outcome, err := (&PushStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("push failed on a target advance: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepRebase {
		t.Fatalf("push did not request a rewind: %+v", outcome)
	}
	pending, err := sctx.DB.GetPendingTargetRewind(sctx.Run.ID)
	if err != nil || pending == nil {
		t.Fatalf("no pending rewind recorded: %+v %v", pending, err)
	}
	if pending.OldTargetSHA == pending.TargetSHA {
		t.Fatalf("the rewind recorded no target advance: %+v", pending)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.TargetAdvanceRewinds != 1 {
		t.Fatalf("the rewind was not charged: %+v %v", run, err)
	}
	// Nothing was published: the rewind is the pre-publication path.
	if run.LastPushedSHA != nil {
		t.Fatalf("a rewound push published anyway: %+v", run.LastPushedSHA)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("the remote moved before the rewind: %s want %s", got, head)
	}
}

// TestPushStep_TargetAdvanceConflictAtThePushBoundaryStillParks proves a REAL
// conflict still parks at a gate instead of silently pushing.
func TestPushStep_TargetAdvanceConflictAtThePushBoundaryStillParks(t *testing.T) {
	sctx, dir, _ := unpublishedAttestationFixture(t)
	// The advanced target touches the same file the branch changed.
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("conflicting target content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "conflicting target")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	outcome, err := (&PushStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("push failed instead of rewinding: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepRebase {
		t.Fatalf("push did not request a rewind: %+v", outcome)
	}
	sctx.PRContextAfterStep = false
	out, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a conflicting rewind failed hard instead of parking: %v", err)
	}
	if out == nil || !out.NeedsApproval || !strings.Contains(out.Findings, "conflict on rebase") {
		t.Fatalf("the conflict did not park clearly: %+v", out)
	}
}

// TestPushStep_AnUnreadableTargetNeverBecomesANewFailureMode proves the
// target-advance check adds no failure of its own: a target that cannot be
// read leaves the attestation's own comparison as the authority.
func TestPushStep_AnUnreadableTargetNeverBecomesANewFailureMode(t *testing.T) {
	sctx, dir, _ := unpublishedAttestationFixture(t)
	// The upstream remote is gone: the target cannot be read.
	if err := os.RemoveAll(sctx.Repo.UpstreamURL); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	rewind, err := detectTargetAdvanceAtPublication(sctx, types.StepPush)
	if err != nil {
		t.Fatalf("an unreadable target became a failure: %v", err)
	}
	if rewind {
		t.Fatal("an unreadable target was classified as an advance")
	}
	pending, err := sctx.DB.GetPendingTargetRewind(sctx.Run.ID)
	if err != nil || pending != nil {
		t.Fatalf("an unreadable target recorded a pending rewind: %+v %v", pending, err)
	}
}

// TestPushStep_APublishedRunCannotRewindAtThePushBoundary proves the rewind
// is refused once the run published: a published head is not this run's to
// rewrite.
func TestPushStep_APublishedRunCannotRewindAtThePushBoundary(t *testing.T) {
	sctx, dir, _ := unpublishedAttestationFixture(t)
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: sctx.Run.HeadSHA}); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "advance.md"), []byte("target moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "advance target")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	rewind, err := detectTargetAdvanceAtPublication(sctx, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if rewind {
		t.Fatal("a published run rewound")
	}
	pending, err := sctx.DB.GetPendingTargetRewind(sctx.Run.ID)
	if err != nil || pending != nil {
		t.Fatalf("a published run recorded a pending rewind: %+v %v", pending, err)
	}
}
