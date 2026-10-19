package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRContextGuardRechecksTerminalCIAgainstLivePR(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, liveState, liveSource, liveBase           string
		movedHead, movedTarget, missingTarget, pending, wantError bool
	}{
		{name: "pending closed", pending: true, recorded: "closed", liveState: "closed", liveSource: "test/repo", liveBase: "main"},
		{name: "target advanced after close", recorded: "closed", liveState: "closed", liveSource: "test/repo", liveBase: "main", movedTarget: true, wantError: true},
		{name: "missing live target", recorded: "closed", liveState: "closed", liveSource: "test/repo", liveBase: "main", missingTarget: true, wantError: true},
		{name: "closed", recorded: "closed", liveState: "closed", liveSource: "test/repo", liveBase: "main"},
		{name: "reopened", recorded: "closed", liveState: "open", liveSource: "test/repo", liveBase: "main", wantError: true},
		{name: "retargeted", recorded: "closed", liveState: "closed", liveSource: "test/repo", liveBase: "develop", wantError: true},
		{name: "pushed", recorded: "closed", liveState: "closed", liveSource: "test/repo", liveBase: "main", movedHead: true, wantError: true},
		{name: "different source", recorded: "closed", liveState: "closed", liveSource: "other/repo", liveBase: "main", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			prURL := "https://github.com/test/repo/pull/42"
			selection := pipeline.PRTargetSelection{PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "closed"); err != nil {
				t.Fatal(err)
			}
			baselineFacts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"closed","merged":false,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, prURL, head)
			baselineBin := fakeCLIBinDir(t)
			linkTestBinary(t, baselineBin, "gh")
			sctx.Env = fakeCLIEnv(baselineBin, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": baselineFacts})
			sctx.PRContextAfterStep = true
			if decision, err := GuardPRContext(sctx, types.StepCI); err != nil || decision.Target.ForgeHeadSHA != head {
				t.Fatalf("valid terminal comparison = %+v, %v", decision, err)
			}
			if tc.pending {
				sctx.Run.PRState = &tc.recorded
			} else if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, tc.recorded); err != nil {
				t.Fatal(err)
			}
			if tc.movedTarget {
				gitCmd(t, dir, "checkout", "main")
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "move target after close")
				gitCmd(t, dir, "checkout", "feature")
			}
			if tc.missingTarget {
				gitCmd(t, dir, "branch", "-D", "main")
			}
			liveHead := head
			if tc.movedHead {
				liveHead = base
			}
			facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":%q,"merged":%t,"head":{"ref":"feature","sha":%q,"repo":{"full_name":%q}},"base":{"ref":%q}}`, prURL, tc.liveState, false, liveHead, tc.liveSource, tc.liveBase)
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "gh")
			sctx.Env = fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": facts})
			sctx.PRContextAfterStep = true
			decision, err := GuardPRContext(sctx, types.StepCI)
			if (err != nil) != tc.wantError {
				t.Fatalf("terminal guard = %+v, %v; want error %t", decision, err, tc.wantError)
			}
			if !tc.wantError && (decision.RestartFrom != "" || decision.Target.PRURL != prURL || decision.Target.ForgeHeadSHA != head) {
				t.Fatalf("terminal selection = %+v", decision)
			}
			receipt, readErr := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if readErr != nil || receipt.PRURL != prURL || receipt.ForgeHeadSHA != head || receipt.Generation != 1 {
				t.Fatalf("terminal receipt changed: %+v, %v", receipt, readErr)
			}
		})
	}
}

