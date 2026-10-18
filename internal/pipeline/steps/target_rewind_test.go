package steps

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func rewindFixture(t *testing.T) (*pipeline.StepContext, pipeline.PRTargetSelection) {
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
	return c, selected
}

func advanceRewindTarget(t *testing.T, c *pipeline.StepContext) string {
	t.Helper()
	gitCmd(t, c.WorkDir, "checkout", "main")
	name := "advance-" + gitCmd(t, c.WorkDir, "rev-parse", "--short", "HEAD") + ".md"
	if err := os.WriteFile(filepath.Join(c.WorkDir, name), []byte("target change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", name)
	gitCmd(t, c.WorkDir, "commit", "-m", "advance target")
	sha := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, c.WorkDir, "push", "origin", "main")
	gitCmd(t, c.WorkDir, "checkout", "feature")
	return sha
}

func TestTargetRewindEligibleBoundaries(t *testing.T) {
	for _, step := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.CustomGateStepName(types.StepTest, "audit")} {
		t.Run(string(step), func(t *testing.T) {
			c, selected := rewindFixture(t)
			advanceRewindTarget(t, c)
			decision, err := guardPRContextWithSelection(c, step, selected, false)
			if err != nil || decision.RestartFrom != types.StepRebase {
				t.Fatalf("eligible advance must rewind from %s: %+v, %v", step, decision, err)
			}
			run, err := c.DB.GetRun(c.Run.ID)
			if err != nil || run.TargetAdvanceRewinds != 1 {
				t.Fatalf("eligible boundary charge=%+v %v", run, err)
			}
			if pending, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || pending == nil {
				t.Fatalf("eligible boundary has no pending integration: %+v %v", pending, err)
			}

		})
	}
}

func TestTargetRewindRefusesUnexplainedHeadAndPublication(t *testing.T) {
	for _, kind := range []string{"unrecorded descendant", "recorded foreign descendant", "simultaneous fix", "source identity", "forge head", "unattached PR", "push", "PR completed", "rebase skipped"} {
		t.Run(kind, func(t *testing.T) {
			c, selected := rewindFixture(t)
			switch kind {
			case "unrecorded descendant", "recorded foreign descendant", "simultaneous fix":
				gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "foreign descendant")
				if kind != "unrecorded descendant" {
					c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
					if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
						t.Fatal(err)
					}
				}
				c.PRContextAfterStep = kind == "simultaneous fix"
			case "source identity":
				selected.SourceRepo = "other/repo"
				selected.SourceBranch = "other"
			case "forge head":
				selected.PRURL = "https://github.com/test/repo/pull/1"
				selected.SourceRepo = "test/repo"
				selected.SourceBranch = "feature"
				selected.ForgeHeadSHA = c.Run.HeadSHA
			case "unattached PR":
				selected.ForgeHeadSHA = c.Run.BaseSHA
			case "push":
				if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: c.Run.HeadSHA}); err != nil {
					t.Fatal(err)
				}
			case "PR completed", "rebase skipped":
				step, status := types.StepPR, types.StepStatusCompleted
				if kind == "rebase skipped" {
					step, status = types.StepRebase, types.StepStatusSkipped
				}
				row, err := c.DB.InsertStepResult(c.Run.ID, step)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.DB.UpdateStepStatus(row.ID, status); err != nil {
					t.Fatal(err)
				}
			}
			advanceRewindTarget(t, c)
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err == nil {
				t.Fatal("unsafe comparison accepted")
			}
			run, err := c.DB.GetRun(c.Run.ID)
			if err != nil || run.TargetAdvanceRewinds != 0 {
				t.Fatalf("unsafe comparison charged: %+v %v", run, err)
			}
			if pending, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || pending != nil {
				t.Fatalf("unsafe comparison left pending state: %+v %v", pending, err)
			}

		})
	}
}

