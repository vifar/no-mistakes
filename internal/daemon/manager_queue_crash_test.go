package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
	if err := m.HandleRespond(id, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(f.t, m, id, types.StepReview)); err == nil || !strings.Contains(err.Error(), "gate already resolved or superseded") {
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

	if err := f.mgr.HandleRespond(waiting, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(t, f.mgr, waiting, types.StepReview)); err != nil {
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
	if err := next.HandleRespond(holder, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(t, next, holder, types.StepReview)); err != nil {
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
	if err := f.d.SetStepPendingResponse(rows[0].ID, boundPendingFixture(t, f.d, rows[0].ID, `{"kind":"reconcile"}`), "already answered; waiting for a run slot", databaseGateSnapshot(t, f.d, rows[0].ID)); err != nil {
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
	if err := f.mgr.HandleRespond(gate, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(t, f.mgr, gate, types.StepReview)); err != nil {
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

// The limits accepted at startup survive a config file that later vanishes or
// stops parsing: scheduling keeps the accepted cap, not the built-in default.
func TestSeededLimitsSurviveAMissingOrUnreadableConfig(t *testing.T) {
	for _, fault := range []string{"removed", "unparseable"} {
		t.Run(fault, func(t *testing.T) {
			f := newFaultFixture(t, "1")
			if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "max_concurrent_agents", "2"); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadGlobal(f.p.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			pipeline.SetMaxConcurrentAgents(0)
			f.mgr.SeedLimits(cfg)
			if pipeline.MaxConcurrentAgents() != 2 {
				t.Fatalf("agent cap = %d, want the seeded 2", pipeline.MaxConcurrentAgents())
			}
			t.Cleanup(func() { pipeline.SetMaxConcurrentAgents(0) })
			if fault == "unparseable" {
				// Launching needs a readable config, so occupy the slot first and
				// check the scheduler's own view after the fault.
				first := f.push("first")
				f.waitStarted(first)
				if err := os.WriteFile(f.p.ConfigFile(), []byte("daemon: [unterminated\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if usage := f.mgr.ReloadLimits(); usage.MaxConcurrentRuns != 1 || usage.InUse != 1 {
					t.Fatalf("slot usage after the config broke = %+v, want the accepted cap 1", usage)
				}
				if _, admitted := f.mgr.sched.tryAdmit("another"); admitted {
					t.Fatal("a second run was admitted: the cap of 1 was replaced by the built-in default")
				}
				if pipeline.MaxConcurrentAgents() != 2 {
					t.Fatalf("agent cap = %d after the fault, want 2", pipeline.MaxConcurrentAgents())
				}
				f.step.release(first)
				return
			}
			if err := os.Remove(f.p.ConfigFile()); err != nil {
				t.Fatal(err)
			}
			// Without the config the launch resolves agent "auto" from PATH;
			// put the mock agent first so the result does not depend on which
			// agents the machine has installed.
			agentDir := t.TempDir()
			writeMockClaude(t, agentDir)
			t.Setenv("PATH", agentDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			first := f.push("first")
			f.waitStarted(first)
			second := f.push("second")
			if got := f.status(second); got != types.RunQueued {
				t.Fatalf("second launch status = %s: the cap of 1 was replaced by the built-in default", got)
			}
			if usage := f.mgr.SlotUsage(); usage.MaxConcurrentRuns != 1 {
				t.Fatalf("slot usage = %+v, want cap 1", usage)
			}
			if pipeline.MaxConcurrentAgents() != 2 {
				t.Fatalf("agent cap = %d after the fault, want 2", pipeline.MaxConcurrentAgents())
			}
			f.step.release(first)
			f.waitStarted(second)
			f.step.release(second)
		})
	}
}

// Queued rows must survive a startup that fails after recovery began: queue
// dispatch is held until IPC is serving, so an IPC bind failure leaves them
// queued (not claimed into crash-looking pending runs) for the next start.
func TestStartupFailureAfterRecoveryLeavesQueuedRowsQueued(t *testing.T) {
	f := newFaultFixture(t, "1")
	// Parked gates are recovered before IPC binds. Each one releases the slot it
	// was admitted with when it parks, which is exactly the event that would start
	// a queued run early if startup did not hold the queue.
	plans := seedRecoveredQueueGates(f)
	if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "hold_slot_while_parked", "false"); err != nil {
		t.Fatal(err)
	}
	repo, err := f.d.GetRepo("fault-repo")
	if err != nil {
		t.Fatal(err)
	}
	var queued []string
	for _, branch := range []string{"q1", "q2"} {
		run, err := f.d.InsertRun(repo.ID, branch, f.head, f.head)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.d.MarkRunQueued(run.ID, `{"trigger":"push"}`); err != nil {
			t.Fatal(err)
		}
		queued = append(queued, run.ID)
	}
	f.mgr.Shutdown()

	// The IPC endpoint cannot be bound: a directory sits where the socket goes.
	if err := os.MkdirAll(filepath.Join(f.p.Socket(), "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = RunWithOptions(f.p, f.d, func() []pipeline.Step { return []pipeline.Step{f.step} })
	if err == nil || !strings.Contains(err.Error(), "bind IPC") {
		t.Fatalf("RunWithOptions error = %v, want the IPC bind failure", err)
	}
	for _, id := range queued {
		if got := f.status(id); got != types.RunQueued {
			t.Fatalf("queued run %s is %s after the failed start; it must stay queued", id, got)
		}
	}
	if len(f.step.startedIDs()) != 0 {
		t.Fatalf("queued runs executed before IPC was serving: %v", f.step.startedIDs())
	}
	for _, plan := range plans {
		if got := f.status(plan.run.ID); got != types.RunRunning {
			t.Fatalf("recovered gate %s is %s after the failed start, want it untouched", plan.run.ID, got)
		}
	}

	// The next start restores and runs them in order.
	if err := os.RemoveAll(f.p.Socket()); err != nil {
		t.Fatal(err)
	}
	sf := &schedFixture{t: t, p: f.p, d: f.d, step: f.step}
	sf.restartDaemon()
	sf.waitStarted(queued[0])
	sf.step.release(queued[0])
	sf.waitStarted(queued[1])
	sf.step.release(queued[1])
	sf.waitStatus(queued[1], types.RunCompleted)
}

// While startup holds the queue, any scheduling event (a recovered gate parking
// and releasing its slot, the config watcher, a limit reload) leaves queued rows
// queued; releasing the hold starts them in order.
func TestHeldQueueStartsNothingUntilReleased(t *testing.T) {
	f := newFaultFixture(t, "1")
	repo, err := f.d.GetRepo("fault-repo")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, branch := range []string{"h1", "h2"} {
		run, err := f.d.InsertRun(repo.ID, branch, f.head, f.head)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.d.MarkRunQueued(run.ID, `{"trigger":"push"}`); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	runs, err := f.mgr.loadQueuedRuns()
	if err != nil {
		t.Fatal(err)
	}
	f.mgr.holdQueue()
	f.mgr.restoreQueuedRuns(runs)
	f.mgr.sched.dispatch()
	f.mgr.ReloadLimits()
	time.Sleep(200 * time.Millisecond)
	for _, id := range ids {
		if got := f.status(id); got != types.RunQueued {
			t.Fatalf("held queued run %s is %s", id, got)
		}
	}
	if st := f.mgr.sched.stats(); st.Active != 0 || st.Queued != 2 {
		t.Fatalf("held queue stats = %+v", st)
	}
	f.mgr.releaseQueue()
	f.waitStarted(ids[0])
	f.step.release(ids[0])
	f.waitStarted(ids[1])
	f.step.release(ids[1])
}

// Turning hold_slot_while_parked off at runtime releases the slots of gates that
// were already parked under it - they never yield again on their own - so queued
// runs are not blocked behind them. The parked gate itself stays parked and
// resumes with priority.
func TestDisablingHoldReleasesAlreadyParkedSlots(t *testing.T) {
	slowWatcher(t, time.Hour)
	f := newFaultFixture(t, "1")
	if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "hold_slot_while_parked", "true"); err != nil {
		t.Fatal(err)
	}
	f.mgr.ReloadLimits()

	parked := f.push("park1")
	f.waitFor("gate to park", func() bool {
		run, err := f.d.GetRun(parked)
		return err == nil && run != nil && run.AwaitingAgentSince != nil
	})
	blocked := f.push("blocked")
	later := f.push("later")
	if st := f.mgr.sched.stats(); st.Active != 1 || st.Queued != 2 {
		t.Fatalf("under hold the parked gate must keep its slot: %+v", st)
	}
	if f.started(blocked) {
		t.Fatal("a queued run started behind a parked gate that holds the only slot")
	}

	if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "hold_slot_while_parked", "false"); err != nil {
		t.Fatal(err)
	}
	usage := f.mgr.ReloadLimits()
	if usage.HoldSlotWhileParked {
		t.Fatalf("reload did not apply the new policy: %+v", usage)
	}
	f.waitStarted(blocked)
	if run, _ := f.d.GetRun(parked); run == nil || run.AwaitingAgentSince == nil || run.Status != types.RunRunning {
		t.Fatalf("the parked gate was disturbed: %+v", run)
	}
	if st := f.mgr.sched.stats(); st.Yielded != 1 || st.Active != 1 {
		t.Fatalf("after disabling hold: %+v, want the gate yielded and the blocked run holding the slot", st)
	}

	// The gate resumes with priority over the run that queued after it parked.
	if err := f.mgr.HandleRespond(parked, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(t, f.mgr, parked, types.StepReview)); err != nil {
		t.Fatal(err)
	}
	f.step.release(blocked)
	f.waitFor("the parked gate to complete first", func() bool { return f.status(parked) == types.RunCompleted })
	f.waitStarted(later)
	f.step.release(later)
}
