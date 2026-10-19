package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorResolvesPRTargetBeforeRebaseAndCarriesItToReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	var observed []types.StepName
	steps := []Step{
		newPassStep(types.StepIntent),
		&adaptiveCallStep{name: types.StepRebase, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.PRTarget == nil || sctx.PRTarget.TargetBranch != "develop" {
				t.Fatalf("rebase target = %+v, want develop", sctx.PRTarget)
			}
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.PRTarget == nil || sctx.PRTarget.TargetBranch != "develop" {
				t.Fatalf("review target = %+v, want develop", sctx.PRTarget)
			}
			return &StepOutcome{}, nil
		}},
	}
	executor := NewExecutor(database, p, nil, nil, steps, nil)
	executor.SetPRContextGuard(func(_ *StepContext, name types.StepName) (PRContextDecision, error) {
		observed = append(observed, name)
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}}, nil
	})
	if err := executor.Execute(context.Background(), run, repo, completionFixture(t, database, run)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed, []types.StepName{types.StepRebase, types.StepRebase, types.StepReview, types.StepReview}) {
		t.Fatalf("guard called at %v", observed)
	}
}

func TestExecutorParkedApprovalRevalidatesPRTargetBeforeFinalStepCompletes(t *testing.T) {
	database, p, run, repo := setupTest(t)
	var observed []types.StepName
	reviewRuns := 0
	steps := []Step{
		newPassStep(types.StepIntent),
		&adaptiveCallStep{name: types.StepRebase, fn: func(*StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepRebase)
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepReview)
			reviewRuns++
			if reviewRuns == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"old-comparison","severity":"warning","description":"old comparison","action":"ask-user","file":"main.go"}]}`}, nil
			}
			comparison, err := database.GetRunPRContext(run.ID)
			if err != nil {
				return nil, err
			}
			reviewComparison, err := database.CaptureReviewComparison(run.ID, db.ReviewComparison{
				SourceRepo: "test/repo", SourceBranch: run.Branch, TargetBranch: comparison.TargetBranch,
				TargetSHA: comparison.TargetSHA, HeadSHA: comparison.LocalHeadSHA,
				MergeBaseSHA: comparison.MergeBaseSHA, DiffDigest: comparison.DiffDigest,
			})
			if err != nil {
				return nil, err
			}
			return &StepOutcome{ReviewedPaths: []string{"main.go"}, ReviewablePaths: []string{"main.go"}, ReviewApprovedHeadSHA: sctx.Run.HeadSHA, ReviewComparisonGeneration: reviewComparison.Generation}, nil
		}},
	}
	var executor *Executor
	responseDone := make(chan error, 1)
	executor = NewExecutor(database, p, nil, nil, steps, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepReview &&
			event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			go func() {
				responseDone <- executor.Respond(types.StepReview, types.ActionApprove, nil, gateSnapshot(executor))
			}()
		}
	})
	checks := 0
	executor.SetPRContextGuard(func(_ *StepContext, name types.StepName) (PRContextDecision, error) {
		checks++
		if checks == 5 {
			if err := database.ResetStepsFrom(run.ID, types.StepRebase.Order()); err != nil {
				return PRContextDecision{}, err
			}
			return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}, RestartFrom: types.StepRebase}, nil
		}
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}}, nil
	})
	workDir := completionFixture(t, database, run)
	if err := os.WriteFile(workDir+"/main.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(context.Background(), workDir, "add", "main.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(context.Background(), workDir, "commit", "-m", "add review fixture"); err != nil {
		t.Fatal(err)
	}
	advanceCompletionFixture(t, database, run, workDir)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed, []types.StepName{types.StepRebase, types.StepReview, types.StepRebase, types.StepReview}) {
		t.Fatalf("executed steps = %v, want fresh rebase and review", observed)
	}
	completed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ReviewApprovedHeadSHA == nil || *completed.ReviewApprovedHeadSHA != run.HeadSHA {
		t.Fatalf("review approval = %v, want current head %s", completed.ReviewApprovedHeadSHA, run.HeadSHA)
	}
}

func TestExecutorRechecksTerminalCIProof(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const head = "1111111111111111111111111111111111111111"
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/repo/pull/7"
	_, err := database.BindRunPRContext(run.ID, db.PRContextCandidate{
		PRURL: prURL, SourceRepo: "acme/repo", SourceBranch: run.Branch,
		ForgeHeadSHA: head, LocalHeadSHA: head, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepCI)}, nil)
	called := false
	executor.SetPRContextGuard(func(sctx *StepContext, step types.StepName) (PRContextDecision, error) {
		called = true
		if step != types.StepCI || !sctx.PRContextAfterStep {
			t.Fatalf("terminal guard context = %s, after=%t", step, sctx.PRContextAfterStep)
		}
		return PRContextDecision{Target: PRTargetSelection{PRURL: prURL, TargetBranch: "develop"}}, nil
	})
	index, err := executor.checkPRContext(context.Background(), run, repo, t.TempDir(), types.StepCI, 0, true)
	if err != nil || index != -1 || !called {
		t.Fatalf("terminal CI context = %d, %v", index, err)
	}
}

func currentTestOwnerProof(t *testing.T, database *db.DB, run *db.Run, pending string) string {
	t.Helper()
	findings, err := types.ParseFindingsJSON(pending)
	if err != nil || len(findings.Items) != 1 {
		t.Fatalf("fixture claim = %+v, %v", findings, err)
	}
	receipt, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := findings.Items[0]
	zero := 0
	result := types.Finding{ID: types.ReviewSupportClaimID(claim), Severity: types.FindingSeverityInfo, Action: types.ActionNoOp, Category: types.FindingCategoryReviewSupportResolved,
		Support: &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: claim.Support.Test.Command}, OwnerResult: &types.FindingOwnerResult{ReviewFindingID: claim.ID, HeadSHA: run.HeadSHA, TargetSHA: receipt.TargetSHA, DiffDigest: receipt.DiffDigest, Generation: receipt.Generation, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Disposition: types.FindingSupportDispositionDisproven, ExitCode: &zero}}}
	encoded, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{result}})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestExecutorReviewPendingTestClaimAdvancesToOwnerStep(t *testing.T) {
	for _, missingProof := range []bool{false, true} {
		database, p, run, repo := setupTest(t)
		testRan := false
		workDir := completionFixture(t, database, run)

		const pending = `{"findings":[{"id":"review-test-1","severity":"warning","description":"old test output needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
		proof := currentTestOwnerProof(t, database, run, pending)
		executor := NewExecutor(database, p, nil, nil, []Step{
			&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}},
			&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
				testRan = true
				findings := proof
				if missingProof {
					findings = ""
				}
				return &StepOutcome{Findings: findings}, nil
			}},
		}, nil)
		err := executor.Execute(context.Background(), run, repo, workDir)
		if !missingProof {
			if err != nil {
				t.Fatalf("valid completion baseline: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("missing current owner proof was accepted")
		}
		if !testRan {
			t.Fatal("pending Test claim never reached Test")
		}
	}
}

