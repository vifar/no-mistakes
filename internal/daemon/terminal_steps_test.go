package daemon

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			n, err := f.mgr.sweepTerminalSteps(at, 5*time.Minute)
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
