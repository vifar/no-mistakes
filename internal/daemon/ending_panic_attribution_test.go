package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type endingPanicAfterCancelStep struct {
	entered chan struct{}
	once    sync.Once
}

func (*endingPanicAfterCancelStep) Name() types.StepName { return types.StepReview }
func (s *endingPanicAfterCancelStep) fail(ctx context.Context) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	panic("independent ending panic")
}
func (s *endingPanicAfterCancelStep) Execute(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.fail(c.Ctx)
	return nil, nil
}
func (s *endingPanicAfterCancelStep) ReconcileApprovalGate(c *pipeline.StepContext) (bool, error) {
	s.fail(c.Ctx)
	return false, nil
}

func TestEndingPanicAfterCancelRetainsAttribution(t *testing.T) {
	for _, mode := range []string{"new", "recovered"} {
		t.Run(mode, func(t *testing.T) {
			f := newFaultFixture(t, "1")
			step := &endingPanicAfterCancelStep{entered: make(chan struct{})}
			f.mgr.steps = func() []pipeline.Step { return []pipeline.Step{step} }
			var id string
			if mode == "new" {
				id = f.push("panic-attribution")
			} else {
				repo, err := f.d.GetRepo("fault-repo")
				if err != nil {
					t.Fatal(err)
				}
				run, err := f.d.InsertRun(repo.ID, "main", f.head, f.head)
				if err != nil {
					t.Fatal(err)
				}
				id = run.ID
				workDir := f.p.WorktreeDir(repo.ID, id)
				if err := gitpkg.WorktreeAdd(context.Background(), f.p.RepoDir(repo.ID), workDir, f.head); err != nil {
					t.Fatal(err)
				}
				if err := f.d.SetRunWorktreeDir(id, workDir); err != nil {
					t.Fatal(err)
				}
				if err := f.d.UpdateRunStatus(id, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				sr, err := f.d.InsertStepResult(id, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.d.StartStep(sr.ID); err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[],"summary":"parked review"}`
				if err := f.d.SetStepFindings(sr.ID, findings); err != nil {
					t.Fatal(err)
				}
				if _, err := f.d.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, f.head, 1); err != nil {
					t.Fatal(err)
				}
				if err := parkGateFixture(f.d, sr.ID, types.StepStatusAwaitingApproval, 1); err != nil {
					t.Fatal(err)
				}
				if err := f.d.SetRunAwaitingAgent(id); err != nil {
					t.Fatal(err)
				}
				run, err = f.d.GetRun(id)
				if err != nil {
					t.Fatal(err)
				}
				cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
				cfg.GateReconcileInterval = time.Millisecond
				cfg.GateReconcileTimeout = time.Minute
				f.mgr.resumeRecoveredRun(recoveredRunPlan{run: run, repo: repo, workDir: workDir, gateDir: f.p.RepoDir(repo.ID), cfg: cfg, agent: recoveredRunTestAgent{}, steps: []pipeline.Step{step}})
			}
			select {
			case <-step.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("panic fixture never reached execution")
			}
			if err := pipeline.AcquireCommitStatusLock(context.Background()); err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(pipeline.ReleaseCommitStatusLock) }
			defer release()
			if _, err := f.mgr.handleCancelWithAttribution(id, &types.RunEndingEvent{Caller: "panic-caller", Client: "axi", Reason: "explicit panic stop"}); err != nil {
				t.Fatal(err)
			}
			release()
			done := make(chan struct{})
			go func() { f.mgr.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("panic settlement did not finish")
			}
			stored, err := f.d.GetRun(id)
			if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, "internal panic: independent ending panic") {
				t.Fatalf("panic outcome changed: %+v, %v", stored, err)
			}
			if stored.EndingEvent == nil || stored.EndingEvent.Caller != "panic-caller" || stored.EndingEvent.Reason != "explicit panic stop" || stored.EndingEvent.Client != "axi" {
				t.Fatalf("panic lost accepted cancellation: %+v", stored.EndingEvent)
			}
		})
	}
}
