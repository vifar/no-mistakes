package steps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRHeadCatchUpWithPipelineCommits(t *testing.T) {
	for _, mode := range []string{"review", "document", "push", "unproven push", "foreign head", "foreign local"} {
		t.Run(mode, func(t *testing.T) {
			f := retargetRefreshFixture(t)
			f.reader.heads = []string{f.sctx.Run.BaseSHA}
			if _, err := f.run(); err != nil {
				t.Fatal(err)
			}
			before, err := f.sctx.DB.GetRunPRContext(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.sctx.WorkDir, "repair.txt"), []byte("repair\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.sctx.WorkDir, "add", "repair.txt")
			gitCmd(t, f.sctx.WorkDir, "commit", "-m", "pipeline repair")
			head := gitCmd(t, f.sctx.WorkDir, "rev-parse", "HEAD")
			if mode == "push" {
				if err := recordPipelineHeadRange(f.sctx, f.local, head); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "foreign local" {
				if err := f.sctx.DB.UpdateRunHeadSHA(f.sctx.Run.ID, head); err != nil {
					t.Fatal(err)
				}
				f.sctx.Run.HeadSHA = head
			}
			f.sctx.PRContextAfterStep = true
			f.reader.heads = []string{f.local}
			if mode == "foreign head" {
				f.reader.heads = []string{f.foreign}
			} else if mode == "foreign local" {
				f.reader.heads = []string{head}
			}
			selected, fresh, err := resolveAndApplyPRTarget(f.sctx, f.reader, f.reader, false)
			if err != nil || fresh {
				t.Fatalf("selection=%+v fresh=%v err=%v", selected, fresh, err)
			}
			step := types.StepReview
			if mode == "document" {
				step = types.StepDocument
			} else if mode == "push" || mode == "unproven push" {
				step = types.StepPush
			}
			decision, err := guardPRContextWithSelection(f.sctx, step, selected, false)
			receipt, readErr := f.sctx.DB.GetRunPRContext(f.sctx.Run.ID)
			if readErr != nil || receipt == nil {
				t.Fatalf("receipt=%+v err=%v", receipt, readErr)
			}
			if mode == "foreign head" || mode == "unproven push" || mode == "foreign local" {
				if err == nil || receipt.LocalHeadSHA != f.local {
					t.Fatalf("unproven transition accepted: decision=%+v receipt=%+v err=%v", decision, receipt, err)
				}
				if mode == "foreign head" && (!strings.Contains(err.Error(), f.foreign) || !strings.Contains(err.Error(), head) || *receipt != *before) {
					t.Fatalf("foreign refusal changed evidence or omitted heads: receipt=%+v err=%v", receipt, err)
				}
				if mode == "foreign local" && *receipt != *before {
					t.Fatalf("foreign local head was recorded as accounted: %+v", receipt)
				}
				return
			}
			if err != nil || decision.RestartFrom != "" || receipt.LocalHeadSHA != head || receipt.ForgeHeadSHA != f.local || receipt.DiffDigest == before.DiffDigest {
				t.Fatalf("pipeline advance with catch-up: decision=%+v receipt=%+v err=%v", decision, receipt, err)
			}
		})
	}
}

func TestPRHeadCatchUpWithTargetRewinds(t *testing.T) {
	for _, mode := range []string{"new target", "pipeline commit", "pending entry", "integration recheck", "further target", "completed rebase", "completed merge", "source changed", "foreign head"} {
		t.Run(mode, func(t *testing.T) {
			c, selected := rewindFixture(t)
			local := c.Run.HeadSHA
			selected.PRURL = "https://github.com/test/repo/pull/42"
			selected.SourceRepo, selected.SourceBranch = "test/repo", "feature"
			selected.State, selected.ForgeHeadSHA = scm.PRStateOpen, c.Run.BaseSHA
			if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
				t.Fatal(err)
			}
			target := advanceRewindTarget(t, c)
			pendingFirst := mode != "new target" && mode != "pipeline commit"
			if pendingFirst {
				if decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil || decision.RestartFrom != types.StepRebase {
					t.Fatalf("initial rewind: %+v %v", decision, err)
				}
			}
			selected.ForgeHeadSHA = local
			step, wantRestart := types.StepRebase, types.StepName("")
			wantCharges := int64(1)
			wantPending := true
			switch mode {
			case "new target":
				step, wantRestart = types.StepTest, types.StepRebase
			case "pipeline commit":
				gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "pipeline repair")
				head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				if err := recordPipelineHeadRange(c, local, head); err != nil {
					t.Fatal(err)
				}
				c.Run.HeadSHA = head
				if err := c.DB.UpdateRunHeadSHA(c.Run.ID, head); err != nil {
					t.Fatal(err)
				}
				c.PRContextAfterStep = true
				step, wantRestart = types.StepReview, types.StepRebase
			case "further target":
				target = advanceRewindTarget(t, c)
				wantRestart, wantCharges = types.StepRebase, 2
			case "completed rebase", "completed merge":
				if mode == "completed rebase" {
					gitCmd(t, c.WorkDir, "rebase", target)
					if isAncestor(c.Ctx, c.WorkDir, local, "HEAD") {
						t.Fatal("fixture did not rewrite the local head")
					}
				} else {
					gitCmd(t, c.WorkDir, "merge", "--no-ff", "-m", "integrate target", target)
				}
				c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
					t.Fatal(err)
				}
				c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
				wantPending = false
			case "source changed":
				gitCmd(t, c.WorkDir, "push", "--force", "origin", "main:feature")
			case "foreign head":
				selected.ForgeHeadSHA = target
			}
			var decision pipeline.PRContextDecision
			var err error
			if mode == "integration recheck" {
				remote := c.Repo.UpstreamURL
				c.Repo.UpstreamURL = "https://github.com/test/repo"
				gitCmd(t, c.WorkDir, "config", "url."+remote+".insteadOf", c.Repo.UpstreamURL)
				bin := fakeCLIBinDir(t)
				linkTestBinary(t, bin, "gh")
				facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"open","head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, selected.PRURL, local)
				c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": facts})
				checked, cleanup, checkErr := checkedRewindTargets(c, rebaseTargets("feature", "main"))
				err = checkErr
				if cleanup != nil {
					err = errors.Join(err, cleanup())
				}
				if err == nil && (len(checked) != 1 || checked[0] != target) {
					t.Fatalf("integration did not pin the checked target: %v", checked)
				}
			} else {
				decision, err = guardPRContextWithSelection(c, step, selected, false)
			}
			if mode == "source changed" || mode == "foreign head" {
				if err == nil {
					t.Fatalf("unsafe rewind accepted: %+v", decision)
				}
			} else if err != nil || decision.RestartFrom != wantRestart {
				t.Fatalf("catch-up with rewind: decision=%+v err=%v", decision, err)
			}
			run, readErr := c.DB.GetRun(c.Run.ID)
			if readErr != nil || run.TargetAdvanceRewinds != wantCharges {
				t.Fatalf("rewind count changed: run=%+v err=%v", run, readErr)
			}
			pending, readErr := c.DB.GetPendingTargetRewind(c.Run.ID)
			if readErr != nil || (pending != nil) != wantPending {
				t.Fatalf("pending=%+v err=%v", pending, readErr)
			}
			if pending != nil && (pending.SourceSHA == nil || *pending.SourceSHA != local) {
				t.Fatalf("catch-up replaced the original source observation: %+v", pending)
			}
			if mode != "foreign head" {
				receipt, readErr := c.DB.GetRunPRContext(c.Run.ID)
				if readErr != nil || receipt.ForgeHeadSHA != local || pending != nil && (pending.ForgeHeadSHA == nil || *pending.ForgeHeadSHA != local) {
					t.Fatalf("catch-up was not recorded consistently: receipt=%+v pending=%+v err=%v", receipt, pending, readErr)
				}
			}
		})
	}
}