func TestTargetRewindPostStepWording(t *testing.T) {
	c, selected := rewindFixture(t)
	c.PRContextAfterStep = true
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepPush, selected, false); err == nil || !strings.Contains(err.Error(), "start a new run") {
		t.Fatalf("published boundary must retain refusal: %v", err)
	}
}

func TestTargetRewindPendingBaselineCannotBeReplacedBeforeRebase(t *testing.T) {
	for _, initiallyAbsent := range []bool{false, true} {
		t.Run(map[bool]string{false: "S0 to S1", true: "absent to present"}[initiallyAbsent], func(t *testing.T) {
			c, selected := rewindFixture(t)
			if initiallyAbsent {
				gitCmd(t, c.WorkDir, "push", "origin", ":feature")
			}
			advanceRewindTarget(t, c)
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, c.WorkDir, "push", "--force", "origin", "main:feature")
			advanceRewindTarget(t, c)
			if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err == nil {
				t.Fatal("second classification replaced the original source observation")
			}
			r, err := c.DB.GetRun(c.Run.ID)
			if err != nil || r.TargetAdvanceRewinds != 1 {
				t.Fatalf("failed classification charged: %+v %v", r, err)
			}
		})
	}
}

func TestTargetRewindRebaseChecksSourceEvenWithoutFurtherTargetAdvance(t *testing.T) {
	for _, kind := range []string{"source changed", "source deleted", "source unavailable", "target rewrite"} {
		t.Run(kind, func(t *testing.T) {
			c, selected := rewindFixture(t)
			advanceRewindTarget(t, c)
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "source changed":
				gitCmd(t, c.WorkDir, "push", "--force", "origin", "main:feature")
			case "source deleted":
				gitCmd(t, c.WorkDir, "push", "origin", ":feature")
			case "source unavailable":
				c.Repo.ForkURL = filepath.Join(t.TempDir(), "missing.git")
			case "target rewrite":
				gitCmd(t, c.WorkDir, "push", "--force", "origin", c.Run.BaseSHA+":main")
			}
			if _, err := (&RebaseStep{}).Execute(c); err == nil {
				t.Fatal("unsafe Rebase integrated or parked instead of failing")
			}
			if head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); head != c.Run.HeadSHA {
				t.Fatalf("checks mutated HEAD: %s", head)
			}
			if refs := gitCmd(t, c.WorkDir, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/rewind/"); refs != "" {
				t.Fatalf("private refs leaked: %s", refs)
			}
		})
	}
}

func TestTargetRewindFurtherAdvanceChargesAndSuccessfulRebaseConsumesPending(t *testing.T) {
	c, selected := rewindFixture(t)
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	target := advanceRewindTarget(t, c)
	if _, err := (&RebaseStep{}).Execute(c); err != nil {
		t.Fatal(err)
	}
	c.PRContextAfterStep = true
	c.PRContextRebaseCompleted = true
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	r, err := c.DB.GetRun(c.Run.ID)
	if err != nil || r.TargetAdvanceRewinds != 2 {
		t.Fatalf("further target not charged: %+v %v", r, err)
	}
	if p, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || p != nil {
		t.Fatalf("successful rebase left pending: %+v %v", p, err)
	}
	gitCmd(t, c.WorkDir, "merge-base", "--is-ancestor", target, "HEAD")
}

func TestTargetRewindCheckedSHAsSurviveSharedTrackingRefMoves(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		t.Run(strategy, func(t *testing.T) {
			c, selected := rewindFixture(t)
			c.Config.Rebase.Strategy = strategy
			checked := advanceRewindTarget(t, c)
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
				t.Fatal(err)
			}
			targets, cleanup, err := checkedRewindTargets(c, rebaseTargets("feature", "main"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := cleanup(); err != nil {
					t.Error(err)
				}
			}()
			sibling := advanceRewindTarget(t, c)
			gitCmd(t, c.WorkDir, "fetch", "origin", "+main:refs/remotes/origin/main")
			for _, target := range targets {
				if strategy == config.RebaseStrategyMerge {
					if _, err := tryMerge(c.Ctx, c, target); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := tryRebase(c.Ctx, c, target); err != nil {
						t.Fatal(err)
					}
				}
			}
			gitCmd(t, c.WorkDir, "merge-base", "--is-ancestor", checked, "HEAD")
			if strings.Contains(gitCmd(t, c.WorkDir, "rev-list", "HEAD"), sibling) {
				t.Fatal("integrated unchecked shared ref")
			}
		})
	}
}

