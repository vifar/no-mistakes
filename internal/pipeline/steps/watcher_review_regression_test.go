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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
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

func TestWatcherTargetRefReadCancellation(t *testing.T) {
	for _, mode := range []string{"ambiguous certified signal", "missing proof", "joined failure", "independent Git failure", "independent signal", "independent SIGTERM", "context cancellation before ref launch", "context cancellation before ref launch missing proof"} {
		t.Run(mode, func(t *testing.T) {
			sc, databasePath, publication := strictStatusContext(t)
			comparison, err := sc.DB.CaptureReviewComparison(sc.Run.ID, db.ReviewComparison{SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", TargetSHA: sc.Run.BaseSHA, HeadSHA: sc.Run.HeadSHA, MergeBaseSHA: sc.Run.BaseSHA, DiffDigest: strings.Repeat("b", 64)})
			if err != nil {
				t.Fatal(err)
			}
			records, err := sc.DB.GetStepsByRun(sc.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := sc.DB.CompleteReviewStep(records[0].ID, sc.Run.ID, sc.Run.HeadSHA, 0, 1, "", comparison.Generation); err != nil {
				t.Fatal(err)
			}
			control, err := sql.Open("sqlite", databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			if _, err := control.Exec(`UPDATE review_comparisons SET approved=1 WHERE run_id=?`, sc.Run.ID); err != nil {
				t.Fatal(err)
			}
			prURL := "https://github.com/test/repo/pull/42"
			if err := sc.DB.UpdateRunPRURL(sc.Run.ID, prURL); err != nil {
				t.Fatal(err)
			}
			sc.Run.PRURL = &prURL
			sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main"}
			var mu sync.Mutex
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p struct{ State string }
				json.NewDecoder(r.Body).Decode(&p)
				mu.Lock()
				states = append(states, p.State)
				mu.Unlock()
				w.WriteHeader(201)
			}))
			defer server.Close()
			env, _ := fakeGHWithBase(t, prURL, "main")
			env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL, "FAKE_CLI_PR_STATE=OPEN")
			for key, value := range environmentEntries(t, env) {
				t.Setenv(key, value)
			}
			(&PushStep{}).ReportPublishedHeadStatus(sc, "success", "")
			if mode == "missing proof" || mode == "context cancellation before ref launch missing proof" {
				if err := sc.DB.InvalidateStatusValidation(sc.Run.ID); err != nil {
					t.Fatal(err)
				}
			}
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			entered := filepath.Join(bin, "entered")
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ] && [ \"$2\" = --exit-code ]; then\n: > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
			if mode == "independent Git failure" {
				script = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ] && [ \"$2\" = --exit-code ]; then\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nexec %q ls-remote --exit-code %q \"$4\"\nfi\nexec %q \"$@\"\n", entered, realGit, t.TempDir(), realGit)
			}
			if mode == "independent signal" || mode == "independent SIGTERM" {
				script = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ] && [ \"$2\" = --exit-code ]; then\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ci := (&CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return sc.Run.BaseSHA, true })
			var readErr error
			wrapper := &transitionStep{name: types.StepCI, execute: func(s *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				var out *pipeline.StepOutcome
				var err error
				if strings.HasPrefix(mode, "context cancellation before ref launch") {
					cancel(errors.New(types.RunCancelReasonAbortedByUser))
					_, err = shipping.RemoteHead(s.Ctx, s.WorkDir, s.Repo.UpstreamURL, "main")
				} else {
					out, err = ci.Execute(s)
				}
				readErr = err
				if mode == "joined failure" {
					err = errors.Join(err, errors.New("independent failure https://user:private-token@example.com/repo"))
				}
				return out, err
			}}
			app := paths.WithRoot(t.TempDir())
			if err := app.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{&watcherReporterStep{}, wrapper}, nil)
			executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, sc.Run, sc.Repo, sc.WorkDir) }()
			if !strings.HasPrefix(mode, "context cancellation before ref launch") {
				deadline := time.After(20 * time.Second)
				for {
					if raw, err := os.ReadFile(entered); err == nil {
						if mode != "independent Git failure" {
							break
						}
						pid, err := strconv.Atoi(string(raw))
						if err == nil && errors.Is(watcherProcessProbe(pid), syscall.ESRCH) {
							break
						}
					}
					select {
					case err := <-done:
						t.Fatalf("watcher exited before target read: %v", err)
					case <-deadline:
						t.Fatal("target Git read did not start")
					case <-time.After(10 * time.Millisecond):
					}
				}
				if mode == "independent signal" || mode == "independent SIGTERM" {
					raw, err := os.ReadFile(entered)
					if err != nil {
						t.Fatal(err)
					}
					pid, err := strconv.Atoi(string(raw))
					if err != nil {
						t.Fatal(err)
					}
					process, err := os.FindProcess(pid)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "independent SIGTERM" {
						if err := process.Signal(syscall.SIGSTOP); err != nil {
							t.Fatal(err)
						}
						if err := process.Signal(syscall.SIGTERM); err != nil {
							t.Fatal(err)
						}
					} else if err := process.Kill(); err != nil {
						t.Fatal(err)
					}
				}
				cancel(errors.New(types.RunCancelReasonAbortedByUser))
				if mode == "independent SIGTERM" {
					raw, _ := os.ReadFile(entered)
					pid, _ := strconv.Atoi(string(raw))
					process, _ := os.FindProcess(pid)
					if err := process.Signal(syscall.SIGCONT); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
						t.Fatal(err)
					}
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("watcher did not cancel")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("watcher did not settle")
			}
			if mode == "independent Git failure" {
				var exit *exec.ExitError
				if !errors.As(readErr, &exit) || exit.ExitCode() != 128 || !errors.Is(readErr, context.Canceled) {
					t.Errorf("independent Git exit lost identity: %v", readErr)
				}
			} else if mode == "independent signal" || mode == "independent SIGTERM" {
				var exit *exec.ExitError
				if !errors.As(readErr, &exit) || !errors.Is(readErr, context.Canceled) {
					t.Errorf("independent signal lost failure identity: %v", readErr)
				}
				if exit != nil {
					status, ok := exit.Sys().(syscall.WaitStatus)
					wanted := syscall.SIGKILL
					if mode == "independent SIGTERM" {
						wanted = syscall.SIGTERM
					}
					if !ok || !status.Signaled() || status.Signal() != wanted {
						t.Errorf("independent leader exit = %v", exit)
					}
				}
			} else if !errors.Is(readErr, context.Canceled) {
				t.Errorf("target read cancellation provenance = %v", readErr)
			}
			if strings.HasPrefix(mode, "context cancellation before ref launch") {
				if _, err := os.Stat(entered); !os.IsNotExist(err) {
					t.Errorf("cancelled context launched a Git process: %v", err)
				}
			}
			if err := sc.DB.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := db.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			run, err := reopened.GetRun(sc.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := types.RunFailed
			if strings.HasPrefix(mode, "context cancellation before ref launch") {
				wantStatus = types.RunCancelled
			}
			if run.Status != wantStatus {
				t.Errorf("durable status = %s, want %s", run.Status, wantStatus)
			}
			if run.Error == nil || strings.Contains(*run.Error, "private-token") {
				t.Errorf("terminal reason leaked or missing: %v", run.Error)
			}
			sc.DB, sc.Run = reopened, run
			for _, requested := range []string{"pending", "success", "failure"} {
				(&PushStep{}).ReportPublishedHeadStatus(sc, requested, "")
			}
			executor.ReportRecoveredPublishedHeadStatus(context.Background(), run, sc.Repo)
			stored, err := reopened.GetCommitStatusPublication(context.Background(), run.ID, publication)
			if err != nil {
				t.Fatal(err)
			}
			eligible := mode == "context cancellation before ref launch"
			if (stored.PreserveSuccess != nil && *stored.PreserveSuccess) != eligible {
				t.Errorf("preserved status = %+v", stored)
			}
			mu.Lock()
			defer mu.Unlock()
			want := "[success failure]"
			if eligible {
				want = "[success]"
			}
			if fmt.Sprint(states) != want {
				t.Errorf("HTTP statuses = %v, want %s", states, want)
			}
		})
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

