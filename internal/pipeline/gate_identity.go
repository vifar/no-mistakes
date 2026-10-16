package pipeline

import "github.com/kunchenguid/no-mistakes/internal/types"

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

func (e *Executor) validateGateLocked(expected types.GateIdentity) error {
	current := e.currentGateLocked()
	if !e.waiting || !expected.SameGate(current) {
		return &GateSupersededError{Current: current}
	}
	return nil
}
