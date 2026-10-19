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
	testExecutorWriterLock(t, "UpdateRunPRState")
}

func TestExecutorDocumentLockIsInfrastructureNotCodeFailure(t *testing.T) {
	for _, operation := range []string{"BeginDocumentPass", "CompleteDocumentPass", "CarryValidationOverDocOnlyCommits"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			testExecutorWriterLock(t, operation)
		})
	}
}

func testExecutorWriterLock(t *testing.T, operation string) {
	t.Helper()
	for _, releaseEarly := range []bool{true, false} {
		name := "past_budget"
		if releaseEarly {
			name = "release_after_timeout"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workdir := completionFixture(t, database, run)
			stepName := types.StepCI
			if operation != "UpdateRunPRState" {
				stepName = types.StepDocument
			}
			from, to := strings.Repeat("a", 40), run.HeadSHA
			var passID string
			if operation == "CompleteDocumentPass" || operation == "CarryValidationOverDocOnlyCommits" {
				var err error
				passID, err = database.BeginDocumentPass(run.ID, from)
				if err != nil {
					t.Fatal(err)
				}
			}
			if operation == "CarryValidationOverDocOnlyCommits" {
				if err := database.CompleteDocumentPass(passID, to, 1, `{}`); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, from); err != nil {
					t.Fatal(err)
				}
				tested, err := database.InsertStepResult(run.ID, types.StepTest)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := types.MarshalFindingsJSON(types.Findings{Summary: "validated", TestedHeadSHA: from})
				if err != nil {
					t.Fatal(err)
				}
				if err := database.SetStepFindings(tested.ID, raw); err != nil {
					t.Fatal(err)
				}
				if err := database.CompleteStepWithStatus(tested.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
			}
			step := &contentionStep{shutdownBlockingStep: newShutdownBlockingStep(stepName)}
			step.execute = func(sctx *StepContext) (*StepOutcome, error) {
				holder, err := sql.Open("sqlite", p.DB())
				if err != nil {
					return nil, err
				}
				defer holder.Close()
				holder.SetMaxOpenConns(1)
				tx, err := holder.Begin()
				if err != nil {
					return nil, err
				}
				defer tx.Rollback()
				if _, err := tx.Exec(`UPDATE runs SET closing_issue_refs=? WHERE id=?`, "test/repo#92", run.ID); err != nil {
					return nil, err
				}
				released := make(chan error, 1)
				if releaseEarly {
					go func() {
						time.Sleep(sqlitewrite.BusyTimeout + time.Second)
						released <- tx.Commit()
					}()
				}
				start := time.Now()
				switch operation {
				case "UpdateRunPRState":
					err = sctx.DB.UpdateRunPRState(sctx.Run.ID, "open")
				case "BeginDocumentPass":
					_, err = sctx.DB.BeginDocumentPass(sctx.Run.ID, from)
				case "CompleteDocumentPass":
					err = sctx.DB.CompleteDocumentPass(passID, to, 1, `{}`)
				case "CarryValidationOverDocOnlyCommits":
					var carried bool
					carried, err = sctx.DB.CarryValidationOverDocOnlyCommits(sctx.Run.ID, from, to)
					if err == nil && !carried {
						t.Error("validation was not carried")
					}
				}
				if releaseEarly {
					if releaseErr := <-released; releaseErr != nil {
						t.Error(releaseErr)
					}
					if err != nil {
						t.Errorf("transient state write: %v", err)
					}
				} else {
					// Let terminal outcome persistence succeed so recovery can check it too.
					if releaseErr := tx.Commit(); releaseErr != nil {
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
				if stepErr != nil {
					t.Fatal(stepErr)
				}
				found := false
				for _, storedStep := range steps {
					if storedStep.StepName != stepName {
						continue
					}
					found = true
					if storedStep.Status != types.StepStatusFailed || storedStep.Error == nil || !sqlitewrite.RecordedContention(*storedStep.Error) {
						t.Fatalf("infrastructure failure stranded an active step: %+v", storedStep)
					}
				}
				if !found {
					t.Fatal("infrastructure failure lost its step")
				}
				// Both manager cleanup and a fresh recovery executor must preserve status.
				executor.ReportPublishedHeadFailure(context.Background(), run, repo)
				recovered := NewExecutor(database, p, nil, nil, []Step{step}, nil)
				recovered.ReportRecoveredPublishedHeadStatus(context.Background(), stored, repo)
			}
			for _, status := range step.reported() {
				if !releaseEarly || status == "failure" {
					t.Fatalf("storage outcome published a fabricated source verdict: %v", step.reported())
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
	executor.recordStorageFailure(run, &sqlitewrite.ContentionError{Operation: "previous execution", Attempts: 2, Cause: errors.New("locked")})
	if _, err := os.Stat(executor.storageFailurePath(run.ID)); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), run, repo, workdir); err == nil {
		t.Fatal("expected code failure")
	}
	if _, err := os.Stat(executor.storageFailurePath(run.ID)); !os.IsNotExist(err) {
		t.Fatalf("fresh execution retained old infrastructure outcome: %v", err)
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
