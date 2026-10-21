package branchsync

import (
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCancelledStalePushOffersAndRecoversCustody(t *testing.T) {
	for _, action := range []string{"inspect", "recover"} {
		t.Run(action, func(t *testing.T) {
			f := newRecoverFixture(t, types.RunCancelled)
			mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.preserved)
			mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
			step, _ := f.db.InsertStepResult(f.run.ID, types.StepPush)
			if err := f.db.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if action == "inspect" {
				state := f.service.InspectCached(f.ctx)
				if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
					t.Fatalf("cancelled run action = %+v, state=%+v", state.NextAction, state)
				}
			} else {
				state := f.service.Recover(f.ctx, false)
				if !state.Recovered {
					t.Fatalf("cancelled stale push was not recovered: %+v", state)
				}
				run, _ := f.db.GetRun(f.run.ID)
				if run.CustodyReturnedAt == nil {
					t.Fatal("custody not stamped")
				}
				if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
					t.Fatalf("head = %s", got)
				}
			}
		})
	}
}

func TestTerminalPushWithAgentNamesRowInsteadOfContinue(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	step, _ := f.db.InsertStepResult(f.run.ID, types.StepPush)
	if err := f.db.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := f.db.SetStepAgentActivity(step.ID, "agent running", &pid); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || !strings.Contains(state.Error, step.ID) || !strings.Contains(state.Error, f.run.ID) || state.NextAction == nil || state.NextAction.Code == "continue_active_run" {
		t.Fatalf("terminal push must name its row and repair command: %+v", state)
	}
}