func TestExecutorSkippedEvidenceOwnerCannotCompletePendingClaim(t *testing.T) {
	for _, missingProof := range []bool{false, true} {
		database, p, run, repo := setupTest(t)
		workDir := completionFixture(t, database, run)

		const pending = `{"findings":[{"id":"review-test-1","severity":"warning","description":"old test output needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
		proof := currentTestOwnerProof(t, database, run, pending)
		executor := NewExecutor(database, p, nil, nil, []Step{
			&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}},
			&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
				findings := proof
				if missingProof {
					findings = ""
				}
				return &StepOutcome{Findings: findings}, nil
			}},
		}, nil)
		if missingProof {
			executor.SetSkippedSteps([]types.StepName{types.StepTest})
		}
		err := executor.Execute(context.Background(), run, repo, workDir)
		if !missingProof {
			if err != nil {
				t.Fatalf("valid completion baseline: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("missing current owner proof was accepted")
		}
	}
}

func TestExecutorCurrentOwnerProofCompletesPendingTestClaim(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	head := run.HeadSHA
	const target = "2222222222222222222222222222222222222222"
	const digest = "3333333333333333333333333333333333333333333333333333333333333333"
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	_, err := database.BindRunPRContext(run.ID, db.PRContextCandidate{
		LocalHeadSHA: head, TargetBranch: "develop", TargetSHA: target,
		MergeBaseSHA: target, DiffDigest: digest,
	}, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{
		ID: "review-claim-1", Severity: types.FindingSeverityWarning,
		Description: "historical test failure", Action: types.ActionNoOp,
		Category: "review-support-pending", Support: &types.FindingSupport{
			ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."},
		},
	}
	claimJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	resolved := claim
	resolved.ID = types.ReviewSupportClaimID(claim)
	resolved.Severity = types.FindingSeverityInfo
	resolved.Category = types.FindingCategoryReviewSupportResolved
	resolved.Support = &types.FindingSupport{
		ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."},
		OwnerResult: &types.FindingOwnerResult{
			ReviewFindingID: claim.ID, HeadSHA: head, TargetSHA: target,
			DiffDigest: digest, Generation: receipt.Generation,
			ObservedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Disposition: types.FindingSupportDispositionDisproven, ExitCode: &zero,
		},
	}
	resolvedJSON, err := json.Marshal(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: claimJSON, ReviewApprovedHeadSHA: head}, nil
		}},
		&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: string(resolvedJSON)}, nil
		}},
	}, nil)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != types.StepTest {
			continue
		}
		if err := database.UpdateStepStatus(step.ID, types.StepStatusSkipped); err != nil {
			t.Fatal(err)
		}
		if err := executor.validateReviewSupportOwners(run.ID, types.StepTest); err == nil {
			t.Fatal("skipped owner with otherwise-current proof was accepted")
		}
	}

}

func TestExecutorTestRestartPrecedesFinalSupportValidation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	const pending = `{"findings":[{"id":"review-claim-1","severity":"warning","description":"old test result needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
	reviewCalls, testCalls := 0, 0
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			reviewCalls++
			if reviewCalls == 1 {
				return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}
			return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}, nil
		}},
		&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
			testCalls++
			if testCalls == 1 {
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}},
	}, nil)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 2 || testCalls != 2 {
		t.Fatalf("restart calls: review=%d test=%d", reviewCalls, testCalls)
	}
}