func TestWatcherImmutableRefErrorsRetainIdentity(t *testing.T) {
	for _, check := range []string{"source", "target"} {
		for _, mode := range []string{"ambiguous signal", "independent signal", "cancelled context"} {
			t.Run(check+"/"+mode, func(t *testing.T) {
				sc, _, _ := strictStatusContext(t)
				sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingImmutableSource, TargetBranch: "main", TargetSHA: sc.Run.BaseSHA}
				realGit, err := testgit.RealGit()
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				entered := filepath.Join(bin, "entered")
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ]; then\n: > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
				if mode == "independent signal" {
					script = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ]; then\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
				}
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				if mode == "cancelled context" {
					cancel()
				}
				go func() {
					if check == "source" {
						done <- shipping.CheckImmutableSource(ctx, sc.Run, sc.Repo, sc.WorkDir)
					} else {
						done <- shipping.CheckImmutableTarget(ctx, sc.Run, sc.Repo, sc.WorkDir, shipping.RemoteHead)
					}
				}()
				if mode != "cancelled context" {
					deadline := time.After(10 * time.Second)
					for {
						if _, err := os.Stat(entered); err == nil {
							break
						}
						select {
						case err := <-done:
							t.Fatalf("ref check exited early: %v", err)
						case <-deadline:
							t.Fatal("ref read never started")
						case <-time.After(10 * time.Millisecond):
						}
					}
					if mode == "independent signal" {
						raw, err := os.ReadFile(entered)
						if err != nil {
							t.Fatal(err)
						}
						pid, err := strconv.Atoi(string(raw))
						if err != nil {
							t.Fatal(err)
						}
						process, err := os.FindProcess(pid)
						if err != nil {
							t.Fatal(err)
						}
						if err := process.Kill(); err != nil {
							t.Fatal(err)
						}
					}
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("%s ref error lost cancellation identity: %v", check, err)
					}
					var exit *exec.ExitError
					if mode != "cancelled context" && !errors.As(err, &exit) {
						t.Errorf("%s ref error lost independent command evidence: %v", check, err)
					}
					if mode != "cancelled context" && err.Error() != "observe shipping ref: remote observation failed" {
						t.Errorf("ref error changed redacted rendering: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("ref check did not cancel")
				}
			})
		}
	}
}

func watcherProcessProbe(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	err = process.Signal(syscall.Signal(0))
	if errors.Is(err, os.ErrProcessDone) {
		return syscall.ESRCH
	}
	return err
}
