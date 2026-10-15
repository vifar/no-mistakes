package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Race real IPC responses against the database park becoming visible. Lowering
// the cap underneath the admitted run leaves it waiting after the response,
// so a stale final park event cannot be hidden by a later completion event.
func TestQueueParkResponseEventOrderThroughIPC(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 2\n")
	holder, _ := f.push("holder")
	f.waitStarted(holder)
	for i := 0; i < 8; i++ {
		f.setCap(2)
		f.reloadSlots()
		id, _ := f.push(fmt.Sprintf("park-race-%d", i))
		events, cancel, err := ipc.Subscribe(f.p.Socket(), &ipc.SubscribeParams{RunID: id})
		if err != nil {
			t.Fatal(err)
		}
		f.setCap(1)
		f.reloadSlots()
		deadline := time.Now().Add(30 * time.Second)
		for {
			run, err := f.d.GetRun(id)
			if err != nil {
				t.Fatal(err)
			}
			if run.AwaitingAgentSince != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("run never parked")
			}
			time.Sleep(time.Millisecond)
		}
		f.respondApprove(id)
		var statuses []string
		observedRunning := false
		settle := time.NewTimer(5 * time.Second)
	collect:
		for {
			select {
			case event, ok := <-events:
				if !ok {
					t.Fatal("subscriber closed before the waiting run finished")
				}
				if event.StepName == nil || *event.StepName != types.StepReview || event.Status == nil {
					continue
				}
				statuses = append(statuses, *event.Status)
				if event.Type == ipc.EventStepStarted && *event.Status == string(types.StepStatusRunning) {
					// The initial execution also starts as running; only the
					// accepted-response event follows the park or snapshot.
					info := f.info(id)
					if info.WaitingForSlot && info.AwaitingAgentSince == nil {
						observedRunning = true
						settle.Reset(100 * time.Millisecond)
					}
				}
			case <-settle.C:
				break collect
			}
		}
		settle.Stop()
		cancel()
		if !observedRunning || len(statuses) == 0 || statuses[len(statuses)-1] != string(types.StepStatusRunning) {
			t.Fatalf("subscriber ended on a stale park: %v", statuses)
		}
		info := f.info(id)
		if info.AwaitingAgentSince != nil || len(info.Steps) != 1 || info.Steps[0].Status != types.StepStatusRunning {
			t.Fatalf("answered snapshot is still parked: %+v", info)
		}
		err = f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: id, Step: types.StepReview, Action: types.ActionApprove}, nil)
		if err == nil || !strings.Contains(err.Error(), "already answered; waiting for a run slot") {
			t.Fatalf("second response: %v", err)
		}
		t.Logf("response/park race %d: subscriber review statuses %v; snapshot running, awaiting_agent_since absent; second respond: %v", i+1, statuses, err)
		if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: id}, nil); err != nil {
			t.Fatal(err)
		}
		f.waitStatus(id, types.RunCancelled, types.RunFailed)
	}
	f.step.release(holder)
	f.waitStatus(holder, types.RunCompleted)
}

// Exercise startup recovery, response acceptance and the subscriber wire
// together. The operator's contract is that an accepted gate is running,
// unparked and cannot accept a second response, even before admission.
func TestQueueRecoveredAcceptedGateStateThroughIPC(t *testing.T) {
	for _, parkedStatus := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		t.Run(string(parkedStatus), func(t *testing.T) {
			seed := newFaultFixture(t, "1")
			plans := seedRecoveredQueueGates(seed)
			for _, plan := range plans {
				rows, err := seed.d.GetStepsByRun(plan.run.ID)
				if err != nil || len(rows) != 1 {
					t.Fatalf("seeded gate: %v, %v", rows, err)
				}
				if err := seed.d.UpdateStepStatus(rows[0].ID, parkedStatus); err != nil {
					t.Fatal(err)
				}
			}
			runTestDaemon(t, seed.p, seed.d, func() []pipeline.Step {
				return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
			}, 30*time.Second)
			f := &schedFixture{t: t, p: seed.p, d: seed.d}
			f.dial()
			var waitingID, holderID string
			for _, plan := range plans {
				info := f.info(plan.run.ID)
				if info.WaitingForSlot {
					waitingID = info.ID
				} else {
					holderID = info.ID
				}
			}
			if waitingID == "" || holderID == "" {
				t.Fatal("startup did not recover one held gate and one capacity waiter")
			}
			if usage := f.slots(); usage.InUse != 1 || usage.Queued != 1 || !usage.HoldSlotWhileParked {
				t.Fatalf("recovered slots: %+v", usage)
			}
			events, cancel, err := ipc.Subscribe(seed.p.Socket(), &ipc.SubscribeParams{RunID: waitingID})
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			select {
			case event := <-events:
				if event.Type != ipc.EventStreamGap {
					t.Fatalf("initial subscription event: %+v", event)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("subscription never acknowledged")
			}
			f.respondApprove(waitingID)
			info := f.info(waitingID)
			if info.Status != types.RunRunning || !info.WaitingForSlot || info.AwaitingAgentSince != nil || len(info.Steps) != 1 || info.Steps[0].Status != types.StepStatusRunning {
				t.Fatalf("accepted gate still advertised as parked: %+v", info)
			}
			if info.Steps[0].LastActivity == nil || *info.Steps[0].LastActivity != "already answered; waiting for a run slot" {
				t.Fatalf("accepted gate activity: %+v", info.Steps[0])
			}
			wire, _ := json.MarshalIndent(info, "", "  ")
			t.Logf("get_run after approving a recovered gate while the other gate holds the only slot:\n%s", wire)
			err = f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: waitingID, Step: types.StepReview, Action: types.ActionApprove}, nil)
			if err == nil || !strings.Contains(err.Error(), "already answered; waiting for a run slot") {
				t.Fatalf("duplicate response refusal: %v", err)
			}
			t.Logf("second respond refused: %v", err)
			select {
			case event := <-events:
				if event.Type != ipc.EventStepStarted || event.Status == nil || *event.Status != string(types.StepStatusRunning) {
					t.Fatalf("accepted-response wire event: %+v", event)
				}
				wire, _ := json.Marshal(event)
				t.Logf("subscriber event: %s", wire)
			case <-time.After(5 * time.Second):
				t.Fatal("subscriber never saw the accepted-response transition")
			}
			// Cancel only these disposable runs; no real pipeline is controlled.
			for _, id := range []string{waitingID, holderID} {
				if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: id}, nil); err != nil {
					t.Fatal(err)
				}
				f.waitStatus(id, types.RunCancelled, types.RunFailed)
			}
		})
	}
}