func TestTargetRewindForcePushObservesButNeverIntegratesSource(t *testing.T) {
	c, selected := rewindFixture(t)
	// The submitted head deliberately discarded the remote feature commit.
	gitCmd(t, c.WorkDir, "checkout", "-B", "feature", c.Run.BaseSHA)
	c.Run.BaseSHA = c.Run.HeadSHA
	if err := os.WriteFile(filepath.Join(c.WorkDir, "replacement.txt"), []byte("replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "replacement.txt")
	gitCmd(t, c.WorkDir, "commit", "-m", "replace source")
	c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	if _, err := (&RebaseStep{}).Execute(c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.WorkDir, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("discarded source content was integrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.WorkDir, "replacement.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestTargetRewindDirectMainIntegratesCheckedTarget(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		for _, fixing := range []bool{false, true} {
			t.Run(strategy+"/"+map[bool]string{false: "normal", true: "fix"}[fixing], func(t *testing.T) {
				c, selected := rewindFixture(t)
				gitCmd(t, c.WorkDir, "checkout", "-B", "main", c.Run.HeadSHA)
				run, err := c.DB.InsertRun(c.Repo.ID, "refs/heads/main", c.Run.HeadSHA, c.Run.BaseSHA)
				if err != nil {
					t.Fatal(err)
				}
				c.Run = run
				c.Config.Rebase.Strategy = strategy
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "checkout", "-b", "advanced-main", c.Run.BaseSHA)
				if err := os.WriteFile(filepath.Join(c.WorkDir, "landed.txt"), []byte("newly landed main content\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "add", "landed.txt")
				gitCmd(t, c.WorkDir, "commit", "-m", "advance main independently")
				target := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				gitCmd(t, c.WorkDir, "push", "origin", "HEAD:main")
				gitCmd(t, c.WorkDir, "checkout", "main")
				decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
				if err != nil || decision.RestartFrom != types.StepRebase {
					t.Fatalf("direct main advance did not rewind: %+v, %v", decision, err)
				}
				c.Fixing = fixing
				outcome, err := (&RebaseStep{}).Execute(c)
				if err != nil || outcome == nil || outcome.NeedsApproval {
					t.Fatalf("direct main integration failed: %+v, %v", outcome, err)
				}
				gitCmd(t, c.WorkDir, "merge-base", "--is-ancestor", target, "HEAD")
				for file, want := range map[string]string{"landed.txt": "newly landed main content\n", "feature.txt": "feature code\n"} {
					if content, err := os.ReadFile(filepath.Join(c.WorkDir, file)); err != nil || string(content) != want {
						t.Fatalf("integration lost %s: %q, %v", file, content, err)
					}
				}
				c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				if pending, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || pending != nil {
					t.Fatalf("successful integration left pending rewind: %+v, %v", pending, err)
				}
			})
		}
	}
}

func TestTargetRewindCompletionRefusesUnintegratedTarget(t *testing.T) {
	c, selected := rewindFixture(t)
	target := advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	before := c.Run.HeadSHA
	if outcome, err := updateHeadSHA(c.Ctx, c, "main", target); err == nil || outcome != nil {
		t.Fatalf("unintegrated rewind reported success: %+v, %v", outcome, err)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil || run.HeadSHA != before || c.Run.HeadSHA != before {
		t.Fatalf("rejected integration changed recorded head: %+v, %v", run, err)
	}
	if pending, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || pending == nil {
		t.Fatalf("rejected integration lost pending rewind: %+v, %v", pending, err)
	}
}

func TestTargetRewindNoopAndFastForwardConsumePending(t *testing.T) {
	for _, mode := range []string{"no-op", "fast-forward"} {
		t.Run(mode, func(t *testing.T) {
			c, selected := rewindFixture(t)
			// Make the advanced target contained in HEAD (no-op), or exactly HEAD
			// (a fast-forward from the original target); the receipt itself need not
			// change when the successful Rebase is bound.
			gitCmd(t, c.WorkDir, "push", "origin", "feature:main")
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
				t.Fatal(err)
			}
			if mode == "fast-forward" {
				gitCmd(t, c.WorkDir, "checkout", "main")
				gitCmd(t, c.WorkDir, "merge", "--ff-only", "feature")
				gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "further fast forward")
				gitCmd(t, c.WorkDir, "push", "origin", "main")
				gitCmd(t, c.WorkDir, "checkout", "feature")
			}
			outcome, err := (&RebaseStep{}).Execute(c)
			if err != nil || outcome.NeedsApproval {
				t.Fatalf("Rebase did not succeed: %+v %v", outcome, err)
			}
			c.PRContextAfterStep = true
			c.PRContextRebaseCompleted = true
			if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
				t.Fatal(err)
			}
			if p, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || p != nil {
				t.Fatalf("pending not consumed: %+v %v", p, err)
			}
		})
	}
}

func TestTargetRewindConflictsRestoreBoundHeadAfterPartialIntegration(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		for _, scenario := range []string{"source fast-forward then target conflict", "source integration then target conflict", "source conflict then target integration"} {
			t.Run(strategy+"/"+scenario, func(t *testing.T) {
				c, selected := rewindFixture(t)
				c.Config.Rebase.Strategy = strategy
				entry := c.Run.HeadSHA
				sourceBase := entry
				if scenario != "source fast-forward then target conflict" {
					sourceBase = c.Run.BaseSHA
				}
				gitCmd(t, c.WorkDir, "checkout", "-b", "remote-source", sourceBase)
				sourceFile := "source.txt"
				if scenario == "source conflict then target integration" {
					sourceFile = "feature.txt"
				}
				if err := os.WriteFile(filepath.Join(c.WorkDir, sourceFile), []byte("source update\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "add", sourceFile)
				gitCmd(t, c.WorkDir, "commit", "-m", "advance source")
				source := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				gitCmd(t, c.WorkDir, "push", "--force", "origin", "HEAD:feature")
				gitCmd(t, c.WorkDir, "checkout", "main")
				targetFile := "feature.txt"
				if scenario == "source conflict then target integration" {
					targetFile = "target.txt"
				}
				if err := os.WriteFile(filepath.Join(c.WorkDir, targetFile), []byte("target update\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "add", targetFile)
				gitCmd(t, c.WorkDir, "commit", "-m", "advance target")
				gitCmd(t, c.WorkDir, "push", "origin", "main")
				gitCmd(t, c.WorkDir, "checkout", "feature")
				if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
					t.Fatal(err)
				}
				outcome, err := (&RebaseStep{}).Execute(c)
				if err != nil || outcome == nil || !outcome.NeedsApproval || !outcome.AutoFixable {
					t.Fatalf("conflicts must reach repair or approval: %+v, %v", outcome, err)
				}
				if head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); head != entry || c.Run.HeadSHA != entry {
					t.Fatalf("conflict changed bound head: live=%s recorded=%s want=%s", head, c.Run.HeadSHA, entry)
				}
				if status := gitStatusPorcelain(t, c.WorkDir); status != "" {
					t.Fatalf("conflict left dirty worktree: %s", status)
				}
				c.PRContextAfterStep = true
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatalf("conflict must survive the executor's post-step guard: %v", err)
				}
				pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
				if err != nil || pending == nil || pending.LocalHeadSHA != entry || pending.SourceSHA == nil || *pending.SourceSHA != source {
					t.Fatalf("conflict lost pending bindings: %+v, %v", pending, err)
				}
			})
		}
	}
}

func TestTargetRewindConflictAbortIsNotSuccessfulIntegration(t *testing.T) {
	c, selected := rewindFixture(t)
	gitCmd(t, c.WorkDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("target side\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "feature.txt")
	gitCmd(t, c.WorkDir, "commit", "-m", "conflicting target")
	gitCmd(t, c.WorkDir, "push", "origin", "main")
	gitCmd(t, c.WorkDir, "checkout", "feature")
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	c.Fixing = true
	c.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, c.WorkDir, "rebase", "--abort")
		return &agent.Result{Text: `{"summary":"aborted instead of integrating"}`}, nil
	}}
	if _, err := (&RebaseStep{}).Execute(c); err == nil {
		t.Fatal("aborted conflict was accepted as completed pending integration")
	}
	if pending, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || pending == nil {
		t.Fatalf("failed conflict integration consumed pending: %+v %v", pending, err)
	}
}

func TestTargetRewindConflictFixesUseExactCheckedSHA(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		t.Run(strategy, func(t *testing.T) {
			c, selected := rewindFixture(t)
			c.Config.Rebase.Strategy = strategy
			gitCmd(t, c.WorkDir, "checkout", "main")
			if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("target content\n"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, c.WorkDir, "add", "feature.txt")
			gitCmd(t, c.WorkDir, "commit", "-m", "conflicting target")
			checked := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
			gitCmd(t, c.WorkDir, "push", "origin", "main")
			gitCmd(t, c.WorkDir, "checkout", "feature")
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
				t.Fatal(err)
			}
			targets, cleanup, err := checkedRewindTargets(c, rebaseTargets("feature", "main"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := cleanup(); err != nil {
					t.Error(err)
				}
			}()
			gitCmd(t, c.WorkDir, "checkout", "main")
			gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "sibling fetched target")
			sibling := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
			gitCmd(t, c.WorkDir, "update-ref", "refs/remotes/origin/main", sibling)
			gitCmd(t, c.WorkDir, "checkout", "feature")
			calls := 0
			c.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
				calls++
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("target content\nfeature code\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "add", "feature.txt")
				if strategy == config.RebaseStrategyMerge {
					gitCmd(t, c.WorkDir, "-c", "core.editor=true", "commit", "--no-edit")
				} else {
					gitCmd(t, c.WorkDir, "-c", "core.editor=true", "rebase", "--continue")
				}
				return &agent.Result{Text: `{"summary":"resolved both sides"}`}, nil
			}}
			for _, target := range targets {
				if strategy == config.RebaseStrategyMerge {
					err = mergeWithAgent(c.Ctx, c, target)
				} else {
					err = rebaseWithAgent(c.Ctx, c, target)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = verifyRewindIntegration(c, target); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatalf("resolver calls=%d want one", calls)
			}
			gitCmd(t, c.WorkDir, "merge-base", "--is-ancestor", checked, "HEAD")
			if strings.Contains(gitCmd(t, c.WorkDir, "rev-list", "HEAD"), sibling) {
				t.Fatal("conflict resolver integrated unchecked shared target")
			}
		})
	}
}

