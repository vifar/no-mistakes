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
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fmt.Sprintf("guide %d\n", calls)), 0o644); err != nil {
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
	if !strings.Contains(out.Findings, "Document discarded a repeated repair of README.md") {
		t.Errorf("existing finding hid the discarded repair: %s", out.Findings)
	}
	var visible Findings
	if err := json.Unmarshal([]byte(out.Findings), &visible); err != nil {
		t.Fatal(err)
	}
	discardRow := false
	for _, finding := range visible.Items {
		if finding.Action == "ask-user" && finding.File == "README.md" && strings.Contains(finding.Description, "Document discarded a repeated repair") {
			discardRow = true
		}
	}
	if !discardRow {
		t.Errorf("discard notice is not an ask-user finding row alongside the earlier gap: %s", out.Findings)
	}
	passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
	if err != nil || len(passes) != 2 || passes[1].FindingsJSON == nil || !strings.Contains(*passes[1].FindingsJSON, "Document discarded a repeated repair of README.md") {
		t.Errorf("discard notice was not persisted alongside the pending gap: %+v, %v", passes, err)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("created %s commits", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("suppressed addition remains: %s", got)
	}
}

func TestDocumentConvergenceCountsAgentCommitsAndFailsInterruptedPassClosed(t *testing.T) {
	for _, scenario := range []string{"over budget", "interrupted", "dirty entry", "staged dirty entry"} {
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
			dirtyEntry := scenario == "dirty entry" || scenario == "staged dirty entry"
			if dirtyEntry {
				if err := os.WriteFile(filepath.Join(dir, "existing.md"), []byte("owner work"), 0o644); err != nil {
					t.Fatal(err)
				}
				if scenario == "staged dirty entry" {
					gitCmd(t, dir, "add", "existing.md")
				}
			}
			statusBefore := gitStatusPorcelain(t, dir)
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
			// The clean-entry contract refuses both index and worktree changes;
			// it must never discard pre-existing work to make Document runnable.
			if dirtyEntry {
				if !strings.Contains(err.Error(), "clean worktree") {
					t.Fatalf("expected clean-entry refusal, got %v", err)
				}
				if got := gitStatusPorcelain(t, dir); got != statusBefore {
					t.Fatalf("dirty entry changed staging: %q, want %q", got, statusBefore)
				}
				if got, err := os.ReadFile(filepath.Join(dir, "existing.md")); err != nil || string(got) != "owner work" {
					t.Fatalf("dirty entry lost owner work: %q, %v", got, err)
				}
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

func TestDocumentConvergenceRetainsEveryDiscardedGap(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fmt.Sprint(calls)), 0o644); err != nil {
			t.Fatal(err)
		}
		descriptions := []string{"A", "B"}
		if calls > 1 {
			descriptions = []string{"B", "C"}
		}
		if calls > 2 {
			descriptions = []string{"D"}
		}
		findings := Findings{Summary: "unresolved gaps"}
		for _, description := range descriptions {
			findings.Items = append(findings.Items, types.Finding{Severity: "warning", Description: description, Action: "ask-user"})
		}
		raw, _ := json.Marshal(findings)
		return &agent.Result{Output: raw}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	for pass := 0; pass < 3; pass++ {
		out, err := (&DocumentStep{}).Execute(sc)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 0 {
			continue
		}
		var findings Findings
		if err := json.Unmarshal([]byte(out.Findings), &findings); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, f := range findings.Items {
			seen[f.Description] = true
		}
		expected := []string{"A", "B", "C"}
		if pass > 1 {
			expected = append(expected, "D")
		}
		for _, description := range expected {
			if !seen[description] {
				t.Errorf("pass %d lost %s: %s", pass, description, out.Findings)
			}
		}
	}
}

func TestDocumentConvergenceNewRepairAfterHeadChange(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		writeAndCommit(fmt.Sprintf("guide%d.md", calls), "repaired\n")(t, dir)
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"repair guide"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	writeAndCommit("feature.txt", "review repair\n")(t, dir)
	sc.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, sc.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "show", sc.Run.HeadSHA+":guide2.md"); got != "repaired" {
		t.Fatalf("repair lost: %q", got)
	}
}

func TestDocumentConvergenceEveryCommitPath(t *testing.T) {
	for _, reverted := range []bool{false, true} {
		t.Run(fmt.Sprint(reverted), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 {
					writeAndCommit("README.md", "first\n")(t, dir)
					if reverted {
						deleteAndCommit("README.md")(t, dir)
					}
					writeAndCommit("guide.md", "guide\n")(t, dir)
				} else {
					writeAndCommit("README.md", "second\n")(t, dir)
					if reverted {
						deleteAndCommit("README.md")(t, dir)
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"gap","action":"ask-user"}],"summary":"guide"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			sc.Config.Document.MaxCommitsPerRun = 6
			if _, err := (&DocumentStep{}).Execute(sc); err != nil {
				t.Fatal(err)
			}
			accepted := sc.Run.HeadSHA
			if _, err := (&DocumentStep{}).Execute(sc); err != nil {
				t.Fatal(err)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
				t.Fatalf("repeat advanced head: %s != %s", got, accepted)
			}
			if !reverted {
				if got := gitCmd(t, dir, "show", "HEAD:README.md"); got != "first" {
					t.Fatalf("repeat content survived: %q", got)
				}
			}
		})
	}
}

