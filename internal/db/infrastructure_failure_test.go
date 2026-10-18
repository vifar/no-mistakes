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
	if stored.Status != types.RunRunning || stored.Error != nil || stored.HeadSHA != "head" {
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
}
