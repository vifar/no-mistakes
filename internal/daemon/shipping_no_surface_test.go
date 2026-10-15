package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const strictNoSurfaceEvidence = `{"findings":[{"id":"test-1","severity":"warning","action":"ask-user","description":"this change has no live-validatable surface; proceed without live validation? (0 of 1 scenarios were driven live against the product); documentation: Markdown has no running product surface"}],"verdict":"no-surface","scenarios":[{"name":"documentation","result":"untested","live":false,"reason":"Markdown has no running product surface"}]}`

type noSurfaceApprovalStep struct {
	steps.TestStep
	findings string
	exit     int
}

func (s *noSurfaceApprovalStep) Name() types.StepName { return types.StepTest }
func (s *noSurfaceApprovalStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: s.findings, ExitCode: s.exit}, nil
}

func TestShippingNoSurfaceManagerAdmission(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, reason := range []string{"", "Documentation correctness confirmed"} {
			for _, kind := range []string{"genuine", "no-go", "inconclusive", "live", "pass", "fail", "empty", "missing-reason", "missing-name", "malformed", "lookalike", "unrelated", "budget", "command-finding", "command-exit", "override"} {
				t.Run(kind+"/"+map[bool]string{false: "live", true: "recovered"}[recovered]+"/"+reason, func(t *testing.T) {
					p := paths.WithRoot(t.TempDir())
					database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
					if err != nil {
						t.Fatal(err)
					}
					defer database.Close()
					repo, head := setupTestGitRepo(t, p, database, "shipping-no-surface")
					dir := repo.WorkingPath
					run, err := database.InsertRunWithShipping(repo.ID, "feature", head, head, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
					if err != nil {
						t.Fatal(err)
					}
					evidence, err := types.ParseFindingsJSON(strictNoSurfaceEvidence)
					if err != nil {
						t.Fatal(err)
					}
					exit := 0
					switch kind {
					case "no-go":
						evidence.Verdict = types.TestVerdictNoGo
					case "inconclusive":
						evidence.Verdict = types.TestVerdictInconclusive
					case "live":
						evidence.Scenarios[0].Live = true
					case "pass":
						evidence.Scenarios[0].Result = types.ScenarioResultPass
					case "fail":
						evidence.Scenarios[0].Result = types.ScenarioResultFail
					case "empty":
						evidence.Scenarios = nil
					case "missing-reason":
						evidence.Scenarios[0].Reason = ""
					case "missing-name":
						evidence.Scenarios[0].Name = ""
					case "lookalike":
						evidence.Items[0].Source = types.FindingSourceUser
					case "unrelated":
						evidence.Items = append(evidence.Items, types.Finding{ID: "other", Severity: "error", Action: "ask-user", Description: "Unresolved defect"})
					case "budget":
						evidence.Items = append(evidence.Items, types.Finding{ID: types.FindingIDTestAgentTimeout, Severity: "warning", Action: "no-op", Description: "Invocation cut"})
					case "command-finding":
						evidence.Items = append(evidence.Items, types.Finding{ID: "command", Severity: "error", Category: types.FindingCategoryTestCommand, Action: "no-op", Description: "Command failed"})
					case "command-exit":
						exit = 1
					}
					raw, err := types.MarshalFindingsJSON(evidence)
					if err != nil {
						t.Fatal(err)
					}
					if kind == "malformed" {
						raw = "{"
					}
					step := &noSurfaceApprovalStep{findings: raw, exit: exit}
					var final ipc.Event
					gateReady := make(chan struct{}, 1)
					executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{step}, func(event ipc.Event) {
						if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepTest && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
							gateReady <- struct{}{}
						}
						if event.Type == ipc.EventRunCompleted {
							final = event
						}
					})
					manager := NewRunManager(database, p, nil)
					manager.runs[run.ID] = &managedRun{run: run, executor: executor}
					// Gate events follow both registration and persistence, including
					// recovery. The deadline detects hangs, not admission latency.
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					done := make(chan error, 1)
					if recovered {
						if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
							t.Fatal(err)
						}
						sr, err := database.InsertStepResult(run.ID, types.StepTest)
						if err != nil {
							t.Fatal(err)
						}
						if err := database.StartStep(sr.ID); err != nil {
							t.Fatal(err)
						}
						if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, exit, 10, &raw); err != nil {
							t.Fatal(err)
						}
						if _, err := database.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 10); err != nil {
							t.Fatal(err)
						}
						run, err = database.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						go func() {
							defer close(done)
							done <- executor.Resume(ctx, run, repo, dir)
						}()
					} else {
						go func() {
							defer close(done)
							done <- executor.Execute(ctx, run, repo, dir)
						}()
					}
					defer func() {
						cancel()
						for range done {
						}
					}()
					select {
					case <-gateReady:
					case err := <-done:
						t.Fatalf("executor exited before acknowledgment: %v", err)
					case <-ctx.Done():
						t.Fatal("Test did not park")
					}
					rows, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 || rows[0].Status != types.StepStatusAwaitingApproval || rows[0].FindingsJSON == nil {
						t.Fatalf("gate event without persisted Test evidence: %+v", rows)
					}
					parked := rows[0]
					original := *parked.FindingsJSON
					if kind == "override" {
						if err := database.SetStepOverrideReason(parked.ID, "Recorded override"); err != nil {
							t.Fatal(err)
						}
					}
					_, err = manager.HandleRespondWithOverrides(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, reason)
					if kind == "genuine" {
						if err != nil {
							t.Fatalf("genuine no-surface acknowledgment refused: %v", err)
						}
						if err := <-done; err != nil {
							t.Fatal(err)
						}
						got, err := database.GetStepResult(parked.ID)
						if err != nil {
							t.Fatal(err)
						}
						if got.Status != types.StepStatusCompleted || got.ApprovalReason == nil || *got.ApprovalReason != reason || got.FindingsJSON == nil || *got.FindingsJSON != original || got.OverrideReason != nil || got.TestOverrideReason() != "" {
							t.Fatalf("acknowledgment lost evidence or became an override: %+v", got)
						}
						if final.Status == nil || *final.Status != string(types.RunCompleted) || final.TestOverrideReason != nil || final.CIOverrideReason != nil {
							t.Fatalf("completion event misreported acknowledgment: %+v", final)
						}
					} else {
						if err == nil {
							t.Fatal("invalid no-surface acknowledgment accepted by manager")
						}
						if _, err := executor.RespondWithOverrides(types.StepTest, types.ActionApprove, nil, nil, nil, nil, reason); err == nil {
							t.Fatal("invalid acknowledgment accepted by executor")
						}
						got, err := database.GetStepResult(parked.ID)
						if err != nil {
							t.Fatal(err)
						}
						if got.Status != types.StepStatusAwaitingApproval || got.ApprovalReason != nil {
							t.Fatalf("refusal changed durable gate: %+v", got)
						}
						if err := executor.Respond(types.StepTest, types.ActionAbort, nil); err != nil {
							t.Fatal(err)
						}
						<-done
					}
				})
			}
		}
	}
}
