package steps

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type mergedFactsHost struct {
	scm.Host
	facts scm.PRFacts
}

func (h *mergedFactsHost) GetPRFacts(context.Context, *scm.PR) (scm.PRFacts, error) {
	return h.facts, nil
}

func TestShippingPushRequiresExactReviewRatherThanDescendant(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	run, err := sctx.DB.InsertRunWithShipping(sctx.Repo.ID, "feature", head, base, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	recordReviewApproval(t, sctx, head)
	if err := os.WriteFile(filepath.Join(dir, "later.txt"), []byte("later"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "later.txt")
	gitCmd(t, dir, "commit", "-m", "later change")
	later := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := assertReviewApprovedPushHead(sctx, later); err == nil {
		t.Fatal("strict shipping published unreviewed descendant")
	}
}

func TestImmutableSourceRebaseNeverIntegratesTarget(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", remote)
	gitCmd(t, dir, "push", "origin", "main", "feature")
	gitCmd(t, dir, "switch", "main")
	if err := os.WriteFile(filepath.Join(dir, "target-only"), []byte("target"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "target-only")
	gitCmd(t, dir, "commit", "-m", "advance target")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "switch", "feature")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = remote
	run, err := sctx.DB.InsertRunWithShipping(sctx.Repo.ID, "feature", head, base, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingImmutableSource})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("immutable source rebased: %s != %s", got, head)
	}
	if got := gitCmd(t, remote, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatal("protected remote changed")
	}
}

func TestShippingPublicationRefusesMovedReviewTarget(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	gitCmd(t, dir, "push", remote, "main", "feature")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = remote
	run, err := sctx.DB.InsertRunWithShipping(sctx.Repo.ID, "feature", head, base, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	c, err := sctx.DB.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "owner/project", SourceBranch: "feature", TargetBranch: "main", TargetSHA: base, HeadSHA: head, MergeBaseSHA: base, DiffDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := sctx.DB.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReviewStep(sr.ID, run.ID, head, 0, 1, "", c.Generation); err != nil {
		t.Fatal(err)
	}
	if err := assertReviewApprovedPushHead(sctx, head); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "switch", "main")
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("target"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "target")
	gitCmd(t, dir, "commit", "-m", "advance target")
	gitCmd(t, dir, "push", remote, "main")
	gitCmd(t, dir, "switch", "feature")
	if err := assertReviewApprovedPushHead(sctx, head); err == nil {
		t.Fatal("publication accepted superseded target comparison")
	}
	outcome, err := (&PushStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RestartFrom != types.StepRebase {
		t.Fatalf("target drift restart = %s", outcome.RestartFrom)
	}
	target := gitCmd(t, remote, "rev-parse", "refs/heads/main")
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !isAncestor(context.Background(), dir, target, "HEAD") {
		t.Fatal("native Rebase did not integrate live target")
	}
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if newHead == head {
		t.Fatal("target advance did not change candidate head")
	}
	url := "https://github.com/owner/project.git"
	gitCmd(t, dir, "config", "url."+remote+".insteadOf", url)
	sctx.Repo.UpstreamURL = url
	sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		f := cleanReviewFindings()
		f.ReviewedPaths = fullReviewCoverage(t, dir, target)
		raw, _ := json.Marshal(f)
		return &agent.Result{Output: raw}, nil
	}}
	reviewed, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.NeedsApproval || reviewed.ReviewApprovedHeadSHA != newHead || reviewed.ReviewComparisonGeneration <= c.Generation {
		t.Fatalf("fresh independent review: %+v", reviewed)
	}
	if err := sctx.DB.CompleteReviewStep(sr.ID, run.ID, newHead, 0, 1, "", reviewed.ReviewComparisonGeneration); err != nil {
		t.Fatal(err)
	}
	if err := assertReviewApprovedPushHead(sctx, newHead); err != nil {
		t.Fatal(err)
	}
}

