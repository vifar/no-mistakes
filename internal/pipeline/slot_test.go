package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// recordingSlot logs the executor's slot traffic in order.
type recordingSlot struct {
	mu        sync.Mutex
	events    []string
	acquireOK error
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
	// Parked: the slot has been given back and not taken again yet.
	if got := slot.log(); len(got) != 2 || got[0] != "acquire" || got[1] != "yield" {
		t.Fatalf("slot events while parked = %v, want [acquire yield] (taken for the step, yielded at the gate)", got)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
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
