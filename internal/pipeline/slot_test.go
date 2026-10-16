package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// recordingSlot logs the executor's slot traffic in order.
type recordingSlot struct {
	mu        sync.Mutex
	events    []string
	acquireOK error
}

type cappedInvocationAgent struct {
	entered chan struct{}
	release chan struct{}
}

func (a *cappedInvocationAgent) Name() string { return "capped" }
func (a *cappedInvocationAgent) Close() error { return nil }
func (a *cappedInvocationAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.entered <- struct{}{}
	select {
	case <-a.release:
		return &agent.Result{Text: `{"summary":"intent","agent_name":"capped","session_id":"s1"}`}, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func TestExecutorAgentCapIncludesBothIntentCallers(t *testing.T) {
	SetMaxConcurrentAgents(1)
	defer SetMaxConcurrentAgents(0)
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	a := &cappedInvocationAgent{entered: make(chan struct{}, 3), release: make(chan struct{})}
	defer close(a.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	step := &adaptiveCallStep{name: types.StepIntent, fn: func(sctx *StepContext) (*StepOutcome, error) {
		done := make(chan error, 3)
		go func() { _, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"}); done <- err }()
		select {
		case <-a.entered:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
		sess := &intent.Session{AgentName: "capped", SessionID: "s1", Messages: []intent.Message{{Role: intent.RoleUser, Text: "cap pipeline agents"}}}
		go func() { _, err := intent.NewAgentSummarizer(sctx.Agent, workDir).Summarize(ctx, sess); done <- err }()
		go func() {
			_, err := intent.NewAgentDisambiguator(sctx.Agent, workDir).Disambiguate(ctx, []string{"file.go"}, []*intent.Match{{Session: sess}})
			done <- err
		}()
		deadline := time.Now().Add(3 * time.Second)
		for {
			agentLimiter.mu.Lock()
			waiting := len(agentLimiter.waiters)
			agentLimiter.mu.Unlock()
			if waiting == 2 {
				break
			}
			select {
			case <-a.entered:
				return nil, errors.New("intent bypassed the agent cap")
			default:
			}
			if time.Now().After(deadline) {
				return nil, errors.New("intent callers never reached the cap")
			}
			time.Sleep(time.Millisecond)
		}
		for i := 0; i < 3; i++ {
			a.release <- struct{}{}
			if err := <-done; err != nil {
				return nil, err
			}
		}
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, nil, a, []Step{step}, nil)
	if err := exec.Execute(ctx, run, repo, workDir); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCapacityWaitDoesNotConsumeInvocationBudget(t *testing.T) {
	for _, mode := range []string{"direct", "step", "session"} {
		t.Run(mode, func(t *testing.T) {
			SetMaxConcurrentAgents(1)
			defer SetMaxConcurrentAgents(0)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a := &cappedInvocationAgent{entered: make(chan struct{}, 1), release: make(chan struct{})}
			held := make(chan error, 1)
			go func() { _, err := limitAgent(a, nil).Run(ctx, agent.RunOpts{}); held <- err }()
			select {
			case <-a.entered:
			case <-ctx.Done():
				t.Fatal("first agent never started")
			}
			budget := 20 * time.Millisecond
			inner := &timeoutAgent{inner: agent.NewNoop(), timeout: budget, stall: budget}
			sctx := &StepContext{Ctx: ctx, Agent: limitAgent(inner, nil), Config: &config.Config{AgentTimeout: budget, AgentStallTimeout: budget}, Sessions: NewRunSessions(nil, "budget", inner, false)}
			done := make(chan error, 1)
			go func() {
				var err error
				switch mode {
				case "direct":
					_, err = sctx.Agent.Run(ctx, agent.RunOpts{})
				case "step":
					_, err = sctx.RunAgent(agent.RunOpts{})
				case "session":
					_, err = sctx.RunAgentSession(SessionRoleFixer, agent.RunOpts{})
				}
				done <- err
			}()
			select {
			case err := <-done:
				close(a.release)
				<-held
				t.Fatalf("waiting agent finished before admission: %v", err)
			case <-time.After(5 * budget):
			}
			close(a.release)
			if err := <-held; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("fresh invocation budget: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("waiting agent did not run after admission")
			}
		})
	}
}

type occupiedRunSlot struct {
	occupied atomic.Bool
	acquires atomic.Int32
}

func (s *occupiedRunSlot) Yield() { s.occupied.Store(true) }
func (s *occupiedRunSlot) Acquire(ctx context.Context) error {
	s.acquires.Add(1)
	if s.occupied.Load() {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return nil
}

func TestExecutorAbortDoesNotWaitForOccupiedSlot(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "recovered"}[recovered], func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := completionFixture(t, database, run)
			step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"review"}`}, nil
			}}
			if recovered {
				if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				sr, err := database.InsertStepResult(run.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStep(sr.ID); err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[],"summary":"review"}`
				if err := database.SetStepFindings(sr.ID, findings); err != nil {
					t.Fatal(err)
				}
				if _, err := database.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, run.HeadSHA, 1); err != nil {
					t.Fatal(err)
				}
				if err := parkGateFixture(database, sr.ID, types.StepStatusAwaitingApproval, 1); err != nil {
					t.Fatal(err)
				}
				if err := database.SetRunAwaitingAgent(run.ID); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			slot := &occupiedRunSlot{}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			exec.SetRunSlot(slot)
			var guards atomic.Int32
			exec.SetPRContextGuard(func(*StepContext, types.StepName) (PRContextDecision, error) {
				guards.Add(1)
				return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if recovered {
					done <- exec.Resume(ctx, run, repo, workDir)
				} else {
					done <- exec.Execute(ctx, run, repo, workDir)
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for !slot.occupied.Load() {
				if time.Now().After(deadline) {
					t.Fatal("run never yielded its slot")
				}
				time.Sleep(time.Millisecond)
			}
			beforeAcquire, beforeGuard := slot.acquires.Load(), guards.Load()
			if err := exec.Respond(types.StepReview, types.ActionAbort, nil, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "aborted by user") {
					t.Fatalf("abort: %v", err)
				}
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("abort waited for another run's slot")
			}
			if slot.acquires.Load() != beforeAcquire || guards.Load() != beforeGuard {
				t.Fatal("abort acquired a slot or revalidated PR context")
			}
		})
	}
}

func (s *recordingSlot) record(e string) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *recordingSlot) Yield() { s.record("yield") }
func (s *recordingSlot) Acquire(context.Context) error {
	s.record("acquire")
	return s.acquireOK
}
func (s *recordingSlot) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestExecutor_YieldsSlotWhileParkedAndReacquiresOnResponse(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"needs review"}`}, nil
		},
	}
	slot := &recordingSlot{}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetRunSlot(slot)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	deadline := time.Now().Add(3 * time.Second)
	for len(slot.log()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Parked: the slot has been given back and not taken again yet.
	if got := slot.log(); len(got) != 2 || got[0] != "acquire" || got[1] != "yield" {
		t.Fatalf("slot events while parked = %v, want [acquire yield] (taken for the step, yielded at the gate)", got)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil, gateSnapshot(exec)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
	}
	got := slot.log()
	if len(got) != 3 || got[2] != "acquire" {
		t.Fatalf("slot events = %v, want a final acquire after the response", got)
	}
}

func TestExecutor_FailsRunWhenSlotWaitIsCancelled(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)

	var calls atomic.Int32
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls.Add(1)
			return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"needs review"}`}, nil
		},
	}
	cancelled := errors.New("aborted while waiting for a slot")
	slot := &recordingSlot{}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetRunSlot(slot)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	slot.mu.Lock()
	slot.acquireOK = cancelled
	slot.mu.Unlock()
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil, gateSnapshot(exec)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, cancelled) {
			t.Fatalf("Execute error = %v, want the slot wait's cause", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestAgentLimiterCapsConcurrentInvocations(t *testing.T) {
	sem := &agentSemaphore{}
	sem.setLimit(2)

	var active, peak atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := sem.acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			defer done()
			n := active.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			<-release
			active.Add(-1)
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for active.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := active.Load(); got != 2 {
		t.Fatalf("active invocations = %d, want exactly the cap of 2", got)
	}
	close(release)
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("peak concurrent invocations = %d, want at most 2", peak.Load())
	}
}

func TestAgentLimiterUnlimitedAndCancelledWaiter(t *testing.T) {
	sem := &agentSemaphore{}
	for i := 0; i < 20; i++ {
		if _, err := sem.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	sem = &agentSemaphore{}
	sem.setLimit(1)
	hold, err := sem.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := sem.acquire(ctx); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error = %v", err)
	}
	hold()
	next, err := sem.acquire(context.Background())
	if err != nil {
		t.Fatalf("a cancelled waiter must not leak the slot: %v", err)
	}
	next()
}

// An agent turn never runs unslotted: a run that yielded its slot for a
// long wait (the CI step's poll) takes one back before spawning the agent.
func TestRunAgentAcquiresRunSlotBeforeInvokingAgent(t *testing.T) {
	slot := &recordingSlot{}
	var eventsAtRun []string
	ag := &hangingAgent{
		name: "slot-agent",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			eventsAtRun = slot.log()
			return &agent.Result{Text: "ok"}, nil
		},
	}
	sctx := &StepContext{
		Ctx: context.Background(), Agent: ag, Slot: slot,
		Config: &config.Config{AgentTimeout: time.Minute, AgentStallTimeout: time.Minute},
	}
	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "fix"}); err != nil {
		t.Fatal(err)
	}
	if len(eventsAtRun) != 1 || eventsAtRun[0] != "acquire" {
		t.Fatalf("slot events when the agent ran = %v, want [acquire]", eventsAtRun)
	}

	// A refused slot means the agent is never started.
	slot.acquireOK = errors.New("cancelled")
	ag.calls = 0
	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "fix"}); err == nil || ag.calls != 0 {
		t.Fatalf("RunAgent with a refused slot = err %v, agent calls %d; want an error and no call", err, ag.calls)
	}
}

