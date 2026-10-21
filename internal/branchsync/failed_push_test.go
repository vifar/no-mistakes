package branchsync

import (
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A legacy failed Push has no publisher, but its old step row still says
// running. The issue's oracle is custody recovery, never continue_active_run.
func failedPushFixture(t *testing.T) (*recoverFixture, *db.StepResult) {
	t.Helper()
	f := newRecoverFixture(t, types.RunFailed)
	step, err := f.db.InsertStepResult(f.run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	return f, step
}

func TestFailedPushStepDoesNotWedgeBranch(t *testing.T) {
	f, _ := failedPushFixture(t)
	state := f.service.InspectCached(f.ctx)
	if state.State == StatePushInProgress || state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("failed Push must offer custody recovery, got state=%s safety=%s next=%+v", state.State, state.Safety, state.NextAction)
	}
}

func TestFailedPushHasSupportedCustodyExit(t *testing.T) {
	f, _ := failedPushFixture(t)
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered {
		t.Fatalf("axi sync --recover must release failed Push: %+v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD=%s, want %s", got, f.preserved)
	}
	stored, err := f.db.GetRun(f.run.ID)
	if err != nil || stored.CustodyReturnedAt == nil {
		t.Fatalf("custody not durably returned: %+v, %v", stored, err)
	}
	state = f.service.InspectCached(f.ctx)
	if state.State != StateCustodyReturned || (state.NextAction != nil && state.NextAction.Code == "continue_active_run") {
		t.Fatalf("recovered branch still blocked: %+v", state)
	}
}

func TestFailedPushRecoveryDoesNotInventPublication(t *testing.T) {
	f, _ := failedPushFixture(t)
	assertUnpublished := func() {
		t.Helper()
		stored, err := f.db.GetRun(f.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.LastPushedSHA != nil || stored.PushGeneration != nil || stored.PRURL != nil {
			t.Fatalf("invented publication: %+v", stored)
		}
		publications, err := f.db.GetCommitStatusPublications(f.ctx, f.run.ID)
		if err != nil || len(publications) != 0 {
			t.Fatalf("invented status: %+v, %v", publications, err)
		}
		if got := mustRun(t, f.remote, "for-each-ref", "refs/heads"); got != "" {
			t.Fatalf("invented remote branch: %s", got)
		}
	}
	assertUnpublished()
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered {
		t.Errorf("recovery failed: %+v", state)
	}
	assertUnpublished()
}

func TestLivePushStillBlocksCustody(t *testing.T) {
	for _, signal := range []string{"running step", "fixing step", "push_active", "terminal process", "terminal push_active"} {
		t.Run(signal, func(t *testing.T) {
			f := newRecoverFixture(t, types.RunRunning)
			if signal == "terminal process" || signal == "terminal push_active" {
				if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, f.preserved); err != nil {
					t.Fatal(err)
				}
			}
			if signal == "push_active" || signal == "terminal push_active" {
				if err := f.db.SetRunPushActive(f.run.ID, true); err != nil {
					t.Fatal(err)
				}
			} else {
				step, err := f.db.InsertStepResult(f.run.ID, types.StepPush)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.StartStep(step.ID); err != nil {
					t.Fatal(err)
				}
				if signal == "fixing step" {
					if err := f.db.StartStepFixRound(step.ID, 1); err != nil {
						t.Fatal(err)
					}
				}
				if signal == "terminal process" {
					pid := os.Getpid()
					if err := f.db.SetStepAgentActivity(step.ID, "publisher still recorded", &pid); err != nil {
						t.Fatal(err)
					}
				}
			}
			if state := f.service.InspectCached(f.ctx); state.State != StatePushInProgress {
				t.Fatalf("publisher was freed: %+v", state)
			}
			if state := f.service.Recover(f.ctx, false); state.Recovered {
				t.Fatalf("publisher custody was freed: %+v", state)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
				t.Fatalf("live publisher moved HEAD: %s", got)
			}
		})
	}
}
