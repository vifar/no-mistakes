package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// faultFixture drives a RunManager directly (no IPC daemon) so a test can
// inject faults between its internal steps.
type faultFixture struct {
	t    *testing.T
	mgr  *RunManager
	d    *db.DB
	p    *paths.Paths
	step *schedStep
	head string
}

func newFaultFixture(t *testing.T, maxRuns string) *faultFixture {
	t.Helper()
	tmp, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	p := paths.WithRoot(tmp)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	cfg := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\ndaemon:\n  max_concurrent_runs: " + maxRuns + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	_, head := setupTestGitRepo(t, p, d, "fault-repo")
	step := newSchedStep()
	mgr := NewRunManager(d, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(mgr.Shutdown)
	return &faultFixture{t: t, mgr: mgr, d: d, p: p, step: step, head: head}
}

func (f *faultFixture) push(branch string) string {
	f.t.Helper()
	// Report a real accepted creation/recreation before injecting scheduler
	// faults; otherwise the live-ref guard rejects the fixture first.
	ref := "refs/heads/" + branch
	gitCmd(f.t, f.p.RepoDir("fault-repo"), "update-ref", "-d", ref)
	gitCmd(f.t, f.p.RepoDir("fault-repo"), "update-ref", ref, f.head)
	id, err := f.mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: f.p.RepoDir("fault-repo"),
		Ref:  "refs/heads/" + branch,
		Old:  "0000000000000000000000000000000000000000",
		New:  f.head,
	})
	if err != nil {
		f.t.Fatalf("push %s: %v", branch, err)
	}
	return id
}

func (f *faultFixture) status(id string) types.RunStatus {
	f.t.Helper()
	run, err := f.d.GetRun(id)
	if err != nil || run == nil {
		f.t.Fatalf("get run %s: %v", id, err)
	}
	return run.Status
}

func (f *faultFixture) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for %s", what)
}

func (f *faultFixture) waitStarted(id string) {
	f.t.Helper()
	f.waitFor("run "+id+" to start executing", func() bool {
		for _, s := range f.step.startedIDs() {
			if s == id {
				return true
			}
		}
		return false
	})
}

func (f *faultFixture) started(id string) bool {
	for _, s := range f.step.startedIDs() {
		if s == id {
			return true
		}
	}
	return false
}

// A claim that fails transiently after dispatch took the run off the queue
// must not strand it: it returns to the head of the queue, ahead of later
// runs, and then starts.
func TestQueueClaimFailureReturnsRunToItsFIFOPosition(t *testing.T) {
	old := claimRetryDelay
	claimRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { claimRetryDelay = old })
	f := newFaultFixture(t, "1")

	var failures atomic.Int32
	realClaim := f.mgr.claimQueued
	var first string
	f.mgr.claimQueued = func(id string) (bool, error) {
		if id == first && failures.Add(1) <= 2 {
			return false, errors.New("injected transient claim error")
		}
		return realClaim(id)
	}

	running := f.push("c0")
	f.waitStarted(running)
	first = f.push("c1")
	second := f.push("c2")

	f.step.release(running)
	// The failing claim for c1 must not let c2 jump it.
	f.waitStarted(first)
	if f.started(second) {
		t.Fatalf("a later queued run started before the run whose claim failed (started = %v)", f.step.startedIDs())
	}
	if got := failures.Load(); got < 3 {
		t.Fatalf("claim attempts for the first queued run = %d, want the two injected failures plus a success", got)
	}
	f.step.release(first)
	f.waitStarted(second)
	f.step.release(second)
	f.waitFor("all runs to complete", func() bool {
		return f.status(running) == types.RunCompleted && f.status(first) == types.RunCompleted && f.status(second) == types.RunCompleted
	})
}

// While its claim keeps failing the run is still abortable, and an abort ends
// it without it ever starting.
func TestQueueClaimFailureRunStaysAbortable(t *testing.T) {
	old := claimRetryDelay
	claimRetryDelay = 30 * time.Millisecond
	t.Cleanup(func() { claimRetryDelay = old })
	f := newFaultFixture(t, "1")

	var attempts atomic.Int32
	var stuck string
	realClaim := f.mgr.claimQueued
	f.mgr.claimQueued = func(id string) (bool, error) {
		if id == stuck {
			attempts.Add(1)
			return false, errors.New("injected persistent claim error")
		}
		return realClaim(id)
	}

	running := f.push("a0")
	f.waitStarted(running)
	stuck = f.push("a1")
	f.step.release(running)
	f.waitFor("the failing claim to be retried", func() bool { return attempts.Load() >= 2 })
	if got := f.status(stuck); got != types.RunQueued {
		t.Fatalf("run with a failing claim status = %s, want it still queued", got)
	}

	if err := f.mgr.HandleCancel(stuck); err != nil {
		t.Fatalf("abort during claim retries: %v", err)
	}
	f.waitFor("the abort to terminalize the run", func() bool { return f.status(stuck) == types.RunCancelled })
	n := attempts.Load()
	time.Sleep(150 * time.Millisecond)
	if after := attempts.Load(); after > n+1 {
		t.Fatalf("claims kept being retried after the abort (%d -> %d)", n, after)
	}
	if f.started(stuck) {
		t.Fatal("an aborted run started")
	}
	// The slot it held was released: the manager is free to run new work.
	next := f.push("a2")
	f.waitStarted(next)
	f.step.release(next)
}

