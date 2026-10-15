package pipeline

import (
	"context"
	"fmt"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RunSlot is the daemon's run-concurrency slot as the pipeline sees it. A run
// holds one while it executes work; the executor hands it back across waits
// that do no work of their own (a gate parked on the operator, the CI step's
// PR monitoring poll) and takes one again before it resumes work.
//
// Both methods are idempotent: Yield on a slot that is not held and Acquire on
// one that is held do nothing. A nil RunSlot means no cap applies.
type RunSlot interface {
	// Yield releases the slot for the duration of a wait. An implementation
	// that is configured to hold slots through waits does nothing.
	Yield()
	// Acquire blocks until the run holds a slot again, ahead of any queued new
	// run, or returns the context's cause if ctx ends first.
	Acquire(ctx context.Context) error
}

// SetRunSlot installs the run's concurrency slot. Embeddings without a daemon
// leave it nil.
func (e *Executor) SetRunSlot(slot RunSlot) { e.slot = slot }

func (e *Executor) yieldSlot() {
	if e.slot != nil {
		e.slot.Yield()
	}
}

func (e *Executor) acquireSlot(ctx context.Context) error {
	if e.slot == nil {
		return nil
	}
	return e.slot.Acquire(ctx)
}

func (e *Executor) acquireAnsweredGateSlot(ctx context.Context, run *db.Run, repo *db.Repo, step types.StepName, stepID string) error {
	e.mu.Lock()
	if e.slot != nil {
		e.slotWaitStep = step
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.slotWaitStep = ""
		e.mu.Unlock()
	}()
	if err := e.db.UpdateStepStatus(stepID, types.StepStatusRunning); err != nil {
		return fmt.Errorf("resume answered gate: %w", err)
	}
	if e.slot != nil {
		if err := e.db.TouchStepActivity(stepID, "already answered; waiting for a run slot"); err != nil {
			return err
		}
	}
	e.emitStepEvent(ipc.EventStepStarted, run, repo, step, string(types.StepStatusRunning))
	if err := e.acquireSlot(ctx); err != nil {
		return err
	}
	return e.db.TouchStepActivity(stepID, "gate resumed")
}

// YieldSlot releases the run's concurrency slot while a step waits on
// something external (the CI step's PR poll). A step that yields must not do
// further work until it calls AcquireSlot; RunAgent acquires on its own, so an
// agent turn can never run unslotted.
func (sctx *StepContext) YieldSlot() {
	if sctx != nil && sctx.Slot != nil {
		sctx.Slot.Yield()
	}
}

// AcquireSlot takes the run's concurrency slot again after YieldSlot.
func (sctx *StepContext) AcquireSlot(ctx context.Context) error {
	if sctx == nil || sctx.Slot == nil {
		return nil
	}
	return sctx.Slot.Acquire(ctx)
}

// agentLimiter caps agent subprocess invocations across every run in the
// process (daemon.max_concurrent_agents). The zero limit means unlimited.
var agentLimiter = &agentSemaphore{}

// SetMaxConcurrentAgents sets the process-wide agent invocation cap. Zero
// removes it. Invocations already running are never interrupted by a change.
func SetMaxConcurrentAgents(limit int) {
	if limit < 0 {
		limit = 0
	}
	agentLimiter.setLimit(limit)
}

type agentSemaphore struct {
	mu      sync.Mutex
	limit   int
	active  int
	waiters []chan struct{}
}

func (s *agentSemaphore) setLimit(limit int) {
	s.mu.Lock()
	s.limit = limit
	s.wakeLocked()
	s.mu.Unlock()
}

// wakeLocked admits waiters while capacity remains, oldest first.
func (s *agentSemaphore) wakeLocked() {
	for len(s.waiters) > 0 && (s.limit == 0 || s.active < s.limit) {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.active++
		close(w)
	}
}

// acquire blocks until an invocation may start and returns the release func.
func (s *agentSemaphore) acquire(ctx context.Context) (func(), error) {
	s.mu.Lock()
	if len(s.waiters) == 0 && (s.limit == 0 || s.active < s.limit) {
		s.active++
		s.mu.Unlock()
		return s.release, nil
	}
	w := make(chan struct{})
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()
	select {
	case <-w:
		return s.release, nil
	case <-ctx.Done():
		s.mu.Lock()
		for i, queued := range s.waiters {
			if queued == w {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				s.mu.Unlock()
				return nil, context.Cause(ctx)
			}
		}
		s.mu.Unlock()
		// Admitted concurrently with cancellation: give the slot back.
		s.release()
		return nil, context.Cause(ctx)
	}
}

func (s *agentSemaphore) release() {
	s.mu.Lock()
	s.active--
	s.wakeLocked()
	s.mu.Unlock()
}

type limitedAgent struct {
	agent.Agent
	slot RunSlot
}

func limitAgent(a agent.Agent, slot RunSlot) agent.Agent {
	if a == nil {
		return nil
	}
	if _, ok := a.(*limitedAgent); ok {
		return a
	}
	return &limitedAgent{Agent: a, slot: slot}
}

func (a *limitedAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	inner, release, err := admitAgent(ctx, a, a.slot)
	if err != nil {
		return nil, err
	}
	defer release()
	return inner.Run(ctx, opts)
}

func (sctx *StepContext) AcquireAgent(ctx context.Context) (agent.Agent, func(), error) {
	if sctx == nil {
		return nil, func() {}, nil
	}
	return admitAgent(ctx, sctx.Agent, sctx.Slot)
}

func admitAgent(ctx context.Context, ag agent.Agent, slot RunSlot) (agent.Agent, func(), error) {
	if ag == nil {
		return nil, func() {}, nil
	}
	a := limitAgent(ag, slot).(*limitedAgent)
	if a.slot != nil {
		if err := a.slot.Acquire(ctx); err != nil {
			return nil, nil, err
		}
	}
	release, err := agentLimiter.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	return a.Agent, release, nil
}

func (a *limitedAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.Agent)
}

func (a *limitedAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.Agent, provider)
}

func (a *limitedAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.Agent)
}

func (a *limitedAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.Agent)
}
