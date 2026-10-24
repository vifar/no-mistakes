package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AcceptedGapCertificate(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, scenario := range []string{"accepted", "clean", "ordinary approval", "override", "wrong tested head", "head drift", "epoch drift", "config drift"} {
			if recovered && (scenario == "epoch drift" || scenario == "config drift") {
				continue
			}
			t.Run(fmt.Sprintf("recovered=%v/%s", recovered, scenario), func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				workDir := completionFixture(t, database, run)
				cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
				executor := NewExecutor(database, p, cfg, nil, nil, nil)
				// These scenarios test proof eligibility, not fixture speed. Keep
				// every validation step live until the test itself ends.
				ctx := t.Context()
				logDir := p.RunLogDir(run.ID)
				if err := os.MkdirAll(logDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				execute := func(step Step) {
					t.Helper()
					rows, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					var sr *db.StepResult
					for _, row := range rows {
						if row.StepName == step.Name() {
							sr = row
						}
					}
					if sr == nil {
						sr, err = database.InsertStepResult(run.ID, step.Name())
					}
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := executor.executeStep(ctx, step, sr, run, repo, workDir, logDir, stepExecutionState{}); err != nil {
						t.Fatal(err)
					}
				}
				execute(&mockStep{name: types.StepReview, outcome: &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}})
				testedHead := run.HeadSHA
				if scenario == "wrong tested head" {
					testedHead = "different-head"
				}
				findings := fmt.Sprintf(`{"findings":[{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive"}],"verdict":"inconclusive","tested_head_sha":%q}`, testedHead)
				var step Step = newApprovalStep(types.StepTest, findings)
				accepted := scenario != "ordinary approval" && scenario != "override" && scenario != "clean"
				if accepted {
					run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
				}
				if scenario == "override" {
					step = newOverrideVerifyingApprovalStep(types.StepTest, findings, func(*StepContext) (string, error) { return "configured command still fails", nil })
				}
				if scenario == "clean" {
					execute(&mockStep{name: types.StepTest, outcome: &StepOutcome{Findings: fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, testedHead)}})
				} else {
					sr, err := database.InsertStepResult(run.ID, types.StepTest)
					if err != nil {
						t.Fatal(err)
					}
					executor.steps = []Step{newPassStep(types.StepReview), step}
					done := make(chan error, 1)
					if recovered {
						if err := database.StartStep(sr.ID); err != nil {
							t.Fatal(err)
						}
						if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
							t.Fatal(err)
						}
						if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
							t.Fatal(err)
						}
						if scenario == "head drift" {
							if _, err := git.Run(ctx, workDir, "commit", "--allow-empty", "-m", "changed head"); err != nil {
								t.Fatal(err)
							}
							head, err := git.HeadSHA(ctx, workDir)
							if err != nil {
								t.Fatal(err)
							}
							if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
								t.Fatal(err)
							}
							run.HeadSHA = head
						}
						shipping := run.Shipping
						run, err = database.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						run.Shipping = shipping
						go func() { done <- executor.Resume(ctx, run, repo, workDir) }()
					} else {
						go func() {
							_, _, err := executor.executeStep(ctx, step, sr, run, repo, workDir, logDir, stepExecutionState{})
							done <- err
						}()
					}
					waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
					for executor.CurrentGate().Nonce == "" {
						select {
						case err := <-done:
							t.Fatalf("executor exited before approval: %v", err)
						case <-ctx.Done():
							t.Fatal("gate never registered")
						case <-time.After(time.Millisecond):
						}
					}
					if !recovered {
						switch scenario {
						case "head drift":
							if _, err := git.Run(ctx, workDir, "commit", "--allow-empty", "-m", "changed head"); err != nil {
								t.Fatal(err)
							}
							head, err := git.HeadSHA(ctx, workDir)
							if err != nil {
								t.Fatal(err)
							}
							if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
								t.Fatal(err)
							}
							run.HeadSHA = head
						case "epoch drift":
							if err := database.InvalidateStatusValidation(run.ID); err != nil {
								t.Fatal(err)
							}
						case "config drift":
							cfg.Document.Instructions = "changed while parked"
						}
					}
					var gaps *db.AcceptedGaps
					if accepted {
						gaps = &db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "Owner accepted broker gap in issue 78", CoveredBy: "broker integration tests"}
					}
					if _, err := executor.RespondWithAcceptedGaps(types.StepTest, types.ActionApprove, nil, nil, nil, nil, "", gaps, gateSnapshot(executor)); err != nil {
						t.Fatal(err)
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal("approval did not complete")
					}
				}
				run.Shipping = nil
				if recovered && scenario != "clean" {
					execute(&mockStep{name: types.StepReview, outcome: &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}})
				}
				execute(newPassStep(types.StepDocument))
				execute(newPassStep(types.StepLint))
				if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "fixture", Ref: "refs/heads/feature"}); err != nil {
					t.Fatal(err)
				}
				digest := StatusConfigDigest(cfg)
				certificate, err := database.BuildStatusCertificate(run.ID, run.HeadSHA, digest)
				if err != nil {
					t.Fatal(err)
				}
				eligible := scenario == "accepted" || scenario == "clean"
				if (certificate != nil) != eligible {
					t.Fatalf("certificate=%v, want eligible=%v", certificate, eligible)
				}
				if !eligible {
					return
				}
				var proof db.StatusCertificate
				if err := json.Unmarshal([]byte(*certificate), &proof); err != nil {
					t.Fatal(err)
				}
				epoch, err := database.StatusValidationEpoch(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				durable, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if proof.SHA != run.HeadSHA || proof.Epoch != epoch || proof.ConfigDigest != digest || durable.PushGeneration == nil || proof.PushGeneration != *durable.PushGeneration {
					t.Fatalf("incorrect certificate binding: %+v", proof)
				}
				if stale, err := database.BuildStatusCertificate(run.ID, run.HeadSHA, "different-config"); err != nil || stale != nil {
					t.Fatalf("wrong config certified: %v, %v", stale, err)
				}
				if stale, err := database.BuildStatusCertificate(run.ID, "different-head", digest); err != nil || stale != nil {
					t.Fatalf("wrong head certified: %v, %v", stale, err)
				}
				if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: run.HeadSHA}); err != nil {
					t.Fatal(err)
				}
				next, err := database.BuildStatusCertificate(run.ID, run.HeadSHA, digest)
				if err != nil || next == nil || *next == *certificate {
					t.Fatalf("push generation did not change certificate: %v, %v", next, err)
				}
				if scenario == "accepted" {
					// A gap record that no longer re-validates against the Test
					// findings (here widened by an unaccepted error finding)
					// loses the certificate even with every binding intact.
					rows, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						if row.StepName != types.StepTest {
							continue
						}
						widened := fmt.Sprintf(`{"findings":[{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive"},{"id":"broken","severity":"error","action":"auto-fix","description":"scenario failed"}],"verdict":"inconclusive","tested_head_sha":%q}`, run.HeadSHA)
						if err := database.SetStepFindings(row.ID, widened); err != nil {
							t.Fatal(err)
						}
					}
					if stale, err := database.BuildStatusCertificate(run.ID, run.HeadSHA, digest); err != nil || stale != nil {
						t.Fatalf("invalidated gap certified: %v, %v", stale, err)
					}
				}
				if err := database.InvalidateStatusValidation(run.ID); err != nil {
					t.Fatal(err)
				}
				if stale, err := database.BuildStatusCertificate(run.ID, run.HeadSHA, digest); err != nil || stale != nil {
					t.Fatalf("stale epoch certified: %v, %v", stale, err)
				}
			})
		}
	}
}