func TestTargetRewindSourceFetchErrorNeverUsesTrackingRef(t *testing.T) {
	c, _ := rewindFixture(t)
	remote := gitCmd(t, c.WorkDir, "remote", "get-url", "origin")
	if err := os.WriteFile(filepath.Join(remote, "refs", "heads", "feature"), []byte(strings.Repeat("f", 40)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The ref exists on the remote, but fetching its missing object must fail.
	gitCmd(t, c.WorkDir, "ls-remote", "--exit-code", remote, "refs/heads/feature")
	_, err := observeRewindBranch(c, remote, "feature", "refs/no-mistakes/rewind/fetch-error/source")
	if err == nil || !strings.Contains(err.Error(), "fetch") {
		t.Fatalf("source fetch failure was not returned: %v", err)
	}
}

type rewindRetargetReader struct {
	*fakePRFactsReader
	before func()
	fail   bool
}

func (r *rewindRetargetReader) SetPRBaseBranch(ctx context.Context, pr *scm.PR, branch string) error {
	r.before()
	if r.fail {
		return fmt.Errorf("forge call outcome uncertain")
	}
	return r.fakePRFactsReader.SetPRBaseBranch(ctx, pr, branch)
}

func TestTargetRewindRetargetMarkerPrecedesForgeAndSurvivesConsumedFlag(t *testing.T) {
	for _, kind := range []string{"success", "uncertain", "already correct"} {
		t.Run(kind, func(t *testing.T) {
			c, facts := selectionFixture(t)
			c.Run.PRURL = &facts.PR.URL
			c.Run.PRBaseBranch = strptr("main")
			c.Run.PRBaseBranchRequested = true
			if kind == "already correct" {
				facts.BaseBranch = "main"
			}
			called := false
			reader := &rewindRetargetReader{fakePRFactsReader: &fakePRFactsReader{read: facts}, fail: kind == "uncertain", before: func() {
				called = true
				r, err := c.DB.GetRun(c.Run.ID)
				if err != nil || !r.RetargetAttempted {
					t.Fatalf("forge called before marker: %+v %v", r, err)
				}
			}}
			_, _, err := resolveAndApplyPRTarget(c, reader, reader, false)
			if (err != nil) != (kind == "uncertain") {
				t.Fatalf("retarget error=%v", err)
			}
			r, readErr := c.DB.GetRun(c.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if r.RetargetAttempted != (kind != "already correct") || called != (kind != "already correct") {
				t.Fatalf("marker=%t called=%t kind=%s", r.RetargetAttempted, called, kind)
			}
			if kind != "uncertain" && r.PRBaseBranchRequested {
				t.Fatal("request not consumed")
			}
		})
	}
}

func TestTargetRewindPublishedRebaseRetainsOriginalBoundaryBehavior(t *testing.T) {
	c, selected := rewindFixture(t)
	if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: c.Run.HeadSHA}); err != nil {
		t.Fatal(err)
	}
	c.Repo.ForkURL = filepath.Join(t.TempDir(), "unavailable-source.git")
	advanceRewindTarget(t, c)
	// L is excluded. The pre-existing Rebase boundary can bind a comparison
	// without adding a source observation or charging a target rewind.
	decision, err := guardPRContextWithSelection(c, types.StepRebase, selected, false)
	if err != nil || decision.RestartFrom != types.StepRebase {
		t.Fatalf("published Rebase behavior changed: %+v %v", decision, err)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil || run.TargetAdvanceRewinds != 0 {
		t.Fatalf("excluded run charged: %+v %v", run, err)
	}
}

func TestTargetRewindRejectsRewriteAndChangedAttachedForgeHead(t *testing.T) {
	for _, kind := range []string{"rewrite", "forge head"} {
		t.Run(kind, func(t *testing.T) {
			c, selected := rewindFixture(t)
			if kind == "forge head" {
				selected.PRURL = "https://github.com/test/repo/pull/42"
				selected.SourceRepo = "test/repo"
				selected.SourceBranch = "feature"
				selected.ForgeHeadSHA = c.Run.HeadSHA
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				selected.ForgeHeadSHA = c.Run.BaseSHA
				advanceRewindTarget(t, c)
			} else {
				advanceRewindTarget(t, c)
				candidate, err := readPRComparison(c, selected)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.DB.BindRunPRContext(c.Run.ID, candidate, types.StepRebase); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "checkout", "--detach", c.Run.BaseSHA)
				gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "sibling target rewrite")
				gitCmd(t, c.WorkDir, "push", "--force", "origin", "HEAD:main")
				gitCmd(t, c.WorkDir, "checkout", "feature")
			}
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err == nil {
				t.Fatal("unsafe target/forge change accepted")
			}
			r, err := c.DB.GetRun(c.Run.ID)
			if err != nil || r.TargetAdvanceRewinds != 0 {
				t.Fatalf("unsafe target/forge change charged: %+v %v", r, err)
			}
		})
	}
}

