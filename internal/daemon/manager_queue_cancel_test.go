package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestQueuedCancellationReleasesBranchCustody(t *testing.T) {
	for _, action := range []string{"abort", "supersede"} {
		t.Run(action, func(t *testing.T) {
			f := newFaultFixture(t, "1")
			holder := f.push("holder")
			f.waitStarted(holder)
			id := f.push("queued")
			if f.status(id) != types.RunQueued {
				t.Fatal("run did not queue behind the occupied slot")
			}
			repo, err := f.d.GetRepo("fault-repo")
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "queued")
			if action == "abort" {
				err = f.mgr.HandleCancel(id)
			} else {
				err = f.mgr.cancelActiveRuns(repo.ID, "queued")
			}
			if err != nil {
				t.Fatal(err)
			}
			run, err := f.d.GetRun(id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunCancelled || run.HeadSHA != f.head || run.TerminalHeadVerifiedAt == nil || run.LastPushedSHA != nil || run.PushGeneration != nil || run.PushActive {
				t.Fatalf("never-started cancellation lost its release proof: %+v", run)
			}
			svc := branchsync.Service{DB: f.d, Repo: repo, WorkDir: repo.WorkingPath, Paths: f.p, GateDir: f.p.RepoDir(repo.ID)}
			state := svc.InspectCached(context.Background())
			if state.State != branchsync.StateUserOwned || state.Safety != "user_owned" || state.NextAction != nil || state.Pipeline.RunID != id {
				t.Fatalf("cancelled queued branch was not released: %+v", state)
			}
			if f.started(id) {
				t.Fatal("cancelled queued run executed")
			}
			f.step.release(holder)
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
