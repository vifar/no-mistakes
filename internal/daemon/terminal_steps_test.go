package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAbortTerminalRunCleansStaleStepAndRemainsIdempotent(t *testing.T) {
	f := newFaultFixture(t, "1")
	run, err := f.d.InsertRun("fault-repo", "feature", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.d.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.HandleCancel(run.ID); err != nil {
		t.Fatalf("abort did not repair terminal run: %v", err)
	}
	got, _ := f.d.GetStepResult(step.ID)
	if got.Status != types.StepStatusFailed || got.CompletedAt == nil {
		t.Fatalf("stale push not closed: %+v", got)
	}
	completed := *got.CompletedAt
	// No stale row remains: preserve the existing CLI's terminal no-op route.
	if err := f.mgr.HandleCancel(run.ID); err == nil || !strings.Contains(err.Error(), "no active run") {
		t.Fatalf("repeat abort = %v", err)
	}
	after, _ := f.d.GetStepResult(step.ID)
	if *after.CompletedAt != completed {
		t.Fatal("idempotent abort rewrote completion")
	}
	final, _ := f.d.GetRun(run.ID)
	if final.Status != types.RunCancelled {
		t.Fatalf("terminal status changed to %s", final.Status)
	}
}

// Inject the observation time: the five-minute boundary never sleeps.
func TestTerminalStepSweeperQuietBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		age                     time.Duration
		live, logGrowth, active bool
		want                    int
	}{
		{name: "young", age: 4 * time.Minute},
		{name: "boundary", age: 5 * time.Minute, want: 1},
		{name: "live process", age: 10 * time.Minute, live: true},
		{name: "log growth", age: 10 * time.Minute, logGrowth: true},
		{name: "active run without native pid", age: 10 * time.Minute, active: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFaultFixture(t, "1")
			run, _ := f.d.InsertRun("fault-repo", "feature", f.head, f.head)
			step, _ := f.d.InsertStepResult(run.ID, types.StepPush)
			if err := f.d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if tc.live {
				pid := os.Getpid() // A real live OS process, not an invented PID.
				if err := f.d.SetStepAgentActivity(step.ID, "agent working", &pid); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.active {
				if err := f.d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
					t.Fatal(err)
				}
			}
			row, _ := f.d.GetStepResult(step.ID)
			at := time.Unix(*row.LastActivityAt, 0).Add(tc.age)
			log := filepath.Join(f.p.RunLogDir(run.ID), "push.log")
			if err := os.MkdirAll(filepath.Dir(log), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(log, nil, 0600); err != nil {
				t.Fatal(err)
			}
			modified := time.Unix(*row.LastActivityAt, 0)
			if tc.logGrowth {
				modified = at.Add(-time.Minute)
				if err := os.WriteFile(log, []byte("still working"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(log, modified, modified); err != nil {
				t.Fatal(err)
			}
			n, err := f.mgr.sweepTerminalSteps(context.Background(), at, 5*time.Minute)
			if err != nil || n != tc.want {
				t.Fatalf("sweep = %d, %v; want %d", n, err, tc.want)
			}
			got, _ := f.d.GetStepResult(step.ID)
			if tc.want == 0 && (got.Status != types.StepStatusRunning || got.CompletedAt != nil) {
				t.Fatalf("working/recent step was swept: %+v", got)
			}
			if tc.want == 1 && (got.Status != types.StepStatusFailed || got.CompletedAt == nil) {
				t.Fatalf("stale row retained: %+v", got)
			}
		})
	}
}

func TestAbortTerminalStepRefusesLiveProcess(t *testing.T) {
	f := newFaultFixture(t, "1")
	run, _ := f.d.InsertRun("fault-repo", "feature", f.head, f.head)
	step, _ := f.d.InsertStepResult(run.ID, types.StepPush)
	if err := f.d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := f.d.SetStepAgentActivity(step.ID, "live agent", &pid); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	err := f.mgr.HandleCancel(run.ID)
	if err == nil || !strings.Contains(err.Error(), step.ID) || !strings.Contains(err.Error(), run.ID) {
		t.Fatalf("abort must identify live row: %v", err)
	}
	got, _ := f.d.GetStepResult(step.ID)
	if got.Status != types.StepStatusRunning || got.CompletedAt != nil {
		t.Fatalf("live process row closed: %+v", got)
	}
}

// An unrelated database read must never hold the lock needed to stop a live run.
func TestTerminalStepSweepDoesNotDelayActiveAbort(t *testing.T) {
	f := newFaultFixture(t, "1")
	id := f.push("running")
	f.waitStarted(id)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.mgr.beforeTerminalStepsRead = func(context.Context) { close(entered); <-release }
	swept := make(chan error, 1)
	go func() {
		_, err := f.mgr.sweepTerminalSteps(context.Background(), time.Now(), 5*time.Minute)
		swept <- err
	}()
	<-entered
	aborted := make(chan error, 1)
	go func() { aborted <- f.mgr.HandleCancel(id) }()
	delayed := false
	var abortErr error
	select {
	case abortErr = <-aborted:
	case <-time.After(time.Second):
		delayed = true
	}
	close(release)
	if delayed {
		abortErr = <-aborted
	}
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	if abortErr != nil {
		t.Fatal(abortErr)
	}
	if delayed {
		t.Fatal("a blocked stale-row read held the ownership lock and delayed active abort")
	}
}

func TestTerminalStepCleanupKeepsDocumentRetentionMeaning(t *testing.T) {
	for _, actualFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(actualFailure), func(t *testing.T) {
			f := newFaultFixture(t, "1")
			run, err := f.d.InsertRun("fault-repo", "feature", f.head, f.head)
			if err != nil {
				t.Fatal(err)
			}
			step, err := f.d.InsertStepResult(run.ID, types.StepDocument)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if actualFailure {
				if err := f.d.FailStep(step.ID, "cannot safely discard owner work", 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.d.RecordIneligibleTerminal(run.ID, "independent Resume failure", types.RunFailed, nil); err != nil {
				t.Fatal(err)
			}
			stored, err := f.d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			reason := worktreeRefusalCleanupReason(f.d, stored)
			if actualFailure && reason == "" {
				t.Fatal("genuine Document failure lost its retention guard")
			}
			if !actualFailure && reason != "" {
				t.Fatalf("interrupted row invented a Document refusal: %s", reason)
			}
		})
	}
}

func TestTerminalMaintenanceShutdownCancelsBlockedIO(t *testing.T) {
	for _, operation := range []string{"database", "probe"} {
		t.Run(operation, func(t *testing.T) {
			slowWatcher(t, 10*time.Millisecond)
			f := newFaultFixture(t, "1")
			run, err := f.d.InsertRun("fault-repo", "stale", f.head, f.head)
			if err != nil {
				t.Fatal(err)
			}
			step, err := f.d.InsertStepResult(run.ID, types.StepPush)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			pid := os.Getpid()
			if err := f.d.SetStepAgentActivity(step.ID, "live", &pid); err != nil {
				t.Fatal(err)
			}
			if err := f.d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			block := func(ctx context.Context) { close(entered); <-ctx.Done() }
			if operation == "database" {
				f.mgr.beforeTerminalStepsRead = block
			} else {
				f.mgr.terminalProcessRunning = func(ctx context.Context, _ int) (bool, error) {
					block(ctx)
					return false, nil
				}
			}
			f.mgr.StartLimitWatcher()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("maintenance did not enter blocked operation")
			}
			if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "max_concurrent_runs", "2"); err != nil {
				t.Fatal(err)
			}
			refreshDeadline := time.After(500 * time.Millisecond)
			for {
				f.mgr.limitsMu.Lock()
				refreshed := f.mgr.limitsCache.max == 2
				f.mgr.limitsMu.Unlock()
				if refreshed {
					break
				}
				select {
				case <-refreshDeadline:
					t.Fatal("blocked maintenance delayed automatic scheduling refresh")
				case <-time.After(time.Millisecond):
				}
			}
			done := make(chan struct{})
			go func() { f.mgr.Shutdown(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not cancel maintenance")
			}
			row, err := f.d.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != types.StepStatusRunning || row.AgentPID == nil {
				t.Fatalf("uncertain process was cleaned: %+v", row)
			}
		})
	}
}

func TestTerminalMaintenanceProbeDeadlineFailsClosed(t *testing.T) {
	f := newFaultFixture(t, "1")
	run, err := f.d.InsertRun("fault-repo", "stale", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.d.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := f.d.SetStepAgentActivity(step.ID, "live", &pid); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	f.mgr.terminalProcessRunning = func(ctx context.Context, _ int) (bool, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("probe has no deadline")
		}
		<-ctx.Done()
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	n, err := f.mgr.repairTerminalSteps(ctx, run.ID, time.Now(), 0)
	if n != 0 || err == nil {
		t.Fatalf("expired probe accepted: %d %v", n, err)
	}
	row, err := f.d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != types.StepStatusRunning {
		t.Fatalf("expired probe closed row: %+v", row)
	}
	_, err = processRunningContext(ctx, pid)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled real probe = %v", err)
	}
}

func TestTerminalMaintenanceScansNeverOverlap(t *testing.T) {
	f := newFaultFixture(t, "1")
	entered := make(chan struct{})
	f.mgr.beforeTerminalStepsRead = func(ctx context.Context) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > terminalStepScanBudget {
			t.Error("scan has no finite budget")
		}
		close(entered)
		<-ctx.Done()
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan error, 1)
	go func() { _, err := f.mgr.sweepTerminalSteps(firstCtx, time.Now(), terminalStepQuietAge); first <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first scan did not enter")
	}
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelSecond()
	n, err := f.mgr.repairTerminalSteps(secondCtx, "terminal", time.Now(), 0)
	if n != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent cleanup = %d %v", n, err)
	}
	cancelFirst()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first scan = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first scan did not cancel")
	}
}