type gateCapacitySlot struct {
	capacity  *agentSemaphore
	release   func()
	yielded   chan struct{}
	acquiring chan struct{}
}

func (s *gateCapacitySlot) Yield() {
	s.release()
	s.release = nil
	close(s.yielded)
}

func (s *gateCapacitySlot) Acquire(ctx context.Context) error {
	if s.release != nil {
		return nil
	}
	s.acquiring <- struct{}{}
	var err error
	s.release, err = s.capacity.acquire(ctx)
	return err
}

func TestExecutorAnsweredGateWaitsAsRunning(t *testing.T) {
	for _, recoveredStatus := range []types.StepStatus{"", types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAnswer} {
			t.Run(string(recoveredStatus)+"/"+string(action), func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				workDir := completionFixture(t, database, run)
				findings := `{"findings":[],"summary":"review"}`
				step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
					return &StepOutcome{NeedsApproval: !sctx.Fixing && !sctx.FinalizingAnswers, Findings: findings}, nil
				}}
				if recoveredStatus != "" {
					if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					sr, err := database.InsertStepResult(run.ID, types.StepReview)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.StartStep(sr.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := database.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, run.HeadSHA, 1); err != nil {
						t.Fatal(err)
					}
					if err := database.ParkStepForApproval(run.ID, sr.ID, recoveredStatus, 0, 1, &findings); err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				capacity := &agentSemaphore{limit: 1}
				releaseA, err := capacity.acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				slot := &gateCapacitySlot{capacity: capacity, release: releaseA, yielded: make(chan struct{}), acquiring: make(chan struct{}, 2)}
				events := make(chan ipc.Event, 32)
				exec := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) { events <- event })
				exec.SetRunSlot(slot)
				done := make(chan error, 1)
				go func() {
					if recoveredStatus != "" {
						done <- exec.Resume(ctx, run, repo, workDir)
					} else {
						done <- exec.Execute(ctx, run, repo, workDir)
					}
				}()
				defer func() { cancel(); <-done }()
				select {
				case <-slot.yielded:
				case <-ctx.Done():
					t.Fatal("A never parked")
				}
				releaseB, err := capacity.acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { releaseB() }()
				for len(events) > 0 {
					<-events
				}
				if err := exec.Respond(types.StepReview, action, nil, gateSnapshot(exec)); err != nil {
					t.Fatal(err)
				}
				select {
				case <-slot.acquiring:
				case <-ctx.Done():
					t.Fatal("A never waited for capacity")
				}
				rows, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 {
					t.Fatalf("steps: %d", len(rows))
				}
				row := rows[0]
				// The gate stays durably parked, with the accepted response recorded
				// beside it, until dispatch begins: a crash now must recover it.
				if (row.Status != types.StepStatusAwaitingApproval && row.Status != types.StepStatusFixReview) || row.PendingResponse == nil || !row.AnsweredWaitingForSlot() || row.LastActivity == nil || *row.LastActivity != "already answered; waiting for a run slot" {
					t.Fatalf("answered gate: status=%s pending=%v activity=%v", row.Status, row.PendingResponse, row.LastActivity)
				}
				current, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.AwaitingAgentSince == nil {
					t.Fatal("answered gate lost its parked marker before dispatch; a restart could not recover it")
				}
				if err := exec.Respond(types.StepReview, action, nil, gateSnapshot(exec)); err == nil || !strings.Contains(err.Error(), "gate already resolved or superseded") {
					t.Fatalf("second response: %v", err)
				}
				select {
				case event := <-events:
					if event.Type != ipc.EventStepStarted || event.Status == nil || *event.Status != string(types.StepStatusRunning) {
						t.Fatalf("resume event: %+v", event)
					}
				default:
					t.Fatal("running event not published before capacity wait")
				}
				releaseB()
				releaseB = func() {}
				select {
				case err := <-done:
					done <- err
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("A did not finish after capacity became available")
				}
				if rows, err := database.GetStepsByRun(run.ID); err != nil || len(rows) != 1 || rows[0].PendingResponse != nil {
					t.Fatalf("dispatch left the pending response behind: %v, %v", rows, err)
				}
			})
		}
	}
}