// Shutdown racing the launch of a run the queue just dispatched: the shutdown
// check and handle registration are one atomic step, so the launch either never
// starts or is cancelled by Shutdown. Here the pause sits between the early
// check and registration.
func TestQueueShutdownRacingQueuedLaunchNeverStartsIt(t *testing.T) {
	f := newFaultFixture(t, "1")
	running := f.push("s0")
	f.waitStarted(running)
	queued := f.push("s1")

	reached := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	defer unblock()
	var once sync.Once
	f.mgr.beforeQueuedRegister = func() {
		once.Do(func() {
			close(reached)
			<-resume
		})
	}

	// Freeing the slot dispatches s1 into the paused launch.
	f.step.release(running)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("queued launch did not reach admission barrier")
	}

	shutdownDone := make(chan struct{})
	go func() {
		f.mgr.Shutdown()
		close(shutdownDone)
	}()
	fenceDeadline := time.Now().Add(time.Second)
	for f.mgr.shutdownState.Load() == shutdownIdle && time.Now().Before(fenceDeadline) {
		time.Sleep(time.Millisecond)
	}
	if f.mgr.shutdownState.Load() == shutdownIdle {
		t.Fatal("shutdown did not fence the paused queue admission")
	}
	unblock()
	select {
	case <-shutdownDone:
	case <-time.After(45 * time.Second):
		t.Fatal("Shutdown did not finish")
	}

	if f.started(queued) {
		t.Fatalf("a queued run started during shutdown (started = %v)", f.step.startedIDs())
	}
	f.mgr.mu.Lock()
	leaked := 0
	for _, owned := range f.mgr.runs {
		if owned.handle != nil {
			leaked++
		}
	}
	f.mgr.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d run handles still registered after shutdown", leaked)
	}
	// The durable row stays queued so the next daemon start drains it.
	if got := f.status(queued); got != types.RunQueued {
		t.Fatalf("queued run status after shutdown = %s, want queued for the next start", got)
	}
	if st := f.mgr.sched.stats(); st.Active != 0 {
		t.Fatalf("slots still in use after shutdown: %+v", st)
	}
}

func TestQueueCancellationWriteFailurePreservesOwnership(t *testing.T) {
	for _, action := range []string{"abort", "supersede", "claim-retry"} {
		t.Run(action, func(t *testing.T) {
			f := newFaultFixture(t, "1")
			running := f.push("holder")
			f.waitStarted(running)
			first := f.push("first")
			second := f.push("second")
			raw, err := sql.Open("sqlite", f.p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`CREATE TRIGGER reject_cancel BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled' BEGIN SELECT RAISE(FAIL, 'injected cancellation write failure'); END`); err != nil {
				t.Fatal(err)
			}
			if action == "claim-retry" {
				var attempts atomic.Int32
				f.mgr.claimQueued = func(id string) (bool, error) {
					attempts.Add(1)
					return false, errors.New("injected claim failure")
				}
				f.step.release(running)
				f.waitFor("the failed claim", func() bool { return attempts.Load() > 0 })
				if st := f.mgr.sched.stats(); st.Active != 0 {
					t.Fatalf("failed claim retained capacity during retry: %+v", st)
				}
			}
			switch action {
			case "abort", "claim-retry":
				err = f.mgr.HandleCancel(first)
			case "supersede":
				gitCmd(t, f.p.RepoDir("fault-repo"), "update-ref", "-d", "refs/heads/first")
				gitCmd(t, f.p.RepoDir("fault-repo"), "update-ref", "refs/heads/first", f.head)
				_, err = f.mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{Gate: f.p.RepoDir("fault-repo"), Ref: "refs/heads/first", Old: strings.Repeat("0", 40), New: f.head})

			}
			if err == nil || !strings.Contains(err.Error(), "injected cancellation write failure") {
				t.Fatalf("cancellation error = %v", err)
			}
			if got := f.status(first); got != types.RunQueued {
				t.Fatalf("status = %s, want queued", got)
			}
			for i, id := range []string{first, second} {
				info := &ipc.RunInfo{ID: id}
				f.mgr.annotateQueue(info)
				if info.QueuePosition != i+1 || info.QueueTotal != 2 {
					t.Fatalf("queue snapshot = %+v", info)
				}
			}
			if _, err := raw.Exec(`DROP TRIGGER reject_cancel`); err != nil {
				t.Fatal(err)
			}
			if err := f.mgr.HandleCancel(first); err != nil {
				t.Fatalf("retry abort: %v", err)
			}
			f.mgr.mu.Lock()
			f.mgr.claimQueued = f.d.ClaimQueuedRun
			f.mgr.mu.Unlock()
			if f.status(first) != types.RunCancelled || f.started(first) {
				t.Fatal("retry did not cancel without starting")
			}
			if action != "claim-retry" {
				f.step.release(running)
			}
			f.waitStarted(second)
			f.step.release(second)
		})
	}
}

