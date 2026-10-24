package pipeline

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func exhaustedStateWrite(operation string) error {
	return &sqlitewrite.ContentionError{Operation: operation, Attempts: 1, Cause: context.DeadlineExceeded}
}

func TestDocumentStateWriteContentionDoesNotReplayStep(t *testing.T) {
	t.Parallel()
	database, p, run, repo := setupTest(t)
	workdir := completionFixture(t, database, run)
	step := &contentionStep{shutdownBlockingStep: newShutdownBlockingStep(types.StepDocument)}
	executions := 0
	step.execute = func(*StepContext) (*StepOutcome, error) {
		executions++
		if executions == 1 {
			return nil, exhaustedStateWrite("CompleteDocumentPass")
		}
		return &StepOutcome{ExitCode: 0}, nil
	}
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	executor.SetForgeContext(&forgecontext.Context{})
	executor.contentionResumeCooldown = time.Millisecond
	err := executor.Execute(context.Background(), run, repo, workdir)
	if !sqlitewrite.IsContention(err) {
		t.Fatalf("lost typed infrastructure error: %T %v", err, err)
	}
	if executions != 1 {
		t.Fatalf("Document executed %d times, want exactly once", executions)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != types.RunFailed || stored.Error == nil || !sqlitewrite.RecordedContention(*stored.Error) {
		t.Fatalf("lost named infrastructure outcome: %+v", stored)
	}
	for _, state := range step.reported() {
		if state == "failure" {
			t.Fatalf("storage contention published a code failure verdict: %v", step.reported())
		}
	}
}

// A finished step whose bookkeeping write lost the connection race must not
// fail the run: the executor resumes the step and the run completes.
func TestStepStateWriteContentionResumesStepInsteadOfFailingRun(t *testing.T) {
	for _, operation := range []string{"UpdateRunPRState", "SetStepFindings"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			database, p, run, repo := setupTest(t)
			workdir := completionFixture(t, database, run)
			step := &contentionStep{shutdownBlockingStep: newShutdownBlockingStep(types.StepCI)}
			executions := 0
			step.execute = func(*StepContext) (*StepOutcome, error) {
				executions++
				if executions == 1 {
					return nil, exhaustedStateWrite(operation)
				}
				return &StepOutcome{ExitCode: 0}, nil
			}
			executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			executor.SetForgeContext(&forgecontext.Context{})
			executor.contentionResumeCooldown = 10 * time.Millisecond
			if err := executor.Execute(context.Background(), run, repo, workdir); err != nil {
				t.Fatalf("contended state write failed the run: %v", err)
			}
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != types.RunCompleted || executions != 2 {
				t.Fatalf("status=%s executions=%d error=%v", stored.Status, executions, stored.Error)
			}
			for _, state := range step.reported() {
				if state == "failure" {
					t.Fatalf("storage contention published a failure verdict: %v", step.reported())
				}
			}
		})
	}
}

// Resumes are bounded. Once exhausted, the run keeps the named
// infrastructure outcome (never a code verdict) for an operator rerun.
func TestStepStateWriteContentionAfterExhaustedResumesIsInfrastructure(t *testing.T) {
	t.Parallel()
	database, p, run, repo := setupTest(t)
	workdir := completionFixture(t, database, run)
	step := &contentionStep{shutdownBlockingStep: newShutdownBlockingStep(types.StepCI)}
	executions := 0
	step.execute = func(*StepContext) (*StepOutcome, error) {
		executions++
		return nil, exhaustedStateWrite("UpdateRunPRState")
	}
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	executor.SetForgeContext(&forgecontext.Context{})
	executor.contentionResumeCooldown = 10 * time.Millisecond
	err := executor.Execute(context.Background(), run, repo, workdir)
	if !sqlitewrite.IsContention(err) {
		t.Fatalf("error=%T %v", err, err)
	}
	if executions != ContentionResumes+1 {
		t.Fatalf("executions=%d, want %d", executions, ContentionResumes+1)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Error == nil || !sqlitewrite.RecordedContention(*stored.Error) {
		t.Fatalf("lost named infrastructure outcome: %+v", stored)
	}
}

// A cancelled run stops resuming at once so shutdown keeps its own outcome.
func TestStepContentionResumeStopsWhenRunContextEnds(t *testing.T) {
	executor := NewExecutor(nil, nil, nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writes := 0
	start := time.Now()
	err := executor.resumeContendedWrite(ctx, io.Discard, types.StepReview, func() error {
		writes++
		return exhaustedStateWrite("SetStepFindings")
	})
	if !sqlitewrite.IsContention(err) || writes != 1 || time.Since(start) > time.Second {
		t.Fatalf("writes=%d elapsed=%v err=%v", writes, time.Since(start), err)
	}
}

func TestResumeContendedWriteRepeatsIdempotentWrite(t *testing.T) {
	executor := NewExecutor(nil, nil, nil, nil, nil, nil)
	executor.contentionResumeCooldown = time.Millisecond
	writes := 0
	var log strings.Builder
	err := executor.resumeContendedWrite(context.Background(), &log, types.StepReview, func() error {
		writes++
		if writes < 3 {
			return exhaustedStateWrite("SetStepFindings")
		}
		return nil
	})
	if err != nil || writes != 3 || !strings.Contains(log.String(), sqlitewrite.ErrorName) {
		t.Fatalf("writes=%d err=%v log=%q", writes, err, log.String())
	}
	other := errors.New("constraint failed")
	writes = 0
	if err := executor.resumeContendedWrite(context.Background(), io.Discard, types.StepReview, func() error {
		writes++
		return other
	}); !errors.Is(err, other) || writes != 1 {
		t.Fatalf("non-contention error was resumed: writes=%d err=%v", writes, err)
	}
}