type publicationRunSlot struct {
	blocked   atomic.Bool
	acquiring chan struct{}
}

func (*publicationRunSlot) Yield() {}
func (s *publicationRunSlot) Acquire(ctx context.Context) error {
	if !s.blocked.Load() {
		return nil
	}
	s.acquiring <- struct{}{}
	<-ctx.Done()
	return context.Cause(ctx)
}

func TestExecutorParkPublicationPrecedesAcceptedResponse(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, parkedStatus := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
			t.Run(map[bool]string{false: "live", true: "recovered"}[recovered]+"/"+string(parkedStatus), func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				workDir := completionFixture(t, database, run)
				findings := `{"findings":[],"summary":"review"}`
				step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
					return &StepOutcome{NeedsApproval: true, Findings: findings}, nil
				}}
				if recovered {
					if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					sr, err := database.InsertStepResult(run.ID, types.StepReview)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.StartStep(sr.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := database.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, run.HeadSHA, 1); err != nil {
						t.Fatal(err)
					}
					if err := database.ParkStepForApproval(run.ID, sr.ID, parkedStatus, 0, 1, &findings); err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				slot := &publicationRunSlot{acquiring: make(chan struct{}, 1)}
				publishing := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				var mu sync.Mutex
				var statuses []string
				exec := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
					if event.StepName == nil || *event.StepName != types.StepReview || event.Status == nil {
						return
					}
					if event.Type == ipc.EventStepCompleted && *event.Status == string(parkedStatus) {
						slot.blocked.Store(true)
						close(publishing)
						<-release
					}
					mu.Lock()
					statuses = append(statuses, *event.Status)
					mu.Unlock()
				})
				exec.SetRunSlot(slot)
				done := make(chan error, 1)
				go func() {
					if recovered {
						done <- exec.Resume(ctx, run, repo, workDir)
					} else {
						done <- exec.Execute(ctx, run, repo, workDir)
					}
				}()
				defer func() { unblock(); cancel(); <-done }()
				if !recovered && parkedStatus == types.StepStatusFixReview {
					waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
					if err := exec.Respond(types.StepReview, types.ActionFix, nil, gateSnapshot(exec)); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-publishing:
				case <-ctx.Done():
					t.Fatal("park event never reached the subscriber")
				}
				attempted := make(chan struct{})
				responseDone := make(chan error, 1)
				go func() {
					close(attempted)
					responseDone <- exec.Respond(types.StepReview, types.ActionApprove, nil, gateSnapshot(exec))
				}()
				<-attempted
				select {
				case err := <-responseDone:
					t.Fatalf("response overtook unfinished park publication: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				unblock()
				select {
				case err := <-responseDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("response remained blocked after park publication")
				}
				select {
				case <-slot.acquiring:
				case <-ctx.Done():
					t.Fatal("answered run never waited for capacity")
				}
				mu.Lock()
				got := append([]string(nil), statuses...)
				mu.Unlock()
				if len(got) < 2 || got[len(got)-2] != string(parkedStatus) || got[len(got)-1] != string(types.StepStatusRunning) {
					t.Fatalf("subscriber statuses = %v, want park followed by running", got)
				}
			})
		}
	}
}

