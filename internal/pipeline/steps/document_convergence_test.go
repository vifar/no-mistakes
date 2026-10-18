package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise real Document invocations and their persisted run through repeated
// validation passes. The agent always proposes another edit, as in the churn.
func TestDocumentConvergenceRepeatedPasses(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fmt.Sprintf("guide %d\n", calls)), 0o644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sr, err := sc.DB.InsertStepResult(sc.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := sc.DB.CaptureReviewComparison(sc.Run.ID, db.ReviewComparison{
		SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", TargetSHA: base,
		HeadSHA: head, MergeBaseSHA: base, DiffDigest: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.CompleteReviewStep(sr.ID, sc.Run.ID, head, 0, 1, "", comparison.Generation); err != nil {
		t.Fatal(err)
	}
	recordReviewApproval(t, sc, head)
	recordTestedHead(t, sc, head)
	for pass := 0; pass < 4; pass++ {
		if _, err := (&DocumentStep{}).Execute(sc); err != nil {
			t.Fatal(err)
		}
		if restart, err := reviewRestartNeededForPush(sc, "", sc.Run.HeadSHA); err != nil || restart {
			t.Fatalf("pass %d restarted validation: %v, %v", pass, restart, err)
		}
		// Recreate the step/context to rule out an in-memory-only stop.
		fresh := *sc
		fresh.Shared = &pipeline.RunShared{}
		sc = &fresh
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("Document churn created %s commits, want 1", got)
	}
	if calls != 1 {
		t.Fatalf("clean Document was invoked %d times, want 1", calls)
	}
	if got := testedHead(t, sc); got != sc.Run.HeadSHA {
		t.Fatalf("tested head %s, want %s", got, sc.Run.HeadSHA)
	}
	got, err := sc.DB.GetReviewComparison(sc.Run.ID)
	if err != nil || got.Generation != comparison.Generation || got.Approved != comparison.Approved {
		t.Fatalf("review generation changed: %+v, %v", got, err)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("dirty: %s", got)
	}
}

func TestDocumentConvergenceCombinedLintFallback(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	housekeepingCalls, lintCalls := 0, 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "housekeeping" {
			housekeepingCalls++
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			lintCalls++
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"housekeeping clean"}`)}, nil
	}}
	sc := newHousekeepingContext(t, ag, dir, base, head, config.Commands{})
	for pass := 0; pass < 2; pass++ {
		if _, err := (&DocumentStep{}).Execute(sc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (&LintStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	if housekeepingCalls != 1 || lintCalls != 1 {
		t.Fatalf("housekeeping=%d lint=%d; skipped Document must leave lint its own pass", housekeepingCalls, lintCalls)
	}
}

// Run the executor through a forced Document restart. Review and Test use
// deterministic outcomes; Document, Git, receipts, resets, and rounds are real.
// Publication is a DB fixture: this journey never pushes to any remote.
func TestDocumentConvergenceExecutorJourney(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	agentCalls, reviewCalls, testCalls, documentCalls, pushCalls := 0, 0, 0, 0, 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		agentCalls++
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fmt.Sprintf("guide %d\n", agentCalls)), 0o644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	steps := []pipeline.Step{
		&transitionStep{name: types.StepReview, execute: func(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			reviewCalls++
			comparison, err := c.DB.CaptureReviewComparison(c.Run.ID, db.ReviewComparison{SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", TargetSHA: base, HeadSHA: c.Run.HeadSHA, MergeBaseSHA: base, DiffDigest: strings.Repeat("a", 64)})
			if err != nil {
				return nil, err
			}
			return &pipeline.StepOutcome{ReviewApprovedHeadSHA: c.Run.HeadSHA, ReviewComparisonGeneration: comparison.Generation}, nil
		}},
		&transitionStep{name: types.StepTest, execute: func(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			testCalls++
			return &pipeline.StepOutcome{Findings: fmt.Sprintf(`{"findings":[],"summary":"tests passed","tested_head_sha":%q}`, c.Run.HeadSHA)}, nil
		}},
		&transitionStep{name: types.StepDocument, execute: func(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			documentCalls++
			return (&DocumentStep{}).Execute(c)
		}},
		&transitionStep{name: types.StepPush, execute: func(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			pushCalls++
			if pushCalls > 3 {
				return nil, fmt.Errorf("Document churn exceeded journey bound")
			}
			restart, err := reviewRestartNeededForPush(c, "", c.Run.HeadSHA)
			if err != nil {
				return nil, err
			}
			if restart {
				return &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
			}
			if pushCalls == 1 {
				return &pipeline.StepOutcome{RestartFrom: types.StepDocument}, nil
			}
			return &pipeline.StepOutcome{}, c.DB.UpdateRunPublication(c.Run.ID, db.PushBinding{HeadSHA: c.Run.HeadSHA, TargetKind: "upstream", TargetFingerprint: strings.Repeat("b", 64), Ref: "refs/heads/feature"})
		}},
	}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	completed := false
	executor := pipeline.NewExecutor(sc.DB, p, sc.Config, ag, steps, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted && event.Status != nil && *event.Status == string(types.RunCompleted) {
			completed = true
		}
	})
	executor.SetPRContextGuard(func(c *pipeline.StepContext, _ types.StepName) (pipeline.PRContextDecision, error) {
		candidate, err := testgit.RunComparisonCandidate(c.Run, dir, "main", base, "", "test/repo", "feature")
		if err != nil {
			return pipeline.PRContextDecision{}, err
		}
		_, err = c.DB.AdvanceRunPRContext(c.Run.ID, candidate)
		return pipeline.PRContextDecision{Target: pipeline.PRTargetSelection{SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main"}}, err
	})
	if err := executor.Execute(context.Background(), sc.Run, sc.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if !completed || reviewCalls != 1 || testCalls != 1 || documentCalls != 2 || agentCalls != 1 || pushCalls != 2 {
		t.Fatalf("completed=%v Review=%d Test=%d Document=%d agent=%d Push=%d", completed, reviewCalls, testCalls, documentCalls, agentCalls, pushCalls)
	}
	comparison, err := sc.DB.GetReviewComparison(sc.Run.ID)
	if err != nil || comparison.Generation != 1 {
		t.Fatalf("review generation changed: %+v, %v", comparison, err)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("created %s commits, want 1", got)
	}
	results, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.StepName != types.StepReview && result.StepName != types.StepTest {
			continue
		}
		rounds, err := sc.DB.GetRoundsByStep(result.ID)
		if err != nil || len(rounds) != 1 {
			t.Fatalf("validation rounds changed for %s: %+v, %v", result.StepName, rounds, err)
		}
		if result.StepName == types.StepReview && (rounds[0].ReviewedHeadSHA == nil || *rounds[0].ReviewedHeadSHA != head) {
			t.Fatalf("historical Review anchor was rewritten: %+v", rounds[0])
		}
		if result.StepName == types.StepTest {
			if rounds[0].FindingsJSON == nil {
				t.Fatal("historical Test evidence missing")
			}
			findings, err := types.ParseFindingsJSON(*rounds[0].FindingsJSON)
			if err != nil || findings.TestedHeadSHA != head {
				t.Fatalf("historical Test evidence was rewritten: %+v, %v", findings, err)
			}
		}
	}
	passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
	if err != nil || len(passes) != 1 || passes[0].CarriedReviewFrom == nil || *passes[0].CarriedReviewFrom != head {
		t.Fatalf("missing explicit carry: %+v, %v", passes, err)
	}
}

func TestDocumentConvergenceSuppressedCleanProposalRetainsGap(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("guide%d.md", calls)), []byte("guide\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"gap still open","action":"ask-user"}],"summary":"add guide"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"close gap"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil || !out.NeedsApproval || !strings.Contains(out.Findings, "gap still open") {
		t.Fatalf("discarded edits erased the gap: %+v, %v", out, err)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("created %s commits", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("suppressed addition remains: %s", got)
	}
}

func TestDocumentConvergenceCountsAgentCommitsAndFailsInterruptedPassClosed(t *testing.T) {
	for _, scenario := range []string{"over budget", "interrupted", "dirty entry"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				for i := 0; i < 3; i++ {
					writeAndCommit(fmt.Sprintf("guide%d.md", i), "guide\n")(t, dir)
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"add guides"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			if scenario == "interrupted" {
				if _, err := sc.DB.BeginDocumentPass(sc.Run.ID, head); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "dirty entry" {
				if err := os.WriteFile(filepath.Join(dir, "existing.md"), []byte("owner work"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := (&DocumentStep{}).Execute(sc)
			if scenario == "over budget" {
				if err != nil || !out.NeedsApproval {
					t.Fatalf("over-budget result: %+v, %v", out, err)
				}
				if got := gitStatusPorcelain(t, dir); got != "" {
					t.Fatalf("over-budget edits remain: %s", got)
				}
			} else if err == nil || calls != 0 {
				t.Fatalf("unsafe entry launched agent: %d calls, %v", calls, err)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("over-budget/unsafe pass advanced head: %s", got)
			}
		})
	}
}

func TestDocumentCarryPreservesStrictAndUnsafePathRestarts(t *testing.T) {
	for _, scenario := range []string{"documentation", "code", "instructions", "configuration", "code renamed to docs", "strict"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				p := "README.md"
				switch scenario {
				case "code":
					p = "main.go"
				case "instructions":
					p = "AGENTS.md"
				case "configuration":
					p = "example.yaml"
				case "code renamed to docs":
					gitCmd(t, dir, "mv", "feature.txt", p)
				}
				if err := os.WriteFile(filepath.Join(dir, p), []byte("updated\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update documentation"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			sc.Config.Document.DocOnlyPaths = []string{"**"}
			if scenario == "strict" {
				sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
			}
			recordReviewApproval(t, sc, head)
			recordTestedHead(t, sc, head)
			if _, err := (&DocumentStep{}).Execute(sc); err != nil {
				t.Fatal(err)
			}
			wantRestart := scenario != "documentation"
			restart, err := reviewRestartNeededForPush(sc, "", sc.Run.HeadSHA)
			if err != nil || restart != wantRestart {
				t.Fatalf("restart=%v, want %v; %v", restart, wantRestart, err)
			}
			wantHead := sc.Run.HeadSHA
			if wantRestart {
				wantHead = head
			}
			persisted, err := sc.DB.GetRun(sc.Run.ID)
			if err != nil || persisted.ReviewApprovedHeadSHA == nil || *persisted.ReviewApprovedHeadSHA != wantHead {
				t.Fatalf("approval changed incorrectly: %+v, %v", persisted, err)
			}
			if got := testedHead(t, sc); got != wantHead {
				t.Fatalf("tested head=%s, want %s", got, wantHead)
			}
		})
	}
}

func TestDocumentConvergenceSameFiles(t *testing.T) {
	for _, selfCommit := range []bool{false, true} {
		t.Run(fmt.Sprint(selfCommit), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fmt.Sprintf("guide %d\n", calls)), 0o644); err != nil {
					t.Fatal(err)
				}
				if selfCommit {
					gitCmd(t, dir, "add", "README.md")
					gitCmd(t, dir, "commit", "-qm", "agent documentation")
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"owner decision needed","action":"ask-user"}],"summary":"update guide"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			for pass := 0; pass < 2; pass++ {
				out, err := (&DocumentStep{}).Execute(sc)
				if err != nil {
					t.Fatal(err)
				}
				if !out.NeedsApproval {
					t.Fatal("convergence erased an unresolved gap")
				}
			}
			if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
				t.Fatalf("same-file pass created %s commits, want 1", got)
			}
			if got := gitStatusPorcelain(t, dir); got != "" {
				t.Fatalf("suppressed edits remain: %s", got)
			}
		})
	}
}

func TestDocumentConvergenceCap(t *testing.T) {
	for _, limit := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("guide%d.md", calls)), []byte("guide\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"another gap remains","action":"ask-user"}],"summary":"add guide"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			raw, err := config.LoadRepoFromBytes([]byte(fmt.Sprintf("document:\n  max_commits_per_run: %d\n", limit)))
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Merge(&config.GlobalConfig{}, raw)
			sc.Config.Document = cfg.Document
			for pass := 0; pass < 4; pass++ {
				if _, err := (&DocumentStep{}).Execute(sc); err != nil {
					t.Fatal(err)
				}
			}
			if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != fmt.Sprint(limit) {
				t.Fatalf("limit %d created %s commits", limit, got)
			}
		})
	}
}

func TestDocOnlyCarryRejectsAmbiguousChanges(t *testing.T) {
	for _, p := range []string{"docs/build.go", "docs/config.yaml", "docs/config.yml", "docs/prompt.md", "docs/RULES.md", "docs/copilot-instructions.md", "docs/AGENTS.override.md", "docs/CLAUDE.local.md", "docs/../README.md", "/docs/guide.md", "C:/docs/guide.md", " docs/guide.md"} {
		if isDocOnlyPathFor(p, []string{"**"}) {
			t.Errorf("broad trusted glob accepted %q", p)
		}
	}
	for _, kind := range []string{"intermediate code", "symlink", "executable"} {
		t.Run(kind, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			switch kind {
			case "intermediate code":
				writeAndCommit("code.go", "package code\n")(t, dir)
				deleteAndCommit("code.go")(t, dir)
				writeAndCommit("README.md", "guide\n")(t, dir)
			case "symlink":
				if err := os.Symlink("feature.txt", filepath.Join(dir, "README.md")); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "README.md")
				gitCmd(t, dir, "commit", "-qm", "link")
			case "executable":
				writeAndCommit("README.md", "guide\n")(t, dir)
				gitCmd(t, dir, "update-index", "--chmod=+x", "README.md")
				gitCmd(t, dir, "commit", "-qm", "mode")
			}
			if ok, err := docOnlyRange(sc, head, gitCmd(t, dir, "rev-parse", "HEAD")); err != nil || ok {
				t.Fatalf("unsafe range accepted: %v, %v", ok, err)
			}
		})
	}
}
