package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFailedRunClosesOnlyUnfinishedSteps(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "local-upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.InsertRun(repo.ID, "other", "other-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	otherStep, err := d.InsertStepResult(other.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(otherStep.ID); err != nil {
		t.Fatal(err)
	}
	statuses := []types.StepStatus{types.StepStatusPending, types.StepStatusRunning, types.StepStatusFixing, types.StepStatusAwaitingApproval, types.StepStatusFixReview, types.StepStatusCompleted, types.StepStatusFailed, types.StepStatusSkipped}
	ids := make([]string, len(statuses))
	for i, status := range statuses {
		step, err := d.InsertStepResult(run.ID, types.StepName("custom-step-"+string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = step.ID
		if _, err := d.sql.Exec(`UPDATE step_results SET status=?, findings_json='{"findings":[]}', error='prior evidence', log_path='step.log' WHERE id=?`, status, step.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.RecordIneligibleTerminal(run.ID, "post-push comparison refused", types.RunFailed, nil); err != nil {
		t.Fatal(err)
	}
	for i, status := range statuses {
		row, err := d.GetStepResult(ids[i])
		if err != nil {
			t.Fatal(err)
		}
		unfinished := status == types.StepStatusRunning || status == types.StepStatusFixing || status == types.StepStatusAwaitingApproval || status == types.StepStatusFixReview
		if unfinished {
			if row.Status != types.StepStatusFailed || row.CompletedAt == nil || row.AgentPID != nil || row.PendingResponse != nil {
				t.Errorf("%s stayed unfinished: %+v", status, row)
			}
		} else if row.Status != status || row.Error == nil || *row.Error != "prior evidence" {
			t.Errorf("changed finished/pending evidence: %+v", row)
		}
		if row.FindingsJSON == nil || *row.FindingsJSON != `{"findings":[]}` || row.LogPath == nil || *row.LogPath != "step.log" {
			t.Errorf("lost step evidence: %+v", row)
		}
	}
	row, err := d.GetStepResult(otherStep.ID)
	if err != nil || row.Status != types.StepStatusRunning {
		t.Fatalf("changed another run: %+v, %v", row, err)
	}
}

func TestFailedRunAndStepSettlementIsAtomic(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "local-upstream", "main")
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
	if _, err := d.sql.Exec(`CREATE TRIGGER reject_step_settlement BEFORE UPDATE ON step_results BEGIN SELECT RAISE(ABORT, 'step settlement unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordIneligibleTerminal(run.ID, "failure", types.RunFailed, nil); err == nil {
		t.Error("terminal run committed despite failed step settlement")
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("partial terminal write: %+v, %v", stored, err)
	}
	row, err := d.GetStepResult(step.ID)
	if err != nil || row.Status != types.StepStatusRunning {
		t.Fatalf("partial step write: %+v, %v", row, err)
	}
}