func TestPRContextGuardKeepsProviderEquivalentSourceIdentity(t *testing.T) {
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderAzureDevOps} {
		t.Run(string(provider), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			sctx.ForgeContext = &forgecontext.Context{Provider: provider}
			selection := pipeline.PRTargetSelection{SourceRepo: "Other/Fork", SourceBranch: "feature", TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
				t.Fatal(err)
			}
			selection.SourceRepo = "other/fork"
			unchanged, err := guardPRContextWithSelection(sctx, types.StepTest, selection, false)
			if err != nil || unchanged.RestartFrom != "" {
				t.Fatalf("case-only observation = %+v, %v", unchanged, err)
			}
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil || receipt.Generation != 1 || receipt.SourceRepo != "Other/Fork" {
				t.Fatalf("case-only receipt = %+v, %v", receipt, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "documented.md"), []byte("new\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "documented.md")
			gitCmd(t, dir, "commit", "-m", "document")
			sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			sctx.PRContextAfterStep = true
			advanced, err := guardPRContextWithSelection(sctx, types.StepDocument, selection, false)
			if err != nil || advanced.RestartFrom != "" {
				t.Fatalf("forward edit after case-only observation = %+v, %v", advanced, err)
			}
			receipt, err = sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil || receipt.Generation != 2 || receipt.LocalHeadSHA != sctx.Run.HeadSHA || receipt.SourceRepo != "Other/Fork" {
				t.Fatalf("advanced receipt = %+v, %v", receipt, err)
			}
		})
	}
}

func TestPRContextGuardDoesNotFoldCaseForForgejoSource(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderForgejo}
	selection := pipeline.PRTargetSelection{SourceRepo: "Other/Fork", SourceBranch: "feature", TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepTest, selection, false); err != nil {
		t.Fatalf("unchanged case-sensitive source: %v", err)
	}
	selection.SourceRepo = "other/fork"
	if _, err := guardPRContextWithSelection(sctx, types.StepTest, selection, false); err == nil {
		t.Fatal("accepted a different case-sensitive source repository")
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil || receipt.Generation != 1 || receipt.SourceRepo != "Other/Fork" {
		t.Fatalf("receipt changed after source conflict: %+v, %v", receipt, err)
	}
}

func TestPRContextGuardAttachesPRAfterSourceCaseChange(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	selection := pipeline.PRTargetSelection{SourceRepo: "Other/Fork", SourceBranch: "feature", TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	selection.SourceRepo = "other/fork"
	selection.PRURL = "https://github.com/other/fork/pull/7"
	selection.ForgeHeadSHA = head
	decision, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false)
	if err != nil || decision.RestartFrom != "" {
		t.Fatalf("first PR attachment = %+v, %v", decision, err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil || receipt.Generation != 2 || receipt.SourceRepo != "Other/Fork" || receipt.PRURL != selection.PRURL {
		t.Fatalf("attached receipt = %+v, %v", receipt, err)
	}
}

func TestPRContextGuardExternalCIOwnerRejectsDifferentPRHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.ExternalCIOwner = types.ExternalCIOwnerControllerShipPR
	selection := pipeline.PRTargetSelection{
		PRURL: "https://github.com/example/repo/pull/7", TargetBranch: "main", SourceRepo: "example/repo", SourceBranch: "feature",
		ForgeHeadSHA: headSHA,
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepCI, selection, false); err != nil {
		t.Fatalf("valid external-owner head: %v", err)
	}
	selection.ForgeHeadSHA = strings.Repeat("a", 40)
	if _, err := guardPRContextWithSelection(sctx, types.StepCI, selection, false); err == nil || !strings.Contains(err.Error(), "differs from local head") {
		t.Fatalf("guard error = %v, want exact PR head refusal", err)
	}
}

func TestPRContextGuardRequiresPublishedHeadBeforePRMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prHead   func(*testing.T, string, string, string) string
		wantFail bool
	}{
		{name: "exact head", prHead: func(_ *testing.T, _ string, _ string, head string) string { return head }},
		{name: "older ancestor", prHead: func(_ *testing.T, _ string, base string, _ string) string { return base }, wantFail: true},
		{name: "rebased sibling", prHead: func(t *testing.T, dir, base, _ string) string {
			gitCmd(t, dir, "checkout", "-b", "older-pr-head", base)
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "older PR head")
			oldHead := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "feature")
			return oldHead
		}, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			baseline := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			valid := pipeline.PRTargetSelection{PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(baseline, types.StepPR, valid, false); err != nil {
				t.Fatalf("valid published head: %v", err)
			}
			selection := pipeline.PRTargetSelection{
				PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo", SourceBranch: "feature",
				ForgeHeadSHA: tc.prHead(t, dir, base, head), TargetBranch: "main",
			}
			if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
				t.Fatal(err)
			}
			before, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = guardPRContextWithSelection(sctx, types.StepPR, selection, false)
			if tc.wantFail {
				if err == nil || !strings.Contains(err.Error(), "differs from local head") {
					t.Fatalf("PR mutation guard error = %v, want exact-head refusal", err)
				}
				after, readErr := sctx.DB.GetRunPRContext(sctx.Run.ID)
				if readErr != nil || *after != *before {
					t.Fatalf("PR receipt changed on refusal: before=%+v after=%+v err=%v", before, after, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact published head refused: %v", err)
			}
		})
	}
}

