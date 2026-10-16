package daemon

import (
	"testing"

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
	stale.Nonce = "old-park"
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
