package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
// Slot operations are idempotent: Yield on a slot that is not held and Acquire on
// one that is held do nothing. A nil RunSlot means no cap applies.
type RunSlot interface {
	// Reserve registers resume priority without waiting for capacity. Acceptance
	// calls it before acknowledging a gate response; Acquire consumes the claim.
	Reserve()
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

// yieldSlotForGateWait gives the slot back for a parked gate's wait - unless the
// gate was already answered. A response (or reconciliation claim) accepted before
// this point has recorded its pending response and is about to dispatch; the
// slot the scheduler granted for exactly that must not be handed to a queued
// new run first. The decision runs under the same lock as acceptance, so a
// response is either accepted before it (no yield) or after it (the run yields
// and re-acquires with priority, as for any answer that arrives during the wait).
func (e *Executor) yieldSlotForGateWait() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.slotWaitStep != "" {
		return
	}
	e.yieldSlot()
}

func (e *Executor) acquireSlot(ctx context.Context) error {
	if e.slot == nil {
		return nil
	}
	return e.slot.Acquire(ctx)
}

// pendingGateResponse is the durable form of an accepted gate response whose
// run is waiting for a run slot. It carries everything dispatch needs, so a
// daemon that stops during the wait can re-apply the response after a restart.
type pendingGateResponse struct {
	Gate             types.GateIdentity   `json:"gate"`
	ParkedMS         *int64               `json:"parked_ms,omitempty"`
	Kind             string               `json:"kind,omitempty"`
	Action           types.ApprovalAction `json:"action,omitempty"`
	FindingIDs       []string             `json:"finding_ids,omitempty"`
	IgnoreFindingIDs []string             `json:"ignore_finding_ids,omitempty"`
	Instructions     map[string]string    `json:"instructions,omitempty"`
	AddedFindings    []types.Finding      `json:"added_findings,omitempty"`
	ApprovalReason   string               `json:"approval_reason,omitempty"`
	AcceptedGaps     *db.AcceptedGaps     `json:"accepted_gaps,omitempty"`
}

func encodePendingResponse(r *approvalResponse, parkedMS int64, gate types.GateIdentity) (string, error) {
	p := pendingGateResponse{Gate: gate, Kind: "reconcile", ParkedMS: &parkedMS}
	if r != nil {
		p.Kind = ""
		p.Action = r.action
		p.FindingIDs = r.findingIDs
		p.IgnoreFindingIDs = r.ignoreFindingIDs
		p.Instructions = r.instructions
		p.AddedFindings = r.addedFindings
		p.ApprovalReason = r.approvalReason
		p.AcceptedGaps = r.acceptedGaps
	}
	raw, err := json.Marshal(p)
	return string(raw), err
}

func decodePendingResponse(raw string, current types.GateIdentity) (*approvalResponse, *int64, error) {
	var p pendingGateResponse
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, nil, fmt.Errorf("decode recorded gate response: %w", err)
	}
	if !p.Gate.SameGate(current) {
		return nil, nil, &GateSupersededError{Current: current}
	}
	if p.Kind == "reconcile" {
		return nil, p.ParkedMS, nil
	}
	switch p.Action {
	case types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAnswer:
	default:
		return nil, nil, fmt.Errorf("recorded gate response has unusable action %q", p.Action)
	}
	return &approvalResponse{
		gate:             p.Gate,
		action:           p.Action,
		findingIDs:       p.FindingIDs,
		ignoreFindingIDs: p.IgnoreFindingIDs,
		instructions:     p.Instructions,
		addedFindings:    p.AddedFindings,
		approvalReason:   p.ApprovalReason,
		acceptedGaps:     p.AcceptedGaps,
	}, p.ParkedMS, nil
}

// answeredActivity is what status surfaces show for a gate whose response is
// accepted but cannot dispatch until a run slot frees.
const answeredActivity = "already answered; waiting for a run slot"