func TestExecutorReconciliationWaitsDurablyForCapacity(t *testing.T) {
	for _, status := range []types.StepStatus{"", types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		t.Run(string(status), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := completionFixture(t, database, run)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			capacity := &agentSemaphore{limit: 1}
			release, err := capacity.acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			slot := &gateCapacitySlot{capacity: capacity, yielded: make(chan struct{}), acquiring: make(chan struct{}, 2)}
			step := &reconcilingApprovalStep{name: types.StepCI, release: make(chan struct{})}
			step.resolved.Store(true)
			if status != "" {
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
				if err := database.SetStepPendingResponse(row.ID, boundPendingFixture(t, database, row.ID, `{"kind":"reconcile"}`), answeredActivity, databaseGateSnapshot(t, database, row.ID)); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				close(step.release)
			} else {
				slot.release = release
			}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			exec.SetRunSlot(slot)
			done := make(chan error, 1)
			go func() {
				if status == "" {
					done <- exec.Execute(ctx, run, repo, workDir)
				} else {
					done <- exec.Resume(ctx, run, repo, workDir)
				}
			}()
			defer func() { cancel(); <-done }()
			if status == "" {
				select {
				case <-slot.yielded:
				case <-ctx.Done():
					t.Fatal("gate did not release capacity")
				}
				release, err = capacity.acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				close(step.release)
			}
			defer func() { release() }()
			select {
			case <-slot.acquiring:
			case <-ctx.Done():
				t.Fatal("accepted reconciliation did not wait for capacity")
			}
			rows, err := database.GetStepsByRun(run.ID)
			if err != nil || len(rows) != 1 || !rows[0].AnsweredWaitingForSlot() || rows[0].PendingResponse == nil {
				t.Fatalf("reconciliation must remain durably marked before dispatch: %+v, %v", rows, err)
			}
			var pending struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal([]byte(*rows[0].PendingResponse), &pending); err != nil || pending.Kind != "reconcile" {
				t.Fatalf("pending reconciliation = %+v, %v", pending, err)
			}
			if status != "" && step.calls.Load() != 0 {
				t.Fatal("recovered reconciliation ran before admission")
			}
			if err := exec.Respond(types.StepCI, types.ActionApprove, nil, gateSnapshot(exec)); err == nil || !strings.Contains(err.Error(), "gate already resolved or superseded") {
				t.Fatalf("repeat response = %v", err)
			}
			release()
			release = func() {}
			select {
			case err := <-done:
				done <- err
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("reconciled gate did not complete after admission")
			}
			current, err := database.GetRun(run.ID)
			if err != nil || current.Status != types.RunCompleted || current.AwaitingAgentSince != nil || step.calls.Load() != 1 {
				t.Fatalf("completion = %+v, %v; reconciliation calls = %d", current, err, step.calls.Load())
			}
			rows, err = database.GetStepsByRun(run.ID)
			if err != nil || rows[0].PendingResponse != nil || rows[0].Status != types.StepStatusCompleted {
				t.Fatalf("pending reconciliation survived dispatch: %+v, %v", rows, err)
			}
		})
	}
}

