package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

type restartLockStep struct {
	Step
	beforeName func()
}

func (s *restartLockStep) Name() types.StepName {
	s.beforeName()
	return s.Step.Name()
}

func TestExecutorRestartContentionPreservesInfrastructureFailure(t *testing.T) {
	for _, path := range []string{"live", "recovered_gate", "recovered_remainder"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			database, p, run, repo := setupTest(t)
			workDir := completionFixture(t, database, run)
			restartRequested := false
			push := &contentionStep{shutdownBlockingStep: newShutdownBlockingStep(types.StepPush)}
			push.execute = func(*StepContext) (*StepOutcome, error) {
				restartRequested = true
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			review := &restartLockStep{Step: newPassStep(types.StepReview), beforeName: func() {}}
			steps := []Step{review, push}
			if path != "live" {
				reviewed, err := database.InsertStepResult(run.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.CompleteStepWithStatus(reviewed.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
				if path == "recovered_gate" {
					findings := `{"findings":[{"id":"push-1","severity":"warning","description":"needs fix","action":"ask-user"}],"summary":"needs fix"}`
					pending, err := database.InsertStepResult(run.ID, types.StepPush)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.StartStep(pending.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := database.InsertStepRound(pending.ID, 1, "initial", &findings, nil, 1); err != nil {
						t.Fatal(err)
					}
					if err := database.ParkStepForApproval(run.ID, pending.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
						t.Fatal(err)
					}
					if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					if err := database.SetRunAwaitingAgent(run.ID); err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
				} else if _, err := database.InsertStepResult(run.ID, types.StepPush); err != nil {
					t.Fatal(err)
				}
			}
			holder, err := sql.Open("sqlite", p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			holder.SetMaxOpenConns(1)
			released := make(chan error, 1)
			locked := false
			gateReady := make(chan struct{}, 1)
			review.beforeName = func() {
				if !restartRequested || locked {
					return
				}
				if _, err := holder.Exec("BEGIN IMMEDIATE"); err != nil {
					t.Errorf("acquire competing writer: %v", err)
					return
				}
				locked = true
				go func() {
					time.Sleep(sqlitewrite.Budget + sqlitewrite.BusyTimeout + time.Second)
					_, err := holder.Exec("ROLLBACK")
					released <- err
				}()
			}
			executor := NewExecutor(database, p, nil, nil, steps, func(event ipc.Event) {
				if event.StepName != nil && *event.StepName == types.StepPush && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					gateReady <- struct{}{}
				}
			})
			executor.SetForgeContext(&forgecontext.Context{})
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if path == "recovered_gate" {
				done := make(chan error, 1)
				go func() { done <- executor.Resume(ctx, run, repo, workDir) }()
				select {
				case <-gateReady:
					if responseErr := executor.Respond(types.StepPush, types.ActionFix, []string{"push-1"}, gateSnapshot(executor)); responseErr != nil {
						t.Error(responseErr)
						cancel()
					}
				case <-ctx.Done():
					t.Error("recovered gate was never published")
				}
				err = <-done
			} else if path == "recovered_remainder" {
				executor.initializeRunScopes(run.ID)
				executor.workDir = workDir
				err = executor.executeRecoveredRemainder(ctx, run, repo, workDir, t.TempDir(), 1, "")
			} else {
				err = executor.Execute(ctx, run, repo, workDir)
			}
			if locked {
				if releaseErr := <-released; releaseErr != nil {
					t.Fatal(releaseErr)
				}
			}
			var contention *sqlitewrite.ContentionError
			var cause *sqlite.Error
			if !locked || !errors.As(err, &contention) || !errors.As(err, &cause) {
				t.Fatalf("restart discarded storage cause: locked=%v error=%T %v", locked, err, err)
			}
			if contention.Operation != "ResetStepsFrom" || contention.Attempts < 2 || cause.Code()&0xff != 5 {
				t.Fatalf("incorrect restart contention: %+v, SQLite code %d", contention, cause.Code())
			}
			data, readErr := os.ReadFile(executor.storageFailurePath(run.ID))
			if readErr != nil {
				t.Fatal(readErr)
			}
			var marker storageFailureRecord
			if err := json.Unmarshal(data, &marker); err != nil || marker.RunID != run.ID || marker.Error != contention.Error() {
				t.Fatalf("lost independent storage outcome: %+v %v", marker, err)
			}
			stored, readErr := database.GetRun(run.ID)
			if readErr != nil || stored == nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != contention.Error() {
				t.Fatalf("lost terminal storage outcome: %+v %v", stored, readErr)
			}
			results, readErr := database.GetStepsByRun(run.ID)
			if readErr != nil || len(results) != 2 {
				t.Fatalf("lost restart step records: %+v %v", results, readErr)
			}
			if results[1].StepName != types.StepPush || results[1].Status != types.StepStatusFailed || results[1].Error == nil || *results[1].Error != contention.Error() {
				t.Fatalf("restart failure stranded its active step: %+v", results[1])
			}
			executor.ReportPublishedHeadFailure(context.Background(), stored, repo)
			recovered := NewExecutor(database, p, nil, nil, steps, nil)
			recovered.SetForgeContext(&forgecontext.Context{})
			recovered.ReportRecoveredPublishedHeadStatus(context.Background(), stored, repo)
			if states := push.reported(); len(states) != 0 {
				t.Fatalf("storage failure fabricated forge verdicts: %v", states)
			}
		})
	}
}