func TestExecutorUnresolvedTestClaimStopsBeforePublication(t *testing.T) {
	for _, missingProof := range []bool{false, true} {
		database, p, run, repo := setupTest(t)
		workDir := completionFixture(t, database, run)

		const pending = `{"findings":[{"id":"review-claim-1","severity":"warning","description":"old test output needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
		pushRan := false
		proof := currentTestOwnerProof(t, database, run, pending)
		executor := NewExecutor(database, p, nil, nil, []Step{
			&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}},
			&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
				findings := proof
				if missingProof {
					findings = ""
				}
				return &StepOutcome{Findings: findings}, nil
			}},
			&adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
				pushRan = true
				return &StepOutcome{}, nil
			}},
		}, nil)
		err := executor.Execute(context.Background(), run, repo, workDir)
		if !missingProof {
			if err != nil {
				t.Fatalf("valid completion baseline: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("missing current owner proof was accepted")
		}
		if pushRan {
			t.Fatal("unresolved Test claim reached Push")
		}
	}
}

func TestExecutorExplicitExternalCIOwnerCarriesPendingClaim(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	run, err := database.InsertRunWithExternalCIOwner(repo.ID, "feature-external-ci", baseRun.HeadSHA, baseRun.BaseSHA,
		nil, "", "", "", "develop", false, false, types.ExternalCIOwnerControllerShipPR, nil)
	if err != nil {
		t.Fatal(err)
	}
	workDir := completionFixture(t, database, run)
	_, err = database.BindRunPRContext(run.ID, db.PRContextCandidate{
		SourceRepo: "acme/repo", SourceBranch: "feature", LocalHeadSHA: run.HeadSHA, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	const pending = `{"findings":[{"id":"review-ci-1","severity":"warning","description":"historical CI failure","action":"no-op","category":"review-support-pending","support":{"claim_type":"ci","ci":{"check_id":"old-check","head_sha":"1111111111111111111111111111111111111111"}}}]}`
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
		}},
		newPassStep(types.StepPush), newPassStep(types.StepPR), newPassStep(types.StepCI),
	}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPush, types.StepPR, types.StepCI})
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("explicit external CI handoff was refused: %v", err)
	}
	current, err := database.GetRun(run.ID)
	if err != nil || current.Status != types.RunCompleted {
		t.Fatalf("run = %+v, err = %v; want completed with pending external CI", current, err)
	}
}

func TestExecutorExternalCIBypassRequiresHandoffSourceIdentity(t *testing.T) {
	for _, missingSource := range []bool{false, true} {
		database, p, baseRun, repo := setupTest(t)
		const head = "1111111111111111111111111111111111111111"
		run, err := database.InsertRunWithExternalCIOwner(repo.ID, "feature-external-ci-no-source", head, baseRun.BaseSHA,
			nil, "", "", "", "develop", false, false, types.ExternalCIOwnerControllerShipPR, nil)
		if err != nil {
			t.Fatal(err)
		}
		sourceRepo, sourceBranch := "acme/repo", run.Branch
		if missingSource {
			sourceRepo, sourceBranch = "", ""
		}
		workDir := completionFixture(t, database, run)
		receipt, err := database.GetRunPRContext(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		candidate := receipt.PRContextCandidate
		candidate.SourceRepo, candidate.SourceBranch = sourceRepo, sourceBranch
		if _, err := database.BindRunPRContext(run.ID, candidate, types.StepRebase); err != nil {
			t.Fatal(err)
		}

		const pending = `{"findings":[{"id":"review-ci-no-source","severity":"warning","description":"historical CI failure","action":"no-op","category":"review-support-pending","support":{"claim_type":"ci","ci":{"check_id":"old-check","head_sha":"1111111111111111111111111111111111111111"}}}]}`
		executor := NewExecutor(database, p, nil, nil, []Step{
			&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}},
			newPassStep(types.StepPush), newPassStep(types.StepPR), newPassStep(types.StepCI),
		}, nil)
		executor.SetSkippedSteps([]types.StepName{types.StepPush, types.StepPR, types.StepCI})
		err = executor.Execute(context.Background(), run, repo, workDir)
		if !missingSource {
			if err != nil {
				t.Fatalf("valid source handoff: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("external CI bypass completed without a handoff source identity")
		}
	}
}

func TestExecutorExternalCIBypassRequiresPipelineAuthoredDescendant(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	reviewedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "docs.md", "post-review document update\n")
	execGit(t, workDir, "add", "docs.md")
	execGit(t, workDir, "commit", "-m", "document")
	advancedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}

	run, err := database.InsertRunWithExternalCIOwner(repo.ID, "feature-external-ci-advanced", reviewedHead, baseRun.BaseSHA,
		nil, "", "", "", "develop", false, false, types.ExternalCIOwnerControllerShipPR, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{
		SourceRepo: "acme/repo", SourceBranch: "feature-external-ci-advanced", LocalHeadSHA: reviewedHead, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	const pending = `{"findings":[{"id":"review-ci-advanced","severity":"warning","description":"historical CI failure","action":"no-op","category":"review-support-pending","support":{"claim_type":"ci","ci":{"check_id":"old-check","head_sha":"1111111111111111111111111111111111111111"}}}]}`
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(review.ID, pending); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	pendingRound := pending
	if _, err := database.InsertReviewStepRound(review.ID, 1, "initial", &pendingRound, nil, reviewedHead, 1); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(step.ID, types.StepStatusSkipped); err != nil {
			t.Fatal(err)
		}
	}
	executor := NewExecutor(database, p, nil, nil, nil, nil)
	executor.workDir = workDir
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err != nil {
		t.Fatalf("valid external handoff: %v", err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, advancedHead); err != nil {
		t.Fatal(err)
	}
	receipt.LocalHeadSHA = advancedHead
	if _, err := database.AdvanceRunPRContext(run.ID, receipt); err != nil {
		t.Fatal(err)
	}

	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err == nil {
		t.Fatal("external CI bypass accepted an unrecorded descendant")
	}
	if err := database.RecordPipelineCommitHead(run.ID, reviewedHead, advancedHead); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err != nil {
		t.Fatalf("pipeline-authored document descendant: %v", err)
	}
}

func TestExecutorTestSupportRejectsPostReviewDocumentCommitAtCompletion(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	reviewedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "docs.md", "post-review document update\n")
	execGit(t, workDir, "add", "docs.md")
	execGit(t, workDir, "commit", "-m", "document")
	advancedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}

	run, err := database.InsertRun(repo.ID, "feature-test-advanced", reviewedHead, baseRun.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{
		LocalHeadSHA: reviewedHead, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	bound, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{
		ID: "review-test-advanced", Severity: types.FindingSeverityWarning,
		Description: "historical test failure", Action: types.ActionNoOp,
		Category: types.FindingCategoryReviewSupportPending,
		Support:  &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."}},
	}
	claimJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(review.ID, claimJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	zero := 0
	resolved := claim
	resolved.ID = types.ReviewSupportClaimID(claim)
	resolved.Severity = types.FindingSeverityInfo
	resolved.Category = types.FindingCategoryReviewSupportResolved
	resolved.Support = &types.FindingSupport{
		ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."},
		OwnerResult: &types.FindingOwnerResult{
			ReviewFindingID: claim.ID, HeadSHA: reviewedHead, TargetSHA: receipt.TargetSHA,
			DiffDigest: receipt.DiffDigest, Generation: bound.Generation,
			ObservedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Disposition: types.FindingSupportDispositionDisproven, ExitCode: &zero,
		},
	}
	resolvedJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	testStep, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(testStep.ID, resolvedJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(testStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, nil, nil)
	executor.workDir = workDir
	if err := executor.validateReviewSupportOwners(run.ID, types.StepTest); err != nil {
		t.Fatalf("current Test support was not accepted before the document commit: %v", err)
	}
	failed := 1
	resolved.Support.OwnerResult.Disposition = types.FindingSupportDispositionSupported
	resolved.Support.OwnerResult.ExitCode = &failed
	resolved.Severity = types.FindingSeverityError
	resolved.Action = types.ActionAskUser
	failedJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(testStep.ID, failedJSON); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepTest); err == nil {
		t.Fatal("supported Test claim passed without an operator override")
	}
	if err := database.SetStepOverrideReason(testStep.ID, "approved configured command failure"); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepTest); err != nil {
		t.Fatalf("approved Test failure was rejected: %v", err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, advancedHead); err != nil {
		t.Fatal(err)
	}
	receipt.LocalHeadSHA = advancedHead
	if _, err := database.AdvanceRunPRContext(run.ID, receipt); err != nil {
		t.Fatal(err)
	}

	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err == nil {
		t.Fatal("completion accepted Test proof from before a document commit")
	}
}

func TestExecutorCIHistoricalClaimAcceptsFinalComparisonProof(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	reviewedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "docs.md", "owned document correction\n")
	execGit(t, workDir, "add", "docs.md")
	execGit(t, workDir, "commit", "-m", "document")
	finalHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature-ci-forward", reviewedHead, baseRun.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{LocalHeadSHA: reviewedHead, TargetBranch: "main", TargetSHA: reviewedHead,
		MergeBaseSHA: reviewedHead, DiffDigest: "3333333333333333333333333333333333333333333333333333333333333333"}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepReview); err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{ID: "review-ci-forward", Severity: types.FindingSeverityWarning, Action: types.ActionNoOp,
		Category: types.FindingCategoryReviewSupportPending, Description: "historical CI failure",
		Support: &types.FindingSupport{ClaimType: types.FindingClaimCI,
			CI: &types.FindingCISupport{CheckID: "bitbucket-status:build", HeadSHA: reviewedHead}}}
	claimJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(review.ID, claimJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, finalHead); err != nil {
		t.Fatal(err)
	}
	receipt.LocalHeadSHA = finalHead
	if _, err := database.AdvanceRunPRContext(run.ID, receipt); err != nil {
		t.Fatal(err)
	}
	current, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved := claim
	resolved.ID = types.ReviewSupportClaimID(claim)
	resolved.Category = types.FindingCategoryReviewSupportResolved
	resolved.Support = &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: claim.Support.CI,
		OwnerResult: &types.FindingOwnerResult{ReviewFindingID: claim.ID, HeadSHA: finalHead,
			TargetSHA: current.TargetSHA, DiffDigest: current.DiffDigest, Generation: current.Generation,
			ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Disposition: types.FindingSupportDispositionDisproven, CheckState: "pass"}}
	resolvedJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(ci.ID, resolvedJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(ci.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, nil, nil)
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err != nil {
		t.Fatalf("final CI owner result refused: %v", err)
	}
	resolved.Support.OwnerResult.Disposition = types.FindingSupportDispositionSupported
	resolved.Support.OwnerResult.CheckState = "fail:failure"
	resolved.Severity = types.FindingSeverityError
	resolved.Action = types.ActionAskUser
	failedJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(ci.ID, failedJSON); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err == nil {
		t.Fatal("supported CI claim passed without an operator override")
	}
	if err := database.SetStepOverrideReason(ci.ID, "approved required-check failure"); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err != nil {
		t.Fatalf("approved CI failure was rejected: %v", err)
	}
	resolved.Support.OwnerResult.Disposition = types.FindingSupportDispositionDisproven
	resolved.Support.OwnerResult.CheckState = "pass"
	resolved.Support.OwnerResult.Generation--
	staleJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(ci.ID, staleJSON); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err == nil {
		t.Fatal("completion accepted stale CI proof")
	}
}

func TestExecutorRecoveredApprovalRevalidatesMovedPRTargetBeforeUsingParkedVerdict(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	intent, err := database.InsertStepResult(run.ID, types.StepIntent)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(intent.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	rebase, err := database.InsertStepResult(run.ID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(rebase.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(review.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"severity":"warning","description":"old comparison","action":"ask-user"}]}`
	if err := database.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	oldHead := run.HeadSHA
	if err := os.WriteFile(workDir+"/main.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(context.Background(), workDir, "add", "main.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(context.Background(), workDir, "commit", "-m", "add review fixture"); err != nil {
		t.Fatal(err)
	}
	advanceCompletionFixture(t, database, run, workDir)
	if _, err := database.InsertReviewStepRound(review.ID, 1, "initial", &findings, nil, oldHead, 1); err != nil {
		t.Fatal(err)
	}
	if err := parkGateFixture(database, review.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var observed []types.StepName
	steps := []Step{
		newPassStep(types.StepIntent),
		&adaptiveCallStep{name: types.StepRebase, fn: func(*StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepRebase)
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepReview)
			comparison, err := database.GetRunPRContext(run.ID)
			if err != nil {
				return nil, err
			}
			reviewComparison, err := database.CaptureReviewComparison(run.ID, db.ReviewComparison{
				SourceRepo: "test/repo", SourceBranch: run.Branch, TargetBranch: comparison.TargetBranch,
				TargetSHA: comparison.TargetSHA, HeadSHA: comparison.LocalHeadSHA,
				MergeBaseSHA: comparison.MergeBaseSHA, DiffDigest: comparison.DiffDigest,
			})
			if err != nil {
				return nil, err
			}
			return &StepOutcome{ReviewedPaths: []string{"main.go"}, ReviewablePaths: []string{"main.go"}, ReviewApprovedHeadSHA: run.HeadSHA, ReviewComparisonGeneration: reviewComparison.Generation}, nil
		}},
	}
	var executor *Executor
	responseDone := make(chan error, 1)
	executor = NewExecutor(database, p, nil, nil, steps, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepReview &&
			event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			rounds, err := database.GetRoundsByStep(review.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 2 {
				t.Fatalf("approval used parked review before revalidation: rounds=%d", len(rounds))
			}
			go func() {
				responseDone <- executor.Respond(types.StepReview, types.ActionApprove, nil, gateSnapshot(executor))
			}()
		}
	})
	executor.workDir = workDir
	checks := 0
	executor.SetPRContextGuard(func(_ *StepContext, name types.StepName) (PRContextDecision, error) {
		checks++
		if checks == 1 {
			if err := database.ResetStepsFrom(run.ID, types.StepRebase.Order()); err != nil {
				return PRContextDecision{}, err
			}
			return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}, RestartFrom: types.StepRebase}, nil
		}
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}}, nil
	})
	// A carried warning may park the fresh review. Answer its gate event above
	// rather than imposing a wall-clock deadline on the restart's DB writes.
	if err := executor.Resume(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed, []types.StepName{types.StepRebase, types.StepReview}) {
		t.Fatalf("executed steps = %v, want fresh rebase and review", observed)
	}
	recoveredReview, err := database.GetStepResult(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredReview.Status != types.StepStatusCompleted {
		t.Fatalf("review status = %s", recoveredReview.Status)
	}
	rounds, err := database.GetRoundsByStep(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("review rounds = %d, want parked review plus fresh review", len(rounds))
	}
	completed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ReviewApprovedHeadSHA == nil || *completed.ReviewApprovedHeadSHA != run.HeadSHA || *completed.ReviewApprovedHeadSHA == oldHead {
		t.Fatalf("review approval = %v, want moved Git head %s (old parked head %s must not certify it)", completed.ReviewApprovedHeadSHA, run.HeadSHA, oldHead)
	}
}

