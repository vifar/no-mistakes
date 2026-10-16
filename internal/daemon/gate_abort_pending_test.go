package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestManager_AbortPendingResponseClearsRecoveryAndSlot(t *testing.T) {
	testAbortGate(t, false, true)
}

func TestManager_AbortRecoveringResponseBeforeDispatch(t *testing.T) {
	testAbortGate(t, true, true)
}

func TestManager_AbortParkedGateFailsRun(t *testing.T) {
	testAbortGate(t, false, false)
}

// Observe real slot calls rather than sleeping until admission probably ran.
type abortObservedSlot struct {
	*runSlot
	yieldedCh   chan struct{}
	acquireCh   chan struct{}
	yieldOnce   sync.Once
	acquireOnce sync.Once
}

func (s *abortObservedSlot) Yield() {
	s.runSlot.Yield()
	s.yieldOnce.Do(func() { close(s.yieldedCh) })
}

func (s *abortObservedSlot) Acquire(ctx context.Context) error {
	s.acquireOnce.Do(func() { close(s.acquireCh) })
	return s.runSlot.Acquire(ctx)
}

type abortGateStep struct {
	calls            atomic.Int32
	failureStatus    string
	failureRunStatus types.RunStatus
	failureError     string
}

func (s *abortGateStep) ReportPublishedHeadStatus(sctx *pipeline.StepContext, state, _ string) {
	s.failureStatus = state
	s.failureRunStatus = sctx.Run.Status
	if sctx.Run.Error != nil {
		s.failureError = *sctx.Run.Error
	}
}

