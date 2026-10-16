package daemon

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDaemon_RespondRefusesStaleGateIdentityThroughIPC(t *testing.T) {
	f := newSchedFixture(t, "")
	id, _ := f.push("park-bound-response")
	f.waitParked(id)
	info := f.info(id)
	if len(info.Steps) != 1 || info.Steps[0].Gate == nil {
		t.Fatalf("status did not expose gate identity: %+v", info.Steps)
	}
	current := *info.Steps[0].Gate
	if current.Nonce == "" || current.RoundID == "" || current.RunID != id {
		t.Fatalf("incomplete status identity: %+v", current)
	}
	stale := current
	var advanced ipc.RespondResult
	if err := f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: id, Step: types.StepReview, Action: types.ActionAnswer, ExpectedGate: &current}, &advanced); err != nil || !advanced.OK {
		t.Fatalf("gate A response: %+v, %v", advanced, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		info := f.info(id)
		if len(info.Steps) == 1 && info.Steps[0].Status == types.StepStatusAwaitingApproval && info.Steps[0].Gate != nil && info.Steps[0].Gate.Nonce != stale.Nonce {
			current = *info.Steps[0].Gate
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gate B did not park")
		}
		time.Sleep(time.Millisecond)
	}
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAnswer} {
		var refusal ipc.RespondResult
		if err := f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: id, Step: types.StepReview, Action: action}, &refusal); err != nil {
			t.Fatal(err)
		}
		if refusal.OK || refusal.Code != "gate_superseded" || refusal.CurrentGate == nil || *refusal.CurrentGate != current {
			t.Fatalf("unbound %s response: %+v", action, refusal)
		}
		info := f.info(id)
		if len(info.Steps) != 1 || info.Steps[0].Status != types.StepStatusAwaitingApproval || info.Steps[0].Gate == nil || *info.Steps[0].Gate != current {
			t.Fatalf("unbound response changed the gate: %+v", info.Steps)
		}
	}
	var result ipc.RespondResult
	err := f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: id, Step: types.StepReview, Action: types.ActionApprove, ExpectedGate: &stale}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Code != "gate_superseded" || result.CurrentGate == nil || *result.CurrentGate != current {
		t.Fatalf("stale response result = %+v", result)
	}
	if err := f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: id, Step: types.StepReview, Action: types.ActionApprove, ExpectedGate: &current}, &result); err != nil || !result.OK {
		t.Fatalf("current gate refused: %+v, %v", result, err)
	}
	f.waitStatus(id, types.RunCompleted)
}
