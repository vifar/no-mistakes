package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestQueuedCancellationReleasesBranchCustody(t *testing.T) {
	for _, action := range []string{"abort", "supersede"} {
		t.Run(action, func(t *testing.T) {
			f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
			holder, _ := f.push("holder")
			f.waitStarted(holder)
			id, _ := f.push("queued")
			if f.status(id) != types.RunQueued {
				t.Fatal("run did not queue behind the occupied slot")
			}
			repo, err := f.d.GetRepo(f.repoID)
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "queued")
			cancelled := []string{id}
			latest := id
			if action == "supersede" {
				latest, _ = f.push("queued")
				cancelled = append(cancelled, latest)
				f.assertQueued(latest, 1, 1)
				if got := f.status(id); got != types.RunCancelled {
					t.Fatalf("superseded run status = %s, want cancelled", got)
				}
			}
			// Cancel the replacement too: a released latest run must not hide
			// an older, superseded run that incorrectly retained custody.
			if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: latest}, nil); err != nil {
				t.Fatal(err)
			}
			for _, cancelledID := range cancelled {
				run, err := f.d.GetRun(cancelledID)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status != types.RunCancelled || run.HeadSHA != f.head || run.TerminalHeadVerifiedAt == nil || run.LastPushedSHA != nil || run.PushGeneration != nil || run.PushActive {
					t.Fatalf("never-started cancellation lost its release proof: %+v", run)
				}
				f.assertNoWorktree(cancelledID)
				for _, startedID := range f.step.startedIDs() {
					if startedID == cancelledID {
						t.Fatal("cancelled queued run executed")
					}
				}
			}
			svc := branchsync.Service{DB: f.d, Repo: repo, WorkDir: repo.WorkingPath, Paths: f.p, GateDir: f.p.RepoDir(repo.ID)}
			state := svc.InspectCached(context.Background())
			if state.State != branchsync.StateUserOwned || state.Safety != "user_owned" || state.NextAction != nil || state.Pipeline.RunID != latest {
				t.Fatalf("cancelled queued branch was not released: %+v", state)
			}
			wire, _ := json.MarshalIndent(state, "", "  ")
			t.Logf("branch_sync after live %s of never-started queued runs:\n%s", action, wire)
			f.step.release(holder)
			f.waitStatus(holder, types.RunCompleted)
		})
	}
}

type cancellationExitAgent struct {
	recoveredRunTestAgent
	started   chan struct{}
	cancelled chan struct{}
	exit      chan struct{}
}

func (a *cancellationExitAgent) Run(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
	close(a.started)
	<-ctx.Done()
	close(a.cancelled)
	<-a.exit
	return nil, context.Cause(ctx)
}

type cancellationExitStep struct{ agent *cancellationExitAgent }

func (*cancellationExitStep) Name() types.StepName { return types.StepTest }

func (s *cancellationExitStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	copyCtx := *sctx
	copyCtx.Agent = s.agent
	_, err := copyCtx.RunAgent(agent.RunOpts{Prompt: "blocking cancellation workload", CWD: sctx.WorkDir})
	return nil, err
}