// acceptApprovalGate consumes the parked gate for a response (resp) or for a
// reconciliation/resume claim (resp nil). The caller holds e.mu, the same lock
// held through park publication, so a stale park cannot overtake this event.
//
// With a run slot (every daemon run), acceptance does not move the step: until
// dispatch begins (see acquireAnsweredGateSlot, which first waits for capacity)
// the gate stays durably parked with the accepted response recorded beside it.
// A daemon that stops anywhere in that window - waiting for a slot, or between
// acceptance and the gate loop reading the response - recovers the gate and
// re-applies the response (see RegisterRecoveredGate), instead of finding a
// running step it must treat as stale. Without a slot there is
// nothing to wait for and the step moves at once.
// Resume priority is reserved before acceptance returns, so a queued new run
// cannot take the next free slot while this gate's loop catches up.
func (e *Executor) acceptApprovalGate(resp *approvalResponse) error {
	row, err := e.db.GetStepResult(e.waitingStepResultID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("cannot read the parked step")
	}
	current := row.GateIdentity()
	expected := e.currentGateLocked()
	if resp != nil {
		expected = resp.gate
	}
	if !expected.SameGate(current) || (row.Status != types.StepStatusAwaitingApproval && row.Status != types.StepStatusFixReview) || row.PendingResponse != nil {
		return &GateSupersededError{Current: current}
	}
	run, err := e.db.GetRun(row.RunID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("cannot read the parked run")
	}
	parkedMS := int64(0)
	if !e.waitingSince.IsZero() {
		parkedMS = time.Since(e.waitingSince).Milliseconds()
	}
	if e.slot == nil {
		// No daemon, no run slots: nothing can make dispatch wait, so the step
		// moves at once (embeddings and focused tests).
		if err := e.db.ResumeStepFromApproval(run.ID, row.ID, "gate resumed", parkedMS, current); err != nil {
			return fmt.Errorf("resume answered gate: %w", err)
		}
	} else {
		raw, err := encodePendingResponse(resp, parkedMS, current)
		if err != nil {
			return fmt.Errorf("record gate response: %w", err)
		}
		if err := e.db.SetStepPendingResponse(row.ID, raw, answeredActivity, current); err != nil {
			return err
		}
		e.slot.Reserve()
		e.slotWaitStep = e.waitingStep
		e.answeredParkedMS = parkedMS
	}
	e.emitStepEvent(ipc.EventStepStarted, run, &db.Repo{ID: run.RepoID}, e.waitingStep, string(types.StepStatusRunning))
	return nil
}

func (e *Executor) gateParkedMS(parkStart time.Time) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.slotWaitStep != "" {
		return e.answeredParkedMS
	}
	return time.Since(parkStart).Milliseconds()
}

// endGateWait closes the parked-time accounting after a gate wait returns. A
// response waiting for a slot keeps the gate parked, and its accounting, until
// dispatch begins (acquireAnsweredGateSlot).
func (e *Executor) endGateWait(runID string, parkStart time.Time, waitErr error) {
	parkedMS := e.gateParkedMS(parkStart)
	e.mu.Lock()
	waiting := e.slotWaitStep != ""
	if waitErr != nil {
		e.slotWaitStep = ""
	}
	e.mu.Unlock()
	if waitErr == nil && waiting {
		return
	}
	if dbErr := e.db.CompleteRunAwaitingAgent(runID, parkedMS); dbErr != nil {
		slog.Warn("failed to complete awaiting-agent state in db", "run", runID, "error", dbErr)
	}
}

// acquireAnsweredGateSlot waits for the run slot an accepted response needs,
// then begins dispatch: the step moves to running and the pending response and
// awaiting-agent marker are cleared together. Until then the gate stays
// recoverable.
func (e *Executor) acquireAnsweredGateSlot(ctx context.Context, runID, stepID string, expected types.GateIdentity) (err error) {
	e.mu.Lock()
	parkedMS := e.answeredParkedMS
	pending := e.slotWaitStep != ""
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		if expected.SameGate(e.gate) {
			if err != nil && !errors.Is(err, ErrParkedAtShutdown) && !errors.Is(err, ErrDaemonShutdown) {
				if dbErr := e.db.CompleteRunAwaitingAgent(runID, parkedMS); dbErr != nil {
					slog.Warn("failed to complete awaiting-agent state in db", "run", runID, "error", dbErr)
				}
			}
			e.slotWaitStep = ""
		}
		e.mu.Unlock()
	}()
	if err := e.acquireSlot(ctx); err != nil {
		// A daemon shutdown during the slot wait leaves the answered gate
		// parked with its recorded response, which recovery re-applies.
		if errors.Is(err, ErrDaemonShutdown) {
			return ErrParkedAtShutdown
		}
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Cancellation may race a capacity grant. Never dispatch an accepted
	// response after abort, even when Acquire returned a slot successfully.
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if !expected.SameGate(e.gate) {
		return &GateSupersededError{Current: e.gate}
	}
	row, err := e.db.GetStepResult(stepID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("cannot read answered gate")
	}
	if !expected.SameGate(row.GateIdentity()) || (pending && (row.PendingResponse == nil || (row.Status != types.StepStatusAwaitingApproval && row.Status != types.StepStatusFixReview))) {
		return &GateSupersededError{Current: row.GateIdentity()}
	}
	if row.PendingResponse != nil {
		if _, _, err := decodePendingResponse(*row.PendingResponse, row.GateIdentity()); err != nil {
			return err
		}
	}
	if e.slot == nil {
		return nil
	}
	if err := e.db.ResumeStepFromApproval(runID, stepID, "gate resumed", parkedMS, expected); err != nil {
		return fmt.Errorf("resume answered gate: %w", err)
	}
	return nil
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

// MaxConcurrentAgents reports the process-wide agent invocation cap (0 means
// unlimited).
func MaxConcurrentAgents() int {
	agentLimiter.mu.Lock()
	defer agentLimiter.mu.Unlock()
	return agentLimiter.limit
}