func TestPRHeadCatchUpAtAttestationRead(t *testing.T) {
	for _, mode := range []string{"current head", "proposed advance", "foreign head", "target moved", "ordinary update"} {
		t.Run(mode, func(t *testing.T) {
			f := retargetRefreshFixture(t)
			f.reader.heads = []string{f.sctx.Run.BaseSHA}
			if _, err := f.run(); err != nil {
				t.Fatal(err)
			}
			selected, _, err := resolveAndApplyPRTarget(f.sctx, f.reader, f.reader, false)
			if err != nil {
				t.Fatal(err)
			}
			f.sctx.PRTarget = &selected
			pr := prFromOwnedURL(selected.PRURL)
			host := &mutationComparisonHost{body: compliantPipelineBody(t, f.sctx.Run.BaseSHA), facts: f.reader.facts}
			host.facts.HeadSHA = f.local
			proposed := f.local
			switch mode {
			case "proposed advance":
				gitCmd(t, f.sctx.WorkDir, "commit", "--allow-empty", "-m", "proposed repair")
				proposed = gitCmd(t, f.sctx.WorkDir, "rev-parse", "HEAD")
			case "foreign head":
				host.facts.HeadSHA = f.foreign
			case "target moved":
				gitCmd(t, f.sctx.WorkDir, "checkout", "release")
				gitCmd(t, f.sctx.WorkDir, "commit", "--allow-empty", "-m", "advance target")
				gitCmd(t, f.sctx.WorkDir, "checkout", "feature")
			}
			err = restampPRAttestationWithSteps(f.sctx.Ctx, host, pr, proposed, nil, nil, pipelineAttestationPolicy{}, func() error {
				return verifyPRMutationComparison(f.sctx, host, pr, proposed, mode != "ordinary update")
			})
			if mode == "foreign head" || mode == "target moved" || mode == "ordinary update" {
				if err == nil || host.writes != 0 {
					t.Fatalf("unsafe attestation written: writes=%d err=%v", host.writes, err)
				}
				if mode == "target moved" && !errors.Is(err, errTargetComparisonAdvanced) {
					t.Fatalf("catch-up hid target movement: %v", err)
				}
				if mode == "foreign head" && (!strings.Contains(err.Error(), f.foreign) || !strings.Contains(err.Error(), f.local)) {
					t.Fatalf("foreign refusal omitted the compared heads: %v", err)
				}
				return
			}
			if err != nil || host.writes != 1 {
				t.Fatalf("accounted catch-up blocked attestation: writes=%d err=%v", host.writes, err)
			}
			receipt, err := f.sctx.DB.GetRunPRContext(f.sctx.Run.ID)
			if err != nil || receipt.LocalHeadSHA != f.local || receipt.ForgeHeadSHA != f.local || f.sctx.PRTarget.ForgeHeadSHA != f.local {
				t.Fatalf("attestation lost the refreshed head or changed its local comparison: receipt=%+v err=%v", receipt, err)
			}
		})
	}
}

