package steps

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type nonRejectionDiagnostic struct{ error }

func (nonRejectionDiagnostic) StructuredOutputRejected() bool { return false }
func (e nonRejectionDiagnostic) Unwrap() error                { return e.error }

func TestCheckpointRetryRejectsStructuredOutputFailures(t *testing.T) {
	for _, step := range []pipeline.Step{&ReviewStep{}, &TestStep{}} {
		for _, envelope := range []string{"direct", "wrapped", "joined first", "joined last", "false wrapper"} {
			t.Run(string(step.Name())+"/"+envelope, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				original := errors.New("missing final structured response")
				rejection := agent.RejectStructuredOutput(original)
				var failure error = rejection
				other := nonRejectionDiagnostic{errors.New("additional diagnostic")}
				switch envelope {
				case "wrapped":
					failure = fmt.Errorf("adapter: %w", rejection)
				case "joined first":
					failure = errors.Join(rejection, other)
				case "joined last":
					failure = errors.Join(other, rejection)
				case "false wrapper":
					failure = nonRejectionDiagnostic{rejection}
				}
				calls := 0
				ag := &mockAgent{name: "local-rejection", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					calls++
					return nil, failure
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{step}, nil)
				err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
				if !errors.Is(err, original) || !agent.IsStructuredOutputRejected(err) || !errors.Is(err, pipeline.ErrAgentInvocationFailed) || calls != 3 {
					t.Fatalf("bounded formatting rejection lost: calls=%d err=%v", calls, err)
				}
				rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
				if err != nil || len(rows) != 1 || rows[0].Status != types.StepStatusFailed || rows[0].FailureCause != nil {
					t.Fatalf("output rejection acquired an eligible cause: rows=%+v err=%v", rows, err)
				}
				_, err = pipeline.PlanRetryCheckpoint(sctx.DB, sctx.Run, []pipeline.Step{step})
				var refusal *pipeline.RetryRefusal
				if !errors.As(err, &refusal) || refusal.Proof != pipeline.RetryProofCause {
					t.Fatalf("output rejection admitted for checkpoint retry: %v", err)
				}
			})
		}
	}
}
