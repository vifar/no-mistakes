package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

type processExitError struct{ message string }

func (e processExitError) Error() string { return e.message }

func (processExitError) Unwrap() error { return &exec.ExitError{} }

func TestTestStep_AgentCrashRetryExitThenSuccess(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "crash-then-valid", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			return nil, processExitError{"agent exited: exit status 1"}
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.AgentCrashRetries = 1
	testgit.BindRunComparison(t, sctx.DB, sctx.Run, dir, "main", "", "", "")
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
	if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err != nil || calls != 2 {
		t.Fatalf("Test crash did not recover: calls=%d error=%v", calls, err)
	}
}

// Exercise the real parser and executor together, not only a marked mock error.
func TestDocumentStep_AgentCrashRetryMalformedOutput(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "malformed-output", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{not json`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.AgentCrashRetries = 1
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&DocumentStep{}}, nil)
	err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
	if err == nil || calls != 1 {
		t.Fatalf("malformed output was retried: calls=%d error=%v", calls, err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil || steps[0].FailureCause != nil {
		t.Fatalf("parser rejection became a manual continuation cause: steps=%+v error=%v", steps, err)
	}
}

func TestTestStep_AgentCrashRetryRefusesMovedEffectiveBase(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "base-moved", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		gitCmd(t, dir, "update-ref", "refs/heads/main", head)
		return nil, processExitError{"agent exited: exit status 1"}
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.AgentCrashRetries = 1
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
	if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil || calls != 1 {
		t.Fatalf("moved effective base reused: calls=%d error=%v", calls, err)
	} else if !strings.Contains(err.Error(), "effective validation base changed") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
}

func TestTestStep_AgentCrashRetryKeepsCommandFailureTerminal(t *testing.T) {
	t.Parallel()
	for _, retries := range []int{0, 1} {
		t.Run(fmt.Sprint(retries), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "crashed-evidence-agent", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, processExitError{"agent exited: exit status 1"}
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{
				Test: "git diff --exit-code --no-index base.txt feature.txt",
			})
			sctx.Config.AgentCrashRetries = retries
			executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
			if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil || calls != 1 {
				t.Fatalf("known command failure retried: calls=%d error=%v", calls, err)
			}
			rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil || len(rows) != 1 || rows[0].FailureCause != nil {
				t.Fatalf("command failure misclassified: rows=%+v err=%v", rows, err)
			}
			_, err = pipeline.PlanRetryCheckpoint(sctx.DB, sctx.Run, []pipeline.Step{&TestStep{}})
			var refusal *pipeline.RetryRefusal
			if !errors.As(err, &refusal) || refusal.Proof != pipeline.RetryProofCause {
				t.Fatalf("known command failure allowed checkpoint retry: %v", err)
			}
		})
	}
}

func TestDocumentStep_AgentCrashRetryPermanentFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{name: "raw Pi 401", message: "401 unauthorized"},
		{name: "raw Grok auth", message: "provider authentication failed"},
		{name: "raw quota", message: "insufficient quota"},
	} {
		message := tc.message
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "permanent-error", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, errors.New(message)
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.AgentCrashRetries = 1
			executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&DocumentStep{}}, nil)
			if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil || calls != 1 {
				t.Fatalf("permanent failure retried: calls=%d error=%v", calls, err)
			}
		})
	}
}