func TestPRHeadCatchUpWhilePushCommitsLeftoverEdits(t *testing.T) {
	c, dir, upstream := unpublishedAttestationFixture(t)
	c.Repo.URLsVerified = true
	local := c.Run.HeadSHA
	receipt, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := receipt.PRContextCandidate
	stale.ForgeHeadSHA = c.Run.BaseSHA
	if _, err := c.DB.AdvanceRunPRContext(c.Run.ID, stale); err != nil {
		t.Fatal(err)
	}
	c.PRTarget = &pipeline.PRTargetSelection{
		PRURL: receipt.PRURL, SourceRepo: receipt.SourceRepo, SourceBranch: receipt.SourceBranch,
		TargetBranch: receipt.TargetBranch, ForgeHeadSHA: local, State: scm.PRStateOpen,
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("pipeline edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := (&PushStep{}).Execute(c)
	if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("Push refused accounted catch-up instead of reviewing its commit: outcome=%+v err=%v", outcome, err)
	}
	receipt, err = c.DB.GetRunPRContext(c.Run.ID)
	if err != nil || receipt.LocalHeadSHA == local || receipt.ForgeHeadSHA != local {
		t.Fatalf("Push lost its commit or catch-up observation: receipt=%+v err=%v", receipt, err)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != local {
		t.Fatalf("Push published before revalidation: %s", remote)
	}
}