func TestQueueSpecReadFailureFailsLaunch(t *testing.T) {
	f := newFaultFixture(t, "1")
	running := f.push("holder")
	f.waitStarted(running)
	queued := f.push("queued")
	raw, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`ALTER TABLE runs DROP COLUMN queue_spec`); err != nil {
		t.Fatal(err)
	}
	f.step.release(running)
	f.waitFor("queue spec read to fail the launch", func() bool { return f.status(queued) == types.RunFailed })
	run, err := f.d.GetRun(queued)
	if err != nil {
		t.Fatal(err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "read queued run spec:") {
		t.Fatalf("launch error = %v", run.Error)
	}
	if f.started(queued) {
		t.Fatal("run executed without its persisted spec")
	}
}

func seedRecoveredQueueGates(f *faultFixture) []recoveredRunPlan {
	if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "hold_slot_while_parked", "true"); err != nil {
		f.t.Fatal(err)
	}
	global, err := config.LoadGlobal(f.p.ConfigFile())
	if err != nil {
		f.t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	repo, err := f.d.GetRepo("fault-repo")
	if err != nil {
		f.t.Fatal(err)
	}
	var plans []recoveredRunPlan
	for _, branch := range []string{"recovered-a", "recovered-b"} {
		run, err := f.d.InsertRun(repo.ID, branch, f.head, f.head)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := f.d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
			f.t.Fatal(err)
		}
		workDir := f.p.WorktreeDir(repo.ID, run.ID)
		if err := gitpkg.WorktreeAdd(context.Background(), f.p.RepoDir(repo.ID), workDir, f.head); err != nil {
			f.t.Fatal(err)
		}
		testgit.BindRunComparison(f.t, f.d, run, workDir, "main", "", "", "")
		step, err := f.d.InsertStepResult(run.ID, types.StepReview)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := f.d.StartStep(step.ID); err != nil {
			f.t.Fatal(err)
		}
		findings := `{"findings":[],"summary":"needs approval"}`
		if err := f.d.SetStepFindings(step.ID, findings); err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.d.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, f.head, 1); err != nil {
			f.t.Fatal(err)
		}
		if err := parkGateFixture(f.d, step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
			f.t.Fatal(err)
		}
		if err := f.d.SetRunAwaitingAgent(run.ID); err != nil {
			f.t.Fatal(err)
		}
		run, err = f.d.GetRun(run.ID)
		if err != nil {
			f.t.Fatal(err)
		}
		plans = append(plans, recoveredRunPlan{run: run, repo: repo, workDir: workDir, gateDir: f.p.RepoDir(repo.ID), cfg: cfg, agent: recoveredRunTestAgent{}, steps: []pipeline.Step{&mockApprovalStep{name: types.StepReview}}})
	}
	return plans
}

func TestRecoveredParkedRunsUseBoundedPriorityAdmission(t *testing.T) {
	f := newFaultFixture(t, "1")
	plans := seedRecoveredQueueGates(f)
	f.mgr.resumeRecoveredRuns(plans)
	queued := f.push("new")
	if st := f.mgr.sched.stats(); st.Active != 1 || st.Queued != 2 {
		t.Fatalf("recovery stats = %+v", st)
	}
	info := &ipc.RunInfo{ID: plans[1].run.ID}
	f.mgr.annotateQueue(info)
	if !info.WaitingForSlot || info.QueuePosition != 1 {
		t.Fatalf("recovered waiter = %+v", info)
	}
	for i, plan := range plans {
		f.waitFor("recovered gate to accept approval", func() bool {
			err := f.mgr.HandleRespond(plan.run.ID, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(t, f.mgr, plan.run.ID, types.StepReview))
			return err == nil
		})
		f.waitFor("recovered run to complete", func() bool { return f.status(plan.run.ID) == types.RunCompleted })
		if i == 0 && f.started(queued) {
			t.Fatal("new run started before recovered gates completed")
		}
	}
	f.waitStarted(queued)
	f.step.release(queued)
}