func TestDocOnlyCarryRejectsHiddenGitlink(t *testing.T) {
	for _, caller := range []string{"Document", "Push"} {
		t.Run(caller, func(t *testing.T) {
			for _, setting := range []string{"diff.ignoreSubmodules", "submodule.dependency.ignore"} {
				t.Run(setting, func(t *testing.T) {
					dir, base, head := setupGitRepo(t)
					gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",dependency")
					if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"dependency\"]\n path = dependency\n url = ./dependency\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, dir, "add", ".gitmodules")
					gitCmd(t, dir, "commit", "-qm", "add dependency")
					head = gitCmd(t, dir, "rev-parse", "HEAD")
					sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{Lint: "true"})
					recordReviewApproval(t, sc, head)
					recordTestedHead(t, sc, head)
					gitCmd(t, dir, "config", setting, "all")
					gitCmd(t, dir, "update-index", "--cacheinfo", "160000,"+base+",dependency")
					if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, dir, "add", "README.md")
					gitCmd(t, dir, "commit", "-qm", "update dependency and guide")
					to := gitCmd(t, dir, "rev-parse", "HEAD")
					recordDocumentPass(t, sc, head, to)
					sc.Run.HeadSHA = to
					if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, to); err != nil {
						t.Fatal(err)
					}
					if caller == "Document" {
						if err := carryDocumentValidation(sc, head, to); err != nil {
							t.Fatal(err)
						}
						if got := testedHead(t, sc); got != head {
							t.Errorf("Document carried over gitlink: %s", got)
						}
					} else {
						restart, err := reviewRestartNeededForPush(sc, "", to)
						if err != nil || !restart {
							t.Errorf("Push carried over gitlink: restart=%v err=%v", restart, err)
						}
					}
				})
			}
		})
	}
}

func TestDocumentCandidatePathsIncludesEveryWorkspaceLayer(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{Lint: "true"})
	writeAndCommit("committed.md", "temporary\n")(t, dir)
	deleteAndCommit("committed.md")(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "staged.md"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "staged.md")
	if err := os.Remove(filepath.Join(dir, "staged.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, untracked, err := documentCandidatePaths(sc, head)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	for _, p := range []string{"committed.md", "staged.md", "feature.txt", "untracked.md"} {
		if !seen[p] {
			t.Errorf("missing %s: %q", p, paths)
		}
	}
	if len(untracked) != 1 || untracked[0] != "untracked.md" {
		t.Fatalf("untracked cleanup paths: %q", untracked)
	}
}
