package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if _, err := database.InsertStepRound(row.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepPendingResponse(row.ID, boundPendingFixture(t, database, row.ID, `{"action":"approve"}`), answeredActivity, databaseGateSnapshot(t, database, row.ID)); err != nil {
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
	saved, _ := database.GetStepResult(row.ID)
	exec.gate = saved.GateIdentity()
	if err := exec.acquireAnsweredGateSlot(context.Background(), run.ID, row.ID, exec.gate); err == nil {
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

func TestRecoveredGateFailuresClearPendingState(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		for _, marker := range []string{"", `{"action":"approve","parked_ms":10000}`, `{"kind":"reconcile","parked_ms":10000}`} {
			for _, failure := range []string{"cancelled", "log-directory", "log-configuration", "comparison", "dispatch", "retry-cancel", "fatal-reconcile", "shutdown-admission", "shutdown-comparison", "shutdown-retry", "shutdown-dispatched"} {
				if marker == "" && !strings.HasPrefix(failure, "shutdown-") {
					continue
				}
				if (failure == "retry-cancel" || failure == "fatal-reconcile" || failure == "shutdown-retry") && !strings.Contains(marker, `"kind"`) {
					continue
				}
				t.Run(string(status)+"/"+marker+"/"+failure, func(t *testing.T) {
					marker := marker
					preserveShutdown := strings.HasPrefix(failure, "shutdown-") && failure != "shutdown-dispatched"
					database, p, run, repo := setupTest(t)
					workDir := completionFixture(t, database, run)
					if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					row, err := database.InsertStepResult(run.ID, types.StepCI)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.StartStep(row.ID); err != nil {
						t.Fatal(err)
					}
					findings := `{"findings":[],"summary":"closed PR"}`
					if _, err := database.InsertStepRound(row.ID, 1, "initial", &findings, nil, 1); err != nil {
						t.Fatal(err)
					}
					if err := database.ParkStepForApproval(run.ID, row.ID, status, 0, 1, &findings); err != nil {
						t.Fatal(err)
					}
					if marker != "" {
						marker = boundPendingFixture(t, database, row.ID, marker)
						if err := database.SetStepPendingResponse(row.ID, marker, answeredActivity, databaseGateSnapshot(t, database, row.ID)); err != nil {
							t.Fatal(err)
						}
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					step := &reconcilingApprovalStep{name: types.StepCI}
					step.resolved.Store(true)
					slot := &recordingSlot{}
					exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
					exec.SetRunSlot(slot)
					exec.SetGateReconcileTimings(time.Hour, time.Second)
					ctx, cancel := context.WithCancelCause(context.Background())
					defer cancel(context.Canceled)
					switch failure {
					case "shutdown-admission":
						cancel(ErrDaemonShutdown)
					case "shutdown-dispatched":
						calls := 0
						exec.SetPRContextGuard(func(*StepContext, types.StepName) (PRContextDecision, error) {
							calls++
							if calls == 1 {
								return PRContextDecision{}, nil
							}
							cancel(ErrDaemonShutdown)
							return PRContextDecision{}, context.Canceled
						})
					case "shutdown-comparison":
						exec.SetPRContextGuard(func(*StepContext, types.StepName) (PRContextDecision, error) {
							cancel(ErrDaemonShutdown)
							return PRContextDecision{}, context.Canceled
						})
					case "cancelled":
						cancel(errors.New(types.RunCancelReasonAbortedByUser))
					case "log-directory":
						if err := os.MkdirAll(filepath.Dir(p.RunLogDir(run.ID)), 0755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(p.RunLogDir(run.ID), nil, 0600); err != nil {
							t.Fatal(err)
						}
					case "log-configuration":
						if err := os.MkdirAll(filepath.Join(p.RunLogDir(run.ID), commandConfigurationFile), 0755); err != nil {
							t.Fatal(err)
						}
					case "comparison":
						exec.SetPRContextGuard(func(*StepContext, types.StepName) (PRContextDecision, error) {
							return PRContextDecision{}, errors.New("comparison read failed")
						})
					case "dispatch":
						control, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
						if err != nil {
							t.Fatal(err)
						}
						defer control.Close()
						if _, err := control.Exec(`CREATE TRIGGER fail_recovered_dispatch BEFORE UPDATE OF status ON step_results WHEN NEW.status = 'running' BEGIN SELECT RAISE(FAIL, 'dispatch write failed'); END`); err != nil {
							t.Fatal(err)
						}
					case "retry-cancel", "shutdown-retry":
						transient := error(errors.New("temporary GetPRState failure"))
						step.err.Store(&transient)
					case "fatal-reconcile":
						fatal := error(ErrFatalGateReconciliation)
						step.err.Store(&fatal)
					}
					done := make(chan error, 1)
					go func() { done <- exec.Resume(ctx, run, repo, workDir) }()
					if failure == "retry-cancel" || failure == "shutdown-retry" {
						deadline := time.After(3 * time.Second)
						for len(slot.log()) == 0 {
							select {
							case <-deadline:
								t.Fatal("reconciliation never yielded for retry")
							case <-time.After(time.Millisecond):
							}
						}
						if failure == "shutdown-retry" {
							cancel(ErrDaemonShutdown)
						} else {
							cancel(errors.New(types.RunCancelReasonAbortedByUser))
						}
					}
					select {
					case err := <-done:
						if err == nil {
							t.Fatal("recovery succeeded despite injected failure")
						}
						if preserveShutdown && !errors.Is(err, ErrParkedAtShutdown) {
							t.Fatalf("shutdown recovery error = %v, want ErrParkedAtShutdown", err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("recovery did not stop")
					}
					persisted, err := database.GetStepResult(row.ID)
					if err != nil {
						t.Fatal(err)
					}
					if preserveShutdown {
						if persisted.Status != status || (marker == "" && persisted.PendingResponse != nil) || (marker != "" && (persisted.PendingResponse == nil || *persisted.PendingResponse != marker)) {
							t.Fatalf("shutdown changed the parked gate: %+v", persisted)
						}
						current, err := database.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						if current.Status != types.RunRunning || current.AwaitingAgentSince == nil || *current.AwaitingAgentSince != *run.AwaitingAgentSince || current.ParkedMS != run.ParkedMS {
							t.Fatalf("shutdown changed the parked run: %+v", current)
						}
						restarted := NewExecutor(database, p, nil, nil, []Step{step}, nil)
						if err := restarted.RegisterRecoveredGate(current); err != nil {
							t.Fatalf("shutdown lost recoverability: %v", err)
						}
						return
					}
					if persisted.Status != types.StepStatusFailed || persisted.PendingResponse != nil || persisted.AnsweredWaitingForSlot() {
						t.Fatalf("stopped recovery left an active gate: %+v", persisted)
					}
					current, err := database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					wantStatus := types.RunFailed
					if failure == "cancelled" || failure == "retry-cancel" {
						wantStatus = types.RunCancelled
					}
					if current.Status != wantStatus || current.AwaitingAgentSince != nil || (marker != "" && current.ParkedMS != 10000) {
						t.Fatalf("stopped recovery = %+v; want %s, no park, and 10000ms parked", current, wantStatus)
					}
				})
			}
		}
	}
}
