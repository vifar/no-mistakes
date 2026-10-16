package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Keep every POST, including repeated states, so a stale failure or duplicate
// pending cannot be hidden by a fake that stores only the latest status.
type attachStatusForge struct {
	mu    sync.Mutex
	posts map[string][]string
}

func (f *attachStatusForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var payload struct{ State, Context string }
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || r.Method != http.MethodPost || payload.Context != "no-mistakes" {
		http.Error(w, "invalid status POST", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.posts[r.URL.Path] = append(f.posts[r.URL.Path], payload.State)
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (f *attachStatusForge) states(repo, sha string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts["/repos/"+repo+"/statuses/"+sha])
}

func TestPipelineAttachCommitStatus(t *testing.T) {
	for _, scenario := range []string{"same SHA", "abort resubmit", "abort only", "validated", "fails", "cancelled", "fails after push", "skipped Test", "fresh head", "plugin", "other provider", "fork", "resume", "posting fails", "remote unavailable", "unattached Push", "skipped Push"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare", ".")
			target, slug := "https://github.com/test/repo", "test/repo"
			if scenario == "fork" {
				target, slug = "https://github.com/fork/repo", "fork/repo"
				sc.Repo.ForkURL = target
			}
			gitCmd(t, dir, "config", "url."+remote+".insteadOf", target)
			gitCmd(t, dir, "remote", "set-url", "origin", "https://github.com/test/repo")
			if scenario != "fresh head" {
				gitCmd(t, dir, "push", target, head+":refs/heads/feature")
			}
			sc.Repo.URLsVerified = true
			sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
			forge := &attachStatusForge{posts: map[string][]string{}}
			server := httptest.NewServer(forge)
			defer server.Close()
			env, _ := fakeGH(t, "")
			env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL, "FAKE_CLI_PR_FACTS_LIST_JSON=[[]]")
			for k, v := range environmentEntries(t, env) {
				t.Setenv(k, v)
			}
			sc.Env = env
			publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: slug, Validated: true}
			if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
				t.Fatal(err)
			}
			sc.Run.Status = types.RunFailed
			if strings.HasPrefix(scenario, "abort") {
				sc.Run.Status = types.RunCancelled
			}
			if err := sc.DB.UpdateRunStatus(sc.Run.ID, sc.Run.Status); err != nil {
				t.Fatal(err)
			}
			push := &PushStep{}
			old := *sc
			oldReporter := commitStatusReporter(&old, &publication)
			push.ReportPublishedHeadStatus(sc, "failure", "")
			assertStates := func(want ...string) {
				t.Helper()
				if got := forge.states(slug, head); !slices.Equal(got, want) {
					t.Fatalf("status POSTs = %v, want %v", got, want)
				}
			}
			assertStates("failure")
			if scenario == "abort only" {
				push.ReportPublishedHeadStatus(sc, "failure", "")
				assertStates("failure")
				return
			}
			// Model a crash after A's failure POST but before its LastState
			// write. Recovery must really try to post again unless B retires A;
			// deduplication of A's initial failure must not mask broken ownership.
			publication.LastState = "pending"
			if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
				t.Fatal(err)
			}
			run, err := sc.DB.InsertRun(sc.Repo.ID, sc.Run.Branch, head, base)
			if err != nil {
				t.Fatal(err)
			}
			sc.Run = run
			if scenario == "unattached Push" {
				recordReviewApproval(t, sc, head)
				testgit.BindRunComparison(t, sc.DB, run, dir, "main", "", slug, "feature")
				if _, err := push.Execute(sc); err != nil {
					t.Fatal(err)
				}
				assertStates("failure")
				attempt, err := sc.DB.GetCommitStatusAttempt(run.ID)
				if err != nil || attempt != nil {
					t.Fatalf("unattached up-to-date attempt = %+v, %v", attempt, err)
				}
				return
			}
			if scenario == "plugin" {
				sc.Config.ProviderPlugins = config.ProviderPlugins{"custom": {Command: "unavailable-plugin", Hosts: []string{"github.com"}}}
			}
			if scenario == "other provider" {
				sc.ForgeContext.Provider = scm.ProviderForgejo
			}
			if scenario == "remote unavailable" {
				gitCmd(t, dir, "config", "--unset-all", "url."+remote+".insteadOf")
				gitCmd(t, dir, "config", "url."+remote+"/missing.insteadOf", target)
			}
			attaches := scenario != "fresh head" && scenario != "plugin" && scenario != "other provider" && scenario != "remote unavailable"
			if scenario == "posting fails" {
				server.Close()
			}
			var steps []pipeline.Step
			for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
				steps = append(steps, &transitionStep{name: name, execute: func(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
					if name == types.StepReview {
						if attaches && scenario != "posting fails" {
							assertStates("failure", "pending")
						} else {
							assertStates("failure")
						}
						if attaches {
							p, err := ctx.DB.GetCommitStatusPublication(ctx.Ctx, run.ID, publication)
							if err != nil || p == nil || p.Validated || p.CIEligible {
								t.Fatalf("attached publication = %+v, %v; want unvalidated and CI-ineligible", p, err)
							}
							// A second attach, as on recovery, must preserve LastState and post no duplicate.
							push.AttachPublishedHeadStatus(ctx)
							retired, err := ctx.DB.GetCommitStatusPublication(ctx.Ctx, old.Run.ID, publication)
							if err != nil || retired != nil {
								t.Fatalf("old publication still owns status: %+v, %v", retired, err)
							}
							oldReporter(context.Background(), "failure", "", old.Log)
							push.ReportPublishedHeadStatus(&old, "failure", "")
							if scenario != "posting fails" {
								assertStates("failure", "pending")
							}
						}
						switch scenario {
						case "same SHA", "abort resubmit", "fails":
							return nil, errors.New("validation failed")
						case "cancelled":
							return nil, errors.New(types.RunCancelReasonAbortedByUser)
						}
					}
					out := &pipeline.StepOutcome{}
					if name == types.StepReview {
						out.ReviewApprovedHeadSHA = head
					}
					if name == types.StepTest {
						out.Findings = fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, head)
						out.Skipped = scenario == "skipped Test"
					}
					return out, nil
				}})
			}
			steps = append(steps, push)
			if scenario == "fails after push" {
				steps = append(steps, &transitionStep{name: types.StepCI, execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) { return nil, errors.New("CI failed") }})
			}
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			executor := pipeline.NewExecutor(sc.DB, p, sc.Config, sc.Agent, steps, nil)
			executor.SetForgeContext(sc.ForgeContext)
			guard := func(ctx *pipeline.StepContext, _ types.StepName) (pipeline.PRContextDecision, error) {
				candidate, err := testgit.RunComparisonCandidate(ctx.Run, dir, "main", base, "", slug, "feature")
				if err != nil {
					return pipeline.PRContextDecision{}, err
				}
				_, err = ctx.DB.AdvanceRunPRContext(run.ID, candidate)
				return pipeline.PRContextDecision{Target: pipeline.PRTargetSelection{SourceRepo: slug, SourceBranch: "feature", TargetBranch: "main"}}, err
			}
			executor.SetPRContextGuard(guard)
			if scenario == "remote unavailable" || scenario == "skipped Push" {
				executor.SetSkippedSteps([]types.StepName{types.StepPush})
			}
			if scenario == "resume" {
				// Model a gate persisted across daemon restart. Resume must attach before
				// it consumes the accepted response and enters the remaining steps.
				steps = append([]pipeline.Step{&transitionStep{name: types.StepRebase, execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) { return &pipeline.StepOutcome{}, nil }}}, steps...)
				var gate *db.StepResult
				for _, step := range steps {
					sr, err := sc.DB.InsertStepResult(run.ID, step.Name())
					if err != nil {
						t.Fatal(err)
					}
					if step.Name() == types.StepRebase {
						gate = sr
					}
				}
				findings := `{"findings":[],"summary":"resume"}`
				if err = sc.DB.StartStep(gate.ID); err != nil {
					t.Fatal(err)
				}
				if err = sc.DB.SetStepFindings(gate.ID, findings); err != nil {
					t.Fatal(err)
				}
				if _, err = sc.DB.InsertStepRound(gate.ID, 1, "initial", &findings, nil, 0); err != nil {
					t.Fatal(err)
				}
				if err = sc.DB.ParkStepForApproval(run.ID, gate.ID, types.StepStatusAwaitingApproval, 0, 0, &findings); err != nil {
					t.Fatal(err)
				}
				if err = sc.DB.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				if err = sc.DB.SetRunAwaitingAgent(run.ID); err != nil {
					t.Fatal(err)
				}
				stored, err := sc.DB.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				*run = *stored
				executor = pipeline.NewExecutor(sc.DB, p, sc.Config, sc.Agent, steps, nil)
				executor.SetForgeContext(sc.ForgeContext)
				executor.SetPRContextGuard(guard)
				if err = executor.RegisterRecoveredGate(run); err != nil {
					t.Fatal(err)
				}
				if err = executor.Respond(types.StepRebase, types.ActionApprove, nil, gateSnapshot(executor)); err != nil {
					t.Fatal(err)
				}
				err = executor.Resume(context.Background(), run, sc.Repo, dir)
			} else {
				err = executor.Execute(context.Background(), run, sc.Repo, dir)
			}
			wantError := scenario == "same SHA" || scenario == "abort resubmit" || scenario == "fails" || scenario == "cancelled" || scenario == "fails after push"
			if (err != nil) != wantError {
				t.Fatalf("run error = %v, wantError %v", err, wantError)
			}
			switch scenario {
			case "plugin", "other provider", "remote unavailable", "posting fails":
				assertStates("failure")
			case "same SHA", "abort resubmit", "fails", "cancelled", "skipped Test", "skipped Push":
				assertStates("failure", "pending", "failure")
			case "fails after push":
				assertStates("failure", "pending", "success", "failure")
			default:
				assertStates("failure", "pending", "success")
			}
		})
	}
}

