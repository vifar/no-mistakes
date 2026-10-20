package steps

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestWatcherStrictProofRequiresExecutedPass(t *testing.T) {
	for _, mode := range []string{"approve", "recovered approve", "reconciled", "recovered reconciled", "pass", "revalidated repair"} {
		for _, gated := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
			if (mode == "pass" || mode == "revalidated repair") && gated != types.StepReview {
				continue
			}
			t.Run(mode+"/"+string(gated), func(t *testing.T) {
				sc, databasePath, publication := strictStatusContext(t)
				control, err := sql.Open("sqlite", databasePath)
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				if _, err := control.Exec(`DELETE FROM status_validation_steps`); err != nil {
					t.Fatal(err)
				}
				if _, err := control.Exec(`DELETE FROM step_results`); err != nil {
					t.Fatal(err)
				}
				remote := t.TempDir()
				gitCmd(t, remote, "init", "--bare")
				gitCmd(t, sc.WorkDir, "config", "url."+remote+".insteadOf", sc.Repo.UpstreamURL)
				sc.Repo.URLsVerified = true
				bindComparison := func(head string) {
					digest := sha256.Sum256([]byte(gitCmd(t, sc.WorkDir, "diff", "--binary", sc.Run.BaseSHA, head)))
					if _, err := sc.DB.BindRunPRContext(sc.Run.ID, db.PRContextCandidate{
						SourceRepo: "test/repo", SourceBranch: sc.Run.Branch, LocalHeadSHA: head,
						TargetBranch: "main", TargetSHA: sc.Run.BaseSHA, MergeBaseSHA: sc.Run.BaseSHA,
						DiffDigest: hex.EncodeToString(digest[:]),
					}, types.StepRebase); err != nil {
						t.Fatal(err)
					}
				}
				bindComparison(sc.Run.HeadSHA)

				var mu sync.Mutex
				var states []string
				var requestPaths []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload struct{ State string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					mu.Lock()
					states = append(states, payload.State)
					requestPaths = append(requestPaths, r.URL.Path)
					mu.Unlock()
					w.WriteHeader(201)
				}))
				defer server.Close()
				env, _ := fakeGH(t, "")
				env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL, "FAKE_CLI_PR_FACTS_LIST_JSON=[]")
				for key, value := range environmentEntries(t, env) {
					t.Setenv(key, value)
				}
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				gateReady := make(chan struct{}, 1)
				eligible := mode == "pass" || mode == "revalidated repair"
				calls := map[types.StepName]int{}
				var steps []pipeline.Step
				for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
					step := &transitionStep{name: name, execute: func(s *pipeline.StepContext) (*pipeline.StepOutcome, error) {
						calls[name]++
						out := &pipeline.StepOutcome{}
						if name == types.StepReview {
							out.ReviewApprovedHeadSHA = s.Run.HeadSHA
						}
						if name == types.StepTest {
							out.Findings = fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, s.Run.HeadSHA)
						}
						if !eligible && name == gated {
							out.NeedsApproval = true
							out.Findings = `{"findings":[{"id":"unresolved","severity":"warning","description":"unresolved gate finding","action":"ask-user"}]}`
						}
						return out, nil
					}}
					if name == gated && strings.Contains(mode, "reconciled") {
						steps = append(steps, &watcherReconciledStep{transitionStep: step})
					} else {
						steps = append(steps, step)
					}
				}
				steps = append(steps, &PushStep{})
				ciCalls := 0
				steps = append(steps, &transitionStep{name: types.StepCI, execute: func(s *pipeline.StepContext) (*pipeline.StepOutcome, error) {
					ciCalls++
					cert, err := s.DB.BuildStatusCertificate(s.Run.ID, s.Run.HeadSHA, pipeline.StatusConfigDigest(s.Config))
					if err != nil {
						return nil, err
					}
					if (cert != nil) != eligible {
						t.Errorf("strict certificate eligibility = %v, want %v", cert != nil, eligible)
					}
					if !eligible {
						var bindings int
						if err := control.QueryRow(`SELECT COUNT(*) FROM status_validation_steps v JOIN step_results s ON s.id=v.step_id WHERE s.run_id=? AND s.step_name=?`, s.Run.ID, gated).Scan(&bindings); err != nil {
							return nil, err
						}
						if bindings != 0 {
							t.Errorf("approved %s produced %d fresh bindings", gated, bindings)
						}
					}
					if mode == "revalidated repair" && ciCalls == 1 {
						if err := s.DB.InvalidateStatusValidation(s.Run.ID); err != nil {
							return nil, err
						}
						gitCmd(t, s.WorkDir, "commit", "--allow-empty", "-m", "repair")
						s.Run.HeadSHA = gitCmd(t, s.WorkDir, "rev-parse", "HEAD")
						if err := s.DB.UpdateRunHeadSHA(s.Run.ID, s.Run.HeadSHA); err != nil {
							return nil, err
						}
						bindComparison(s.Run.HeadSHA)
						return &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
					}
					cancel(errors.New(types.RunCancelReasonAbortedByUser))
					return nil, s.Ctx.Err()
				}})
				app := paths.WithRoot(t.TempDir())
				if err := app.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, steps, func(e ipc.Event) {
					if e.Type == ipc.EventStepCompleted && e.StepName != nil && *e.StepName == gated && e.Status != nil && *e.Status == string(types.StepStatusAwaitingApproval) {
						gateReady <- struct{}{}
					}
				})
				executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
				executor.SetGateReconcileTimings(time.Millisecond, time.Second)
				recovered := strings.HasPrefix(mode, "recovered ")
				if recovered {
					for _, step := range steps {
						sr, err := sc.DB.InsertStepResult(sc.Run.ID, step.Name())
						if err != nil {
							t.Fatal(err)
						}
						if step.Name().Order() < gated.Order() {
							if err := sc.DB.CompleteStep(sr.ID, 0, 1, ""); err != nil {
								t.Fatal(err)
							}
							if step.Name() == types.StepTest {
								if err := sc.DB.SetStepFindings(sr.ID, fmt.Sprintf(`{"tested_head_sha":%q}`, sc.Run.HeadSHA)); err != nil {
									t.Fatal(err)
								}
							}
							if err := sc.DB.RecordStatusValidationStep(sr.ID, sc.Run.ID, sc.Run.HeadSHA, pipeline.StatusConfigDigest(sc.Config), 0); err != nil {
								t.Fatal(err)
							}
						}
						if step.Name() == gated {
							if err := sc.DB.StartStep(sr.ID); err != nil {
								t.Fatal(err)
							}
							findings := `{"findings":[{"id":"unresolved","severity":"warning","description":"unresolved gate finding","action":"ask-user"}]}`
							if _, err := sc.DB.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, sc.Run.HeadSHA, 1); err != nil {
								t.Fatal(err)
							}
							if err := sc.DB.ParkStepForApproval(sc.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
								t.Fatal(err)
							}
						}
					}
					sc.Run, err = sc.DB.GetRun(sc.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				go func() {
					if recovered {
						done <- executor.Resume(ctx, sc.Run, sc.Repo, sc.WorkDir)
					} else {
						done <- executor.Execute(ctx, sc.Run, sc.Repo, sc.WorkDir)
					}
				}()
				defer func() { cancel(nil) }()
				if !eligible && !strings.Contains(mode, "reconciled") {
					select {
					case <-gateReady:
					case err := <-done:
						t.Fatalf("exited before approval: %v", err)
					case <-time.After(20 * time.Second):
						t.Fatal("gate did not park")
					}
					if err := executor.Respond(gated, types.ActionApprove, nil, gateSnapshot(executor)); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("watcher did not cancel")
					}
					if context.Cause(ctx) == nil {
						t.Fatalf("watcher exited before operator cancellation: %v", err)
					}
				case <-time.After(20 * time.Second):
					t.Fatal("watcher did not settle")
				}
				if mode == "revalidated repair" {
					for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
						if calls[name] != 2 {
							t.Errorf("%s executions = %d, want 2", name, calls[name])
						}
					}
				}
				publication.SHA = sc.Run.HeadSHA
				if err := sc.DB.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := db.Open(databasePath)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				run, err := reopened.GetRun(sc.Run.ID)
				if err != nil || run.Status != types.RunCancelled {
					t.Fatalf("durable cancellation = %+v, %v", run, err)
				}
				sc.DB, sc.Run = reopened, run
				for _, requested := range []string{"pending", "success", "failure"} {
					(&PushStep{}).ReportPublishedHeadStatus(sc, requested, "")
				}
				executor.ReportRecoveredPublishedHeadStatus(context.Background(), run, sc.Repo)
				log, err := os.ReadFile(filepath.Join(app.RunLogDir(run.ID), "ci.log"))
				if err != nil || strings.Contains(string(log), "private-token") {
					t.Errorf("redacted step log contract failed: %v", err)
				}
				stored, err := reopened.GetCommitStatusPublication(context.Background(), run.ID, publication)
				if err != nil || stored == nil {
					t.Fatalf("publication = %+v, %v", stored, err)
				}
				preserved := stored.PreserveSuccess != nil && *stored.PreserveSuccess
				if preserved != eligible || (stored.StrictCertificate != nil) != eligible {
					t.Errorf("preservation/certificate = %+v, want eligible %v", stored, eligible)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(states) == 0 {
					t.Fatal("no actual HTTP status calls")
				}
				want := "failure"
				if eligible {
					want = "success"
				}
				var currentStates []string
				for i, path := range requestPaths {
					if strings.HasSuffix(path, "/"+publication.SHA) {
						currentStates = append(currentStates, states[i])
					}
				}
				if eligible && fmt.Sprint(currentStates) != "[pending success]" {
					t.Errorf("certified current head received replacing HTTP calls: %v", currentStates)
				}
				if len(currentStates) == 0 || currentStates[len(currentStates)-1] != want {
					t.Errorf("HTTP statuses = %v, want last %s", states, want)
				}
			})
		}
	}
}

type watcherReporterStep struct{ PushStep }

func (*watcherReporterStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{}, nil
}

type watcherReconciledStep struct{ *transitionStep }

func (*watcherReconciledStep) ReconcileApprovalGate(*pipeline.StepContext) (bool, error) {
	return true, nil
}