func (*abortGateStep) Name() types.StepName { return types.StepReview }
func (s *abortGateStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls.Add(1)
	if err := bindMockRunComparison(sctx); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"operator decision"}`}, nil
}

func testAbortGate(t *testing.T, recovering, pending bool) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, head := setupTestGitRepo(t, p, database, "abort-pending")
	run, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	workDir := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), workDir, head); err != nil {
		t.Fatal(err)
	}
	testgit.BindRunComparison(t, database, run, workDir, "main", "", repo.UpstreamURL, run.Branch)
	m := NewRunManager(database, p, nil)
	m.sched = newSlotScheduler(func() (int, bool) { return 1, false }, &m.mu)
	step := &abortGateStep{}
	cfg := &config.Config{}
	var done <-chan struct{}
	var holder *runSlot
	var rowID string
	var events []ipc.Event
	if recovering {
		if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		row, err := database.InsertStepResult(run.ID, types.StepReview)
		if err != nil {
			t.Fatal(err)
		}
		rowID = row.ID
		if err := database.StartStep(row.ID); err != nil {
			t.Fatal(err)
		}
		findings := `{"findings":[],"summary":"operator decision"}`
		if _, err := database.InsertReviewStepRound(row.ID, 1, "initial", &findings, nil, head, 1); err != nil {
			t.Fatal(err)
		}
		if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
			t.Fatal(err)
		}
		run, err = database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		old := pipeline.NewExecutor(database, p, cfg, nil, []pipeline.Step{step}, nil)
		if err := old.RegisterRecoveredGate(run); err != nil {
			t.Fatal(err)
		}
		gate := old.CurrentGate()
		// Keep acceptance durably pending without reserving the recovery
		// manager's slot. This old scheduler represents the stopped process.
		oldSched := newSlotScheduler(func() (int, bool) { return 1, false }, &sync.Mutex{})
		old.SetRunSlot(&runSlot{s: oldSched, runID: run.ID})
		if err := old.Respond(types.StepReview, types.ActionAnswer, nil, &gate); err != nil {
			t.Fatal(err)
		}
		holder, _ = m.sched.tryAdmit("holder")
		m.resumeRecoveredRun(recoveredRunPlan{run: run, repo: repo, workDir: workDir, gateDir: p.RepoDir(repo.ID), cfg: cfg, agent: agent.NewNoop(), steps: []pipeline.Step{step}})
		m.mu.Lock()
		owned := m.runs[run.ID]
		done = owned.handle.done
		if owned.state != ownershipRecoveringGate {
			t.Errorf("recovery already dispatched: %v", owned.state)
		}
		m.mu.Unlock()
	} else {
		parked := make(chan struct{}, 1)
		executor := pipeline.NewExecutor(database, p, cfg, nil, []pipeline.Step{step}, func(event ipc.Event) {
			events = append(events, event)
			if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
				parked <- struct{}{}
			}
		})
		initialSlot, ok := m.sched.tryAdmit(run.ID)
		if !ok {
			t.Fatal("initial slot unavailable")
		}
		slot := &abortObservedSlot{runSlot: initialSlot, yieldedCh: make(chan struct{}), acquireCh: make(chan struct{})}
		executor.SetRunSlot(slot)
		ctx, cancel := context.WithCancelCause(context.Background())
		handle := &runHandle{runCtx: ctx, cancel: cancel, done: make(chan struct{})}
		m.runs[run.ID] = &managedRun{run: run, state: ownershipClaimed, handle: handle, executor: executor}
		done = handle.done
		go func() { defer close(handle.done); defer slot.Release(); _ = executor.Execute(ctx, run, repo, workDir) }()
		t.Cleanup(func() {
			cancel(context.Canceled)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("executor did not stop")
			}
		})
		select {
		case <-parked:
		case <-time.After(5 * time.Second):
			t.Fatal("run did not park")
		}
		select {
		case <-slot.yieldedCh:
		case <-time.After(5 * time.Second):
			t.Fatal("park did not yield")
		}
		holder, _ = m.sched.tryAdmit("holder")
		gate := executor.CurrentGate()
		rowID = gate.StepResultID
		if pending {
			if err := m.HandleRespond(run.ID, types.StepReview, types.ActionAnswer, nil, &gate); err != nil {
				t.Fatal(err)
			}
			select {
			case <-slot.acquireCh:
			case <-time.After(5 * time.Second):
				t.Fatal("response did not wait for capacity")
			}
		}
	}
	if holder == nil {
		t.Fatal("holder did not occupy the only slot")
	}
	t.Cleanup(holder.Release)
	t.Cleanup(func() {
		_ = m.HandleCancel(run.ID)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("cancelled run did not stop")
		}
	})
	row, err := database.GetStepResult(rowID)
	if err != nil || (row.PendingResponse != nil) != pending {
		t.Fatalf("pending response: %+v, %v; want pending=%v", row, err, pending)
	}
	if err := m.HandleRespond(run.ID, types.StepReview, types.ActionAbort, nil, nil); err != nil {
		t.Fatalf("identity-less abort refused: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not stop the run")
	}
	wantStatus := types.RunCancelled
	wantError := types.RunCancelReasonAbortedByUser
	if !pending {
		wantStatus = types.RunFailed
		wantError = "step review: aborted by user"
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != wantStatus || stored.Error == nil || *stored.Error != wantError || stored.AwaitingAgentSince != nil {
		t.Fatalf("abort outcome: %+v, %v", stored, err)
	}
	row, err = database.GetStepResult(rowID)
	if err != nil || row.PendingResponse != nil {
		t.Fatalf("abort left replayable response: %+v, %v", row, err)
	}
	if !pending {
		if row.Status != types.StepStatusFailed || row.Error == nil || *row.Error != "aborted by user" {
			t.Fatalf("parked abort step: %+v", row)
		}
		if step.failureStatus != "failure" || step.failureRunStatus != types.RunFailed || step.failureError != wantError {
			t.Fatalf("parked abort commit status: %+v", step)
		}
		if len(events) < 2 {
			t.Fatalf("missing terminal events: %+v", events)
		}
		stepEvent, runEvent := events[len(events)-2], events[len(events)-1]
		if stepEvent.Type != ipc.EventStepCompleted || stepEvent.Status == nil || *stepEvent.Status != "failed" || stepEvent.Error == nil || *stepEvent.Error != "aborted by user" {
			t.Fatalf("parked abort step event: %+v", stepEvent)
		}
		if runEvent.Type != ipc.EventRunCompleted || runEvent.Status == nil || *runEvent.Status != "failed" || runEvent.Error == nil || *runEvent.Error != wantError {
			t.Fatalf("parked abort run event: %+v", runEvent)
		}
	}
	m.mu.Lock()
	reserved := len(m.sched.prio)
	m.mu.Unlock()
	if stats := m.sched.stats(); stats.Active != 1 || reserved != 0 {
		t.Fatalf("abort retained a slot or reservation: stats=%+v reserved=%d", stats, reserved)
	}
	rounds, err := database.GetRoundsByStep(rowID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("accepted response dispatched: rounds=%d err=%v", len(rounds), err)
	}
	wantCalls := int32(1)
	if recovering {
		wantCalls = 0
	}
	if step.calls.Load() != wantCalls {
		t.Fatalf("dispatch count = %d, want %d", step.calls.Load(), wantCalls)
	}
	// Re-open the saved database as a new manager, then run its actual recovery
	// discovery. A cancelled response must never become a recovery candidate.
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewRunManager(reopened, p, nil)
	if plans := restarted.recoverableParkedRuns(context.Background()); len(plans) != 0 {
		t.Fatalf("restart would reapply aborted response: %+v", plans)
	}
}
