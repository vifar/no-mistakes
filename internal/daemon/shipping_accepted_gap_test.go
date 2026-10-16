package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const inconclusiveGapEvidence = `{"findings":[{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive (1 of 2 scenarios were driven live against the product); untested: broker timeout"}],"verdict":"inconclusive","scenarios":[{"name":"happy path","result":"pass","live":true,"evidence":"ok"},{"name":"broker timeout","result":"untested","live":false,"reason":"no live broker fault injection"}]}`

func TestAcceptedGapApprovalManagerAndExecutor(t *testing.T) {
	const reason = "owner approved via first mate, 2026-10-14"
	const coveredBy = "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
	for _, recovered := range []bool{false, true} {
		for _, strict := range []bool{true, false} {
			name := map[bool]string{false: "live", true: "recovered"}[recovered] + "/" + map[bool]string{true: "strict", false: "plain"}[strict]
			t.Run(name, func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				repo, head := setupTestGitRepo(t, p, database, "shipping-accepted-gap")
				var shipping *types.ShippingOptions
				if strict {
					shipping = &types.ShippingOptions{Mode: types.ShippingFull}
				}
				run, err := database.InsertRunWithShipping(repo.ID, "feature", head, head, nil, "", "", "", "main", false, nil, shipping)
				if err != nil {
					t.Fatal(err)
				}
				raw := inconclusiveGapEvidence
				step := &noSurfaceApprovalStep{findings: raw}
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
					if _, err := database.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 10); err != nil {
						t.Fatal(err)
					}
					if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 10, &raw); err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					go func() {
						defer close(done)
						done <- executor.Resume(ctx, run, repo, repo.WorkingPath)
					}()
				} else {
					go func() {
						defer close(done)
						done <- executor.Execute(ctx, run, repo, repo.WorkingPath)
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
					t.Fatalf("executor exited before the gate: %v", err)
				case <-ctx.Done():
					t.Fatal("Test did not park")
				}
				rows, err := database.GetStepsByRun(run.ID)
				if err != nil || len(rows) != 1 || rows[0].Status != types.StepStatusAwaitingApproval {
					t.Fatalf("parked rows = %+v, %v", rows, err)
				}
				parked := rows[0]
				gaps := &db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: coveredBy}

				refused := func(label string, call func() error) {
					t.Helper()
					if err := call(); err == nil {
						t.Fatalf("%s accepted", label)
					}
					got, err := database.GetStepResult(parked.ID)
					if err != nil || got.Status != types.StepStatusAwaitingApproval || got.AcceptedGapsJSON != nil || got.ApprovalReason != nil {
						t.Fatalf("%s changed the parked gate: %+v, %v", label, got, err)
					}
				}
				if strict {
					refused("plain strict approve", func() error {
						_, err := manager.HandleRespondWithOverrides(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, "", managerGateSnapshot(t, manager, run.ID, types.StepTest))
						return err
					})
					refused("strict approve with a Test exception reason", func() error {
						_, err := manager.HandleRespondWithOverrides(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, reason, managerGateSnapshot(t, manager, run.ID, types.StepTest))
						return err
					})
					refused("executor plain strict approve", func() error {
						_, err := executor.RespondWithOverrides(types.StepTest, types.ActionApprove, nil, nil, nil, nil, "", gateSnapshot(executor))
						return err
					})
					refused("unknown finding", func() error {
						_, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, reason, &db.AcceptedGaps{FindingIDs: []string{"missing"}, Reason: reason, CoveredBy: coveredBy}, managerGateSnapshot(t, manager, run.ID, types.StepTest))
						return err
					})
					refused("gap on a fix action", func() error {
						_, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionFix, []string{"test-verdict"}, nil, nil, nil, reason, gaps, managerGateSnapshot(t, manager, run.ID, types.StepTest))
						return err
					})
				} else {
					refused("non-strict accept-gap", func() error {
						_, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, reason, gaps, managerGateSnapshot(t, manager, run.ID, types.StepTest))
						return err
					})
					refused("executor non-strict accept-gap", func() error {
						_, err := executor.RespondWithAcceptedGaps(types.StepTest, types.ActionApprove, nil, nil, nil, nil, reason, gaps, gateSnapshot(executor))
						return err
					})
					if err := executor.Respond(types.StepTest, types.ActionAbort, nil, nil); err != nil {
						t.Fatal(err)
					}
					<-done
					return
				}

				if _, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, reason, gaps, managerGateSnapshot(t, manager, run.ID, types.StepTest)); err != nil {
					t.Fatalf("valid accepted gap refused: %v", err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				got, err := database.GetStepResult(parked.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != types.StepStatusCompleted || got.ApprovalReason != nil || got.OverrideReason != nil || got.SkipReason != nil || got.AcceptedGapsJSON == nil {
					t.Fatalf("accepted gap row = %+v", got)
				}
				recorded, err := got.RecordedAcceptedGaps()
				if err != nil || recorded == nil || recorded.Reason != reason || recorded.CoveredBy != coveredBy || strings.Join(recorded.FindingIDs, ",") != "test-verdict" || recorded.Verdict != types.TestVerdictInconclusive || recorded.AcceptedAt == "" || len(recorded.Findings) != 1 {
					t.Fatalf("recorded gaps = %+v, %v", recorded, err)
				}
				if got.StrictApprovalRefusal("") != "" || !strings.Contains(got.TestOverrideReason(), "Test gap accepted") {
					t.Fatalf("completed row not honored: %q", got.StrictApprovalRefusal(""))
				}
				if final.Status == nil || *final.Status != string(types.RunCompleted) || final.TestOverrideReason == nil || !strings.Contains(*final.TestOverrideReason, coveredBy) {
					t.Fatalf("completion event = %+v", final)
				}
			})
		}
	}
}