func TestClaimedCancellationDoesNotRequireDatabaseAccess(t *testing.T) {
	for _, branch := range []string{"running", "parked"} {
		for _, action := range []string{"abort", "supersede"} {
			for _, fault := range []string{"readonly", "closed"} {
				t.Run(branch+"/"+action+"/"+fault, func(t *testing.T) {
					f := newFaultFixture(t, "1")
					id := f.push(branch)
					f.waitStarted(id)
					if branch == "parked" {
						f.waitFor("run to park", func() bool {
							run, err := f.d.GetRun(id)
							return err == nil && run.AwaitingAgentSince != nil
						})
					}
					readonly, err := db.OpenReadOnly(f.p.DB())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { f.mgr.Shutdown(); readonly.Close() })
					if _, err := readonly.CancelQueuedRun(id, types.RunCancelReasonAbortedByUser); err == nil {
						t.Fatal("fixture allowed a write on the read-only connection")
					}
					if fault == "closed" {
						if err := readonly.Close(); err != nil {
							t.Fatal(err)
						}
						if _, err := readonly.GetRun(id); err == nil {
							t.Fatal("fixture allowed a read on a closed connection")
						}
					}
					f.mgr.db = readonly
					reason := types.RunCancelReasonAbortedByUser
					if action == "abort" {
						err = f.mgr.HandleCancel(id)
					} else {
						reason = types.RunCancelReasonSuperseded
						err = f.mgr.cancelActiveRuns("fault-repo", branch)
					}
					if err != nil {
						t.Fatalf("cancel claimed run: %v", err)
					}
					f.waitFor("claimed run to stop", func() bool { return f.status(id) == types.RunCancelled })
					run, err := f.d.GetRun(id)
					if err != nil {
						t.Fatal(err)
					}
					if run.Error == nil || *run.Error != reason {
						t.Fatalf("cancellation reason = %v, want %s", run.Error, reason)
					}
					f.mgr.wg.Wait()
					if st := f.mgr.sched.stats(); st.Active != 0 {
						t.Fatalf("stopped run retained capacity: %+v", st)
					}
				})
			}
		}
	}
}

func TestRecoveredGateWaitingForSlotAcceptsResponses(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		t.Run(string(status), func(t *testing.T) {
			for _, action := range []types.ApprovalAction{types.ActionAbort, types.ActionApprove} {
				t.Run(string(action), func(t *testing.T) {
					f := newFaultFixture(t, "1")
					plans := seedRecoveredQueueGates(f)
					rows, err := f.d.GetStepsByRun(plans[1].run.ID)
					if err != nil || len(rows) != 1 {
						t.Fatalf("recovered step: %v, %v", rows, err)
					}
					if err := f.d.UpdateStepStatus(rows[0].ID, status); err != nil {
						t.Fatal(err)
					}
					f.mgr.resumeRecoveredRuns(plans)
					queued := f.push("new")
					waitingID := plans[1].run.ID
					info := &ipc.RunInfo{ID: waitingID}
					f.mgr.annotateQueue(info)
					if !info.WaitingForSlot || info.QueuePosition != 1 {
						t.Fatalf("recovered run did not wait behind the held slot: %+v", info)
					}
					sub, err := f.mgr.Subscribe(waitingID)
					if err != nil {
						t.Fatal(err)
					}
					defer sub.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, ok := sub.Next(ctx); !ok {
						t.Fatal("missing initial stream gap")
					}
					if err := f.mgr.HandleRespond(waitingID, types.StepReview, action, nil, managerGateSnapshot(t, f.mgr, waitingID, types.StepReview)); err != nil {
						t.Fatalf("respond before admission: %v", err)
					}
					if action == types.ActionAbort {
						f.waitFor("waiting gate to abort without capacity", func() bool { return f.status(waitingID) == types.RunCancelled })
						run, err := f.d.GetRun(waitingID)
						if err != nil || run.AwaitingAgentSince != nil {
							t.Fatalf("aborted recovery remained parked: run=%+v error=%v", run, err)
						}
					} else {
						row, err := f.d.GetStepResult(rows[0].ID)
						if err != nil {
							t.Fatal(err)
						}
						if !row.AnsweredWaitingForSlot() || row.LastActivity == nil || *row.LastActivity != "already answered; waiting for a run slot" {
							t.Fatalf("answered step lost its recorded response: %+v", row)
						}
						run, err := f.d.GetRun(waitingID)
						if err != nil {
							t.Fatal(err)
						}
						if run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
							t.Fatalf("answered run is not durably parked for recovery: %+v", run)
						}
						if err := f.mgr.HandleRespond(waitingID, types.StepReview, action, nil, managerGateSnapshot(t, f.mgr, waitingID, types.StepReview)); err == nil || !strings.Contains(err.Error(), "gate already resolved or superseded") {
							t.Fatalf("duplicate response: %v", err)
						}
						event, ok := sub.Next(ctx)
						if !ok || event.Type != ipc.EventStepStarted || event.Status == nil || *event.Status != string(types.StepStatusRunning) {
							t.Fatalf("answered gate event: %+v, %v", event, ok)
						}
					}
					if st := f.mgr.sched.stats(); st.Active != 1 {
						t.Fatalf("recovered admission exceeded cap: %+v", st)
					}
					if f.started(queued) {
						t.Fatal("new work overtook recovered gates")
					}
					if err := f.mgr.HandleRespond(plans[0].run.ID, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(t, f.mgr, plans[0].run.ID, types.StepReview)); err != nil {
						t.Fatal(err)
					}
					if action == types.ActionApprove {
						f.waitFor("early approval to complete after admission", func() bool { return f.status(waitingID) == types.RunCompleted })
					}
					f.waitStarted(queued)
					f.step.release(queued)
				})
			}
		})
	}
}