func TestPRContextGuardRejectsUnattachedExistingPRBeforeCreation(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	baseline := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	valid := pipeline.PRTargetSelection{SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(baseline, types.StepPR, valid, false); err != nil {
		t.Fatalf("valid published head: %v", err)
	}
	selection := pipeline.PRTargetSelection{
		SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: base, TargetBranch: "main",
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err == nil || !strings.Contains(err.Error(), "differs from local head") {
		t.Fatalf("unattached existing PR guard error = %v, want exact-head refusal", err)
	}
}

func TestPRContextGuardPinsActualTargetAndStopsAfterTargetMoves(t *testing.T) {
	dir, mainSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "develop", mainSHA)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, mainSHA, headSHA, config.Commands{})
	selection := pipeline.PRTargetSelection{TargetBranch: "develop"}
	first, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false)
	if err != nil || first.RestartFrom != "" {
		t.Fatalf("first context = %+v, %v", first, err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil || receipt == nil || receipt.TargetBranch != "develop" || receipt.LocalHeadSHA != headSHA || receipt.TargetSHA != mainSHA || receipt.DiffDigest == "" {
		t.Fatalf("incorrect durable comparison: %+v, %v", receipt, err)
	}
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "checkout", "develop")
	if err := os.WriteFile(filepath.Join(dir, "target-move.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "target-move.txt")
	gitCmd(t, dir, "commit", "-m", "move target")
	gitCmd(t, dir, "checkout", "feature")
	_, err = guardPRContextWithSelection(sctx, types.StepTest, selection, false)
	if err == nil || !strings.Contains(err.Error(), "start a new run") {
		t.Fatalf("target movement error = %v, want a new run", err)
	}
	updated, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || updated.ReviewApprovedHeadSHA == nil {
		t.Fatalf("completed review history was reset: %+v, %v", updated, err)
	}
	resetStep, err := sctx.DB.GetStepResult(step.ID)
	if err != nil || resetStep.Status != types.StepStatusCompleted {
		t.Fatalf("review step was replayed: %+v, %v", resetStep, err)
	}
}

func TestPRContextGuardAdvancesAfterDocumentEditWithoutReplayingTest(t *testing.T) {
	dir, base, reviewedHead := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, reviewedHead, config.Commands{})
	selection := pipeline.PRTargetSelection{TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "documentation.md"), []byte("documented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "documentation.md")
	gitCmd(t, dir, "commit", "-m", "document change")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx.Run.HeadSHA = newHead
	sctx.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(sctx, types.StepDocument, selection, false)
	if err != nil || decision.RestartFrom != "" {
		t.Fatalf("forward document edit = %+v, %v; want no restart", decision, err)
	}
	stored, err := sctx.DB.GetStepResult(review.ID)
	if err != nil || stored.Status != types.StepStatusCompleted {
		t.Fatalf("review was reset after document edit: %+v, %v", stored, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != reviewedHead {
		t.Fatalf("review anchor lost: %+v, %v", run, err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil || receipt.LocalHeadSHA != newHead {
		t.Fatalf("new comparison not bound: %+v, %v", receipt, err)
	}
}

func TestPRContextGuardRevalidatesAfterPipelinePushAdvancesHead(t *testing.T) {
	dir, base, reviewedHead := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, reviewedHead, config.Commands{})
	selection := pipeline.PRTargetSelection{TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(testStep.ID, `{"findings":[{"severity":"info","description":"new test file written by agent","action":"no-op"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(testStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "formatted.txt"), []byte("formatted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "formatted.txt")
	gitCmd(t, dir, "commit", "-m", "format source")
	publishedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
		HeadSHA: publishedHead, TargetKind: "upstream", TargetFingerprint: "test", Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = publishedHead
	sctx.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(sctx, types.StepPush, selection, false)
	if err != nil || decision.RestartFrom != "" {
		t.Fatalf("pipeline push context = %+v, %v; want no restart", decision, err)
	}
	stored, err := sctx.DB.GetStepResult(review.ID)
	if err != nil || stored.Status != types.StepStatusCompleted {
		t.Fatalf("review was replayed for the published head: %+v, %v", stored, err)
	}
	storedTest, err := sctx.DB.GetStepResult(testStep.ID)
	if err != nil || storedTest.Status != types.StepStatusCompleted || storedTest.FindingsJSON == nil {
		t.Fatalf("test evidence was replayed for the published head: %+v, %v", storedTest, err)
	}
}

func TestPRContextGuardAcceptsPublishedCIRepairHead(t *testing.T) {
	for _, afterStep := range []bool{false, true} {
		t.Run(map[bool]string{false: "before CI", true: "after CI"}[afterStep], func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			selection := pipeline.PRTargetSelection{TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("fixed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "repair.txt")
			gitCmd(t, dir, "commit", "-m", "ci repair")
			publishedHead := gitCmd(t, dir, "rev-parse", "HEAD")
			if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
				HeadSHA: publishedHead, TargetKind: "upstream", TargetFingerprint: "test", Ref: "refs/heads/feature",
			}); err != nil {
				t.Fatal(err)
			}
			sctx.Run.HeadSHA = publishedHead
			sctx.PRContextAfterStep = afterStep
			decision, err := guardPRContextWithSelection(sctx, types.StepCI, selection, false)
			if err != nil || decision.RestartFrom != "" {
				t.Fatalf("published CI repair context = %+v, %v; want forward receipt", decision, err)
			}
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil || receipt.LocalHeadSHA != publishedHead {
				t.Fatalf("published repair receipt = %+v, %v", receipt, err)
			}
		})
	}
}

func TestPRContextGuardProvesMergedComparison(t *testing.T) {
	type mergedCase struct {
		name, mode                                                                   string
		advance, changedResult, targetMoved, missingMerge, detachedMerge, beforeStep bool
		qualified, strict, shipping                                                  bool
		omitObservation, earlyChange, stacked                                        bool
		wantError                                                                    bool
	}
	cases := []mergedCase{
		{name: "already merged at CI entry", mode: "merge", beforeStep: true},
		{name: "already squashed at CI entry", mode: "squash", beforeStep: true},
		{name: "already rebased at CI entry", mode: "rebase", beforeStep: true},
		{name: "merge", mode: "merge"},
		{name: "squash", mode: "squash"},
		{name: "multiple rebased commits", mode: "rebase"},
		{name: "target advances after merge", mode: "merge", advance: true},
		{name: "merge changes result", mode: "merge", changedResult: true, wantError: true},
		{name: "target changed before merge", mode: "merge", targetMoved: true, wantError: true},
		{name: "qualified advanced target merge", mode: "merge", targetMoved: true, qualified: true},
		{name: "qualified advanced target squash", mode: "squash", targetMoved: true, qualified: true},
		{name: "qualified advanced target rebase", mode: "rebase", targetMoved: true, qualified: true},
		{name: "qualified advanced target merge at CI entry", mode: "merge", targetMoved: true, qualified: true, beforeStep: true},
		{name: "qualified advanced target with later advance", mode: "merge", targetMoved: true, qualified: true, advance: true},
		{name: "qualified advanced target changed result", mode: "merge", targetMoved: true, qualified: true, changedResult: true, wantError: true},
		{name: "qualified advanced target corrupt rebased prefix", mode: "rebase", targetMoved: true, qualified: true, earlyChange: true, wantError: true},
		{name: "qualified advanced target without observation", mode: "merge", targetMoved: true, qualified: true, omitObservation: true, wantError: true},
		{name: "qualified advanced target strict protection", mode: "merge", targetMoved: true, qualified: true, strict: true, wantError: true},
		{name: "qualified advanced target strict shipping", mode: "merge", targetMoved: true, qualified: true, shipping: true, wantError: true},
		{name: "qualified stacked target merge", mode: "merge", targetMoved: true, qualified: true, stacked: true},
		{name: "qualified stacked target squash", mode: "squash", targetMoved: true, qualified: true, stacked: true},
		{name: "qualified stacked target rebase", mode: "rebase", targetMoved: true, qualified: true, stacked: true},
		{name: "qualified stacked target at CI entry", mode: "merge", targetMoved: true, qualified: true, stacked: true, beforeStep: true},
		{name: "qualified stacked target with later advance", mode: "merge", targetMoved: true, qualified: true, stacked: true, advance: true},
		{name: "qualified stacked target changed result", mode: "merge", targetMoved: true, qualified: true, stacked: true, changedResult: true, wantError: true},
		{name: "qualified stacked target corrupt rebased prefix", mode: "rebase", targetMoved: true, qualified: true, stacked: true, earlyChange: true, wantError: true},
		{name: "qualified stacked target without observation", mode: "merge", targetMoved: true, qualified: true, stacked: true, omitObservation: true, wantError: true},
		{name: "qualified stacked target strict protection", mode: "merge", targetMoved: true, qualified: true, stacked: true, strict: true, wantError: true},
		{name: "qualified stacked target strict shipping", mode: "merge", targetMoved: true, qualified: true, stacked: true, shipping: true, wantError: true},
		{name: "missing merge evidence", mode: "merge", missingMerge: true, wantError: true},
		{name: "merge absent from target", mode: "merge", detachedMerge: true, wantError: true},
	}
	run := func(t *testing.T, tc mergedCase) {
		dir, base, head := setupGitRepo(t)
		stackedPrefix := head
		if tc.mode == "rebase" || tc.stacked {
			if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "second.txt")
			gitCmd(t, dir, "commit", "-m", "second change")
			head = gitCmd(t, dir, "rev-parse", "HEAD")
		}
		if tc.stacked && tc.mode == "rebase" {
			if err := os.WriteFile(filepath.Join(dir, "third.txt"), []byte("third change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "third.txt")
			gitCmd(t, dir, "commit", "-m", "third change")
			head = gitCmd(t, dir, "rev-parse", "HEAD")
		}
		sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
		prURL := "https://github.com/test/repo/pull/42"
		selection := pipeline.PRTargetSelection{PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
		if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
			t.Fatal(err)
		}
		if tc.qualified {
			review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.CompleteReviewStep(review.ID, sctx.Run.ID, head, 0, 1, "review.log"); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: head}); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetRunCIReady(sctx.Run.ID, true); err != nil {
				t.Fatal(err)
			}
		}
		if tc.shipping {
			sctx.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
		}
		before, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "checkout", "main")
		if tc.targetMoved {
			if tc.stacked {
				gitCmd(t, dir, "merge", "--ff-only", stackedPrefix)
			}
			if err := os.WriteFile(filepath.Join(dir, "unreviewed.txt"), []byte("unreviewed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "unreviewed.txt")
			gitCmd(t, dir, "commit", "-m", "target moved")
			if tc.qualified && !tc.strict && !tc.shipping && !tc.omitObservation {
				gitCmd(t, dir, "checkout", "feature")
				bin := fakeCLIBinDir(t)
				linkTestBinary(t, bin, "gh")
				sctx.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh", "FAKE_CLI_PROTECTION_JSON": `{"strict":false}`, "FAKE_CLI_RULES_JSON": `[]`, "FAKE_CLI_MERGEABLE": "MERGEABLE"})
				if decision, err := guardPRContextWithSelection(sctx, types.StepCI, selection, false); err != nil || decision.RestartFrom != "" {
					t.Fatalf("retain qualified comparison before merge: %+v %v", decision, err)
				}
				retained, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil || retained.CIReadyAt == nil {
					t.Fatalf("qualified readiness not retained: %+v %v", retained, err)
				}
				clearCIMonitorReady(sctx)
				if decision, err := guardPRContextWithSelection(sctx, types.StepCI, selection, false); err != nil || decision.RestartFrom != "" {
					t.Fatalf("recover readiness before merge: %+v %v", decision, err)
				}
				if err := sctx.DB.SetRunCIReady(sctx.Run.ID, true); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "checkout", "main")
			}
		}
		switch tc.mode {
		case "squash":
			gitCmd(t, dir, "merge", "--squash", "feature")
			gitCmd(t, dir, "commit", "-m", "squash reviewed head")
		case "rebase":
			// Cherry-pick each commit onto the target, as a provider rebase merge
			// does. A different committer forces new commit IDs.
			rebaseBase := base
			if tc.stacked && tc.targetMoved {
				rebaseBase = stackedPrefix
			}
			commits := strings.Fields(gitCmd(t, dir, "rev-list", "--reverse", rebaseBase+".."+head))
			for i, commit := range commits {
				gitCmd(t, dir, "-c", "user.name=Merge bot", "cherry-pick", commit)
				if tc.earlyChange && i == 0 {
					if err := os.WriteFile(filepath.Join(dir, "unexpected.txt"), []byte("corrupt rebased prefix\n"), 0600); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, dir, "add", "unexpected.txt")
					gitCmd(t, dir, "commit", "--amend", "--no-edit")
				}
			}
		default:
			gitCmd(t, dir, "merge", "--no-ff", "feature", "-m", "merge reviewed head")
		}
		if tc.changedResult {
			if err := os.WriteFile(filepath.Join(dir, "unexpected.txt"), []byte("changed result\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "unexpected.txt")
			gitCmd(t, dir, "commit", "--amend", "--no-edit")
		}
		mergedSHA := gitCmd(t, dir, "rev-parse", "HEAD")
		if tc.advance {
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "target advances after merge")
		}
		if tc.detachedMerge {
			gitCmd(t, dir, "reset", "--hard", base)
		}
		if tc.missingMerge {
			mergedSHA = ""
		}
		gitCmd(t, dir, "checkout", "feature")
		state := "merged"
		if !tc.beforeStep {
			sctx.Run.PRState = &state
		}
		facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"closed","merged":true,"merge_commit_sha":%q,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, prURL, mergedSHA, head)
		binDir := fakeCLIBinDir(t)
		linkTestBinary(t, binDir, "gh")
		sctx.Env = fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": facts, "FAKE_CLI_PROTECTION_JSON": fmt.Sprintf(`{"strict":%t}`, tc.strict), "FAKE_CLI_RULES_JSON": `[]`})
		sctx.PRContextAfterStep = !tc.beforeStep
		decision, err := GuardPRContext(sctx, types.StepCI)
		if tc.missingMerge && err != nil && !strings.Contains(err.Error(), "lacks a full merge commit SHA") {
			t.Fatalf("missing merge rejection = %v, want provider merge identity error", err)
		}
		if (err != nil) != tc.wantError {
			t.Fatalf("merged guard = %+v, %v; want error %t", decision, err, tc.wantError)
		}
		after, readErr := sctx.DB.GetRunPRContext(sctx.Run.ID)
		if readErr != nil || *after != *before {
			t.Fatalf("merged receipt changed: before=%+v after=%+v error=%v", before, after, readErr)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantError {
				baseline := tc
				baseline.changedResult, baseline.targetMoved, baseline.missingMerge, baseline.detachedMerge, baseline.wantError = false, false, false, false, false
				baseline.strict, baseline.shipping = false, false
				baseline.earlyChange = false
				run(t, baseline)
			}
			run(t, tc)
		})
	}
}

func TestPRContextGuardRejectsCorruptTerminalComparison(t *testing.T) {
	for _, field := range []string{"merge base", "diff digest"} {
		t.Run(field, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
			prURL := "https://github.com/test/repo/pull/42"
			selection := pipeline.PRTargetSelection{PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
				t.Fatal(err)
			}
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			candidate := receipt.PRContextCandidate
			if field == "merge base" {
				candidate.MergeBaseSHA = head
			} else {
				candidate.DiffDigest = strings.Repeat("0", 64)
			}

			state := "closed"
			sctx.Run.PRState = &state
			sctx.PRContextAfterStep = true
			facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"closed","merged":false,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, prURL, head)
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "gh")
			sctx.Env = fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": facts})
			if decision, err := GuardPRContext(sctx, types.StepCI); err != nil || decision.Target.ForgeHeadSHA != head {
				t.Fatalf("valid terminal receipt = %+v, %v", decision, err)
			}
			if _, err := sctx.DB.AdvanceRunPRContext(sctx.Run.ID, candidate); err != nil {
				t.Fatal(err)
			}
			if _, err := GuardPRContext(sctx, types.StepCI); err == nil {
				t.Fatal("accepted corrupt terminal comparison")
			}
		})
	}
}