func TestImmutablePromotionMergeSettlesExecutorWithoutSourceMutation(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "branch", "-m", "feature", "dev")
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	gitCmd(t, dir, "push", remote, "main", "dev")
	url := "https://github.com/test/repo.git"
	gitCmd(t, dir, "config", "url."+remote+".insteadOf", url)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = url
	run, err := sctx.DB.InsertRunWithShipping(sctx.Repo.ID, "dev", head, base, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingImmutableSource, TargetBranch: "main", TargetSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	c, err := sctx.DB.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "test/repo", SourceBranch: "dev", TargetBranch: "main", TargetSHA: base, HeadSHA: head, MergeBaseSHA: base, DiffDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := sctx.DB.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReviewStep(sr.ID, run.ID, head, 0, 1, "", c.Generation); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/test/repo/pull/42"
	if err := sctx.DB.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	run.PRURL = &prURL
	wrongFacts := scm.PRFacts{State: scm.PRStateMerged, URL: prURL, SourceRepo: "test/repo", SourceBranch: "dev", HeadSHA: head, TargetRepo: "test/repo", TargetBranch: "main", TargetSHA: head}
	if err := verifyShippingMergedProof(sctx, &mergedFactsHost{facts: wrongFacts}, &scm.PR{URL: prURL}); err == nil {
		t.Fatal("merged proof accepted an unapproved target SHA")
	}
	env, _ := fakeGHWithBase(t, prURL, "main")
	env = append(env, "FAKE_CLI_PR_STATE=MERGED", "FAKE_CLI_PR_FACTS_JSON="+fmt.Sprintf(`{"number":42,"html_url":%q,"state":"closed","merged":true,"head":{"ref":"dev","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main","sha":%q,"repo":{"full_name":"test/repo"}}}`, prURL, head, base))
	ci := &CIStep{}
	wrapper := &transitionStep{name: types.StepCI, execute: func(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		gitCmd(t, dir, "push", remote, "HEAD:refs/heads/main")
		ctx.Env = env
		return ci.Execute(ctx)
	}}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	var completed *ipc.Event
	enrollExecutorGate(t, p, sctx.Repo.ID)
	executor := pipeline.NewExecutor(sctx.DB, p, sctx.Config, sctx.Agent, []pipeline.Step{wrapper}, func(e ipc.Event) {
		if e.Type == ipc.EventRunCompleted {
			copy := e
			completed = &copy
		}
	})
	if err := executor.Execute(context.Background(), run, sctx.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if completed == nil || completed.Status == nil || *completed.Status != string(types.RunCompleted) {
		t.Fatalf("terminal public event: %+v", completed)
	}
	if got := gitCmd(t, remote, "rev-parse", "refs/heads/dev"); got != head {
		t.Fatal("protected source changed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("pipeline source changed")
	}
}

func TestShippingReviewRestartsRebaseForAdvanceBeforeCapture(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	gitCmd(t, dir, "push", remote, "main", "feature")
	url := "https://github.com/test/repo"
	gitCmd(t, dir, "config", "url."+remote+".insteadOf", url)
	gitCmd(t, dir, "switch", "main")
	if err := os.WriteFile(filepath.Join(dir, "target-before-review"), []byte("advance"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "target-before-review")
	gitCmd(t, dir, "commit", "-m", "advance target before Review")
	gitCmd(t, dir, "push", remote, "main")
	gitCmd(t, dir, "switch", "feature")
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		f := cleanReviewFindings()
		f.ReviewedPaths = []string{"feature.txt"}
		raw, _ := json.Marshal(f)
		return &agent.Result{Output: raw}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	run, err := sctx.DB.InsertRunWithShipping(sctx.Repo.ID, "feature", head, base, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	out, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.RestartFrom != types.StepRebase || out.ReviewApprovedHeadSHA != "" || calls != 0 {
		t.Fatalf("stale integrated target reviewed: restart=%s approved=%s calls=%d", out.RestartFrom, out.ReviewApprovedHeadSHA, calls)
	}
	c, err := sctx.DB.GetReviewComparison(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Approved {
		t.Fatal("unintegrated target approved")
	}
}