func TestTargetRewindPermitsEarlierOwnedFixButRejectsAtTheCap(t *testing.T) {
	c, selected := rewindFixture(t)
	gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "owned fix")
	c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepDocument, selected, false); err != nil {
		t.Fatal(err)
	}
	c.PRContextAfterStep = false
	c.Config.Rebase.MaxTargetAdvanceRewinds = 1
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err == nil || !strings.Contains(err.Error(), "max_target_advance_rewinds=1") {
		t.Fatalf("cap not enforced: %v", err)
	}
	r, err := c.DB.GetRun(c.Run.ID)
	if err != nil || r.TargetAdvanceRewinds != 1 {
		t.Fatalf("cap refusal charged: %+v %v", r, err)
	}
}

// G's real adoption observes submitted code without publishing this run's work.
// Keep ordinary target rewinds available after the pending status POST.
func TestTargetRewindAllowsRealAttachAdoption(t *testing.T) {
	c, selected := rewindFixture(t)
	eligible, err := c.DB.CanRewindTarget(c.Run.ID, types.StepTest)
	if err != nil || !eligible {
		t.Fatalf("unpublished fixture ineligible: %t %v", eligible, err)
	}
	remote := c.Repo.UpstreamURL
	forgeURL := "https://github.com/test/repo"
	gitCmd(t, c.WorkDir, "config", "url."+remote+".insteadOf", forgeURL)
	c.Repo.UpstreamURL = forgeURL
	c.Repo.URLsVerified = true
	c.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	forge := &attachStatusForge{posts: map[string][]string{}}
	server := httptest.NewServer(forge)
	t.Cleanup(server.Close)
	env, _ := fakeGH(t, "")
	c.Env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
	(&PushStep{}).AttachPublishedHeadStatus(c)
	publication, err := c.DB.GetCommitStatusPublication(c.Ctx, c.Run.ID, db.CommitStatusPublication{SHA: c.Run.HeadSHA, Host: "github.com", Repo: "test/repo"})
	if err != nil || publication == nil || !publication.AdoptedOnly || publication.Validated || publication.CIEligible {
		t.Fatalf("real G attachment missing or credited validation: %+v %v", publication, err)
	}
	if states := forge.states("test/repo", c.Run.HeadSHA); len(states) != 1 || states[0] != "pending" {
		t.Fatalf("real G attach status POSTs = %v", states)
	}
	if eligible, err = c.DB.CanRewindTarget(c.Run.ID, types.StepTest); err != nil || !eligible {
		t.Fatalf("G-adopted run ineligible: %t %v", eligible, err)
	}
	advanceRewindTarget(t, c)
	if _, err = guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatalf("G-adopted run refused target advance: %v", err)
	}
	receipt, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidate := db.PRContextCandidate{LocalHeadSHA: receipt.LocalHeadSHA, TargetBranch: receipt.TargetBranch, TargetSHA: gitCmd(t, c.WorkDir, "rev-parse", "main"), MergeBaseSHA: receipt.MergeBaseSHA, DiffDigest: receipt.DiffDigest}
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil || pending == nil {
		t.Fatalf("missing adopted-head rewind: %+v %v", pending, err)
	}
	if _, err = c.DB.RewindRunPRContext(c.Run.ID, candidate, db.TargetAdvanceRewindRequest{ExpectedGeneration: receipt.Generation, Step: types.StepTest, MaxRewinds: 3, SourceSHA: pending.SourceSHA}); err != nil {
		t.Fatalf("transaction refused G-adopted run: %v", err)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil || run.TargetAdvanceRewinds != 1 || run.PendingTargetRewindID == nil {
		t.Fatalf("G-adopted run did not retain pending rewind: %+v %v", run, err)
	}
}