func TestExecutorTargetRewindArchivesJustProducedReviewVerdict(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	reviewCalls := 0
	outcome := &StepOutcome{Findings: `{"findings":[],"summary":"clean before target moved"}`, ReviewApprovedHeadSHA: run.HeadSHA}
	wantHead := outcome.ReviewApprovedHeadSHA
	steps := []Step{newPassStep(types.StepRebase), &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) { reviewCalls++; return outcome, nil }}, newPassStep(types.StepTest)}
	executor := NewExecutor(database, p, nil, nil, steps, nil)
	rewound := false
	executor.SetPRContextGuard(func(c *StepContext, step types.StepName) (PRContextDecision, error) {
		receipt, err := database.GetRunPRContext(run.ID)
		if err != nil {
			return PRContextDecision{}, err
		}
		if step == types.StepRebase && c.PRContextAfterStep && rewound {
			pending, err := database.GetPendingTargetRewind(run.ID)
			if err != nil {
				return PRContextDecision{}, err
			}
			if pending != nil {
				_, err = database.CompleteRebasePRContext(run.ID, receipt.PRContextCandidate, receipt.Generation)
				if err != nil {
					return PRContextDecision{}, err
				}
			}
		}
		if step == types.StepReview && c.PRContextAfterStep && !rewound {
			candidate := receipt.PRContextCandidate
			candidate.TargetSHA = "4444444444444444444444444444444444444444"
			_, err := database.RewindRunPRContext(run.ID, candidate, db.TargetAdvanceRewindRequest{ExpectedGeneration: receipt.Generation, Step: step, MaxRewinds: 3})
			if err != nil {
				return PRContextDecision{}, err
			}
			rewound = true
			return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}, RestartFrom: types.StepRebase}, nil
		}
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}, nil
	})
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	history, err := database.GetTargetRewindHistory(run.ID)
	if err != nil || len(history) != 1 || history[0].BoundaryJSON == nil {
		t.Fatalf("missing boundary result: %+v %v", history, err)
	}
	var saved StepOutcome
	if err := json.Unmarshal([]byte(*history[0].BoundaryJSON), &saved); err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(saved.Findings)
	if err != nil || len(findings.Items) != 0 || findings.Summary != "clean before target moved" || saved.ReviewApprovedHeadSHA != wantHead || !saved.ReviewCarryResolved || saved.NeedsApproval {
		t.Fatalf("lost verdict: %+v", saved)
	}
	if reviewCalls != 2 {
		t.Fatalf("mock guard should rerun invalidated step, got %d", reviewCalls)
	}
}