func TestClaimedCancellationWaitsForAgentExit(t *testing.T) {
	for _, path := range []string{"inline", "queued", "recovered"} {
		for _, action := range []string{"abort", "supersede"} {
			t.Run(path+"/"+action, func(t *testing.T) {
				f := newFaultFixture(t, "1")
				ag := &cancellationExitAgent{started: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
				var once sync.Once
				release := func() { once.Do(func() { close(ag.exit) }) }
				t.Cleanup(release)
				step := &cancellationExitStep{agent: ag}
				var id, branch string
				if path == "recovered" {
					plan := seedRecoveredQueueGates(f)[0]
					plan.steps = append(plan.steps, step)
					id, branch = plan.run.ID, plan.run.Branch
					if _, err := f.d.InsertStepResult(id, step.Name()); err != nil {
						t.Fatal(err)
					}
					f.mgr.resumeRecoveredRun(plan)
					if err := f.mgr.HandleRespond(id, types.StepReview, types.ActionApprove, nil); err != nil {
						t.Fatal(err)
					}
				} else {
					var holder string
					if path == "queued" {
						holder = f.push("holder")
						f.waitStarted(holder)
					}
					f.mgr.steps = func() []pipeline.Step { return []pipeline.Step{step} }
					branch = "blocking"
					id = f.push(branch)
					if holder != "" {
						if f.status(id) != types.RunQueued {
							t.Fatal("run did not queue")
						}
						f.step.release(holder)
					}
				}
				select {
				case <-ag.started:
				case <-time.After(30 * time.Second):
					t.Fatal("agent did not start")
				}
				cancelResult := make(chan error, 1)
				if action == "abort" {
					cancelResult <- f.mgr.HandleCancel(id)
				} else {
					go func() {
						cancelResult <- f.mgr.cancelActiveRuns("fault-repo", branch)
					}()
				}
				select {
				case <-ag.cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not reach the agent immediately")
				}
				if status := f.status(id); !status.Active() {
					t.Fatalf("run became %s while its agent was still executing", status)
				}
				release()
				f.waitFor("agent exit to terminalize the run", func() bool { return f.status(id) == types.RunCancelled })
				select {
				case err := <-cancelResult:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("cancellation did not finish after agent exit")
				}
				run, err := f.d.GetRun(id)
				if err != nil {
					t.Fatal(err)
				}
				want := types.RunCancelReasonAbortedByUser
				if action == "supersede" {
					want = types.RunCancelReasonSuperseded
				}
				if run.Error == nil || *run.Error != want {
					t.Fatalf("terminal error = %v, want %q", run.Error, want)
				}
			})
		}
	}
}

// routedStep runs the blocking agent workload for one branch and the plain
// controllable step for every other.
type routedStep struct {
	blockingBranch string
	blocking       *cancellationExitAgent
	plain          *schedStep
}

func (*routedStep) Name() types.StepName { return types.StepReview }

func (s *routedStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.Run.Branch != s.blockingBranch {
		return s.plain.Execute(sctx)
	}
	copyCtx := *sctx
	copyCtx.Agent = s.blocking
	_, err := copyCtx.RunAgent(agent.RunOpts{Prompt: "blocking workload", CWD: sctx.WorkDir})
	return nil, err
}

// Supersession snapshots a branch's runs and then cancels each. A queued run
// that dispatch claims in between is cancelled as claimed, and the replacement
// must wait for that run's workload to stop like for any other: it may not
// start while the old run's agent is still shutting down.
func TestSupersessionWaitsForARunClaimedAfterTheSnapshot(t *testing.T) {
	f := newFaultFixture(t, "2")
	ag := &cancellationExitAgent{started: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(ag.exit) }) }
	t.Cleanup(release)
	routed := &routedStep{blockingBranch: "same", blocking: ag, plain: f.step}
	f.mgr.steps = func() []pipeline.Step { return []pipeline.Step{routed} }

	holderA := f.push("holder-a")
	holderB := f.push("holder-b")
	f.waitStarted(holderA)
	f.waitStarted(holderB)
	queued := f.push("same")
	if f.status(queued) != types.RunQueued {
		t.Fatal("run did not queue behind the two holders")
	}

	reached := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	f.mgr.afterSupersedeSnapshot = func() {
		pauseOnce.Do(func() {
			close(reached)
			<-resume
		})
	}
	result := make(chan error, 1)
	go func() { result <- f.mgr.cancelActiveRuns("fault-repo", "same") }()
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("supersession never snapshotted the branch")
	}

	// A slot frees; dispatch claims the queued run and its workload starts.
	f.step.release(holderA)
	select {
	case <-ag.started:
	case <-time.After(60 * time.Second):
		t.Fatal("the queued run never started after the snapshot")
	}
	close(resume)

	select {
	case <-ag.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("supersession did not signal the run it found claimed")
	}
	select {
	case err := <-result:
		t.Fatalf("supersession returned (%v) while the old run's workload was still stopping", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("supersession never finished after the workload stopped")
	}
	f.waitFor("superseded run to be cancelled", func() bool { return f.status(queued) == types.RunCancelled })
	f.step.release(holderB)
}