// Immutable promotions verify a published source without taking the ordinary
// publication path. They must still settle an owned attachment using this run's
// exact-head gate evidence, without creating a status for an unattached run.
func TestImmutablePromotionAttachCommitStatus(t *testing.T) {
	for _, scenario := range []string{"validated", "skipped Test", "unvalidated Test", "failed Lint", "unattached"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare", ".")
			target := "https://github.com/test/repo"
			gitCmd(t, dir, "config", "url."+remote+".insteadOf", target)
			gitCmd(t, dir, "push", target, "main", "feature")
			sc.Repo.URLsVerified = true
			sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
			run, err := sc.DB.InsertRunWithShipping(sc.Repo.ID, "refs/heads/feature", head, base, nil, "", "", "", "main", false, nil,
				&types.ShippingOptions{Mode: types.ShippingImmutableSource, TargetBranch: "main", TargetSHA: base})
			if err != nil {
				t.Fatal(err)
			}
			sc.Run = run
			forge := &attachStatusForge{posts: map[string][]string{}}
			server := httptest.NewServer(forge)
			t.Cleanup(server.Close)
			env, _ := fakeGH(t, "")
			sc.Env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
			assertStates := func(want ...string) {
				t.Helper()
				if got := forge.states("test/repo", head); !slices.Equal(got, want) {
					t.Fatalf("status POSTs = %v, want %v", got, want)
				}
			}
			push := &PushStep{}
			if scenario != "unattached" {
				push.AttachPublishedHeadStatus(sc)
				assertStates("pending")
			}
			comparison, err := sc.DB.CaptureReviewComparison(run.ID, db.ReviewComparison{
				SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", TargetSHA: base,
				HeadSHA: head, MergeBaseSHA: base, DiffDigest: strings.Repeat("b", 64),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
				step, err := sc.DB.InsertStepResult(run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				if name == types.StepReview {
					err = sc.DB.CompleteReviewStep(step.ID, run.ID, head, 0, 1, "", comparison.Generation)
				} else {
					status := types.StepStatusCompleted
					if name == types.StepTest {
						testedHead := head
						if scenario == "unvalidated Test" {
							testedHead = base
						}
						if err := sc.DB.SetStepFindings(step.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, testedHead)); err != nil {
							t.Fatal(err)
						}
						if scenario == "skipped Test" {
							status = types.StepStatusSkipped
						}
					}
					if name == types.StepLint && scenario == "failed Lint" {
						status = types.StepStatusFailed
					}
					err = sc.DB.UpdateStepStatusWithDuration(step.ID, status, 1)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := push.Execute(sc); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "validated":
				assertStates("pending", "success")
			case "unattached":
				assertStates()
			default:
				assertStates("pending")
			}
			sc.Run.Status = types.RunCompleted
			if scenario == "failed Lint" {
				sc.Run.Status = types.RunFailed
			}
			if err := sc.DB.UpdateRunStatus(run.ID, sc.Run.Status); err != nil {
				t.Fatal(err)
			}
			if sc.Run.Status == types.RunFailed {
				push.ReportPublishedHeadStatus(sc, "failure", "")
			} else {
				push.ReportPublishedHeadStatus(sc, "success", "")
			}
			switch scenario {
			case "validated":
				assertStates("pending", "success")
			case "unattached":
				assertStates()
				attempt, err := sc.DB.GetCommitStatusAttempt(run.ID)
				if err != nil || attempt != nil {
					t.Fatalf("unattached immutable attempt = %+v, %v", attempt, err)
				}
			default:
				assertStates("pending", "failure")
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("immutable local head changed: %s", got)
			}
			if got := gitCmd(t, dir, "status", "--porcelain"); got != "" {
				t.Fatalf("immutable worktree changed: %s", got)
			}
			if got := gitCmd(t, remote, "rev-parse", "refs/heads/feature"); got != head {
				t.Fatalf("immutable remote head changed: %s", got)
			}
		})
	}
}
