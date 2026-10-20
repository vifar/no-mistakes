package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutorAfterStepGuardRefusalFailsTheStep proves a run whose
// after-step comparison guard refuses does not leave its step "running".
//
// The measured failure: the run failed at the after-push guard while its push
// step was still "running", and branch_sync then reported
// blocked_push_in_progress with no supported exit - the branch could not be
// reconciled at all.
func TestExecutorAfterStepGuardRefusalFailsTheStep(t *testing.T) {
	d, p, run, repo := setupTest(t)
	dir := completionFixture(t, d, run)
	guardErr := errors.New("check PR context after push: PR comparison changed after review")
	e := NewExecutor(d, p, &config.Config{}, nil, []Step{newPassStep(types.StepRebase), newPassStep(types.StepPush)}, nil)
	e.SetPRContextGuard(func(c *StepContext, step types.StepName) (PRContextDecision, error) {
		if step == types.StepPush && c.PRContextAfterStep {
			return PRContextDecision{}, guardErr
		}
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := e.Execute(ctx, run, repo, dir)
	if err == nil {
		t.Fatal("the guard refusal did not fail the run")
	}
	if !strings.Contains(err.Error(), "PR comparison changed after review") {
		t.Fatalf("the run error = %v, want the guard's refusal", err)
	}
	// The step is failed, not running: a running step on a terminal run
	// wedges branch_sync in push_in_progress.
	rows, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName != types.StepPush {
			continue
		}
		if row.Status == types.StepStatusRunning || row.Status == types.StepStatusFixing {
			t.Fatalf("the refused step stayed %s: %+v", row.Status, row)
		}
		if row.Status != types.StepStatusFailed {
			t.Fatalf("the refused step is %s, want failed: %+v", row.Status, row)
		}
		if row.Error == nil || !strings.Contains(*row.Error, "PR comparison changed after review") {
			t.Fatalf("the failed step does not name the refusal: %+v", row.Error)
		}
	}
}
