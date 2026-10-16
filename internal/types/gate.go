package types

import "fmt"

// GateIdentity names one publication of a parked approval gate. Nonce changes
// even when the same round parks again; Status describes its current state.
type GateIdentity struct {
	RunID        string     `json:"run_id"`
	StepResultID string     `json:"step_result_id"`
	Step         StepName   `json:"step"`
	RoundID      string     `json:"round_id"`
	Nonce        string     `json:"nonce"`
	Status       StepStatus `json:"status"`
}

// SameGate compares authority, independently of the gate's current status.
func (g GateIdentity) SameGate(other GateIdentity) bool {
	return g.RunID != "" && g.StepResultID != "" && g.Step != "" && g.RoundID != "" && g.RunID == other.RunID && g.StepResultID == other.StepResultID && g.Step == other.Step && g.RoundID == other.RoundID && g.Nonce != "" && g.Nonce == other.Nonce
}

// GateSupersededError refuses a response to a resolved or replaced gate.
// Current describes the current gate, or the last gate with its current status.
type GateSupersededError struct{ Current GateIdentity }

func (e *GateSupersededError) Error() string {
	return fmt.Sprintf("gate already resolved or superseded; current run=%s step=%s round=%s nonce=%s status=%s", e.Current.RunID, e.Current.Step, e.Current.RoundID, e.Current.Nonce, e.Current.Status)
}
