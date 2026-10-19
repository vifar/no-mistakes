package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_CustomGateExecutionProofTracksRestarts(t *testing.T) {
	for _, scenario := range []string{"clean", "warning", "formatter review restart", "epoch drift", "config drift", "head during gate", "epoch during gate", "config during gate"} {
		t.Run(scenario, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := completionFixture(t, database, run)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
			executor := NewExecutor(database, p, cfg, nil, nil, nil)
			name := types.CustomGateStepName(types.StepRebase, "budget")
			sr, err := database.InsertStepResult(run.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			format := func() {
				if err := os.WriteFile(filepath.Join(workDir, "formatted.txt"), []byte("formatter result"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := git.CommitAll(context.Background(), workDir, "format"); err != nil {
					t.Fatal(err)
				}
				head, err := git.HeadSHA(context.Background(), workDir)
				if err != nil {
					t.Fatal(err)
				}
				run.HeadSHA = head
				if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
					t.Fatal(err)
				}
			}
			step := &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
				switch scenario {
				case "head during gate":
					format()
				case "epoch during gate":
					if err := database.InvalidateStatusValidation(run.ID); err != nil {
						return nil, err
					}
				case "config during gate":
					cfg.Document.Instructions = "changed during gate"
				}
				findings := ""
				if scenario == "warning" {
					findings = `{"findings":[{"severity":"warning","description":"near budget"}]}`
				}
				return &StepOutcome{Findings: findings}, nil
			}}
			logDir := p.RunLogDir(run.ID)
			if err := os.MkdirAll(logDir, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if scenario == "warning" {
				done := make(chan error, 1)
				go func() {
					_, _, err := executor.executeStep(ctx, step, sr, run, repo, workDir, logDir, stepExecutionState{})
					done <- err
				}()
				deadline := time.Now().Add(5 * time.Second)
				for executor.Respond(name, types.ActionApprove, nil, gateSnapshot(executor)) != nil {
					if time.Now().After(deadline) {
						t.Fatal("warning gate did not accept approval")
					}
					time.Sleep(10 * time.Millisecond)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("warning approval did not finish")
				}
			} else if _, _, err := executor.executeStep(ctx, step, sr, run, repo, workDir, logDir, stepExecutionState{}); err != nil {
				t.Fatal(err)
			}
			originalHead := run.HeadSHA
			switch scenario {
			case "formatter review restart":
				format()
				if err := database.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
					t.Fatal(err)
				}
				retained, err := database.GetStepResult(sr.ID)
				if err != nil || retained.Status != types.StepStatusCompleted {
					t.Fatalf("after-Rebase gate not retained: %+v, %v", retained, err)
				}
				if run.HeadSHA == originalHead {
					t.Fatal("formatter did not create head B")
				}
			case "epoch drift":
				if err := database.InvalidateStatusValidation(run.ID); err != nil {
					t.Fatal(err)
				}
			case "config drift":
				cfg.Document.Instructions = "changed after gate"
			}
			digest := StatusConfigDigest(cfg)
			epoch, err := database.StatusValidationEpoch(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, run.HeadSHA); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature"}); err != nil {
				t.Fatal(err)
			}
			for _, core := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
				coreStep, err := database.InsertStepResult(run.ID, core)
				if err != nil {
					t.Fatal(err)
				}
				if core == types.StepTest {
					if err := database.SetStepFindings(coreStep.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, run.HeadSHA)); err != nil {
						t.Fatal(err)
					}
				}
				if err := database.CompleteStep(coreStep.ID, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
				if err := database.RecordStatusValidationStep(coreStep.ID, run.ID, run.HeadSHA, digest, epoch); err != nil {
					t.Fatal(err)
				}
			}
			eligible := scenario == "clean" || scenario == "warning"
			certificate, err := database.BuildStatusCertificate(run.ID, run.HeadSHA, digest)
			if err != nil {
				t.Fatal(err)
			}
			if (certificate != nil) != eligible {
				t.Errorf("%s certificate=%v", scenario, certificate != nil)
			}
			publication := db.CommitStatusPublication{SHA: run.HeadSHA, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "success", StrictCertificate: certificate}
			if err := database.PutCommitStatusPublication(run.ID, publication); err != nil {
				t.Fatal(err)
			}
			if err := database.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, run.HeadSHA, digest, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetCommitStatusPublication(context.Background(), run.ID, publication)
			if err != nil {
				t.Fatal(err)
			}
			if (stored.PreserveSuccess != nil && *stored.PreserveSuccess) != eligible {
				t.Fatalf("%s preservation=%+v", scenario, stored)
			}
		})
	}
}
