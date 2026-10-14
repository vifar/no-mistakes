package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_ProtectedPathRetryUsesPersistedRepair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		rebase     bool
		revalidate bool
		addOnly    bool
		unverified bool
	}{
		{name: "added_finding_only_publish", addOnly: true},
		{name: "added_finding_only_revalidate", addOnly: true, revalidate: true},
		{name: "rebased_conflict", rebase: true},
		{name: "unverified_rewrite_stays_refused", rebase: true, unverified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCIRepairFixture(t, tc.revalidate, nil)
			if tc.rebase {
				gitCmd(t, f.dir, "checkout", "main")
				if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("base change\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", "feature.txt")
				gitCmd(t, f.dir, "commit", "-m", "advance conflicting base")
				gitCmd(t, f.dir, "push", "origin", "main")
				gitCmd(t, f.dir, "checkout", "feature")
				f.sctx.Env = append(fakeCIGHMergeable(t, "OPEN", `[]`, "CONFLICTING"),
					"FAKE_CLI_PR_HEAD_SHA="+f.headSHA,
					`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`,
				)
			}
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if tc.rebase {
					if _, err := stepGitRun(f.sctx, "rebase", "main"); err == nil {
						t.Error("fixture rebase must conflict")
					}
					if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("base change\nresolved feature\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, f.dir, "add", "feature.txt")
					gitCmd(t, f.dir, "-c", "core.editor=true", "rebase", "--continue")
				}
				if err := os.WriteFile(filepath.Join(f.dir, "package.lock"), []byte("staged lock\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", "package.lock")
				for file, content := range map[string]string{"package.lock": "refused lock\n", "fix.go": "retained repair\n"} {
					if err := os.WriteFile(filepath.Join(f.dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"repair CI","code_change_needed":true,"findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
			}}
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
			}
			if got := gitCmd(t, f.dir, "show", ":package.lock"); got != "staged lock" {
				t.Fatalf("refusal changed index: %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(f.dir, "package.lock")); err != nil || string(got) != "refused lock\n" {
				t.Fatalf("refusal changed working file: %q, %v", got, err)
			}
			for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepPush} {
				sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.sctx.DB.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
			}
			persistCIRefusal(t, f, outcome)
			gitCmd(t, f.dir, "restore", "--source=HEAD", "--staged", "--worktree", "--", "package.lock")
			if tc.unverified {
				gitCmd(t, f.dir, "update-ref", "HEAD", "main")
			}
			f.sctx.Config.Commands.Test = "git cat-file -e HEAD:fix.go"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			green := append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`),
				// attestHeadBeforePush discovers the PR via FindPR before every
				// publish, matching the persisted PRURL the fixture set up.
				`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`,
			)
			ci := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { cancel(); return ctx.Err() }}
			// Push now also attests the PR's pipeline before every push (see
			// attestHeadBeforePush), so it needs the same fake gh reachability
			// CI already gets here - real production code shares one process
			// environment across every step, but the executor this test drives
			// resets StepContext.Env per step and this fixture only injects it
			// explicitly via reconcileEnvStep.
			steps := []pipeline.Step{
				reconcileEnvStep{step: &ReviewStep{}, env: green},
				reconcileEnvStep{step: &TestStep{}, env: green},
				reconcileEnvStep{step: &PushStep{}, env: green},
				reconcileEnvStep{step: ci, env: green},
			}
			reviews := 0
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				findings := cleanReviewFindings()
				if strings.HasPrefix(opts.Purpose, "review") {
					reviews++
					findings.ReviewedPaths = fullReviewCoverage(t, f.dir, f.sctx.Run.BaseSHA)
				}
				output, err := json.Marshal(findings)
				return &agent.Result{Output: output}, err
			}}
			parked := make(chan struct{}, 1)
			gates := 0
			executor := pipeline.NewExecutor(f.sctx.DB, paths.WithRoot(t.TempDir()), f.sctx.Config, ag, steps, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
					gates++
					if gates == 1 {
						parked <- struct{}{}
					} else {
						cancel()
					}
				}
			})
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- executor.Resume(ctx, run, f.sctx.Repo, f.dir) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Error("executor did not stop")
				}
			})
			select {
			case <-parked:
			case err := <-done:
				done <- err
				t.Fatalf("could not resume refusal: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("refusal did not resume")
			}
			refused, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(refused.Items) < 1 || refused.Items[0].ID != "protected-path-refusal" {
				t.Fatalf("invalid refusal: %+v, %v", refused, err)
			}
			selected := []string{refused.Items[0].ID}
			var added []types.Finding
			if tc.addOnly {
				selected = []string{}
				added = []types.Finding{{ID: "user-1", Severity: "info", Description: "publish the retained repair", Action: types.ActionAutoFix}}
			}
			// The parked gate carries every finding the resumed CI step
			// reported - including ones the fixture's refusal outcome did not
			// name - so decline all of them except the selection explicitly.
			// An omission is not a decline, and the daemon refuses a fix
			// response that leaves a gate finding unaccounted for.
			selectedSet := make(map[string]bool, len(selected))
			for _, id := range selected {
				selectedSet[id] = true
			}
			var ignored []string
			gateSteps, gateErr := f.sctx.DB.GetStepsByRun(run.ID)
			if gateErr != nil {
				t.Fatal(gateErr)
			}
			for _, sr := range gateSteps {
				if sr.StepName != types.StepCI || sr.FindingsJSON == nil {
					continue
				}
				gate, err := types.ParseFindingsJSON(*sr.FindingsJSON)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range gate.Items {
					if item.ID != "" && !selectedSet[item.ID] {
						ignored = append(ignored, item.ID)
					}
				}
			}
			if _, err := executor.RespondWithOverrides(types.StepCI, types.ActionFix, selected, ignored, nil, added, ""); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				done <- err
			// Rebased repairs run Review, Test, attestation, and Push before
			// monitoring resumes. Allow for Windows Git process startup costs.
			case <-time.After(60 * time.Second):
				t.Fatal("explicit retry did not finish")
			}
			local, remote := f.localHead(t), f.remoteHead(t)
			if tc.unverified {
				if gates != 2 || reviews != 0 || remote != f.headSHA || !strings.Contains(gitStatusPorcelain(t, f.dir), "fix.go") {
					t.Fatalf("unverified head escaped refusal: gates=%d reviews=%d remote=%s dirty=%q error=%v", gates, reviews, remote, gitStatusPorcelain(t, f.dir), err)
				}
				t.Logf("unverified retry: gates=%d reviews=%d remote=%s status=%q", gates, reviews, remote, gitStatusPorcelain(t, f.dir))
				return
			}
			if gates != 1 || remote == f.headSHA || remote != local || gitStatusPorcelain(t, f.dir) != "" {
				t.Fatalf("retry stranded retained repair: gates=%d local=%s remote=%s dirty=%q error=%v", gates, local, remote, gitStatusPorcelain(t, f.dir), err)
			}
			if got := gitCmd(t, f.upstream, "show", "refs/heads/feature:fix.go"); got != "retained repair" {
				t.Fatalf("publication lost retained repair: %q", got)
			}
			wantReviews := 0
			if tc.rebase || tc.revalidate {
				wantReviews = 1
			}
			if reviews != wantReviews {
				t.Fatalf("review executions = %d, want %d", reviews, wantReviews)
			}
			results, err := f.sctx.DB.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wantReviews == 1 {
				for _, sr := range results[:3] {
					if sr.Status != types.StepStatusCompleted || sr.StartedAt == nil {
						t.Fatalf("repair skipped required %s: %+v", sr.StepName, sr)
					}
					t.Logf("persisted revalidation: step=%s status=%s started_at=%d", sr.StepName, sr.Status, *sr.StartedAt)
				}
			}
			t.Logf("retained CI repair publication: before=%s local=%s remote=%s remote:fix.go=%q status=%q", f.headSHA, local, remote, gitCmd(t, f.upstream, "show", "refs/heads/feature:fix.go"), gitStatusPorcelain(t, f.dir))
		})
	}
}

func persistCIRefusal(t *testing.T, f *ciRepairFixture, outcome *pipeline.StepOutcome) {
	t.Helper()
	if f.sctx.StepResultID == "" {
		sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
		if err != nil {
			t.Fatal(err)
		}
		f.sctx.StepResultID = sr.ID
	}
	if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.StartStep(f.sctx.StepResultID); err != nil {
		t.Fatal(err)
	}
	rounds, err := f.sctx.DB.GetRoundsByStep(f.sctx.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sctx.DB.InsertStepRound(f.sctx.StepResultID, len(rounds)+1, "initial", &outcome.Findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.ParkStepForApproval(f.sctx.Run.ID, f.sctx.StepResultID, types.StepStatusAwaitingApproval, outcome.ExitCode, 1, &outcome.Findings); err != nil {
		t.Fatal(err)
	}
}

func TestCIStep_ProtectedPathRetryFinishesRetainedRepairWithGreenChecks(t *testing.T) {
	t.Parallel()
	for _, revalidate := range []bool{false, true} {
		name := "publish"
		if revalidate {
			name = "revalidate"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			f := newCIRepairFixture(t, revalidate, func(dir string) {
				calls++
				for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "retained repair\n"} {
					if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			})
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
			}
			persistCIRefusal(t, f, outcome)
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`),
				`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`)
			outcome, err = f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) || calls != 1 || f.localHead(t) != f.headSHA {
				t.Fatalf("unresolved retry bypassed refusal or reran fixer: %+v, %v calls=%d", outcome, err, calls)
			}
			if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
				t.Fatal(err)
			}
			f.sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`),
				`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`)
			outcome, err = f.run(t)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("retry: %+v, %v\n%s", outcome, err, f.log())
			}
			if f.localHead(t) == f.headSHA {
				t.Fatalf("green checks bypassed retained repair: remote=%s dirty=%q\n%s", f.remoteHead(t), gitStatusPorcelain(t, f.dir), f.log())
			}
			if calls != 1 {
				t.Fatalf("retry ran another fixer over retained work: %d calls", calls)
			}
			if got := gitCmd(t, f.dir, "show", "HEAD:fix.go"); got != "retained repair" {
				t.Fatalf("commit lost retained repair: %q", got)
			}
			if revalidate {
				if outcome == nil || outcome.RestartFrom != types.StepReview || f.remoteHead(t) != f.headSHA {
					t.Fatalf("retry skipped required pipeline revalidation: %+v remote=%s", outcome, f.remoteHead(t))
				}
				if strings.Contains(f.log(), ciChecksPassedMsg) {
					t.Fatal("reported checks passed before revalidation/publication")
				}
			} else if f.remoteHead(t) != f.localHead(t) || !strings.Contains(f.log(), "CI readiness lacks a current PR comparison receipt") {
				t.Fatalf("retry did not publish and withhold unproven readiness: local=%s remote=%s\n%s", f.localHead(t), f.remoteHead(t), f.log())
			}
		})
	}
}

func TestCIStep_ProtectedPathRetryPublicationFailureKeepsRefusal(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, func(dir string) {
		for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "retained repair\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	f.sctx.Config.ProtectedPaths = []string{"*.lock"}
	outcome, err := f.run(t)
	if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
		t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
	}
	persistCIRefusal(t, f, outcome)
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = outcome.Findings
	if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
		t.Fatal(err)
	}
	f.sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`),
		`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`)
	hooks := t.TempDir()
	gitCmd(t, f.upstream, "config", "core.hooksPath", hooks)
	rejectPush := filepath.Join(hooks, "pre-receive")
	if err := os.WriteFile(rejectPush, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outcome, err = f.run(t)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
		t.Fatalf("unfinished publication lost refusal: %+v, %v\n%s", outcome, err, f.log())
	}
	persistCIRefusal(t, f, outcome)
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("retry findings: %+v, %v", findings, err)
	}
	var refusal types.Finding
	for _, finding := range findings.Items {
		if finding.File == "package.lock" {
			refusal = finding
		}
	}
	if !strings.Contains(refusal.Description, `rule "*.lock"`) {
		t.Fatalf("retry lost original path or rule: %+v", findings)
	}
	if strings.Contains(f.log(), ciChecksPassedMsg) || f.remoteHead(t) != f.headSHA {
		t.Fatal("unfinished publication advanced remote or reported checks passed")
	}
	if got, err := os.ReadFile(filepath.Join(f.dir, "fix.go")); err != nil || string(got) != "retained repair\n" {
		t.Fatalf("lost retained work: %q, %v", got, err)
	}
	f.sctx.PreviousFindings = outcome.Findings
	if err := os.Remove(rejectPush); err != nil {
		t.Fatal(err)
	}
	outcome, err = f.run(t)
	if !errors.Is(err, context.Canceled) || f.remoteHead(t) == f.headSHA || f.remoteHead(t) != f.localHead(t) {
		t.Fatalf("second explicit retry did not finish publication: %+v, %v\n%s", outcome, err, f.log())
	}
}

