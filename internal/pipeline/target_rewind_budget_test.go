package pipeline

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The old comparison spends its repair allowance and parks. Its response
// triggers a rewind; a fresh conflict must receive its own automatic repair.
func TestTargetRewindParkedResponseResetsFixBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d, p, r, repo := setupTest(t)
	dir := completionFixture(t, d, r)
	cfg := &config.Config{AutoFix: config.AutoFix{Rebase: 1}}
	calls, checks := 0, 0
	var parks atomic.Int32
	result := make(chan error, 4)
	step := &adaptiveCallStep{name: types.StepRebase, fn: func(c *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 2 {
			return &StepOutcome{DurationOverrideMS: 1000, NeedsApproval: true, Findings: `{"findings":[{"id":"old","severity":"warning","file":"f.go","description":"old blocked fix","action":"ask-user"}]}`}, nil
		}
		if calls == 3 {
			row, err := d.GetStepResult(c.StepResultID)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := d.GetRoundsByStep(c.StepResultID)
			if err != nil {
				t.Fatal(err)
			}
			if c.Fixing || c.AutoFixRound || c.PreviousFindings != "" || (row.DurationMS != nil && *row.DurationMS != 0) || len(rounds) != 0 {
				t.Errorf("new generation retained prior state: fixing=%t auto=%t row=%+v rounds=%+v", c.Fixing, c.AutoFixRound, row, rounds)
			}
		}
		if calls == 4 {
			if !c.Fixing || !c.AutoFixRound {
				t.Errorf("fresh conflict did not get automatic repair")
			}
			return &StepOutcome{DurationOverrideMS: 10}, nil
		}
		return &StepOutcome{DurationOverrideMS: 10, NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"conflict","severity":"warning","file":"f.go","description":"conflict","action":"auto-fix"}]}`}, nil
	}}
	var e *Executor
	e = NewExecutor(d, p, cfg, nil, []Step{step}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
			n := parks.Add(1)
			go func() {
				action := types.ActionFix
				if n > 1 {
					action = types.ActionAbort
				}
				err := e.Respond(types.StepRebase, action, []string{"old"}, gateSnapshot(e))
				if err != nil {
					t.Logf("response failed: %v", err)
					cancel()
				}
				result <- err
			}()
		}
	})
	e.SetPRContextGuard(func(c *StepContext, name types.StepName) (PRContextDecision, error) {
		checks++
		if checks == 4 {
			prev, err := d.GetRunPRContext(r.ID)
			if err != nil {
				return PRContextDecision{}, err
			}
			candidate := prev.PRContextCandidate
			candidate.TargetSHA = strings.Repeat("a", 40)
			_, err = d.RewindRunPRContext(r.ID, candidate, db.TargetAdvanceRewindRequest{ExpectedGeneration: prev.Generation, Step: name, MaxRewinds: 3})
			return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}, RestartFrom: types.StepRebase}, err
		}
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}, nil
	})
	err := e.Execute(ctx, r, repo, dir)
	for i := int32(0); i < parks.Load(); i++ {
		if responseErr := <-result; responseErr != nil {
			t.Fatal(responseErr)
		}
	}
	rows, readErr := d.GetStepsByRun(r.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	rounds, readErr := d.GetRoundsByStep(rows[0].ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(rounds) != 2 || rounds[0].Round != 1 || rounds[0].Trigger != "initial" || rounds[1].Round != 2 || rounds[1].Trigger != "auto_fix" {
		t.Errorf("new generation round history not reset: %+v", rounds)
	}
	if rows[0].DurationMS == nil || *rows[0].DurationMS != 20 {
		t.Errorf("new generation retained execution counters: row=%+v", rows[0])
	}
	if err != nil {
		t.Errorf("execution failed: %v", err)
	}
	if calls != 4 {
		t.Fatalf("new comparison inherited spent fix budget: calls=%d checks=%d parks=%d error=%v; want fourth call to automatically fix new conflict", calls, checks, parks.Load(), err)
	}
}
