package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInfrastructureTerminationIsAtomicAndSettlesActiveSteps(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test", "git@github.com:test/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	past := now() - 60
	if _, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since=?, parked_ms=1500 WHERE id=?`, past, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER reject_infrastructure_step BEFORE UPDATE OF status ON step_results BEGIN SELECT RAISE(ABORT, 'injected terminal step failure'); END`); err != nil {
		t.Fatal(err)
	}
	message := sqlitewrite.ErrorName + ": infrastructure failure"
	if err := d.FailRunInfrastructure(run.ID, message, "verified"); err == nil {
		t.Fatal("expected terminal write fault")
	}
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != types.RunRunning || stored.Error != nil || stored.HeadSHA != "head" || stored.AwaitingAgentSince == nil || *stored.AwaitingAgentSince != past || stored.ParkedMS != 1500 {
		t.Fatalf("run changed without its step: %+v", stored)
	}
	if _, err := d.sql.Exec(`DROP TRIGGER reject_infrastructure_step`); err != nil {
		t.Fatal(err)
	}
	if err := d.FailRunInfrastructure(run.ID, message, "verified"); err != nil {
		t.Fatal(err)
	}
	stored, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != message || stored.HeadSHA != "verified" || stored.TerminalHeadVerifiedAt == nil || got.Status != types.StepStatusFailed || got.Error == nil || *got.Error != message {
		t.Fatalf("infrastructure state not settled: run=%+v step=%+v", stored, got)
	}
	if stored.AwaitingAgentSince != nil || stored.ParkedMS < 61500 || stored.ParkedMS > 63500 {
		t.Fatalf("terminal run still parked or lost parked time: %+v", stored)
	}
}

func TestInfrastructureTerminationParkedAccounting(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		for _, state := range []string{"unsettled", "already_settled", "future_marker", "null_total"} {
			t.Run(string(status)+"/"+state, func(t *testing.T) {
				d, _, run := openSessionTestDB(t)
				if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				step, err := d.InsertStepResult(run.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.ParkStepForApproval(run.ID, step.ID, status, 0, 0, nil); err != nil {
					t.Fatal(err)
				}
				var marker any = now() - 60
				var total any = 1500
				minTotal, maxTotal := int64(61500), int64(63500)
				switch state {
				case "already_settled":
					marker = nil
					minTotal, maxTotal = 1500, 1500
				case "future_marker":
					marker = now() + 3600
					minTotal, maxTotal = 1500, 1500
				case "null_total":
					total = nil
					minTotal, maxTotal = 60000, 62000
				}
				if _, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since=?, parked_ms=? WHERE id=?`, marker, total, run.ID); err != nil {
					t.Fatal(err)
				}
				message := sqlitewrite.ErrorName + ": infrastructure failure"
				if err := d.FailRunInfrastructure(run.ID, message, ""); err != nil {
					t.Fatal(err)
				}
				stored, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != types.RunFailed || stored.AwaitingAgentSince != nil || stored.ParkedMS < minTotal || stored.ParkedMS > maxTotal {
					t.Fatalf("terminal parked accounting: %+v", stored)
				}
				got, err := d.GetStepResult(step.ID)
				if err != nil || got.Status != types.StepStatusFailed {
					t.Fatalf("parked step not failed: %+v, %v", got, err)
				}
				if err := d.FailRunInfrastructure(run.ID, message, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := d.RecoverStaleRuns("restart"); err != nil {
					t.Fatal(err)
				}
				recovered, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if recovered.AwaitingAgentSince != nil || recovered.ParkedMS != stored.ParkedMS || recovered.Error == nil || *recovered.Error != message {
					t.Fatalf("repeated settlement or recovery changed terminal truth: %+v", recovered)
				}
			})
		}
	}
}
