package daemon

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	shutdownCIBranch     = "ci-monitor"
	shutdownParkedBranch = "parked"
	shutdownAgentBranch  = "mid-agent"
)

// shutdownMixedFirstStep sends each branch to a different place when the
// daemon stops: the CI branch on into CI monitoring with an already-created
// PR, the parked branch to an approval gate, and the agent branch into a step
// that is still working.
type shutdownMixedFirstStep struct{}

func (shutdownMixedFirstStep) Name() types.StepName { return types.StepDocument }
func (shutdownMixedFirstStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.Run.Branch != shutdownParkedBranch {
		url := "https://github.com/test/repo/pull/" + sctx.Run.Branch
		if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, url); err != nil {
			return nil, err
		}
		sctx.Run.PRURL = &url
	}
	if err := bindMockRunComparison(sctx); err != nil {
		return nil, err
	}
	switch sctx.Run.Branch {
	case shutdownParkedBranch:
		if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, sctx.Run.HeadSHA); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"needs review"}`}, nil
	case shutdownAgentBranch:
		<-sctx.Ctx.Done()
		return nil, sctx.Ctx.Err()
	}
	return &pipeline.StepOutcome{}, nil
}

// shutdownMonitoringCIStep polls an open PR until the daemon stops it.
type shutdownMonitoringCIStep struct{}

func (shutdownMonitoringCIStep) Name() types.StepName { return types.StepCI }
func (shutdownMonitoringCIStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	sctx.YieldSlot()
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

func startMixedShutdownDaemon(t *testing.T, p *paths.Paths, d *db.DB) chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{shutdownMixedFirstStep{}, shutdownMonitoringCIStep{}}
		})
	}()
	waitForDaemonReady(t, p)
	return errCh
}

func pushBranchRun(t *testing.T, p *paths.Paths, workDir, branch, headSHA string) string {
	t.Helper()
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/"+branch)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var pushed ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("shutdown-ci-monitor"),
		Ref:  "refs/heads/" + branch,
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushed); err != nil {
		t.Fatal(err)
	}
	return pushed.RunID
}

func waitForStepStatus(t *testing.T, d *db.DB, runID string, step types.StepName, status types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		steps, _ := d.GetStepsByRun(runID)
		for _, row := range steps {
			if row.StepName == step && row.Status == status {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s step %s never reached %s", runID, step, status)
}

func runStep(t *testing.T, d *db.DB, runID string, step types.StepName) *db.StepResult {
	t.Helper()
	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range steps {
		if row.StepName == step {
			return row
		}
	}
	t.Fatalf("run %s has no %s step", runID, step)
	return nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// A stop or forced restart while a run monitors CI for an open PR must keep
// that run as ci_monitor_interrupted across the restart, not fail it, while a
// parked run still resumes and a run cut off mid-step still fails.
func TestShutdown_CIMonitorRunSurvivesRestartAsInterrupted(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "shutdown-ci-monitor")

	errCh := startMixedShutdownDaemon(t, p, d)
	ciRun := pushBranchRun(t, p, repo.WorkingPath, shutdownCIBranch, headSHA)
	parkedRun := pushBranchRun(t, p, repo.WorkingPath, shutdownParkedBranch, headSHA)
	agentRun := pushBranchRun(t, p, repo.WorkingPath, shutdownAgentBranch, headSHA)
	waitForStepStatus(t, d, ciRun, types.StepCI, types.StepStatusRunning)
	waitForStepStatus(t, d, parkedRun, types.StepDocument, types.StepStatusAwaitingApproval)
	waitForStepStatus(t, d, agentRun, types.StepDocument, types.StepStatusRunning)
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := d.GetRun(parkedRun)
		if err != nil {
			t.Fatal(err)
		}
		if run.AwaitingAgentSince != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("parked run never recorded its awaiting marker")
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopParkedRunDaemon(t, p, errCh)

	stopped, err := d.GetRun(ciRun)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status == types.RunFailed {
		t.Errorf("CI-monitor run after stop = failed (%q), want it left for interrupted recovery", stringValue(stopped.Error))
	}
	if ci := runStep(t, d, ciRun, types.StepCI); ci.Status == types.StepStatusFailed {
		t.Errorf("CI step after stop = failed (%q), want it left for interrupted recovery", stringValue(ci.Error))
	}
	agent, err := d.GetRun(agentRun)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != types.RunFailed || stringValue(agent.Error) != pipeline.ErrDaemonShutdown.Error() {
		t.Errorf("mid-step run after stop = %s (%q), want failed with %q", agent.Status, stringValue(agent.Error), pipeline.ErrDaemonShutdown)
	}

	// The next start records the CI-monitor run as interrupted and resumes the
	// parked gate.
	errCh = startMixedShutdownDaemon(t, p, d)
	defer stopParkedRunDaemon(t, p, errCh)

	recovered, err := d.GetRun(ciRun)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != types.RunCIMonitorInterrupted || stringValue(recovered.Error) != types.RunCIMonitorInterruptedReason {
		t.Fatalf("CI-monitor run after restart = %s (%q), want %s", recovered.Status, stringValue(recovered.Error), types.RunCIMonitorInterrupted)
	}
	if ci := runStep(t, d, ciRun, types.StepCI); ci.Status != types.StepStatusSkipped || stringValue(ci.Error) != types.RunCIMonitorInterruptedReason {
		t.Fatalf("CI step after restart = %s (%q), want skipped as interrupted", ci.Status, stringValue(ci.Error))
	}
	if document := runStep(t, d, ciRun, types.StepDocument); document.Status != types.StepStatusCompleted {
		t.Fatalf("finished step of the CI-monitor run after restart = %s, want completed", document.Status)
	}
	parked, err := d.GetRun(parkedRun)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status != types.RunRunning || parked.AwaitingAgentSince == nil {
		t.Fatalf("parked run after restart = %s awaiting %v (%q), want still parked", parked.Status, parked.AwaitingAgentSince, stringValue(parked.Error))
	}
	if gate := runStep(t, d, parkedRun, types.StepDocument); gate.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("parked gate after restart = %s, want awaiting_approval", gate.Status)
	}
}
