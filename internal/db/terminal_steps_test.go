package db

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #102: cancelling the run must settle the executing row in the same write.
func TestTerminalCancellationClosesActiveSteps(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepPush)
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordIneligibleTerminal(run.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled, nil); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.StepStatusFailed || got.CompletedAt == nil || got.AgentPID != nil {
		t.Fatalf("cancelled run retained executing step: %+v", got)
	}
}

func TestCloseTerminalStepRejectsChangedEvidence(t *testing.T) {
	for _, change := range []string{"activity", "pid", "run active", "completed"} {
		t.Run(change, func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
			step, _ := d.InsertStepResult(run.ID, types.StepPush)
			if err := d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			before, _ := d.GetStepResult(step.ID)
			var err error
			switch change {
			case "activity":
				_, err = d.sql.Exec("UPDATE step_results SET last_activity_at=last_activity_at+1 WHERE id=?", step.ID)
			case "pid":
				pid := 123
				err = d.SetStepAgentActivity(step.ID, "agent started", &pid)
			case "run active":
				err = d.UpdateRunStatus(run.ID, types.RunRunning)
			case "completed":
				err = d.CompleteStep(step.ID, 0, 1, "push.log")
			}
			if err != nil {
				t.Fatal(err)
			}
			changed, err := d.CloseTerminalStep(context.Background(), before, *before.StartedAt+600)
			if err != nil || changed {
				t.Fatalf("changed %s evidence was overwritten: %t %v", change, changed, err)
			}
		})
	}
}

// Issue #102 must not reinterpret the existing certified CI watcher outcome.
func TestTerminalStepCleanupPreservesCertifiedWatcher(t *testing.T) {
	d, run, publication := certifiedStatusFixture(t)
	ci, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, "head", "config", types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	candidates, err := d.TerminalExecutingSteps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range candidates {
		if step.ID == ci.ID {
			t.Fatal("certified watcher was offered to generic stale-step cleanup")
		}
	}
	row, _ := d.GetStepResult(ci.ID)
	changed, err := d.CloseTerminalStep(context.Background(), row, *row.StartedAt+600)
	if err != nil || changed {
		t.Fatalf("certified watcher evidence changed: %t %v", changed, err)
	}
	got, err := d.GetCommitStatusPublication(context.Background(), run.ID, publication)
	if err != nil || got.PreserveSuccess == nil || !*got.PreserveSuccess {
		t.Fatalf("preserved success lost: %+v %v", got, err)
	}
}

func TestTerminalMaintenanceDatabaseWaitIsCancellable(t *testing.T) {
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/tmp/repo", "origin", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			step, err := d.InsertStepResult(run.ID, types.StepPush)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			step, err = d.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := d.sql.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			waits := d.sql.Stats().WaitCount
			go func() {
				if operation == "read" {
					_, err := d.TerminalExecutingSteps(ctx)
					done <- err
				} else {
					_, err := d.CloseTerminalStep(ctx, step, time.Now().Unix())
					done <- err
				}
			}()
			deadline := time.After(time.Second)
			for d.sql.Stats().WaitCount == waits {
				select {
				case <-deadline:
					t.Fatal("maintenance did not wait for held connection")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled database operation succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("database wait ignored cancellation")
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			row, err := d.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != types.StepStatusRunning {
				t.Fatalf("cancelled operation changed row: %+v", row)
			}
		})
	}
}