// A daemon stop after the gap is written but before the step completes leaves
// a parked row holding a valid durable acceptance. Recovery must complete on
// that acceptance, not overwrite it with an ordinary approval reason.
func TestAcceptedGapSurvivesACrashBetweenRecordAndCompletion(t *testing.T) {
	const reason = "owner approved via first mate, 2026-10-14"
	const coveredBy = "TestBrokerTimeoutMocked"
	for _, response := range []string{"plain", "repeat-gap", "plain-with-reason"} {
		t.Run(response, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo, head := setupTestGitRepo(t, p, database, "shipping-accepted-gap-crash")
			run, err := database.InsertRunWithShipping(repo.ID, "feature", head, head, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
			if err != nil {
				t.Fatal(err)
			}
			raw := inconclusiveGapEvidence
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
			if _, err := database.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 10); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 10, &raw); err != nil {
				t.Fatal(err)
			}
			// The previous daemon wrote the acceptance, then stopped.
			if _, err := database.RecordAcceptedGaps(sr.ID, db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: coveredBy}, true); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			var final ipc.Event
			gateReady := make(chan struct{}, 1)
			executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{&noSurfaceApprovalStep{findings: raw}}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepTest && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					gateReady <- struct{}{}
				}
				if event.Type == ipc.EventRunCompleted {
					final = event
				}
			})
			manager := NewRunManager(database, p, nil)
			manager.runs[run.ID] = &managedRun{run: run, executor: executor}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer close(done)
				done <- executor.Resume(ctx, run, repo, repo.WorkingPath)
			}()
			defer func() {
				cancel()
				for range done {
				}
			}()
			select {
			case <-gateReady:
			case err := <-done:
				t.Fatalf("executor exited before the gate: %v", err)
			case <-ctx.Done():
				t.Fatal("recovered Test did not park")
			}
			parked, err := database.GetStepResult(sr.ID)
			if err != nil || parked.AcceptedGapsJSON == nil || parked.ValidAcceptedGaps() == nil {
				t.Fatalf("recovery lost the durable acceptance: %+v, %v", parked, err)
			}

			if !stepToInfo(database, parked).AcceptedGapRecorded {
				t.Fatal("run info does not expose the recovered accepted gap")
			}

			if response == "plain-with-reason" {
				if _, err := manager.HandleRespondWithOverrides(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, "another reason entirely", managerGateSnapshot(t, manager, run.ID, types.StepTest)); err == nil {
					t.Fatal("a plain approval carrying its own reason was admitted over a recorded gap")
				}
				got, err := database.GetStepResult(sr.ID)
				if err != nil || got.Status != types.StepStatusAwaitingApproval || got.ApprovalReason != nil || got.ValidAcceptedGaps() == nil {
					t.Fatalf("refusal damaged the recorded gap: %+v, %v", got, err)
				}
				if err := executor.Respond(types.StepTest, types.ActionAbort, nil, nil); err != nil {
					t.Fatal(err)
				}
				<-done
				return
			}
			var gaps *db.AcceptedGaps
			approvalReason := ""
			if response == "repeat-gap" {
				gaps = &db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: coveredBy}
				approvalReason = reason
			}
			if _, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, approvalReason, gaps, managerGateSnapshot(t, manager, run.ID, types.StepTest)); err != nil {
				t.Fatalf("approval over the recorded gap refused: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			got, err := database.GetStepResult(sr.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != types.StepStatusCompleted || got.ApprovalReason != nil || got.OverrideReason != nil || got.ValidAcceptedGaps() == nil || got.StrictApprovalRefusal("") != "" {
				t.Fatalf("recovered completion invalidated the gap: %+v", got)
			}
			if !strings.Contains(got.TestOverrideReason(), "Test gap accepted") || final.Status == nil || *final.Status != string(types.RunCompleted) {
				t.Fatalf("completion lost the audit: %q, %+v", got.TestOverrideReason(), final)
			}
		})
	}
}