// pauseAt installs a one-shot pause in a manager hook and returns a channel
// that closes when a run reaches it, plus the function that lets it go on.
func pauseAt(hook *func(string)) (reached <-chan struct{}, resume func()) {
	r := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	*hook = func(string) {
		once.Do(func() {
			close(r)
			<-gate
		})
	}
	return r, func() { close(gate) }
}

func (f *faultFixture) completeAndPause(branch string, hook *func(string)) (id string, resume func()) {
	f.t.Helper()
	reached, resume := pauseAt(hook)
	id = f.push(branch)
	f.waitStarted(id)
	f.step.release(id)
	select {
	case <-reached:
	case <-time.After(60 * time.Second):
		f.t.Fatal("run never reached the pause")
	}
	f.waitFor("run to be recorded completed", func() bool { return f.status(id) == types.RunCompleted })
	return id, resume
}

// R14: an abort that races a run's completion must never turn the completed run
// into a cancelled one - in the window before the run is marked finishing
// (conditional persistence) and while it is finishing (no cancellation at all).
func TestAbortRacingCompletionKeepsCompleted(t *testing.T) {
	for _, window := range []string{"before-finishing", "finishing"} {
		t.Run(window, func(t *testing.T) {
			f := newFaultFixture(t, "1")
			hook := &f.mgr.afterExecute
			if window == "finishing" {
				hook = &f.mgr.whileFinishing
			}
			id, resume := f.completeAndPause("race", hook)
			if err := f.mgr.HandleCancel(id); err != nil {
				t.Fatalf("abort of a run that just completed: %v", err)
			}
			run, err := f.d.GetRun(id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunCompleted || (run.Error != nil && *run.Error != "") {
				t.Fatalf("aborted-during-completion run = status %s error %v, want completed untouched", run.Status, run.Error)
			}
			resume()
			f.mgr.wg.Wait()
			if got := f.status(id); got != types.RunCompleted {
				t.Fatalf("status after the run finished = %s, want completed", got)
			}
		})
	}
}

// Supersede selects claimed and queued runs but never a finishing one: its
// outcome stays completed and the new push does not wait for its cleanup.
func TestSupersedeOfClaimedQueuedAndFinishingRuns(t *testing.T) {
	t.Run("claimed", func(t *testing.T) {
		f := newFaultFixture(t, "1")
		old := f.push("same")
		f.waitStarted(old)
		replacement := f.push("same")
		f.waitFor("old run to be superseded", func() bool { return f.status(old) == types.RunCancelled })
		run, _ := f.d.GetRun(old)
		if run.Error == nil || *run.Error != types.RunCancelReasonSuperseded {
			t.Fatalf("superseded run error = %v", run.Error)
		}
		f.waitStarted(replacement)
		f.step.release(replacement)
		f.waitFor("replacement to complete", func() bool { return f.status(replacement) == types.RunCompleted })
	})
	t.Run("queued", func(t *testing.T) {
		f := newFaultFixture(t, "1")
		holder := f.push("holder")
		f.waitStarted(holder)
		old := f.push("same")
		if f.status(old) != types.RunQueued {
			t.Fatalf("run did not queue: %s", f.status(old))
		}
		replacement := f.push("same")
		if got := f.status(old); got != types.RunCancelled {
			t.Fatalf("superseded queued run status = %s, want cancelled", got)
		}
		if f.started(old) {
			t.Fatal("a superseded queued run started")
		}
		info := &ipc.RunInfo{ID: replacement}
		f.mgr.annotateQueue(info)
		if info.QueuePosition != 1 || info.QueueTotal != 1 {
			t.Fatalf("replacement queue snapshot = %+v", info)
		}
		f.step.release(holder)
		f.waitStarted(replacement)
		f.step.release(replacement)
	})
	t.Run("finishing", func(t *testing.T) {
		f := newFaultFixture(t, "2")
		old, resume := f.completeAndPause("same", &f.mgr.whileFinishing)
		pushed := make(chan string, 1)
		go func() { pushed <- f.push("same") }()
		var replacement string
		select {
		case replacement = <-pushed:
		case <-time.After(30 * time.Second):
			resume()
			t.Fatal("a new push waited for the finishing run's cleanup")
		}
		if got := f.status(old); got != types.RunCompleted {
			t.Fatalf("finishing run status after a new push = %s, want completed", got)
		}
		f.waitStarted(replacement)
		f.step.release(replacement)
		resume()
	})
}

// A recovered gate waiting for a slot is cancelled by an explicit abort without
// ever needing capacity, and the cap is never exceeded meanwhile.
func TestAbortOfRecoveredGateWaitingForSlot(t *testing.T) {
	f := newFaultFixture(t, "1")
	plans := seedRecoveredQueueGates(f)
	f.mgr.resumeRecoveredRuns(plans)
	waiting := plans[1].run.ID
	info := &ipc.RunInfo{ID: waiting}
	f.mgr.annotateQueue(info)
	if !info.WaitingForSlot {
		t.Fatalf("second recovered gate is not waiting: %+v", info)
	}
	if err := f.mgr.HandleCancel(waiting); err != nil {
		t.Fatalf("abort of the waiting recovered gate: %v", err)
	}
	f.waitFor("waiting recovered gate to be cancelled", func() bool { return f.status(waiting) == types.RunCancelled })
	if got := f.status(plans[0].run.ID); got != types.RunRunning {
		t.Fatalf("the admitted recovered gate status = %s, want it untouched", got)
	}
	if st := f.mgr.sched.stats(); st.Active != 1 || st.Queued != 0 {
		t.Fatalf("slots after aborting the waiter = %+v, want the admitted gate only", st)
	}
	if err := f.mgr.HandleRespond(plans[0].run.ID, types.StepReview, types.ActionApprove, nil, managerGateSnapshot(t, f.mgr, plans[0].run.ID, types.StepReview)); err != nil {
		t.Fatal(err)
	}
	f.waitFor("admitted gate to complete", func() bool { return f.status(plans[0].run.ID) == types.RunCompleted })
}

// The claimed-cancellation contract applies to the public RPC, including its
// authorization guard: an unreadable database must not keep a workload alive.
func TestClaimedCancelRPCStopsWorkWhenDatabaseReadsFail(t *testing.T) {
	const childSocket = "NM_TEST_CANCEL_NESTED_SOCKET"
	const childRun = "NM_TEST_CANCEL_NESTED_RUN"
	if socket := os.Getenv(childSocket); socket != "" {
		client, err := ipc.Dial(socket)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		err = client.CallWithTimeout(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: os.Getenv(childRun)}, nil, 3*time.Second)
		if err == nil || !strings.Contains(err.Error(), gatecontext.ErrorCode) {
			t.Fatalf("daemon descendant cancellation = %v, want nested caller refusal", err)
		}
		return
	}
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	id, _ := f.push("claimed-read-failure")
	f.waitStarted(id)
	raw, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Change only this disposable database. Both the handler and its guard
	// now receive a real SQLite read error, rather than a mocked DB result.
	if _, err := raw.Exec("ALTER TABLE runs RENAME TO unreadable_runs"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := raw.Exec("ALTER TABLE unreadable_runs RENAME TO runs"); err != nil {
			t.Errorf("restore disposable database: %v", err)
		}
	}()
	// A real child of this in-process daemon remains forbidden from controlling
	// the pipeline, even though the same SQLite fault must not block an operator.
	child := exec.Command(os.Args[0], "-test.run=^TestClaimedCancelRPCStopsWorkWhenDatabaseReadsFail$", "-test.timeout=10s")
	child.Env = append(os.Environ(), childSocket+"="+f.p.Socket(), childRun+"="+id)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("nested cancellation guard: %v\n%s", err, output)
	}
	if got := f.slots().InUse; got != 1 {
		t.Fatalf("nested caller changed occupied slots to %d, want 1", got)
	}
	err = f.client.CallWithTimeout(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: id}, nil, 3*time.Second)
	t.Logf("cancel_run response with database reads unavailable: %v", err)
	t.Logf("get_slots after cancellation request: %+v", f.slots())
	if err != nil {
		t.Fatalf("claimed abort must reach its cancellation handle before database access: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.slots().InUse != 0 {
		if time.Now().After(deadline) {
			t.Fatal("claimed workload kept its slot after abort with an unreadable database")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// queuedRowThenBreakListing leaves a durable queued row and then makes listing
// the queue fail, standing in for a transient database error at startup.
func queuedRowThenBreakListing(t *testing.T, f *faultFixture) string {
	t.Helper()
	repo, err := f.d.GetRepo("fault-repo")
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.d.InsertRun(repo.ID, "orphan", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.MarkRunQueued(run.ID, `{"trigger":"push"}`); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`ALTER TABLE runs DROP COLUMN queued_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.QueuedRunIDs(); err == nil {
		t.Fatal("fixture did not break the queue listing")
	}
	return run.ID
}

// A failure to read the durable queue is reported before anything is
// registered or changed.
func TestLoadQueuedRunsReportsListingFailure(t *testing.T) {
	f := newFaultFixture(t, "1")
	id := queuedRowThenBreakListing(t, f)
	runs, err := f.mgr.loadQueuedRuns()
	if err == nil || !strings.Contains(err.Error(), "list queued runs") {
		t.Fatalf("loadQueuedRuns = %v, %v; want a clear listing failure", runs, err)
	}
	f.mgr.mu.Lock()
	owned := len(f.mgr.runs)
	f.mgr.mu.Unlock()
	if owned != 0 || f.mgr.sched.stats().Queued != 0 {
		t.Fatalf("a failed load left %d runs owned and %d queued", owned, f.mgr.sched.stats().Queued)
	}
	if got := f.status(id); got != types.RunQueued {
		t.Fatalf("a failed load changed the queued row to %s", got)
	}
}

// The daemon refuses to serve when the queue cannot be restored, instead of
// running with durable queued rows that nothing owns or can abort.
func TestDaemonStartupFailsWhenQueuedRunsCannotBeRestored(t *testing.T) {
	f := newFaultFixture(t, "1")
	queuedRowThenBreakListing(t, f)
	f.mgr.Shutdown()
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(f.p, f.d, nil) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "restore queued runs") {
			t.Fatalf("RunWithOptions error = %v, want a clear queue restoration failure", err)
		}
	case <-time.After(60 * time.Second):
		if client, err := ipc.Dial(f.p.Socket()); err == nil {
			_ = client.Call(ipc.MethodShutdown, testShutdownParams(t, f.p), nil)
			client.Close()
		}
		t.Fatal("daemon kept serving although its queued runs could not be restored")
	}
}

// An earlier launch that saw a full cap must get a slot freed meanwhile ahead
// of a later launch: the capacity decision and the queue insertion are one step.
func TestAdmissionKeepsEarlierFullCapLaunchAheadOfLaterLaunch(t *testing.T) {
	f := newFaultFixture(t, "1")
	holder := f.push("holder")
	f.waitStarted(holder)

	reached := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	f.mgr.afterFullDecision = func() {
		once.Do(func() {
			close(reached)
			<-resume
		})
	}
	earlierID := make(chan string, 1)
	go func() { earlierID <- f.push("earlier") }()
	<-reached // the earlier launch has seen a full cap and is not queued yet

	f.step.release(holder)
	f.waitFor("the holder's slot to free", func() bool { return f.mgr.sched.stats().Active == 0 })

	laterID := make(chan string, 1)
	go func() { laterID <- f.push("later") }()
	time.Sleep(200 * time.Millisecond) // let the later launch reach admission
	close(resume)

	earlier, later := <-earlierID, <-laterID
	f.waitStarted(earlier)
	if f.started(later) {
		t.Fatalf("the later launch took the freed slot ahead of the earlier one (started = %v)", f.step.startedIDs())
	}
	if got := f.status(later); got != types.RunQueued {
		t.Fatalf("later launch status = %s, want queued behind the earlier one", got)
	}
	f.step.release(earlier)
	f.waitStarted(later)
	f.step.release(later)
}

// Queue validation happens before recovery has any side effect, so when it
// fails the parked gates are neither resumed nor cancelled by the failure: they
// stay parked and recoverable, and the next start resumes them.
func TestFailedQueueRestoreLeavesParkedGatesRecoverable(t *testing.T) {
	f := newFaultFixture(t, "2")
	plans := seedRecoveredQueueGates(f)
	queuedID := queuedRowThenBreakListing(t, f)
	f.mgr.Shutdown()

	err := RunWithOptions(f.p, f.d, func() []pipeline.Step { return []pipeline.Step{&mockApprovalStep{name: types.StepReview}} })
	if err == nil || !strings.Contains(err.Error(), "restore queued runs") {
		t.Fatalf("RunWithOptions error = %v, want the queue restoration failure", err)
	}
	for _, plan := range plans {
		run, err := f.d.GetRun(plan.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != types.RunRunning || run.AwaitingAgentSince == nil || run.Error != nil {
			t.Fatalf("parked gate %s after the failed start = status %s awaiting %v error %v; want it untouched", run.ID, run.Status, run.AwaitingAgentSince, run.Error)
		}
	}

	// The database recovers: restore the column the fault dropped.
	raw, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE runs ADD COLUMN queued_at INTEGER`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	runTestDaemon(t, f.p, f.d, func() []pipeline.Step { return []pipeline.Step{&mockApprovalStep{name: types.StepReview}} }, 30*time.Second)
	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, plan := range plans {
		deadline := time.Now().Add(30 * time.Second)
		for {
			var response ipc.RespondResult
			err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: plan.run.ID, Step: types.StepReview, Action: types.ActionApprove, ExpectedGate: ipcGateSnapshot(t, client, plan.run.ID, types.StepReview)}, &response)
			if err == nil && response.OK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("recovered gate %s never accepted a response on the next start: %v", plan.run.ID, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if got := f.status(queuedID); got != types.RunQueued && got != types.RunPending && got != types.RunRunning {
		t.Fatalf("queued row after the successful start = %s, want it still owned", got)
	}
}

// When the cap is full and recording the run as queued fails, the freshly
// inserted row must not be left pending outside the ownership table, where it
// could be neither aborted nor dispatched and would block the branch: it ends
// through the shared setup-failure path, so a later launch works normally.
func TestAdmissionQueueWriteFailureEndsTheRunAndFreesTheBranch(t *testing.T) {
	f := newFaultFixture(t, "1")
	holder := f.push("holder")
	f.waitStarted(holder)

	f.mgr.markQueued = func(string, string) (int64, error) {
		return 0, errors.New("injected queue write failure")
	}
	gitCmd(t, f.p.RepoDir("fault-repo"), "update-ref", "refs/heads/victim", f.head)
	_, err := f.mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: f.p.RepoDir("fault-repo"),
		Ref:  "refs/heads/victim",
		Old:  "0000000000000000000000000000000000000000",
		New:  f.head,
	})
	if err == nil || !strings.Contains(err.Error(), "injected queue write failure") {
		t.Fatalf("launch error = %v, want the queue write failure", err)
	}
	runs, err := f.d.GetRunsByRepo("fault-repo")
	if err != nil {
		t.Fatal(err)
	}
	var orphan *db.Run
	for _, run := range runs {
		if run.Branch == "victim" {
			orphan = run
		}
	}
	if orphan == nil {
		t.Fatal("the rejected launch left no row to inspect")
	}
	if orphan.Status != types.RunFailed || orphan.Error == nil || !strings.Contains(*orphan.Error, "queue run: injected queue write failure") {
		t.Fatalf("rejected run = status %s error %v, want failed with the cause", orphan.Status, orphan.Error)
	}
	f.mgr.mu.Lock()
	_, owned := f.mgr.runs[orphan.ID]
	f.mgr.mu.Unlock()
	if owned {
		t.Fatal("the rejected run is still in the ownership table")
	}
	if active, err := f.d.GetActiveRun("fault-repo", "victim"); err != nil || active != nil {
		t.Fatalf("branch active-run lookup = %+v, %v; the failed launch must not block it", active, err)
	}
	// Attached clients are told, not left waiting on a stream that never ends.
	sub, err := f.mgr.Subscribe(orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, ok := sub.Next(ctx); !ok {
		t.Fatal("missing initial stream gap")
	}
	if _, ok := sub.Next(ctx); ok || ctx.Err() != nil {
		t.Fatalf("the rejected run's event stream was left open (ctx err %v)", ctx.Err())
	}

	// A later launch on the same branch is an ordinary one.
	f.mgr.markQueued = f.d.MarkRunQueued
	next := f.push("victim")
	if got := f.status(next); got != types.RunQueued {
		t.Fatalf("later launch status = %s, want queued behind the holder", got)
	}
	f.step.release(holder)
	f.waitStarted(next)
	f.step.release(next)
	f.waitFor("later launch to complete", func() bool { return f.status(next) == types.RunCompleted })
}

// A completed run is marked finishing while it still holds its slot. Release
// dispatches queued runs synchronously, so a push landing during that dispatch
// must already see the decided run as finishing, never as a claimed run to
// cancel and wait on.
func TestRunIsFinishingBeforeItReleasesItsSlot(t *testing.T) {
	f := newFaultFixture(t, "1")
	type observation struct {
		state  runOwnership
		active int
	}
	seen := make(chan observation, 1)
	f.mgr.whileFinishing = func(runID string) {
		f.mgr.mu.Lock()
		state := f.mgr.runs[runID].state
		f.mgr.mu.Unlock()
		select {
		case seen <- observation{state: state, active: f.mgr.sched.stats().Active}:
		default:
		}
	}
	id := f.push("same")
	f.waitStarted(id)
	f.step.release(id)
	var got observation
	select {
	case got = <-seen:
	case <-time.After(60 * time.Second):
		t.Fatal("run never reached finishing")
	}
	if got.state != ownershipFinishing {
		t.Fatalf("ownership while finishing = %v, want finishing", got.state)
	}
	if got.active != 1 {
		t.Fatalf("active slots when the run became finishing = %d, want 1 (slot released only after finishing)", got.active)
	}
	f.waitFor("run to complete", func() bool { return f.status(id) == types.RunCompleted })
}
