package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// pushBoundaryRewindFixture builds a run whose validation completed and whose
// Push step is about to run. The guard reports a target advance at the Push
// boundary exactly once, so the executor must take the rewind path.
func pushBoundaryRewindFixture(t *testing.T, advanceAtPush bool) (*Executor, *db.Run, *db.Repo, string, *bool) {
	t.Helper()
	d, p, run, repo := setupTest(t)
	dir := completionFixture(t, d, run)
	rewound := false
	pushRuns := 0
	e := NewExecutor(d, p, &config.Config{}, nil, []Step{newPassStep(types.StepRebase), newPassStep(types.StepReview), newPassStep(types.StepTest), newPassStep(types.StepDocument), newPassStep(types.StepLint), &countingPushStep{count: &pushRuns}}, nil)
	e.SetPRContextGuard(func(c *StepContext, step types.StepName) (PRContextDecision, error) {
		decision := PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}
		previous, err := d.GetRunPRContext(run.ID)
		if err != nil {
			return decision, err
		}
		if previous == nil {
			return decision, nil
		}
		if advanceAtPush && !rewound && step == types.StepPush {
			candidate := previous.PRContextCandidate
			candidate.TargetSHA = strings.Repeat("c", 40)
			candidate.MergeBaseSHA = strings.Repeat("c", 40)
			if _, err := d.RewindRunPRContext(run.ID, candidate, db.TargetAdvanceRewindRequest{ExpectedGeneration: previous.Generation, Step: step, MaxRewinds: 3}); err != nil {
				return decision, err
			}
			rewound = true
			decision.RestartFrom = types.StepRebase
		}
		return decision, nil
	})
	return e, run, repo, dir, &rewound
}

// countingPushStep records how many times the Push step ran.
type countingPushStep struct {
	count *int
}

func (s *countingPushStep) Name() types.StepName { return types.StepPush }

func (s *countingPushStep) Execute(c *StepContext) (*StepOutcome, error) {
	*s.count++
	return &StepOutcome{}, nil
}

// TestExecutorTargetAdvanceAtThePushBoundaryRewindsAndThenPublishes proves
// the whole executor flow: a target advance at the Push boundary sends the
// run back to Rebase, the validation steps run again, and the run then
// publishes. Before the fix, the run died at Push with "live target commit,
// merge base, or diff differs from PR comparison receipt before update".
func TestExecutorTargetAdvanceAtThePushBoundaryRewindsAndThenPublishes(t *testing.T) {
	e, run, repo, dir, rewound := pushBoundaryRewindFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Execute(ctx, run, repo, dir); err != nil {
		t.Fatalf("the run died on a target advance at the push boundary: %v", err)
	}
	if !*rewound {
		t.Fatal("the target advance never reached the rewind")
	}
	// The run completed.
	current, err := e.db.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != types.RunCompleted {
		t.Fatalf("the run status = %s, want completed: %v", current.Status, current.Error)
	}
	// The validation ran again on the new head: the archived history records
	// the rewind, and the steps were reset by it.
	history, err := e.db.GetTargetRewindHistory(run.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("missing durable rewind history: %+v %v", history, err)
	}
}

// TestExecutorTargetAdvanceAtThePushBoundaryIsBounded proves the rewind at
// the Push boundary spends the run's rewind budget like every other
// boundary, so a target that keeps moving cannot loop the run forever.
func TestExecutorTargetAdvanceAtThePushBoundaryIsBounded(t *testing.T) {
	d, p, run, repo := setupTest(t)
	dir := completionFixture(t, d, run)
	rewinds := 0
	e := NewExecutor(d, p, &config.Config{}, nil, []Step{newPassStep(types.StepRebase), newPassStep(types.StepReview), newPassStep(types.StepTest), newPassStep(types.StepDocument), newPassStep(types.StepLint), newPassStep(types.StepPush)}, nil)
	e.SetPRContextGuard(func(c *StepContext, step types.StepName) (PRContextDecision, error) {
		decision := PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}
		previous, err := d.GetRunPRContext(run.ID)
		if err != nil || previous == nil {
			return decision, err
		}
		if step == types.StepPush {
			// The real classification path: the budget is the database's, and
			// a spent budget refuses the rewind instead of looping. A target
			// that did not advance is not a rewind at all.
			eligible, err := d.CanRewindTarget(run.ID, step)
			if err != nil {
				return decision, err
			}
			if !eligible {
				return decision, fmt.Errorf("PR comparison changed after rebase; start a new run for the current target and head")
			}
			candidate := previous.PRContextCandidate
			candidate.TargetSHA = strings.Repeat("c", 40)
			candidate.MergeBaseSHA = strings.Repeat("c", 40)
			if previous.TargetSHA == candidate.TargetSHA {
				return decision, fmt.Errorf("PR comparison changed after rebase; the target did not advance")
			}
			rewinds++
			if _, err := d.RewindRunPRContext(run.ID, candidate, db.TargetAdvanceRewindRequest{ExpectedGeneration: previous.Generation, Step: step, MaxRewinds: 1}); err != nil {
				return decision, err
			}
			decision.RestartFrom = types.StepRebase
		}
		return decision, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = e.Execute(ctx, run, repo, dir)
	if rewinds > 2 {
		t.Fatalf("the push boundary rewind looped %d times", rewinds)
	}
}
