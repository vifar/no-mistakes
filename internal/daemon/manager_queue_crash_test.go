package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// answeredInfo is what a status read reports for a run, as the daemon builds it.
func (f *faultFixture) answeredInfo(m *RunManager, id string) *ipc.RunInfo {
	f.t.Helper()
	run, err := f.d.GetRun(id)
	if err != nil || run == nil {
		f.t.Fatalf("get run %s: %v", id, err)
	}
	steps, err := f.d.GetStepsByRun(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return m.annotateQueue(runToInfo(f.d, run, steps))
}

// assertAnsweredWaiting checks the contract of an accepted gate response that is
// waiting for a run slot: reported as answered and running (never as a decision
// still owed), but durably still a parked gate with its response recorded, and
// a second response refused.
func (f *faultFixture) assertAnsweredWaiting(m *RunManager, id, phase string) {
	f.t.Helper()
	info := f.answeredInfo(m, id)
	if len(info.Steps) != 1 || info.Steps[0].Status != types.StepStatusRunning || !info.Steps[0].ResponsePending || info.AwaitingAgent || info.AwaitingAgentSince != nil || !info.WaitingForSlot {
		f.t.Fatalf("%s: status = %+v, want an answered run waiting for a slot", phase, info)
	}
	rows, err := f.d.GetStepsByRun(id)
	if err != nil || len(rows) != 1 {
		f.t.Fatalf("%s: steps: %v, %v", phase, rows, err)
	}
	if !rows[0].AnsweredWaitingForSlot() {
		f.t.Fatalf("%s: persisted step = status %s pending %v; the gate must stay parked with its response recorded", phase, rows[0].Status, rows[0].PendingResponse)
	}
	run, err := f.d.GetRun(id)
	if err != nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		f.t.Fatalf("%s: persisted run = %+v, %v; it must stay a recoverable parked run", phase, run, err)
	}
	if err := m.HandleRespond(id, types.StepReview, types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "already answered; waiting for a run slot") {
		f.t.Fatalf("%s: second response = %v, want the already-answered refusal", phase, err)
	}
}

// With every slot full, an accepted response must survive a daemon crash during
// the wait: the next start recovers the gate, admits it with priority, and
// re-applies the recorded response once a slot frees.
func TestAcceptedResponseSurvivesCrashWhileWaitingForSlot(t *testing.T) {
	f := newFaultFixture(t, "1")
	plans := seedRecoveredQueueGates(f)
	// Resume newest first, the order a restart recovers them in, so the same
	// gate waits for capacity both before and after the crash.
	f.mgr.resumeRecoveredRuns([]recoveredRunPlan{plans[1], plans[0]})
	holder, waiting := plans[1].run.ID, plans[0].run.ID
	if info := f.answeredInfo(f.mgr, waiting); !info.WaitingForSlot {
		t.Fatalf("setup: gate %s is not waiting for capacity: %+v", waiting, info)
	}

	if err := f.mgr.HandleRespond(waiting, types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("accept response while the only slot is held: %v", err)
	}
	f.assertAnsweredWaiting(f.mgr, waiting, "before the crash")

	// Crash: a new daemon takes over the same database and worktrees. The old
	// manager is abandoned mid-wait, exactly as a killed process would be.
	next := NewRunManager(f.d, f.p, f.mgr.steps)
	t.Cleanup(next.Shutdown)
	recovered := next.recoverableParkedRuns(context.Background())
	preserved := map[string]struct{}{}
	for _, plan := range recovered {
		preserved[plan.run.ID] = struct{}{}
	}
	if _, ok := preserved[waiting]; !ok || len(recovered) != 2 {
		t.Fatalf("startup recovered %d gates (answered gate included: %v), want both", len(recovered), ok)
	}
	if _, err := f.d.RecoverStaleRunsExcept("daemon crashed during execution", preserved); err != nil {
		t.Fatal(err)
	}
	if got := f.status(waiting); got != types.RunRunning {
		t.Fatalf("answered gate status after crash recovery = %s, want it recovered, not failed", got)
	}
	next.resumeRecoveredRuns(recovered)
	f.assertAnsweredWaiting(next, waiting, "after the restart")

	// The holder finishes; the answered gate is admitted and its recorded
	// approval executes with no new response.
	if err := next.HandleRespond(holder, types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve the slot holder: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for f.status(waiting) != types.RunCompleted {
		if time.Now().After(deadline) {
			run, _ := f.d.GetRun(waiting)
			t.Fatalf("recorded response never executed: run %+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, _ := f.d.GetStepsByRun(waiting)
	if len(rows) != 1 || rows[0].PendingResponse != nil || rows[0].Status != types.StepStatusCompleted {
		t.Fatalf("step after dispatch = %+v, want completed with no pending response", rows)
	}
}

func TestRecoveredReconciliationIsReportedAsAnsweredBeforeAdmission(t *testing.T) {
	f := newFaultFixture(t, "1")
	plans := seedRecoveredQueueGates(f)
	rows, err := f.d.GetStepsByRun(plans[1].run.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("parked step: %+v, %v", rows, err)
	}
	if err := f.d.SetStepPendingResponse(rows[0].ID, `{"kind":"reconcile"}`, "already answered; waiting for a run slot"); err != nil {
		t.Fatal(err)
	}
	f.mgr.resumeRecoveredRuns(plans)
	f.assertAnsweredWaiting(f.mgr, plans[1].run.ID, "recovered reconciliation")
}

// A recovered gate that is approved while it waits for capacity keeps the slot
// the scheduler grants it: handing that slot to a queued new run first would
// make an answered gate wait behind work that arrived after it.
func TestAnsweredRecoveredGateKeepsItsPriorityHandoffSlot(t *testing.T) {
	f := newFaultFixture(t, "1")
	plans := seedRecoveredQueueGates(f)
	// hold_slot_while_parked: false - a parked run gives its slot back.
	if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "hold_slot_while_parked", "false"); err != nil {
		t.Fatal(err)
	}
	f.mgr.ReloadLimits()

	holder := f.push("holder")
	f.waitStarted(holder)
	f.mgr.resumeRecoveredRuns(plans[:1])
	gate := plans[0].run.ID
	if info := f.answeredInfo(f.mgr, gate); !info.WaitingForSlot {
		t.Fatalf("setup: recovered gate is not waiting behind the executing run: %+v", info)
	}
	if err := f.mgr.HandleRespond(gate, types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve after registration: %v", err)
	}
	queued := f.push("queued")
	if info := f.answeredInfo(f.mgr, queued); info.QueuePosition != 2 || info.QueueTotal != 2 {
		t.Fatalf("the new run did not queue behind the answered gate: %+v", info)
	}

	f.step.release(holder)
	f.waitStarted(queued)
	if got := f.status(gate); got != types.RunCompleted {
		t.Fatalf("answered gate status = %s when the queued run started; it was handed the slot first", got)
	}
	f.step.release(queued)
}
