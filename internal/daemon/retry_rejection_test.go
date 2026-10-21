package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type retryFailureAgent struct{ failure error }

func (retryFailureAgent) Name() string { return "local-error" }
func (retryFailureAgent) Close() error { return nil }
func (a retryFailureAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return nil, a.failure
}

type retryInvocationStep struct{}

func (retryInvocationStep) Name() types.StepName { return types.StepReview }
func (retryInvocationStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	_, err := sctx.RunAgent(agent.RunOpts{})
	return nil, fmt.Errorf("review invocation: %w", err)
}

func TestRetryAdmissionUsesPersistedOutputRejectionClassification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent stand-ins require POSIX shell")
	}
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejected=%v", rejected), func(t *testing.T) {
			f := newRetryFixture(t, retryFixtureOptions{beforeSource: func(t *testing.T, f *retryFixture) {
				writeFakeAgents(t, f.p, "claude", "", "claude")
			}})
			source, err := f.d.InsertRun(f.repo.ID, "main", f.submitted, f.submitted)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("local adapter failure")
			if rejected {
				failure = agent.RejectStructuredOutput(failure)
			}
			sourceSteps := []pipeline.Step{&mockPassStep{name: types.StepRebase}, retryInvocationStep{}, &mockPassStep{name: types.StepTest}}
			executor := pipeline.NewExecutor(f.d, f.p, &config.Config{}, retryFailureAgent{failure}, sourceSteps, nil)
			if err := executor.Execute(context.Background(), source, f.repo, f.repo.WorkingPath); !errors.Is(err, failure) {
				t.Fatalf("source failed for the wrong reason: %v", err)
			}
			rows, err := f.d.GetStepsByRun(source.ID)
			if err != nil || len(rows) != 3 || (rows[1].FailureCause == nil) != rejected {
				t.Fatalf("persisted classification: rows=%+v err=%v", rows, err)
			}
			m := f.manager(t, &countingStep{name: types.StepTest, count: f.executions})
			link, err := m.HandleRetry(context.Background(), f.repo.ID, "main", source.ID, f.submitted)
			if rejected {
				if err == nil || !strings.Contains(err.Error(), "eligible failure proof failed") || link != nil {
					t.Fatalf("rejected output was admitted: link=%+v err=%v", link, err)
				}
				if stored, err := f.d.GetRunRetryBySource(source.ID); err != nil || stored != nil || f.executions.Load() != 0 {
					t.Fatalf("refusal launched a continuation: link=%+v err=%v calls=%d", stored, err, f.executions.Load())
				}
			} else {
				if err != nil || link == nil {
					t.Fatalf("genuine invocation failure lost eligibility: %v", err)
				}
				if run := waitForRunTerminalState(t, f.d, link.RunID); run.Status != types.RunCompleted {
					t.Fatalf("eligible continuation failed: status=%s error=%v", run.Status, run.Error)
				}
			}
		})
	}
}
