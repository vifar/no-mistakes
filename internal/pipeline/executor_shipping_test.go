package pipeline

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A clean findings list cannot waive a Review coverage gate in strict mode.
func TestShippingReviewGateCannotBeApprovedWithoutFullReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
	step := &mockStep{name: types.StepReview, outcome: &StepOutcome{NeedsApproval: true, Findings: `{"items":[]}`, ReviewApprovedHeadSHA: run.HeadSHA, ReviewComparisonGeneration: 1}}
	e := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- e.Execute(ctx, run, repo, dir) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		waiting := e.waiting
		e.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("review did not park")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.Respond(types.StepReview, types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "strict Review") {
		t.Fatalf("coverage gate approval = %v", err)
	}
	if err := e.Respond(types.StepReview, types.ActionSkip, nil); err == nil {
		t.Fatal("strict skip accepted")
	}
	if err := e.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("aborted Review succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not settle")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA != nil || got.Status != types.RunFailed {
		t.Fatalf("unreviewed run gained authority: %+v", got)
	}
}

func TestShippingRevalidationRevokesDurableAndPublicReadiness(t *testing.T) {
	database, p, old, repo := setupTest(t)
	run, err := database.InsertRunWithShipping(repo.ID, "feature", old.HeadSHA, old.BaseSHA, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunCIReadyWithReason(run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(database, p, &config.Config{}, nil, []Step{newPassStep(types.StepRebase), newPassStep(types.StepReview), newPassStep(types.StepCI)}, nil)
	events := collectEvents(e)
	if _, err := e.prepareRestart(run, repo, types.StepRebase, 2); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CIReadyAt != nil || run.CIReadyAt != nil {
		t.Fatal("restart retained old comparison readiness")
	}
	var revoked bool
	for _, event := range events.events {
		if event.Type == ipc.EventCIReadinessChanged && event.CIReady != nil && !*event.CIReady {
			revoked = true
		}
	}
	if !revoked {
		t.Fatal("restart omitted public readiness revocation")
	}
}

// Run 01M4QC5RSZKKXR1YN19BP7907H: the target moved while a strict Review fix
// round ran, so Review asked to restart from Rebase. The selected findings it
// carried had no verifying coverage record, and they used to turn that restart
// into a fix_review gate strict shipping can neither approve nor skip, and a
// further fix round would only request the same restart. The restart must be
// honored, and the restarted Review continues the carried set: its complete
// review verifies the fixed findings, while a finding the operator declined
// still parks rather than shipping unresolved.
func TestShippingReviewRestartInsideFixRoundIsHonored(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected []string
		parkedOn string
	}{
		{name: "every finding selected", selected: []string{"review-1", "review-2"}},
		{name: "one finding declined", selected: []string{"review-1"}, parkedOn: "review-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, baseRun, repo := setupTest(t)
			workDir := completionFixture(t, database, baseRun)
			run, err := database.InsertRunWithShipping(repo.ID, "feature", baseRun.HeadSHA, baseRun.HeadSHA, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
			if err != nil {
				t.Fatal(err)
			}
			bindCompletionFixture(t, database, run, workDir)
			for _, path := range []string{"a.go", "b.go"} {
				if err := os.WriteFile(workDir+"/"+path, []byte("package fixture\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := git.Run(context.Background(), workDir, "add", "a.go", "b.go"); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(context.Background(), workDir, "commit", "-m", "add review fixtures"); err != nil {
				t.Fatal(err)
			}
			advanceCompletionFixture(t, database, run, workDir)
			const findings = `{"findings":[` +
				`{"id":"review-1","severity":"error","file":"a.go","line":1,"description":"first defect","action":"ask-user"},` +
				`{"id":"review-2","severity":"error","file":"b.go","line":2,"description":"second defect","action":"ask-user"}],` +
				`"summary":"2 findings"}`
			var rebases, reviews int
			rebase := &adaptiveCallStep{name: types.StepRebase, fn: func(*StepContext) (*StepOutcome, error) {
				rebases++
				return &StepOutcome{}, nil
			}}
			review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				reviews++
				switch reviews {
				case 1:
					return &StepOutcome{NeedsApproval: true, Findings: findings, ReviewedPaths: []string{"a.go", "b.go"}, ReviewablePaths: []string{"a.go", "b.go"}}, nil
				case 2:
					if !sctx.Fixing {
						t.Error("second review execution is not the operator's fix round")
					}
					return &StepOutcome{RestartFrom: types.StepRebase}, nil
				default:
					if sctx.Fixing || sctx.PreviousFindings != "" {
						t.Errorf("restarted review is not a fresh complete review: fixing=%v previous=%q", sctx.Fixing, sctx.PreviousFindings)
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
					return &StepOutcome{Findings: `{"findings":[],"summary":"clean"}`, ReviewedPaths: []string{"a.go", "b.go"}, ReviewablePaths: []string{"a.go", "b.go"}, ReviewApprovedHeadSHA: run.HeadSHA, ReviewComparisonGeneration: reviewComparison.Generation}, nil
				}
			}}
			exec := NewExecutor(database, p, &config.Config{}, nil, []Step{rebase, review}, nil)
			done, _ := startExecutor(t, exec, run, repo, workDir)

			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			if err := respondFixPartial(t, exec, types.StepReview, tc.selected...); err != nil {
				t.Fatal(err)
			}
			if tc.parkedOn == "" {
				waitExecutorDone(t, done)
			} else {
				deadline := time.Now().Add(10 * time.Second)
				for {
					steps, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					rounds, err := database.GetRoundsByStep(steps[1].ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(rounds) == 3 && steps[1].Status == types.StepStatusAwaitingApproval {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("restarted review did not park: %d rounds, status %s", len(rounds), steps[1].Status)
					}
					time.Sleep(5 * time.Millisecond)
				}
				exec.mu.Lock()
				gate := exec.waitingFindings
				exec.mu.Unlock()
				parsed, err := types.ParseFindingsJSON(gate)
				if err != nil || len(parsed.Items) != 1 || parsed.Items[0].ID != tc.parkedOn {
					t.Fatalf("restarted review parked on %s (%v), want only the declined %s", gate, err, tc.parkedOn)
				}
				if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "strict Review") {
					t.Fatalf("strict approval of the declined finding = %v", err)
				}
				if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
					t.Fatal(err)
				}
				<-done
			}

			if rebases != 2 || reviews != 3 {
				t.Fatalf("rebase ran %d times and review %d times, want the restart to rebase again and fully re-review", rebases, reviews)
			}
			got, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.parkedOn == "" && (got.Status != types.RunCompleted || got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != run.HeadSHA) {
				t.Fatalf("run after honored restart = %+v", got)
			}
			if tc.parkedOn != "" && got.ReviewApprovedHeadSHA != nil {
				t.Fatalf("declined finding shipped with review authority: %+v", got)
			}
		})
	}
}

// The recovered-gate path must hand a Review-requested restart the same carry
// as the live loop: a daemon restart between the park and the operator's
// partial fix may not let the declined finding vanish with the restart.
func TestRecoveredReviewRestartInsideFixRoundKeepsTheDeclinedFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	rebaseResult, err := database.InsertStepResult(run.ID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(rebaseResult.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	_, recoveredRun := seedRecoveredReviewGate(t, database, run, reviewCarryTwoFindings, types.StepStatusAwaitingApproval, "")
	var rebases, reviews atomic.Int32
	rebase := &adaptiveCallStep{name: types.StepRebase, fn: func(*StepContext) (*StepOutcome, error) {
		rebases.Add(1)
		return &StepOutcome{}, nil
	}}
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if reviews.Add(1) == 1 {
			return &StepOutcome{RestartFrom: types.StepRebase}, nil
		}
		return &StepOutcome{Findings: `{"findings":[],"summary":"clean"}`, ReviewedPaths: []string{"service.go", "cache.go"}, ReviewablePaths: []string{"service.go", "cache.go"}}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{rebase, review}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, recoveredRun, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, []string{"review-2"}, nil, nil, ""); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate never accepted the partial fix")
		}
		time.Sleep(10 * time.Millisecond)
	}
	parked := func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return exec.waiting && exec.waitingStep == types.StepReview
	}
	for reviews.Load() < 2 || !parked() {
		if time.Now().After(deadline) {
			t.Fatalf("restarted review did not park; reviews=%d", reviews.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	exec.mu.Lock()
	gate := exec.waitingFindings
	exec.mu.Unlock()
	parsed, err := types.ParseFindingsJSON(gate)
	if err != nil || len(parsed.Items) != 1 || parsed.Items[0].ID != "review-2" {
		t.Fatalf("restarted review parked on %s (%v), want only the declined review-2", gate, err)
	}
	if rebases.Load() != 1 {
		t.Fatalf("restart rebased %d times, want once", rebases.Load())
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	<-done
}
