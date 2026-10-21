package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
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

func TestClaudeTerminalVerdictRejectsCrashAndCheckpointRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local Claude fixture uses POSIX shell")
	}
	defer agent.WithFastBackoff()()
	for _, verdict := range []struct {
		name, stream            string
		schema, limit, eligible bool
		calls                   int
	}{
		{"schema", `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true}`, true, false, false, 1},
		{"usage", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}
{"type":"result","subtype":"error_during_execution","is_error":true}`, false, true, false, 1},
		{"transient", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":503}`, false, false, true, 4},
	} {
		for _, code := range []int{0, 1} {
			for _, step := range []pipeline.Step{&ReviewStep{}, &TestStep{}} {
				t.Run(verdict.name+"/exit="+strconv.Itoa(code)+"/"+string(step.Name()), func(t *testing.T) {
					dir, base, head := setupGitRepo(t)
					scratch := t.TempDir()
					count := filepath.Join(scratch, "calls")
					bin := filepath.Join(scratch, "claude")
					quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
					script := "#!/bin/sh\nprintf 'call\\n' >> " + quote(count) + "\nprintf '%s\\n' " + quote(verdict.stream) + "\nexit " + strconv.Itoa(code) + "\n"
					if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
					ag, err := agent.New(types.AgentClaude, bin, nil)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { ag.Close() })
					sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
					sctx.Config.AgentCrashRetries = 1
					testgit.BindRunComparison(t, sctx.DB, sctx.Run, dir, "main", "", "", "")
					executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{step}, nil)
					failure := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
					if failure == nil || !agent.IsTerminalProviderError(failure) || agent.IsUsageLimitReached(failure) != verdict.limit || errors.Is(failure, agent.ErrStructuredOutputRetriesExhausted) != verdict.schema {
						t.Fatalf("lost native verdict: %v", failure)
					}
					if agent.IsStructuredOutputRejected(failure) {
						t.Fatalf("native verdict became a formatting rejection: %v", failure)
					}
					var exitErr *exec.ExitError
					if errors.As(failure, &exitErr) != (code != 0) {
						t.Fatalf("lost process-exit evidence: %v", failure)
					}
					data, err := os.ReadFile(count)
					if err != nil || len(strings.Fields(string(data))) != verdict.calls {
						t.Fatalf("unexpected adapter or step replay: calls=%q err=%v failure=%v", data, err, failure)
					}
					rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
					if err != nil || len(rows) != 1 || rows[0].Status != types.StepStatusFailed || (rows[0].FailureCause != nil) != verdict.eligible {
						t.Fatalf("persisted failure classification: rows=%+v err=%v", rows, err)
					}
					checkpoint, err := pipeline.PlanRetryCheckpoint(sctx.DB, sctx.Run, []pipeline.Step{step})
					if verdict.eligible {
						if err != nil || checkpoint == nil {
							t.Fatalf("transient invocation lost checkpoint eligibility: %v", err)
						}
					} else {
						var refusal *pipeline.RetryRefusal
						if !errors.As(err, &refusal) || refusal.Proof != pipeline.RetryProofCause {
							t.Fatalf("terminal verdict admitted for checkpoint retry: %v", err)
						}
					}
				})
			}
		}
	}
}
