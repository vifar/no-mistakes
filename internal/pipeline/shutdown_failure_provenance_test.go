package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type shutdownFailureRaceStep struct {
	fail  func() error
	fatal bool
}

func (*shutdownFailureRaceStep) Name() types.StepName                         { return types.StepCI }
func (s *shutdownFailureRaceStep) Execute(*StepContext) (*StepOutcome, error) { return nil, s.fail() }
func (s *shutdownFailureRaceStep) ReconcileApprovalGate(*StepContext) (bool, error) {
	err := s.fail()
	if s.fatal {
		return false, errors.Join(ErrFatalGateReconciliation, err)
	}
	return false, err
}

func TestShutdownRecoveryRequiresCancellationOnly(t *testing.T) {
	for _, boundary := range []string{"execution", "terminal", "recovered gate"} {
		for _, shutdown := range []string{"raw", "planned"} {
			for _, provenance := range []string{"independent Git", "Git with context", "Git with drain and context", "cancellation only"} {
				t.Run(boundary+"/"+shutdown+"/"+provenance, func(t *testing.T) {
					database, p, run, repo := setupTest(t)
					dir := completionFixture(t, database, run)
					setRunPRURL(t, database, run)
					_, commandErr := git.Run(context.Background(), dir, "rev-parse", "--verify", "refs/heads/absent-shutdown-race")
					var exit *exec.ExitError
					if !errors.As(commandErr, &exit) || exit.ExitCode() != 128 {
						t.Fatalf("fixture did not produce independent Git exit 128: %v", commandErr)
					}
					executionErr := commandErr
					switch provenance {
					case "Git with context":
						executionErr = errors.Join(commandErr, context.Canceled)
					case "Git with drain and context":
						executionErr = errors.Join(commandErr, exec.ErrWaitDelay, context.Canceled)
					case "cancellation only":
						executionErr = context.Canceled
					}
					bounded, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					ctx, cancel := context.WithCancelCause(bounded)
					defer cancel(nil)
					var cause error = ErrDaemonShutdown
					if shutdown == "planned" {
						cause = &ControlledShutdownCause{Instance: "runtime", Lifecycle: "lifecycle"}
						if err := database.PrepareControlledShutdown([]string{run.ID}, "runtime", "lifecycle", cause.Error()); err != nil {
							t.Fatal(err)
						}
					}
					step := &shutdownFailureRaceStep{fatal: provenance != "cancellation only", fail: func() error { cancel(cause); return executionErr }}
					executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
					executor.SetRuntimeInstance("runtime")
					executor.SetGateReconcileTimings(time.Millisecond, time.Second)
					executor.workDir = dir
					if boundary != "execution" {
						if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
							t.Fatal(err)
						}
						sr, err := database.InsertStepResult(run.ID, types.StepCI)
						if err != nil {
							t.Fatal(err)
						}
						if err := database.StartStep(sr.ID); err != nil {
							t.Fatal(err)
						}
						if boundary == "recovered gate" {
							findings := `{"findings":[],"summary":"parked CI"}`
							if err := database.SetStepFindings(sr.ID, findings); err != nil {
								t.Fatal(err)
							}
							if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
								t.Fatal(err)
							}
							if err := parkGateFixture(database, sr.ID, types.StepStatusAwaitingApproval, 1); err != nil {
								t.Fatal(err)
							}
							if err := database.SetRunAwaitingAgent(run.ID); err != nil {
								t.Fatal(err)
							}
						}
						run, err = database.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						monitoring, err := database.IsActiveCIMonitorRun(run.ID)
						if err != nil || !monitoring {
							t.Fatalf("fixture must meet durable monitor predicate: %v, %v", monitoring, err)
						}
					}
					var result error
					switch boundary {
					case "execution":
						result = executor.Execute(ctx, run, repo, dir)
					case "recovered gate":
						result = executor.Resume(ctx, run, repo, dir)
					case "terminal":
						cancel(cause)
						result = executor.FailRun(run, repo, executionErr, ctx)
					}
					if result == nil {
						t.Fatal("shutdown race returned success")
					}
					stored, err := database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if provenance == "cancellation only" {
						if !LeftForRecoveryAtShutdown(result) || stored.Status != types.RunRunning {
							t.Fatalf("cancellation must remain recoverable: %v, %s", result, stored.Status)
						}
					} else {
						if LeftForRecoveryAtShutdown(result) || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, "git rev-parse") {
							t.Fatalf("independent Git failure lost to shutdown: %v, %+v", result, stored)
						}
						if !errors.As(result, &exit) || exit.ExitCode() != 128 {
							t.Fatalf("independent command evidence lost: %v", result)
						}
						if strings.Contains(provenance, "context") && !errors.Is(result, context.Canceled) {
							t.Fatalf("context metadata lost: %v", result)
						}
						if provenance == "Git with drain and context" && !errors.Is(result, exec.ErrWaitDelay) {
							t.Fatalf("drain evidence lost: %v", result)
						}
						if _, err := database.RecoverStaleRuns("crash recovery"); err != nil {
							t.Fatal(err)
						}
						recovered, err := database.GetRun(run.ID)
						if err != nil || recovered.Status != types.RunFailed || recovered.Error == nil || *recovered.Error != *stored.Error {
							t.Fatalf("startup replaced independent failure: %+v, %v", recovered, err)
						}
					}
				})
			}
		}
	}
}

func TestRecoverySentinelCannotHideIndependentFailure(t *testing.T) {
	for _, sentinel := range []error{ErrParkedAtShutdown, ErrCIMonitorInterruptedAtShutdown} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			if !LeftForRecoveryAtShutdown(fmt.Errorf("wrapped: %w", sentinel)) {
				t.Fatal("wrapped recovery verdict lost")
			}
			if LeftForRecoveryAtShutdown(errors.Join(sentinel, errors.New("independent failure"))) {
				t.Fatal("joined failure misclassified as recovery")
			}
		})
	}
}