func TestCIStep_ProtectedPathRefusalStopsAutomaticAndManualRepair(t *testing.T) {
	t.Parallel()
	for _, manual := range []bool{false, true} {
		name := "automatic"
		if manual {
			name = "manual"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			invocations := 0
			var indexBefore string
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				invocations++
				if err := os.WriteFile(filepath.Join(dir, "package.lock"), []byte("staged lock\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "package.lock")
				indexBefore = gitCmd(t, dir, "diff", "--cached")
				for file, content := range map[string]string{"package.lock": "rejected edit\n", "fix.txt": "CI repair\n"} {
					if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"repair checks","code_change_needed":true,"findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail","app":"github-actions"},{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps"}]`), `FAKE_CLI_REVIEW_COMMENTS=[{"author":"greptile-apps[bot]","path":"main.go","line":4,"body":"deferred bot finding"}]`)
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.ProtectedPaths = []string{"*.lock"}
			sctx.Config.AutoFix.CI = 3
			sctx.Config.CITimeout = time.Minute
			sctx.Fixing = manual
			if manual {
				sctx.PreviousFindings = ciGateFindingsJSON("test")
				sctx.DeferredFindings = `{"findings":[{"id":"ci-2","severity":"warning","description":"deferred bot finding","action":"ask-user","category":"ci-review-bot","check":"Greptile Review"}],"summary":"review bot finding"}`
			}
			polls := 0
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				polls++
				return nil
			}}
			outcome, err := driveCI(t, step, sctx)
			if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("refusal must park for an operator: outcome=%+v err=%v", outcome, err)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(findings.Items) != 3 {
				t.Fatalf("refusal findings=%+v err=%v", findings, err)
			}
			var refusal types.Finding
			byCategory := map[string]types.Finding{}
			for _, finding := range findings.Items {
				if finding.File == "package.lock" {
					refusal = finding
				}
				byCategory[finding.Category] = finding
			}
			if refusal.Action != types.ActionAskUser || !strings.Contains(refusal.Description, `rule "*.lock"`) {
				t.Errorf("refusal lost the path, rule, or decision: %+v", refusal)
			}
			if byCategory[types.FindingCategoryCICheck].Check != "test" || byCategory[types.FindingCategoryCIReviewBot].Check != "Greptile Review" {
				t.Errorf("refusal lost selected or deferred findings: %+v", findings.Items)
			}
			if invocations != 1 || polls != 0 {
				t.Errorf("refusal retried: invocations=%d polls=%d", invocations, polls)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Errorf("refusal committed: HEAD=%s want %s", got, headSHA)
			}
			if got := gitCmd(t, dir, "diff", "--cached"); got != indexBefore {
				t.Errorf("refusal changed the index: %q", got)
			}
			for file, want := range map[string]string{"package.lock": "rejected edit\n", "fix.txt": "CI repair\n"} {
				if got, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(got) != want {
					t.Errorf("refusal discarded %s: %q err=%v", file, got, err)
				}
			}
		})
	}
}

