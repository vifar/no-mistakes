package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

type endingTerminalRaceStep struct {
	kind    string
	release chan struct{}
}

func (*endingTerminalRaceStep) Name() types.StepName { return types.StepReview }
func (s *endingTerminalRaceStep) Execute(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	<-s.release
	switch s.kind {
	case "panic":
		panic("independent terminal panic")
	case "failure":
		return nil, errors.New("independent terminal failure")
	case "infrastructure":
		return nil, &sqlitewrite.ContentionError{Operation: "terminal fixture", Attempts: 2, Cause: errors.New("locked")}
	}
	if err := bindMockRunComparison(c); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}
func (s *endingTerminalRaceStep) ReconcileApprovalGate(c *pipeline.StepContext) (bool, error) {
	_, err := s.Execute(c)
	if err != nil {
		return false, errors.Join(pipeline.ErrFatalGateReconciliation, err)
	}
	return true, nil
}

func endingRaceWait(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("did not reach %s", label)
	}
}
func endingRaceRecovered(t *testing.T, f *faultFixture, step pipeline.Step) string {
	t.Helper()
	repo, err := f.d.GetRepo("fault-repo")
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.d.InsertRun(repo.ID, "main", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	workDir := f.p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), f.p.RepoDir(repo.ID), workDir, f.head); err != nil {
		t.Fatal(err)
	}
	if err := f.d.SetRunWorktreeDir(run.ID, workDir); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	sr, err := f.d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.StartStep(sr.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"parked review"}`
	if err := f.d.SetStepFindings(sr.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, f.head, 1); err != nil {
		t.Fatal(err)
	}
	if err := parkGateFixture(f.d, sr.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = f.d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	cfg.GateReconcileInterval = time.Millisecond
	cfg.GateReconcileTimeout = time.Minute
	f.mgr.resumeRecoveredRun(recoveredRunPlan{run: run, repo: repo, workDir: workDir, gateDir: f.p.RepoDir(repo.ID), cfg: cfg, agent: recoveredRunTestAgent{}, steps: []pipeline.Step{step}})
	return run.ID
}

func TestEndingCancelDuringTerminalWriteRetainsFinalAcceptedRequest(t *testing.T) {
	for _, execution := range []string{"new", "recovered", "setup"} {
		for _, kind := range []string{"failure", "panic", "infrastructure", "success"} {
			if execution == "setup" && kind != "failure" {
				continue
			}
			t.Run(execution+"/"+kind, func(t *testing.T) {
				logs := captureEndingLog(t)
				entered, release := make(chan struct{}), make(chan struct{})
				var enterOnce, releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				function := "ending_terminal_" + newShutdownToken()
				if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
					enterOnce.Do(func() { close(entered) })
					<-release
					return int64(1), nil
				}); err != nil {
					t.Fatal(err)
				}
				f := newFaultFixture(t, "1")
				control, err := sql.Open("sqlite", f.p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER block_ending_terminal BEFORE UPDATE OF status ON runs WHEN NEW.status IN ('failed','cancelled','completed') BEGIN SELECT %s(); END`, function)); err != nil {
					t.Fatal(err)
				}
				step := &endingTerminalRaceStep{kind: kind, release: make(chan struct{})}
				f.mgr.steps = func() []pipeline.Step { return []pipeline.Step{step} }
				var id string
				var workerDone <-chan struct{}
				afterSetup, resumeSetup := make(chan struct{}), make(chan struct{})
				var setupOnce sync.Once
				unblockSetup := func() { setupOnce.Do(func() { close(resumeSetup) }) }
				defer unblockSetup()
				if execution == "setup" {
					run, err := f.d.InsertRun("fault-repo", "setup", f.head, f.head)
					if err != nil {
						t.Fatal(err)
					}
					id = run.ID
					ctx, cancel := context.WithCancelCause(context.Background())
					defer cancel(nil)
					handle := &runHandle{runCtx: ctx, cancel: cancel, done: make(chan struct{})}
					f.mgr.runs[id] = &managedRun{run: run, state: ownershipClaimed, handle: handle}
					done := make(chan struct{})
					workerDone = done
					go func() {
						defer close(done)
						_ = f.mgr.failRunSetup(context.Background(), id, false, errors.New("independent terminal failure"))
						close(afterSetup)
						<-resumeSetup
						if f.mgr.terminalRunSettled(id) {
							f.mgr.mu.Lock()
							delete(f.mgr.runs, id)
							f.mgr.mu.Unlock()
							close(handle.done)
						}
					}()
				} else {
					if execution == "new" {
						id = f.push("terminal-race")
					} else {
						id = endingRaceRecovered(t, f, step)
					}
					done := make(chan struct{})
					workerDone = done
					go func() { f.mgr.wg.Wait(); close(done) }()
					close(step.release)
				}
				endingRaceWait(t, entered, "blocked terminal write")
				result := make(chan error, 1)
				go func() {
					_, err := f.mgr.handleCancelWithAttribution(id, &types.RunEndingEvent{Caller: "terminal-caller", Client: "axi", Reason: "explicit terminal stop"})
					result <- err
				}()
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Error("storage blocked immediate cancellation")
					unblock()
					if execution == "setup" {
						endingRaceWait(t, afterSetup, "setup terminal return")
					}
					select {
					case err := <-result:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("cancel did not return")
					}
				}
				if !strings.Contains(logs.snapshot(), `"caller":"terminal-caller"`) || !strings.Contains(logs.snapshot(), `"reason":"explicit terminal stop"`) {
					t.Errorf("accepted caller/reason absent from log: %s", logs.snapshot())
				}
				unblock()
				unblockSetup()
				endingRaceWait(t, workerDone, "custody release")
				stored, err := f.d.GetRun(id)
				if err != nil {
					t.Fatal(err)
				}
				want := types.RunFailed
				if kind == "success" {
					want = types.RunCompleted
				}
				if stored.Status != want {
					t.Fatalf("independent outcome changed: %+v", stored)
				}
				if kind == "failure" && (stored.Error == nil || !strings.Contains(*stored.Error, "independent terminal failure")) {
					t.Fatalf("failure replaced: %+v", stored)
				}
				if kind == "panic" && (stored.Error == nil || !strings.Contains(*stored.Error, "independent terminal panic")) {
					t.Fatalf("panic replaced: %+v", stored)
				}
				if kind == "infrastructure" && (stored.Error == nil || !strings.Contains(*stored.Error, sqlitewrite.ErrorName)) {
					t.Fatalf("infrastructure failure replaced: %+v", stored)
				}
				if stored.EndingEvent == nil || stored.EndingEvent.Caller != "terminal-caller" || stored.EndingEvent.Reason != "explicit terminal stop" || stored.EndingEvent.Client != "axi" {
					t.Fatalf("accepted request lost before custody release: %+v", stored.EndingEvent)
				}
			})
		}
	}
}

