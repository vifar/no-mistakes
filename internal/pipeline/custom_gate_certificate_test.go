package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_CustomGateApprovalDoesNotCertifyUnresolvedErrors(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, severity := range []string{"error", "warning"} {
			t.Run(fmt.Sprintf("recovered_%v/%s", recovered, severity), func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				workDir := completionFixture(t, database, run)
				name := types.CustomGateStepName(types.StepLint, "budget")
				findings := fmt.Sprintf(`{"findings":[{"id":"budget","severity":%q,"description":"budget condition","action":"ask-user"}]}`, severity)
				step := newApprovalStep(name, findings)
				cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
				executor := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
				if recovered {
					if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					sr, err := database.InsertStepResult(run.ID, name)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.StartStep(sr.ID); err != nil {
						t.Fatal(err)
					}
					if err := database.SetStepFindings(sr.ID, findings); err != nil {
						t.Fatal(err)
					}
					if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
						t.Fatal(err)
					}
					if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
						t.Fatal(err)
					}
					if err := database.SetRunAwaitingAgent(run.ID); err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					if recovered {
						done <- executor.Resume(ctx, run, repo, workDir)
					} else {
						done <- executor.Execute(ctx, run, repo, workDir)
					}
				}()
				deadline := time.Now().Add(5 * time.Second)
				for {
					if err := executor.Respond(name, types.ActionApprove, nil, gateSnapshot(executor)); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("custom gate did not accept ordinary approval")
					}
					time.Sleep(10 * time.Millisecond)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("approved gate did not complete")
				}
				rows, err := database.GetStepsByRun(run.ID)
				if err != nil || len(rows) != 1 {
					t.Fatalf("approved steps: %+v, %v", rows, err)
				}
				if rows[0].Status != types.StepStatusCompleted || rows[0].FindingsJSON == nil || rows[0].OverrideReason != nil || rows[0].ApprovalReason != nil {
					t.Fatalf("ordinary approval changed: %+v", rows[0])
				}
				if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, run.HeadSHA); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature"}); err != nil {
					t.Fatal(err)
				}
				epoch, err := database.StatusValidationEpoch(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, core := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
					sr, err := database.InsertStepResult(run.ID, core)
					if err != nil {
						t.Fatal(err)
					}
					if core == types.StepTest {
						if err := database.SetStepFindings(sr.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, run.HeadSHA)); err != nil {
							t.Fatal(err)
						}
					}
					if err := database.CompleteStep(sr.ID, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
					if err := database.RecordStatusValidationStep(sr.ID, run.ID, run.HeadSHA, StatusConfigDigest(cfg), epoch); err != nil {
						t.Fatal(err)
					}
				}
				certificate, err := database.BuildStatusCertificate(run.ID, run.HeadSHA, StatusConfigDigest(cfg))
				if err != nil {
					t.Fatal(err)
				}
				if (certificate != nil) != (severity == "warning" && !recovered) {
					t.Fatalf("approved %s gate certification=%v", severity, certificate != nil)
				}
			})
		}
	}
}
