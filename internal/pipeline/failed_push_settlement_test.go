package pipeline

import (
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFailedPushSettlementClosesExecutingRecords(t *testing.T) {
	d, p, run, repo := setupTest(t)
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
	if err := d.SetRunPushActive(run.ID, true); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("check PR context after push: PR comparison changed after review; start a new run for the current target and head")
	e := NewExecutor(d, p, &config.Config{}, nil, nil, nil)
	if err := e.FailRun(run, repo, failure); !errors.Is(err, failure) {
		t.Fatalf("failure lost: %v", err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.PushActive {
		t.Fatalf("terminal run=%+v, %v", stored, err)
	}
	row, err := d.GetStepResult(step.ID)
	if err != nil || row.Status != types.StepStatusFailed || row.CompletedAt == nil || row.Error == nil {
		t.Fatalf("terminal run has contradictory Push record: %+v, %v", row, err)
	}
}
