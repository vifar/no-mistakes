package pipeline

import (
	"context"
	"database/sql"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// An answered gate whose dispatch cannot be persisted fails its step and run.
// Neither may stay durably marked as parked or keep a response to re-apply.
func TestAnsweredGateDispatchFailureClearsTheParkedState(t *testing.T) {
	database, p, run, _ := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	row, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(row.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"review"}`
	if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepPendingResponse(row.ID, `{"action":"approve"}`, answeredActivity); err != nil {
		t.Fatal(err)
	}
	control, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`
		CREATE TRIGGER fail_gate_dispatch
		BEFORE UPDATE OF status ON step_results WHEN NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'dispatch write failed');
		END
	`); err != nil {
		control.Close()
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, p, nil, nil, nil, nil)
	exec.SetRunSlot(&publicationRunSlot{})
	if err := exec.acquireAnsweredGateSlot(context.Background(), run.ID, row.ID); err == nil {
		t.Fatal("dispatch succeeded despite the failed write")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AwaitingAgentSince != nil {
		t.Fatalf("run still marked parked after its dispatch failed: %+v", got)
	}
	if err := database.FailStep(row.ID, "resume answered gate failed", 1); err != nil {
		t.Fatal(err)
	}
	step, err := database.GetStepResult(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if step.PendingResponse != nil {
		t.Fatalf("failed step kept its pending response: %q", *step.PendingResponse)
	}
}
