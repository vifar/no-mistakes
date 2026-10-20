package pipeline

import (
	"context"
	"errors"
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
//
// Each attempt supplies a DISTINCT advanced target, so the run really does
// see a new advance every time. The oracle is the documented budget refusal:
// the database refuses the rewind once the budget is spent, and the run then
// fails with that refusal instead of rewinding again.
func TestExecutorTargetAdvanceAtThePushBoundaryIsBounded(t *testing.T) {
	d, p, run, repo := setupTest(t)
	dir := completionFixture(t, d, run)
	attempts := 0
	var refusal error
	e := NewExecutor(d, p, &config.Config{}, nil, []Step{newPassStep(types.StepRebase), newPassStep(types.StepReview), newPassStep(types.StepTest), newPassStep(types.StepDocument), newPassStep(types.StepLint), newPassStep(types.StepPush)}, nil)
	e.SetPRContextGuard(func(c *StepContext, step types.StepName) (PRContextDecision, error) {
		decision := PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}
		previous, err := d.GetRunPRContext(run.ID)
		if err != nil || previous == nil {
			return decision, err
		}
		if step != types.StepPush {
			return decision, nil
		}
		attempts++
		// A distinct advanced target every time, so "the target did not
		// advance" can never be the reason the attempt stops.
		candidate := previous.PRContextCandidate
		candidate.TargetSHA = strings.Repeat(fmt.Sprintf("%c", 'a'+attempts), 40)
		candidate.MergeBaseSHA = candidate.TargetSHA
		if previous.TargetSHA == candidate.TargetSHA {
			return decision, fmt.Errorf("the fixture did not advance the target")
		}
		if _, err := d.RewindRunPRContext(run.ID, candidate, db.TargetAdvanceRewindRequest{ExpectedGeneration: previous.Generation, Step: step, MaxRewinds: 1}); err != nil {
			refusal = err
			return decision, err
		}
		decision.RestartFrom = types.StepRebase
		return decision, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := e.Execute(ctx, run, repo, dir)
	if err == nil {
		t.Fatal("a run whose target keeps moving completed instead of refusing")
	}
	if refusal == nil {
		t.Fatal("the run stopped for a reason other than the rewind budget")
	}
	if !errors.Is(refusal, db.ErrTargetAdvanceCap) {
		t.Fatalf("the run stopped with %v, want the rewind budget refusal", refusal)
	}
	if attempts != 2 {
		t.Fatalf("the push boundary was reached %d times, want 2 (one rewind and the refusal)", attempts)
	}
	// The budget is spent exactly once.
	current, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.TargetAdvanceRewinds != 1 {
		t.Fatalf("the rewind budget = %d, want 1", current.TargetAdvanceRewinds)
	}
}