func TestEndingCancelAroundAcceptanceClosure(t *testing.T) {
	for _, window := range []string{"before", "after"} {
		t.Run(window, func(t *testing.T) {
			logs := captureEndingLog(t)
			f := newFaultFixture(t, "1")
			hook := &f.mgr.afterExecute
			if window == "after" {
				hook = &f.mgr.whileFinishing
			}
			id, resume := f.completeAndPause("acceptance-closure", hook)
			var once sync.Once
			unblock := func() { once.Do(resume) }
			defer unblock()
			if _, err := f.mgr.handleCancelWithAttribution(id, &types.RunEndingEvent{Caller: "boundary-caller", Reason: "boundary stop"}); err != nil {
				t.Fatal(err)
			}
			logged := strings.Contains(logs.snapshot(), `"caller":"boundary-caller"`)
			if logged != (window == "before") {
				t.Fatalf("acceptance changed: window=%s logged=%v", window, logged)
			}
			unblock()
			f.mgr.wg.Wait()
			stored, err := f.d.GetRun(id)
			if err != nil || stored.Status != types.RunCompleted {
				t.Fatalf("completion changed: %+v %v", stored, err)
			}
			if window == "after" {
				if stored.EndingEvent != nil {
					t.Fatalf("late cancellation created audit: %+v", stored.EndingEvent)
				}
			} else if stored.EndingEvent == nil || stored.EndingEvent.Caller != "boundary-caller" || stored.EndingEvent.Reason != "boundary stop" {
				t.Fatalf("last accepted request lost: %+v", stored.EndingEvent)
			}
		})
	}
}

func TestEndingFinalAuditRetryKeepsAcceptanceClosed(t *testing.T) {
	logs := captureEndingLog(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	function := "ending_final_audit_" + newShutdownToken()
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil, errors.New("transient final audit write failure")
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	f := newFaultFixture(t, "1")
	run, err := f.d.InsertRun("fault-repo", "final-audit", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RecordRunFailure(context.Background(), f.d, run.ID, "independent settled failure", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.mgr.runs[run.ID] = &managedRun{run: run, state: ownershipClaimed, handle: &runHandle{runCtx: ctx, cancel: cancel, done: make(chan struct{})}}
	control, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER retry_final_audit BEFORE UPDATE OF ending_event ON runs BEGIN SELECT %s(); END`, function)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.handleCancelWithAttribution(run.ID, &types.RunEndingEvent{Caller: "final-caller", Reason: "final accepted stop"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- f.mgr.terminalRunSettled(run.ID) }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("custody released without settling the final accepted event")
	case <-time.After(10 * time.Second):
		t.Fatal("final audit did not reach storage")
	}
	returned := make(chan error, 1)
	go func() {
		_, err := f.mgr.handleCancelWithAttribution(run.ID, &types.RunEndingEvent{Caller: "too-late", Reason: "late stop"})
		returned <- err
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		unblock()
		t.Fatal("final audit retry held the ownership lock")
	}
	if strings.Contains(logs.snapshot(), `"caller":"too-late"`) {
		t.Fatal("accepted a cancel after closing acceptance")
	}
	unblock()
	select {
	case settled := <-done:
		if !settled {
			t.Fatal("healthy retry did not settle")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("final audit retry never completed")
	}
	if calls.Load() < 2 {
		t.Fatal("transient audit failure was not retried")
	}
	stored, err := f.d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent settled failure" {
		t.Fatalf("terminal outcome changed: %+v %v", stored, err)
	}
	if stored.EndingEvent == nil || stored.EndingEvent.Caller != "final-caller" || stored.EndingEvent.Reason != "final accepted stop" {
		t.Fatalf("final event changed or lost: %+v", stored.EndingEvent)
	}
}
