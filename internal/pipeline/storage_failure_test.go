package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type contentionStep struct {
	*shutdownBlockingStep
	execute func(*StepContext) (*StepOutcome, error)
}

func (s *contentionStep) Execute(ctx *StepContext) (*StepOutcome, error) { return s.execute(ctx) }

func TestExecutorPRStateLockIsInfrastructureNotCodeFailure(t *testing.T) {
	for _, releaseEarly := range []bool{true, false} {
		name := "past_budget"
		if releaseEarly {
			name = "release_after_timeout"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workdir := completionFixture(t, database, run)
			step := &contentionStep{shutdownBlockingStep: newShutdownBlockingStep(types.StepCI)}
			step.execute = func(sctx *StepContext) (*StepOutcome, error) {
				holder, err := sql.Open("sqlite", p.DB())
				if err != nil {
					return nil, err
				}
				defer holder.Close()
				holder.SetMaxOpenConns(1)
				if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
					return nil, err
				}
				released := make(chan error, 1)
				if releaseEarly {
					go func() {
						time.Sleep(sqlitewrite.BusyTimeout + time.Second)
						_, err := holder.Exec(`ROLLBACK`)
						released <- err
					}()
				}
				start := time.Now()
				err = sctx.DB.UpdateRunPRState(sctx.Run.ID, "open")
				if releaseEarly {
					if releaseErr := <-released; releaseErr != nil {
						t.Error(releaseErr)
					}
					if err != nil {
						t.Errorf("transient state write: %v", err)
					}
				} else {
					// Let terminal outcome persistence succeed so recovery can check it too.
					if _, releaseErr := holder.Exec(`ROLLBACK`); releaseErr != nil {
						t.Error(releaseErr)
					}
					if !sqlitewrite.IsContention(err) {
						t.Errorf("expected named infrastructure error, got %T %v", err, err)
					}
					if time.Since(start) < sqlitewrite.Budget {
						t.Errorf("budget ended early: %v", time.Since(start))
					}
				}
				if err != nil {
					return nil, err
				}
				return &StepOutcome{ExitCode: 0}, nil
			}
			executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			executor.SetForgeContext(&forgecontext.Context{})
			err := executor.Execute(context.Background(), run, repo, workdir)
			if releaseEarly {
				if err != nil || run.Status != types.RunCompleted {
					t.Fatalf("run=%+v error=%v", run, err)
				}
			} else {
				if !sqlitewrite.IsContention(err) || run.Error == nil || !strings.Contains(*run.Error, sqlitewrite.ErrorName) {
					t.Fatalf("run=%+v error=%v", run, err)
				}
				stored, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Error == nil || !sqlitewrite.RecordedContention(*stored.Error) {
					t.Fatalf("lost durable infrastructure error: %+v", stored)
				}
				steps, stepErr := database.GetStepsByRun(run.ID)
				if stepErr != nil || len(steps) != 1 || steps[0].Status != types.StepStatusFailed || steps[0].Error == nil || !sqlitewrite.RecordedContention(*steps[0].Error) {
					t.Fatalf("infrastructure failure stranded an active step: steps=%+v err=%v", steps, stepErr)
				}
				// Both manager cleanup and a fresh recovery executor must preserve status.
				executor.ReportPublishedHeadFailure(context.Background(), run, repo)
				recovered := NewExecutor(database, p, nil, nil, []Step{step}, nil)
				recovered.ReportRecoveredPublishedHeadStatus(context.Background(), stored, repo)
			}
			for _, status := range step.reported() {
				if status == "failure" {
					t.Fatalf("good source received failure status: %v", step.reported())
				}
			}
		})
	}
}

func TestStorageFailureMarkerSurvivesMissingTerminalDatabaseWrite(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newShutdownBlockingStep(types.StepReview)
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	executor.SetForgeContext(&forgecontext.Context{})
	failure := &sqlitewrite.ContentionError{Operation: "UpdateRunPRState", Attempts: 2, Cause: errors.New("locked")}
	executor.recordStorageFailure(run, failure)
	// Simulate a still-unwritable terminal row and then ordinary crash recovery.
	if _, err := database.RecoverStaleRuns("daemon crashed"); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Error == nil || sqlitewrite.RecordedContention(*stored.Error) {
		t.Fatalf("fixture must have lost DB outcome: %+v", stored)
	}
	recovered := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	recovered.SetForgeContext(&forgecontext.Context{})
	recovered.ReportRecoveredPublishedHeadStatus(context.Background(), stored, repo)
	if len(step.reported()) != 0 {
		t.Fatalf("recovery published a code verdict: %v", step.reported())
	}
	if _, err := os.Stat(executor.storageFailurePath(run.ID)); err != nil {
		t.Fatal(err)
	}
	// A fresh attempt clears the marker so a later real code failure stays red.
	if err := recovered.clearStorageFailure(run.ID); err != nil {
		t.Fatal(err)
	}
	recovered.ReportPublishedHeadFailure(context.Background(), stored, repo)
	if len(step.reported()) != 1 || step.reported()[0] != "failure" {
		t.Fatalf("real failure suppressed: %v", step.reported())
	}
}

func TestStorageFailureDurableErrorSuppressesFailureWithoutLogs(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newShutdownBlockingStep(types.StepReview)
	message := sqlitewrite.ErrorName + ": infrastructure failure"
	if err := database.UpdateRunErrorStatus(run.ID, message, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	recovered.ReportRecoveredPublishedHeadStatus(context.Background(), stored, repo)
	if len(step.reported()) != 0 {
		t.Fatalf("recovery published failure: %v", step.reported())
	}
}

func TestCodeFailureMentioningContentionStillReportsFailure(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workdir := completionFixture(t, database, run)
	step := &contentionStep{shutdownBlockingStep: newShutdownBlockingStep(types.StepReview)}
	step.execute = func(*StepContext) (*StepOutcome, error) {
		return nil, errors.New(sqlitewrite.ErrorName + ": text from a failed command")
	}
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	executor.SetForgeContext(&forgecontext.Context{})
	if err := executor.Execute(context.Background(), run, repo, workdir); err == nil {
		t.Fatal("expected code failure")
	}
	if sqlitewrite.RecordedContention(*run.Error) {
		t.Fatalf("code error adopted infrastructure identity: %s", *run.Error)
	}
	reported := step.reported()
	if len(reported) == 0 || reported[len(reported)-1] != "failure" {
		t.Fatalf("code failure was hidden: %v", reported)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	recovered.SetForgeContext(&forgecontext.Context{})
	before := len(step.reported())
	recovered.ReportRecoveredPublishedHeadStatus(context.Background(), stored, repo)
	if len(step.reported()) != before+1 || step.reported()[before] != "failure" {
		t.Fatalf("recovery hid a code failure: %v", step.reported())
	}
}
