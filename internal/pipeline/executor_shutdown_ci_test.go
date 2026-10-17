package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// shutdownBlockingStep runs until its context ends, as a CI monitor polling a
// PR or an agent turn does, and records every published-head status the
// executor reports for the run.
type shutdownBlockingStep struct {
	name    types.StepName
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	states  []string
}

func (s *shutdownBlockingStep) Name() types.StepName { return s.name }

func (s *shutdownBlockingStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	s.once.Do(func() { close(s.started) })
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

func (s *shutdownBlockingStep) ReportPublishedHeadStatus(_ *StepContext, state, _ string) {
	s.mu.Lock()
	s.states = append(s.states, state)
	s.mu.Unlock()
}

func (s *shutdownBlockingStep) reported() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.states...)
}

func newShutdownBlockingStep(name types.StepName) *shutdownBlockingStep {
	return &shutdownBlockingStep{name: name, started: make(chan struct{})}
}

// executeUntilShutdown starts the run, waits for the blocking step, then
// cancels it with the daemon-shutdown cause RunManager.Shutdown uses.
func executeUntilShutdown(t *testing.T, database *db.DB, exec *Executor, run *db.Run, repo *db.Repo, blocking *shutdownBlockingStep) error {
	t.Helper()
	exec.SetForgeContext(&forgecontext.Context{})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("step %s did not start", blocking.name)
	}
	cancel(ErrDaemonShutdown)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not stop after shutdown")
	}
	return nil
}

func setRunPRURL(t *testing.T, database *db.DB, run *db.Run) {
	t.Helper()
	url := "https://github.com/test/repo/pull/7"
	if err := database.UpdateRunPRURL(run.ID, url); err != nil {
		t.Fatal(err)
	}
	run.PRURL = &url
}

func TestExecutor_ShutdownDuringCIMonitorLeavesRunForInterruptedRecovery(t *testing.T) {
	database, p, run, repo := setupTest(t)
	setRunPRURL(t, database, run)
	ci := newShutdownBlockingStep(types.StepCI)
	exec := NewExecutor(database, p, nil, nil, []Step{ci}, nil)

	err := executeUntilShutdown(t, database, exec, run, repo, ci)
	if !errors.Is(err, ErrCIMonitorInterruptedAtShutdown) {
		t.Errorf("Execute() error = %v, want ErrCIMonitorInterruptedAtShutdown", err)
	}
	for _, state := range ci.reported() {
		if state == "failure" {
			t.Errorf("published-head statuses = %v, a shutdown mid-monitor must not report failure", ci.reported())
			break
		}
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning {
		t.Fatalf("run after shutdown = %s (error %q), want running for startup recovery", got.Status, derefString(got.Error))
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusRunning {
		t.Fatalf("CI step after shutdown = %+v, want still running", steps)
	}

	// The next start records it exactly as it records a crash mid-monitor.
	if _, err := database.RecoverStaleRuns("daemon crashed during execution"); err != nil {
		t.Fatal(err)
	}
	got, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCIMonitorInterrupted || got.Error == nil || *got.Error != types.RunCIMonitorInterruptedReason {
		t.Fatalf("recovered run = %s (error %q), want %s", got.Status, derefString(got.Error), types.RunCIMonitorInterrupted)
	}
	steps, err = database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusSkipped || steps[0].Error == nil || *steps[0].Error != types.RunCIMonitorInterruptedReason {
		t.Fatalf("recovered CI step = %s (error %q), want skipped as interrupted", steps[0].Status, derefString(steps[0].Error))
	}
}

func TestExecutor_ShutdownMidStepStillFailsRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		step  types.StepName
		prURL bool
	}{
		// An agent turn in an earlier step is real work cut short.
		{name: "agent step with PR", step: types.StepReview, prURL: true},
		// CI without an already-created PR has nothing to babysit yet.
		{name: "CI without PR", step: types.StepCI, prURL: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if tc.prURL {
				setRunPRURL(t, database, run)
			}
			blocking := newShutdownBlockingStep(tc.step)
			steps := []Step{blocking}
			if tc.step != types.StepCI {
				steps = append(steps, newShutdownBlockingStep(types.StepCI))
			}
			exec := NewExecutor(database, p, nil, nil, steps, nil)

			err := executeUntilShutdown(t, database, exec, run, repo, blocking)
			if err == nil || errors.Is(err, ErrCIMonitorInterruptedAtShutdown) {
				t.Fatalf("Execute() error = %v, want an ordinary failure", err)
			}
			got, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != types.RunFailed || got.Error == nil || *got.Error != ErrDaemonShutdown.Error() {
				t.Fatalf("run after shutdown = %s (error %q), want failed with %q", got.Status, derefString(got.Error), ErrDaemonShutdown)
			}
			failed := false
			for _, state := range blocking.reported() {
				failed = failed || state == "failure"
			}
			if !failed {
				t.Fatalf("published-head statuses = %v, want failure reported", blocking.reported())
			}
		})
	}
}
