package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// consumeApprovalResponse checks the dequeued response, not a fresh identity
// substituted for it. Publication and consumption share mu. An in-flight old
// response cannot clear the replacement gate, even after the queue was drained.
func (e *Executor) consumeApprovalResponse(response approvalResponse, sctx *StepContext) bool {
	e.mu.Lock()
	if response.action != types.ActionAbort && !response.gate.SameGate(e.gate) {
		current := e.gate
		e.mu.Unlock()
		logSupersededResponse(response, current, sctx)
		return false
	}
	e.clearApprovalGateLocked()
	e.mu.Unlock()
	return true
}

func logSupersededResponse(response approvalResponse, current types.GateIdentity, sctx *StepContext) {
	message := fmt.Sprintf("discarding superseded gate response: action=%s run=%s step=%s round=%s nonce=%s; current round=%s nonce=%s", response.action, response.gate.RunID, response.gate.Step, response.gate.RoundID, response.gate.Nonce, current.RoundID, current.Nonce)
	if sctx != nil && sctx.Log != nil {
		sctx.Log(message)
	} else {
		slog.Warn(message)
	}
}

// waitForApprovalDispatch applies the same consumption and admission checks to
// channel responses and saved responses. Replacement during slot admission is
// another stale response, not permission to consume or fail the replacement.
func (e *Executor) waitForApprovalDispatch(ctx context.Context, step Step, sctx *StepContext, findings string, immediate, pendingReconciliation bool, recorded *approvalResponse) (approvalResponse, bool, error) {
	for {
		var response approvalResponse
		var reconciled bool
		var err error
		if recorded != nil {
			response, recorded = *recorded, nil
			if e.afterApprovalDequeue != nil {
				e.afterApprovalDequeue()
			}
			if !e.consumeApprovalResponse(response, sctx) {
				continue
			}
		} else {
			response, reconciled, err = e.waitForApprovalOrReconcile(ctx, step, sctx, findings, immediate, pendingReconciliation)
		}
		if err != nil || response.action == types.ActionAbort {
			return response, reconciled, err
		}
		err = e.acquireAnsweredGateSlot(ctx, sctx.Run.ID, sctx.StepResultID, response.gate)
		var superseded *GateSupersededError
		if errors.As(err, &superseded) {
			e.mu.Lock()
			replaced := !response.gate.SameGate(e.gate)
			e.mu.Unlock()
			if replaced {
				logSupersededResponse(response, superseded.Current, sctx)
				e.yieldSlotForGateWait()
				continue
			}
		}
		return response, reconciled, err
	}
}

// GateSupersededError is shared with the durable admission boundary.
type GateSupersededError = types.GateSupersededError

// CurrentGate snapshots the gate under the same lock used for admission.
func (e *Executor) CurrentGate() types.GateIdentity {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentGateLocked()
}

func (e *Executor) currentGateLocked() types.GateIdentity {
	g := e.gate
	if !e.waiting && g.StepResultID != "" {
		if row, err := e.db.GetStepResult(g.StepResultID); err == nil && row != nil {
			g = row.GateIdentity()
		}
	}
	return g
}

func (e *Executor) validateGateLocked(expected *types.GateIdentity) error {
	current := e.currentGateLocked()
	if !e.waiting || expected == nil || !expected.SameGate(current) {
		return &GateSupersededError{Current: current}
	}
	return nil
}