func TestTestStep_FixMode_ProtectedPathDoesNotReachCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	const ledger = "generated-ledger.json"
	if err := os.WriteFile(filepath.Join(dir, ledger), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ledger)
	gitCmd(t, dir, "commit", "-m", "add tool-owned ledger")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		for file, content := range map[string]string{ledger: "unrelated agent edit\n", "fix.txt": "test repair\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"repair test failure","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	repo, err := config.LoadRepoFromBytes([]byte("commands:\n  test: exit 0\nprotected_paths:\n  - generated-ledger.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	sctx.Config = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(repo, repo, false))
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"test-1","severity":"error","file":"fix.txt","description":"test failed"}]}`
	_, err = (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), ledger) {
		t.Errorf("expected a surfaced protected-path error naming %s, got %v", ledger, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Errorf("fix committed an out-of-scope edit: HEAD = %s, want %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("refusal staged changes: %q", got)
	}
	if got := gitStatusPorcelain(t, dir); !strings.Contains(got, ledger) || !strings.Contains(got, "fix.txt") {
		t.Errorf("refusal must preserve both edits for inspection, status = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, ledger)); err != nil || string(got) != "unrelated agent edit\n" {
		t.Errorf("protected edit was discarded: content = %q, err = %v", got, err)
	}
	t.Logf("Test auto-fix refusal: %v\nHEAD=%s\nindex paths=%q\nworktree status:\n%s", err, gitCmd(t, dir, "rev-parse", "HEAD"), gitCmd(t, dir, "diff", "--cached", "--name-only"), gitStatusPorcelain(t, dir))
}

func TestProtectedPaths_AllAutomaticCommitPathsRefuseWithoutMutation(t *testing.T) {
	t.Parallel()
	for _, caller := range []struct {
		name string
		run  func(*pipeline.StepContext) error
	}{
		{"fix", func(sctx *pipeline.StepContext) error {
			return commitAgentFixes(sctx, types.StepDocument, "update docs", "")
		}},
		{"push", func(sctx *pipeline.StepContext) error {
			_, err := (&PushStep{}).Execute(sctx)
			return err
		}},
		{"ci", func(sctx *pipeline.StepContext) error {
			_, err := (&CIStep{}).commitRepair(sctx, "repair checks", nil)
			return err
		}},
	} {
		t.Run(caller.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.ProtectedPaths = []string{"feature.txt"}
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("unrelated residue"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "feature.txt")
			before := gitCmd(t, dir, "diff", "--cached")
			err := caller.run(sctx)
			if err == nil || !strings.Contains(err.Error(), "feature.txt") {
				t.Fatalf("expected protected-path refusal, got %v", err)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Errorf("HEAD advanced from %s to %s", headSHA, got)
			}
			if got := gitCmd(t, dir, "diff", "--cached"); got != before {
				t.Errorf("refusal altered the index: %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "feature.txt")); err != nil || string(got) != "unrelated residue" {
				t.Errorf("refusal altered the worktree: %q, %v", got, err)
			}
		})
	}
}

func TestProtectedPaths_Staging(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, pattern, edit, file string
		blocked                   bool
	}{
		{"unstaged", "feature.txt", "write", "feature.txt", true},
		{"deleted", "feature.txt", "delete", "feature.txt", true},
		{"rename_source", "feature.txt", "rename", "renamed.txt", true},
		{"rename_destination", "renamed.txt", "rename", "renamed.txt", true},
		{"new_directory", "*.lock", "write", "new/nested/package.lock", true},
		{"subtree", "new/**", "write", "new/nested/package.lock", true},
		{"literal_path", "new/nested/package.lock", "write", "new/nested/package.lock", true},
		{"spaces_and_unicode", "*.lock", "write", "new/ ledger ü.lock", true},
		{"glob_does_not_cross_slash", "new/*.lock", "write", "new/nested/package.lock", false},
		{"clean_protected_file", "feature.txt", "write", "fix.txt", false},
		{"default_opt_out", "", "write", "feature.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			if tc.pattern != "" {
				sctx.Config.ProtectedPaths = []string{tc.pattern}
			}
			file := filepath.Join(dir, filepath.FromSlash(tc.file))
			switch tc.edit {
			case "write":
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("agent edit"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "rename":
				gitCmd(t, dir, "mv", "feature.txt", tc.file)
			}
			before := gitCmd(t, dir, "diff", "--cached")
			err := stagePipelineChanges(sctx)
			if tc.blocked {
				if err == nil || !strings.Contains(err.Error(), "protected") {
					t.Fatalf("expected protected-path error, got %v", err)
				}
				if got := gitCmd(t, dir, "diff", "--cached"); got != before {
					t.Errorf("refusal changed index: %q", got)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); !strings.Contains(got, tc.file) {
					t.Errorf("allowed edit not staged: %q", got)
				}
			}
		})
	}
}

// A rebase onto a base that bumped a submodule moves the recorded pointer but
// leaves a populated checkout at the old commit. Catch-all staging must not
// commit that stale checkout back as the pointer.
func TestStagePipelineChanges_KeepsRecordedSubmodulePointer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		agentFix bool
	}{
		{"with_agent_fix", true},
		{"stale_pointer_only", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := setupGitRepo(t)
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare")
			seed := t.TempDir()
			gitCmd(t, seed, "init", "-b", "main")
			if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, seed, "add", "module.txt")
			gitCmd(t, seed, "commit", "-m", "module base")
			gitCmd(t, seed, "push", remote, "main")
			gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
			gitCmd(t, dir, "commit", "-m", "add module")
			stale := gitCmd(t, dir, "rev-parse", "HEAD:module")

			if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("bumped\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, seed, "commit", "-am", "bump module")
			gitCmd(t, seed, "push", remote, "main")
			bumped := gitCmd(t, seed, "rev-parse", "HEAD")
			gitCmd(t, filepath.Join(dir, "module"), "fetch", "origin")
			gitCmd(t, dir, "update-index", "--cacheinfo", "160000,"+bumped+",module")
			gitCmd(t, dir, "commit", "-m", "base bumped module")
			headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
			if got := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD"); got != stale {
				t.Fatalf("fixture checkout = %s, want stale %s", got, stale)
			}
			if tc.agentFix {
				if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("agent fix\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			committed, err := commitAgentFixesWithResult(sctx, types.StepLint, "fix lint", "", nil)
			if err != nil {
				t.Fatalf("commit agent fixes: %v", err)
			}
			if committed != tc.agentFix {
				t.Fatalf("committed = %v, want %v", committed, tc.agentFix)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD:module"); got != bumped {
				t.Errorf("fix commit recorded submodule pointer %s, want the base's %s (stale checkout %s)", got, bumped, stale)
			}
			if tc.agentFix {
				gitCmd(t, dir, "cat-file", "-e", "HEAD:fix.txt")
			} else if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Errorf("stale pointer alone advanced HEAD from %s to %s", headSHA, got)
			}
		})
	}
}

// A rebase onto a base that removed a submodule leaves its populated checkout
// behind, and catch-all staging would re-add it as an embedded repository the
// base no longer registers. A submodule the change registers is still added.
func TestStagePipelineChanges_DoesNotReAddRemovedPopulatedSubmodule(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "lib.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "lib.txt")
	gitCmd(t, seed, "commit", "-m", "lib base")
	gitCmd(t, seed, "push", remote, "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "lib")
	gitCmd(t, dir, "commit", "-m", "add lib")
	gitCmd(t, dir, "rm", "-q", "--cached", "lib")
	gitCmd(t, dir, "rm", "-q", ".gitmodules")
	gitCmd(t, dir, "commit", "-m", "base removed lib")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if !submoduleWorktreeInitialized(dir, "lib") {
		t.Fatal("fixture lost the populated lib checkout")
	}
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("agent fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	committed, err := commitAgentFixesWithResult(sctx, types.StepLint, "fix lint", "", nil)
	if err != nil {
		t.Fatalf("commit agent fixes: %v", err)
	}
	if !committed {
		t.Fatal("fix commit was not created")
	}
	gitCmd(t, dir, "cat-file", "-e", "HEAD:fix.txt")
	if got := gitCmd(t, dir, "ls-tree", "HEAD", "--", "lib"); got != "" {
		t.Fatalf("fix commit re-added the removed submodule: %q", got)
	}

	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "added")
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatalf("stage registered submodule: %v", err)
	}
	if got := gitCmd(t, dir, "ls-files", "--stage", "--", "added"); !strings.HasPrefix(got, "160000 ") {
		t.Fatalf("registered submodule addition was unstaged: %q", got)
	}
}

func TestProtectedPaths_UnreadableStatusFailsClosed(t *testing.T) {
	t.Parallel()
	sctx := newTestContext(t, &mockAgent{}, t.TempDir(), "", "", config.Commands{})
	sctx.Config.ProtectedPaths = []string{"*.lock"}
	if err := stagePipelineChanges(sctx); err == nil || !strings.Contains(err.Error(), "check protected_paths") {
		t.Fatalf("unreadable git status did not fail closed: %v", err)
	}
}

func writeTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	file := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func captureLogs(sctx *pipeline.StepContext) *[]string {
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	return &logs
}

func stagedNames(t *testing.T, dir string) []string {
	t.Helper()
	out := gitCmd(t, dir, "diff", "--cached", "--name-only", "-z")
	return nulFields(out)
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestStagePipelineChanges_SkipsUntrackedToolScratch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	logs := captureLogs(sctx)
	writeTestFile(t, dir, ".cache/big/file", "scratch")
	writeTestFile(t, dir, ".test-fixture/a/b.txt", "scratch")
	writeTestFile(t, dir, "pkg/.review-deps/x", "scratch")
	writeTestFile(t, dir, "pkg/node_modules/dep/index.js", "scratch")
	writeTestFile(t, dir, "pkg/new_test.go", "package pkg\n")

	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	if got := stagedNames(t, dir); len(got) != 1 || got[0] != "pkg/new_test.go" {
		t.Fatalf("staged = %q, want only pkg/new_test.go", got)
	}
	joined := strings.Join(*logs, "\n")
	for _, want := range []string{"staging skipped untracked tool scratch", ".cache/", ".test-fixture/", "pkg/.review-deps/", "pkg/node_modules/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log missing %q: %s", want, joined)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".cache/big/file")); err != nil {
		t.Errorf("scratch was removed from the worktree: %v", err)
	}
}

func TestStagePipelineChanges_NestedHiddenDirUnderNewDirIsSkipped(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	writeTestFile(t, dir, "newpkg/code.go", "package newpkg\n")
	writeTestFile(t, dir, "newpkg/.tmp/dump.bin", "scratch")
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	if got := stagedNames(t, dir); len(got) != 1 || got[0] != "newpkg/code.go" {
		t.Fatalf("staged = %q, want only newpkg/code.go", got)
	}
}

func TestStagePipelineChanges_StagesNewFilesInTrackedHiddenDirs(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	writeTestFile(t, dir, ".github/ci.yml", "name: ci\n")
	writeTestFile(t, dir, ".config/tool.toml", "a = 1\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add tracked hidden dirs")
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, gitCmd(t, dir, "rev-parse", "HEAD"), config.Commands{})
	writeTestFile(t, dir, ".github/workflows/new.yml", "name: new\n")
	writeTestFile(t, dir, ".github/extra.yml", "name: extra\n")
	writeTestFile(t, dir, ".config/other.toml", "b = 2\n")
	// A new project-configuration directory is not scratch either.
	writeTestFile(t, dir, ".devcontainer/devcontainer.json", "{}\n")
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	got := stagedNames(t, dir)
	for _, want := range []string{".github/workflows/new.yml", ".github/extra.yml", ".config/other.toml", ".devcontainer/devcontainer.json"} {
		if !containsString(got, want) {
			t.Errorf("%s not staged: %q", want, got)
		}
	}
}

func TestStagePipelineChanges_StagingExclude(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.StagingExclude = []string{"*.bin", "generated/**", "docs/out/report.txt"}
	logs := captureLogs(sctx)
	writeTestFile(t, dir, "keep.go", "package x\n")
	writeTestFile(t, dir, "dump.bin", "x")
	writeTestFile(t, dir, "sub/deep/dump.bin", "x")
	writeTestFile(t, dir, "generated/a.go", "x")
	writeTestFile(t, dir, "docs/out/report.txt", "x")
	writeTestFile(t, dir, "docs/out/keep.txt", "x")
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	got := stagedNames(t, dir)
	for _, want := range []string{"keep.go", "docs/out/keep.txt"} {
		if !containsString(got, want) {
			t.Errorf("%s not staged: %q", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("staged = %q, want only keep.go and docs/out/keep.txt", got)
	}
	if joined := strings.Join(*logs, "\n"); !strings.Contains(joined, "dump.bin") || !strings.Contains(joined, "generated/") {
		t.Errorf("skip log missing excluded entries: %s", joined)
	}
}

func TestStagePipelineChanges_RefusesOversizedStagedFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.MaxFileSizeBytes = 100
	writeTestFile(t, dir, "small.txt", "tiny")
	writeTestFile(t, dir, "pkg/huge.dat", strings.Repeat("x", 500))
	writeTestFile(t, dir, "feature.txt", strings.Repeat("y", 400))
	gitCmd(t, dir, "add", "small.txt")
	before := gitCmd(t, dir, "diff", "--cached")

	err := stagePipelineChanges(sctx)
	var refusal *pipeline.ProtectedPathError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected typed refusal, got %v", err)
	}
	for _, want := range []string{"pkg/huge.dat", "feature.txt", "max_file_size_bytes", ".gitignore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if got := gitCmd(t, dir, "diff", "--cached"); got != before {
		t.Errorf("refusal altered the index: %q", got)
	}
	if got, readErr := os.ReadFile(filepath.Join(dir, "pkg/huge.dat")); readErr != nil || len(got) != 500 {
		t.Errorf("refusal altered the worktree: %v", readErr)
	}
	if outcome := pipeline.ProtectedPathOutcome(err); outcome == nil || !outcome.NeedsApproval {
		t.Errorf("refusal does not park at an explicit-response gate: %+v", outcome)
	}

	sctx.Config.MaxFileSizeBytes = 0
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatalf("size check should be disabled at 0: %v", err)
	}
}

func TestStagePipelineChanges_StagesTrackedDeletions(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "base.txt", "modified")
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	got := gitCmd(t, dir, "diff", "--cached", "--name-status")
	if !strings.Contains(got, "D\tfeature.txt") || !strings.Contains(got, "M\tbase.txt") {
		t.Errorf("deletion/modification not staged: %q", got)
	}
}

func TestStagePipelineChanges_LiteralNamesWithSpacesAndGlobs(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	names := []string{"dir/my file.txt", "dir/star*.txt", "dir/q?.txt", "dir/[br].txt", "new dir/ü.txt", "bang!.txt"}
	for _, name := range names {
		writeTestFile(t, dir, name, "content")
	}
	writeTestFile(t, dir, "dir/starX.txt", "should stay untracked? no, it is a normal file")
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	got := stagedNames(t, dir)
	for _, name := range append(names, "dir/starX.txt") {
		if !containsString(got, name) {
			t.Errorf("%q not staged: %q", name, got)
		}
	}
	if len(got) != len(names)+1 {
		t.Errorf("unexpected staged set: %q", got)
	}
}

func TestStagePipelineChanges_OnlyScratchCommitsNothing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	writeTestFile(t, dir, ".cache/pnpm/store", "scratch")
	committed, err := commitAgentFixesWithResult(sctx, types.StepDocument, "update docs", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Error("scratch-only worktree reported a commit")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Errorf("HEAD advanced to %s", got)
	}
}

func TestCommitAgentFixes_LeavesScratchOutOfCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	logs := captureLogs(sctx)
	writeTestFile(t, dir, ".cache/big/file", "scratch")
	writeTestFile(t, dir, "pkg/new_test.go", "package pkg\n")
	if err := commitAgentFixes(sctx, types.StepDocument, "update docs", ""); err != nil {
		t.Fatal(err)
	}
	files := gitCmd(t, dir, "show", "--name-only", "--format=", "HEAD")
	if strings.TrimSpace(files) != "pkg/new_test.go" {
		t.Errorf("commit contains %q, want only pkg/new_test.go", files)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "staging skipped untracked tool scratch: .cache/") {
		t.Errorf("skip was not logged: %q", *logs)
	}
}

func scratchGuardCommit(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		writeTestFile(t, dir, name, content)
	}
	gitCmd(t, dir, "add", "-A", "-f")
	gitCmd(t, dir, "commit", "-m", "agent commit")
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

func TestAssertPublishedRangeClean(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prep    func(t *testing.T, dir string)
		files   map[string]string
		exclude []string
		want    string // substring of the refusal; empty means the range passes
	}{
		{name: "normal range", files: map[string]string{"pkg/a.go": "package pkg\n", "docs/readme.md": "x"}},
		{name: "scratch dir", files: map[string]string{".cache/pnpm/blob": "x", "pkg/a.go": "x"}, want: ".cache/pnpm/blob"},
		{name: "nested scratch dir", files: map[string]string{"pkg/.test-deps/x": "x"}, want: "pkg/.test-deps/x"},
		{name: "node_modules", files: map[string]string{"node_modules/dep/i.js": "x"}, want: "node_modules/dep/i.js"},
		{name: "oversized", files: map[string]string{"pkg/huge.bin": strings.Repeat("x", 600)}, want: "pkg/huge.bin"},
		{name: "staging_exclude", exclude: []string{"*.bin"}, files: map[string]string{"out/a.bin": "x"}, want: "staging_exclude"},
		{name: "new file in existing hidden dir", prep: func(t *testing.T, dir string) {
			scratchGuardCommit(t, dir, map[string]string{".github/ci.yml": "x"})
		}, files: map[string]string{".github/workflows/new.yml": "x"}},
		{name: "known config dir", files: map[string]string{".github/ci.yml": "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "-q", "-b", "work", headSHA)
			sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.MaxFileSizeBytes = 500
			sctx.Config.StagingExclude = tc.exclude
			if tc.prep != nil {
				tc.prep(t, dir)
				// The prepared commit lands on the target, as an earlier merge would.
				gitCmd(t, dir, "branch", "-f", "main", "HEAD")
				gitCmd(t, dir, "checkout", "-q", "-b", "work2")
			}
			head := scratchGuardCommit(t, dir, tc.files)
			err := assertPublishedRangeClean(sctx, head)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected refusal: %v", err)
			case tc.want != "" && err == nil:
				t.Fatal("expected refusal")
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestAssertPublishedRangeClean_CapsListedPaths(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	files := map[string]string{}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf(".fixture/f%02d", i)] = "x"
	}
	head := scratchGuardCommit(t, dir, files)
	err := assertPublishedRangeClean(sctx, head)
	if err == nil || !strings.Contains(err.Error(), "30 path(s)") || !strings.Contains(err.Error(), "and 10 more") {
		t.Fatalf("expected capped list with total count, got %v", err)
	}
}

func TestPublishRunHead_RefusesAgentCommittedScratch(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")
	head := scratchGuardCommit(t, dir, map[string]string{".live-history/day1.json": "x", "pkg/a.go": "package pkg\n"})

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, head)
	err := publishRunHead(sctx, head, "", nil)
	if err == nil || !strings.Contains(err.Error(), ".live-history/day1.json") {
		t.Fatalf("expected scratch refusal, got %v", err)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != submittedHead {
		t.Errorf("remote moved to %s despite refusal", got)
	}
}

func TestStagePipelineChanges_UnmergedIndexPreservedOnOversizedRefusal(t *testing.T) {
	t.Parallel()
	for _, limit := range []int64{100, 0} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.MaxFileSizeBytes = limit
			one := gitCmd(t, dir, "rev-parse", "HEAD:base.txt")
			two := gitCmd(t, dir, "rev-parse", "HEAD:feature.txt")
			input := fmt.Sprintf("0 %s\tbase.txt\n100644 %s 1\tbase.txt\n100644 %s 2\tbase.txt\n100644 %s 3\tbase.txt\n", strings.Repeat("0", 40), one, one, two)
			if _, err := stepGitRunInput(sctx, input, "update-index", "--index-info"); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, dir, "base.txt", "resolved locally")
			writeTestFile(t, dir, "feature.txt", "unstaged edit")
			writeTestFile(t, dir, "huge.dat", strings.Repeat("x", 500))
			writeTestFile(t, dir, "small.txt", "already staged")
			gitCmd(t, dir, "add", "small.txt")
			beforeIndex := gitCmd(t, dir, "ls-files", "--stage", "-z")
			beforeStatus := gitCmd(t, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
			indexPath := gitCmd(t, dir, "rev-parse", "--git-path", "index")
			if !filepath.IsAbs(indexPath) {
				indexPath = filepath.Join(dir, indexPath)
			}
			beforeBytes, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			err = stagePipelineChanges(sctx)
			if limit == 0 {
				if err != nil {
					t.Fatalf("resolved content should stage with the size limit disabled: %v", err)
				}
				if got := gitCmd(t, dir, "ls-files", "--unmerged"); got != "" {
					t.Fatalf("staging did not resolve conflict entries: %s", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "max_file_size_bytes") {
				t.Fatalf("expected oversized refusal, got %v", err)
			}
			afterBytes, err := os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatalf("refusal did not restore byte-exact index: %v", err)
			}
			if got := gitCmd(t, dir, "ls-files", "--stage", "-z"); got != beforeIndex {
				t.Fatalf("refusal changed staged entries or conflict stages: %q", got)
			}
			if got := gitCmd(t, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all"); got != beforeStatus {
				t.Fatalf("refusal changed worktree status: %q", got)
			}
			for name, want := range map[string]string{"base.txt": "resolved locally", "feature.txt": "unstaged edit", "huge.dat": strings.Repeat("x", 500), "small.txt": "already staged"} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(got) != want {
					t.Fatalf("refusal changed %s: %q, %v", name, got, err)
				}
			}
		})
	}
}

func TestAssertPublishedRangeClean_RejectsIntermediateHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path, content string
		exclude             []string
	}{
		{name: "oversized addition", path: "huge.dat", content: strings.Repeat("x", 600)},
		{name: "oversized modification", path: "feature.txt", content: strings.Repeat("x", 600)},
		{name: "hidden scratch", path: ".cache/blob", content: "scratch"},
		{name: "nested scratch", path: "pkg/.cache/blob", content: "scratch"},
		{name: "dependencies", path: "node_modules/dep/index.js", content: "scratch"},
		{name: "excluded path", path: "out/dump.bin", content: "scratch", exclude: []string{"*.bin"}},
	} {
		for _, ending := range []string{"delete", "replace", "merge deleted side branch"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, headSHA := setupGitRepo(t)
				sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
				sctx.Config.MaxFileSizeBytes = 500
				sctx.Config.StagingExclude = tc.exclude
				if ending == "merge deleted side branch" {
					gitCmd(t, dir, "checkout", "-b", "side")
				}
				scratchGuardCommit(t, dir, map[string]string{tc.path: tc.content})
				if ending == "replace" {
					writeTestFile(t, dir, tc.path, "small")
				} else if err := os.Remove(filepath.Join(dir, tc.path)); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "remove offending content")
				if ending == "merge deleted side branch" {
					gitCmd(t, dir, "checkout", "feature")
					scratchGuardCommit(t, dir, map[string]string{"ordinary.txt": "normal"})
					gitCmd(t, dir, "merge", "--no-ff", "side", "-m", "merge side history")
				}
				head := gitCmd(t, dir, "rev-parse", "HEAD")
				err := assertPublishedRangeClean(sctx, head)
				if err == nil || !strings.Contains(err.Error(), tc.path) || !strings.Contains(err.Error(), "rewriting or dropping") {
					t.Fatalf("expected historical content refusal naming %s and history remediation, got %v", tc.path, err)
				}
			})
		}
	}
}

func TestStagePipelineChanges_PreservesIntentToAddOnOversizedRefusal(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.MaxFileSizeBytes = 100
	writeTestFile(t, dir, "intent.go", "package intent\n")
	gitCmd(t, dir, "add", "-N", "intent.go")
	writeTestFile(t, dir, "huge.dat", strings.Repeat("x", 500))
	indexPath := gitCmd(t, dir, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(dir, indexPath)
	}
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	err = stagePipelineChanges(sctx)
	if err == nil || !strings.Contains(err.Error(), "max_file_size_bytes") {
		t.Fatalf("expected oversized refusal, got %v", err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refusal changed index bytes, including intent-to-add flags: %v", err)
	}
	if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); got != "" {
		t.Fatalf("refusal materialized intent-to-add entry: %s", got)
	}
	if got := gitCmd(t, dir, "diff", "--name-only"); got != "intent.go" {
		t.Fatalf("refusal lost intent-to-add entry: %s", got)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(indexPath), ".no-mistakes-index-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("snapshot files were not removed: %v, %v", leftovers, err)
	}
}

func TestStagePipelineChanges_IndexCopyFailureRefusesBeforeMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	missing := filepath.Join(t.TempDir(), "missing-index")
	sctx.Env = append(sctx.Env, "GIT_INDEX_FILE="+missing)
	writeTestFile(t, dir, "feature.txt", "unstaged edit")
	writeTestFile(t, dir, "new.go", "package new\n")
	before := gitCmd(t, dir, "status", "--porcelain=v1", "-z")
	err := stagePipelineChanges(sctx)
	if err == nil || !strings.Contains(err.Error(), "cannot snapshot index") {
		t.Fatalf("expected copy refusal, got %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("staging created missing index: %v", err)
	}
	if got := gitCmd(t, dir, "status", "--porcelain=v1", "-z"); got != before {
		t.Fatalf("copy failure changed worktree or index: %q", got)
	}
}

func TestAssertPublishedRangeClean_ExistingScratchComponentMustBeTree(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"blob", "symlink", "gitlink", "tree"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir, _, headSHA := setupGitRepo(t)
			switch kind {
			case "blob":
				writeTestFile(t, dir, ".cache", "tracked file")
				gitCmd(t, dir, "add", ".cache")
			case "symlink":
				if err := os.Symlink("base.txt", filepath.Join(dir, ".cache")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				gitCmd(t, dir, "add", ".cache")
			case "gitlink":
				gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+headSHA+",.cache")
			case "tree":
				writeTestFile(t, dir, ".cache/tracked.txt", "tracked content")
				gitCmd(t, dir, "add", ".cache")
			}
			gitCmd(t, dir, "commit", "-m", "base scratch component")
			base := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "branch", "-f", "main", base)
			if kind == "blob" || kind == "symlink" {
				if err := os.Remove(filepath.Join(dir, ".cache")); err != nil {
					t.Fatal(err)
				}
			} else if kind == "gitlink" {
				gitCmd(t, dir, "rm", "--cached", ".cache")
			}
			head := scratchGuardCommit(t, dir, map[string]string{".cache/blob": "new scratch"})
			sctx := newTestContext(t, &mockAgent{}, dir, base, head, config.Commands{})
			err := assertPublishedRangeClean(sctx, head)
			if kind == "tree" {
				if err != nil {
					t.Fatalf("existing tree should permit new file: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), ".cache/blob") {
				t.Fatalf("base %s must not exempt new scratch directory: %v", kind, err)
			}
		})
	}
}

func TestAssertPublishedRangeClean_RefusesReusedOversizedBaseBlob(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	content := strings.Repeat("x", 600)
	base := scratchGuardCommit(t, dir, map[string]string{"fixture.dat": content})
	gitCmd(t, dir, "branch", "-f", "main", base)
	head := scratchGuardCommit(t, dir, map[string]string{"copied.dat": content, "feature.txt": content})
	sctx := newTestContext(t, &mockAgent{}, dir, base, head, config.Commands{})
	sctx.Config.MaxFileSizeBytes = 500
	err := assertPublishedRangeClean(sctx, head)
	if err == nil {
		t.Fatal("expected refusal for additions and modifications reusing oversized base blob")
	}
	for _, path := range []string{"copied.dat", "feature.txt"} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("refusal does not name %s: %v", path, err)
		}
	}
}

func TestStagePipelineChanges_SkipsTestPhaseDriver(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, base, head, config.Commands{})
	logs := captureLogs(sctx)
	for _, prefix := range []string{".test-phase-driver", "pkg/.test-phase-driver"} {
		writeTestFile(t, dir, prefix+"/driver", "\x00\x01stand-in binary")
		writeTestFile(t, dir, prefix+"/config.json", "{}")
	}
	writeTestFile(t, dir, "pkg/code.go", "package pkg\n")
	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	if got := stagedNames(t, dir); len(got) != 1 || got[0] != "pkg/code.go" {
		t.Fatalf("driver files must remain untracked: %q", got)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], ".test-phase-driver/") || !strings.Contains((*logs)[0], "pkg/.test-phase-driver/") {
		t.Fatalf("expected one log naming both skipped driver directories: %q", *logs)
	}
}

func TestAssertPublishedRangeClean_RefusesTestPhaseDriver(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, base, head, config.Commands{})
	head = scratchGuardCommit(t, dir, map[string]string{
		".test-phase-driver/driver":     "\x00stand-in binary",
		"pkg/.test-phase-driver/driver": "\x00stand-in binary",
	})
	err := assertPublishedRangeClean(sctx, head)
	if err == nil || !strings.Contains(err.Error(), ".test-phase-driver/driver") || !strings.Contains(err.Error(), "pkg/.test-phase-driver/driver") {
		t.Fatalf("expected refusal naming both driver paths: %v", err)
	}
}

func TestStagePipelineChanges_RefusesOversizedOrdinaryBinary(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, base, head, config.Commands{})
	sctx.Config.MaxFileSizeBytes = 100
	content := strings.Repeat("\x00\x01\x02\x03", 50)
	writeTestFile(t, dir, "tools/driver", content)
	before := gitCmd(t, dir, "ls-files", "--stage")
	err := stagePipelineChanges(sctx)
	if err == nil || !strings.Contains(err.Error(), "tools/driver (200 B)") {
		t.Fatalf("expected refusal naming ordinary binary and size: %v", err)
	}
	if got := gitCmd(t, dir, "ls-files", "--stage"); got != before {
		t.Fatalf("refusal changed the index: %s", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "tools/driver")); err != nil || string(got) != content {
		t.Fatalf("refusal changed binary content: %v", err)
	}
}

func TestAssertPublishedRangeClean_LargeScratchHistoryCountsUniquePaths(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{}, dir, base, head, config.Commands{})
	blob := gitCmd(t, dir, "rev-parse", "HEAD:feature.txt")
	const count = 63000
	var input strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&input, "100644 %s\t.fixture/f%05d\n", blob, i)
	}
	if _, err := stepGitRunInput(sctx, input.String(), "update-index", "--index-info"); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "commit", "-m", "large scratch fixture")
	gitCmd(t, dir, "update-index", "--force-remove", ".fixture/f00000")
	gitCmd(t, dir, "commit", "-m", "remove scratch path")
	gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",.fixture/f00000")
	gitCmd(t, dir, "commit", "-m", "re-add scratch path")
	head = gitCmd(t, dir, "rev-parse", "HEAD")
	err := assertPublishedRangeClean(sctx, head)
	if err == nil || !strings.Contains(err.Error(), "63000 path(s)") || !strings.Contains(err.Error(), "and 62980 more") {
		t.Fatalf("expected exact unique total and capped diagnostics: %v", err)
	}
	if strings.Contains(err.Error(), ".fixture/f00020") {
		t.Fatalf("refusal rendered paths beyond its cap: %v", err)
	}
}

func BenchmarkAppendUniquePaths(b *testing.B) {
	for _, count := range []int{1000, 63000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			paths := make([]string, count)
			for i := range paths {
				paths[i] = fmt.Sprintf("pkg/.scratch-%05d/", i)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var listed []string
				seen := map[string]bool{}
				for repeat := 0; repeat < 2; repeat++ {
					for _, path := range paths {
						listed = appendUnique(listed, seen, path)
					}
				}
				if len(listed) != count {
					b.Fatalf("unique path count = %d, want %d", len(listed), count)
				}
			}
		})
	}
}

func TestCommitAgentFixes_TrackedScratchDirectoryReplacement(t *testing.T) {
	t.Parallel()
	for _, directory := range []string{".config", "pkg/.config", ".config/.nested", "node_modules", "pkg/node_modules"} {
		for _, deletionStaged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deletion_staged=%t", directory, deletionStaged), func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, _ := setupGitRepo(t)
				oldPath := directory + "/old.toml"
				newPath := directory + "/new.toml"
				head := scratchGuardCommit(t, dir, map[string]string{oldPath: "old config\n"})
				sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, baseSHA, head, config.Commands{})
				if err := os.Remove(filepath.Join(dir, oldPath)); err != nil {
					t.Fatal(err)
				}
				if deletionStaged {
					gitCmd(t, dir, "add", "-u")
				}
				writeTestFile(t, dir, newPath, "replacement config\n")
				writeTestFile(t, dir, directory+"/.scratch/blob", "new scratch")
				if err := commitAgentFixes(sctx, types.StepDocument, "replace config", ""); err != nil {
					t.Fatal(err)
				}
				changes := gitCmd(t, dir, "show", "--format=", "--name-status", "--no-renames", "HEAD")
				want := "A\t" + newPath + "\nD\t" + oldPath
				if changes != want {
					t.Fatalf("replacement commit must contain addition and deletion only: got %q, want %q", changes, want)
				}
				if got := gitCmd(t, dir, "show", "HEAD:"+newPath); got != "replacement config" {
					t.Fatalf("replacement content = %q", got)
				}
				if _, err := os.Stat(filepath.Join(dir, directory, ".scratch/blob")); err != nil {
					t.Fatalf("skipped scratch must remain in the worktree: %v", err)
				}
			})
		}
	}
}