type acquisitionHookSlot struct {
	recordingSlot
	onAcquire func()
}

func (s *acquisitionHookSlot) Acquire(ctx context.Context) error {
	s.onAcquire()
	return s.recordingSlot.Acquire(ctx)
}

func TestRecoveredGateKeepsAcceptanceParkedDuration(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		for _, cancellation := range []string{"none", "before-resume", "on-acquire"} {
			t.Run(fmt.Sprintf("reconcile=%t/cancellation=%s", reconcile, cancellation), func(t *testing.T) {
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
				if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
					t.Fatal(err)
				}
				step := &reconcilingApprovalStep{name: types.StepCI}
				step.resolved.Store(true)
				original := NewExecutor(database, p, nil, nil, []Step{step}, nil)
				original.SetRunSlot(&recordingSlot{})
				original.waitingStep = types.StepCI
				original.waitingStepResultID = row.ID
				parked, _ := database.GetStepResult(row.ID)
				original.gate = parked.GateIdentity()
				original.waitingSince = time.Now().Add(-10 * time.Second)
				var response *approvalResponse
				if !reconcile {
					response = &approvalResponse{action: types.ActionApprove, gate: original.gate}
				}
				original.mu.Lock()
				err = original.acceptApprovalGate(response)
				original.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				persisted, err := database.GetStepResult(row.ID)
				if err != nil || persisted.PendingResponse == nil {
					t.Fatalf("pending acceptance: %+v, %v", persisted, err)
				}
				var pending struct {
					ParkedMS int64 `json:"parked_ms"`
				}
				if err := json.Unmarshal([]byte(*persisted.PendingResponse), &pending); err != nil || pending.ParkedMS < 10000 || pending.ParkedMS >= 11000 {
					t.Fatalf("ten-second acceptance duration = %+v, %v", pending, err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				parkedAt := time.Now().Add(-120 * time.Second).Unix()
				run.AwaitingAgentSince = &parkedAt
				recovered := NewExecutor(database, p, nil, nil, []Step{step}, nil)
				recovered.SetRunSlot(&recordingSlot{})
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				if cancellation == "before-resume" {
					cancel(errors.New(types.RunCancelReasonAbortedByUser))
				}
				if cancellation == "on-acquire" {
					recovered.SetRunSlot(&acquisitionHookSlot{onAcquire: func() {
						cancel(errors.New(types.RunCancelReasonAbortedByUser))
					}})
				}
				err = recovered.Resume(ctx, run, repo, workDir)
				cancelled := cancellation != "none"
				if (err != nil) != cancelled {
					t.Fatalf("Resume error = %v, cancelled = %t", err, cancelled)
				}
				current, err := database.GetRun(run.ID)
				if err != nil || current.ParkedMS != pending.ParkedMS || current.AwaitingAgentSince != nil {
					t.Fatalf("recovered accounting = %+v, %v; want frozen %dms", current, err, pending.ParkedMS)
				}
			})
		}
	}
}

// A gate answered before the run reaches its wait keeps the slot it holds for
// dispatch; an unanswered one gives it back.
func TestYieldSlotForGateWaitKeepsTheSlotOfAnAnsweredGate(t *testing.T) {
	slot := &recordingSlot{}
	exec := NewExecutor(nil, nil, nil, nil, nil, nil)
	exec.SetRunSlot(slot)
	exec.yieldSlotForGateWait()
	if got := slot.log(); len(got) != 1 || got[0] != "yield" {
		t.Fatalf("unanswered gate slot events = %v, want [yield]", got)
	}
	exec.mu.Lock()
	exec.slotWaitStep = types.StepReview // a response or reconciliation was accepted
	exec.mu.Unlock()
	exec.yieldSlotForGateWait()
	if got := slot.log(); len(got) != 1 {
		t.Fatalf("answered gate yielded its slot: %v", got)
	}
}

func (*occupiedRunSlot) Reserve()    {}
func (*recordingSlot) Reserve()      {}
func (*gateCapacitySlot) Reserve()   {}
func (*publicationRunSlot) Reserve() {}
