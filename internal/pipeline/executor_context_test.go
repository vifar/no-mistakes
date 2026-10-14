package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Completion includes the executor's bounded five-second terminal Git check
// and process cleanup. The test's wait must outlast that production budget.
const cancellationTestWait = 15 * time.Second

func TestExecutor_ContextCancellation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)

	// Step that blocks until context is cancelled
	started := make(chan struct{})
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			close(started)
			<-sctx.Ctx.Done()
			return nil, sctx.Ctx.Err()
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	select {
	case <-started:
	case <-time.After(cancellationTestWait):
		t.Fatal("step did not start")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
	case <-time.After(cancellationTestWait):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunFailed {
		t.Errorf("expected run status %q, got %q", types.RunFailed, updated.Status)
	}
}

func TestExecutor_ContextCancelCause(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)

	// Two steps: first passes, second blocks until context is cancelled.
	// The handshake proves cancellation reaches the active second step.
	started := make(chan struct{})
	step1 := newPassStep(types.StepReview)
	step2 := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			close(started)
			<-sctx.Ctx.Done()
			return nil, context.Cause(sctx.Ctx)
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)

	cancelReason := fmt.Errorf("cancelled: superseded by new push")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	select {
	case <-started:
	case <-time.After(cancellationTestWait):
		t.Fatal("second step did not start")
	}
	cancel(cancelReason)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
		if !strings.Contains(err.Error(), "superseded by new push") {
			t.Errorf("expected error to contain 'superseded by new push', got %q", err)
		}
	case <-time.After(cancellationTestWait):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCancelled {
		t.Errorf("expected run status %q, got %q", types.RunCancelled, updated.Status)
	}
	if updated.Error == nil || !strings.Contains(*updated.Error, "superseded by new push") {
		var got string
		if updated.Error != nil {
			got = *updated.Error
		}
		t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
	}
}

func TestExecutor_ContextCancelCauseBetweenSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)

	// First step passes and signals, cancel fires before second step starts.
	started := make(chan struct{})
	step1 := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			close(started)
			// Return success only after cancellation, so it is detected between steps.
			<-sctx.Ctx.Done()
			return &StepOutcome{ExitCode: 0}, nil
		},
	}
	step2 := newPassStep(types.StepTest)

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)

	cancelReason := fmt.Errorf("cancelled: superseded by new push")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	select {
	case <-started:
	case <-time.After(cancellationTestWait):
		t.Fatal("first step did not start")
	}
	cancel(cancelReason)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
		if !strings.Contains(err.Error(), "superseded by new push") {
			t.Errorf("expected error to contain 'superseded by new push', got %q", err)
		}
	case <-time.After(cancellationTestWait):
		t.Fatal("executor timed out")
	}

	if step2.callCount() != 0 {
		t.Fatal("second step ran after cancellation between steps")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCancelled {
		t.Errorf("expected run status %q, got %q", types.RunCancelled, updated.Status)
	}
	if updated.Error == nil || !strings.Contains(*updated.Error, "superseded by new push") {
		var got string
		if updated.Error != nil {
			got = *updated.Error
		}
		t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
	}
}
