package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// leftoverEvidenceAgent is a passing Test evidence agent that edits a tracked
// test file and does not commit it, as the agent on vifar/controller#8241 did
// with krakenPromotionQuantityReplacement.migration.test.ts.
func leftoverEvidenceAgent(dir, content string) *mockAgent {
	return &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte(content), 0o600); err != nil {
				return nil, err
			}
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
}

func spendLeftoverBudget(t *testing.T, c *pipeline.StepContext) {
	t.Helper()
	if err := c.DB.RecordLeftoverValidationCommit(c.Run.ID, c.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
}

// TestTestStep_CommitsItsOwnLeftoverEditsSoDocumentPasses is the measured
// failure: Test completed with its agent's uncommitted edit in the worktree.
// Test now owns the edit: it commits it as an attributed Test correction and
// sends the run back to Review, and Document then enters a clean worktree.
func TestTestStep_CommitsItsOwnLeftoverEditsSoDocumentPasses(t *testing.T) {
	c := documentFixture(t)
	head := c.Run.HeadSHA
	c.Agent = leftoverEvidenceAgent(c.WorkDir, "new assertions from the test agent\n")

	outcome, err := (&TestStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("Test did not send its committed edits back to Review: %+v", outcome)
	}
	if subject := lastCommitMessage(t, c.WorkDir); subject != "no-mistakes(test): keep test validation edits" {
		t.Fatalf("the edit was not committed as a Test correction: %q", subject)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got == head || c.Run.HeadSHA != got {
		t.Fatalf("the run head did not follow the Test commit: run=%s head=%s", c.Run.HeadSHA, got)
	}
	if dirty, err := worktreeDirtyPathsOutsideScratch(c); err != nil || len(dirty) != 0 {
		t.Fatalf("Test handed on a dirty worktree: %q %v", dirty, err)
	}
	if allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID); err != nil || allowed {
		t.Fatalf("the leftover budget was not charged: allowed=%v err=%v", allowed, err)
	}

	c.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"nothing to document"}`)}, nil
	}}
	if _, err := (&DocumentStep{}).Execute(c); err != nil {
		t.Fatalf("Document refused after Test settled its edits: %v", err)
	}
}

// TestTestStep_ReportsLeftoverEditsOnceTheBudgetIsSpent is the repeat: the
// re-validation after the first correction wrote the same file again. Test
// must not complete silently and hand it on; it reports the edit for a
// repair, and Approve is refused.
func TestTestStep_ReportsLeftoverEditsOnceTheBudgetIsSpent(t *testing.T) {
	c := documentFixture(t)
	head := c.Run.HeadSHA
	spendLeftoverBudget(t, c)
	c.Agent = leftoverEvidenceAgent(c.WorkDir, "the same assertions again\n")

	outcome, err := (&TestStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.RestartFrom != "" || !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("Test completed over an unowned edit: %+v", outcome)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("Test committed past the spent budget: %s", got)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	var repair *types.Finding
	for i := range findings.Items {
		if findings.Items[i].ID == FindingIDTestLeftoverEdits {
			repair = &findings.Items[i]
		}
	}
	if repair == nil || repair.Action != types.ActionAutoFix || !strings.Contains(repair.Description, "feature.txt") {
		t.Fatalf("the edit was not reported for a repair: %+v", findings.Items)
	}
	if !pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) {
		t.Fatalf("Approve is not refused over the unowned edit: %+v", findings.Items)
	}
}

func TestTestStep_ReportsPreExistingLeftoversOnceTheBudgetIsSpent(t *testing.T) {
	for _, origin := range []string{"pre-existing", "configured-command", "document-cap"} {
		t.Run(origin, func(t *testing.T) {
			c := documentFixture(t)
			head := c.Run.HeadSHA
			spendLeftoverBudget(t, c)
			c.Agent = leftoverEvidenceAgent(c.WorkDir, "")
			if origin == "configured-command" {
				c.Config.Commands.Test = "echo command-edits > feature.txt"
			} else if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("inherited edit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c.WorkDir, "pending.txt"), []byte("inherited proposal\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			scratch := filepath.Join(c.WorkDir, ".journal-live")
			if err := os.MkdirAll(scratch, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scratch, "postgres.log"), []byte("scratch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if origin == "document-cap" {
				outcome, err := (&DocumentStep{}).runCapGapCheck(c, c.Run.BaseSHA, "main")
				if err != nil || outcome == nil || outcome.RestartFrom != types.StepTest {
					t.Fatalf("Document cap did not return the edits to Test: %+v %v", outcome, err)
				}
			}

			outcome, err := (&TestStep{}).Execute(c)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || outcome.RestartFrom != "" || !outcome.NeedsApproval || !outcome.AutoFixable {
				t.Fatalf("Test completed over inherited edits: %+v", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			var repair *types.Finding
			for i := range findings.Items {
				if findings.Items[i].ID == FindingIDTestLeftoverEdits {
					repair = &findings.Items[i]
				}
			}
			if repair == nil || repair.Action != types.ActionAutoFix {
				t.Fatalf("inherited edits have no repair finding: %+v", findings.Items)
			}
			for _, path := range []string{"feature.txt", "pending.txt"} {
				if !strings.Contains(repair.Description, path) {
					t.Fatalf("repair omitted %s: %+v", path, repair)
				}
			}
			if strings.Contains(repair.Description, ".journal-live") || !pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) {
				t.Fatalf("repair included scratch or allowed approval: %+v", findings.Items)
			}
			if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("Test committed inherited edits past the budget: %s", got)
			}
		})
	}
}

func TestTestStep_LeavesPreExistingWorkWhenTheBudgetIsAvailableOrRunIsAbsent(t *testing.T) {
	for _, recordedRun := range []bool{false, true} {
		name := "missing-run"
		if recordedRun {
			name = "budget-available"
		}
		t.Run(name, func(t *testing.T) {
			var c *pipeline.StepContext
			if recordedRun {
				c = documentFixture(t)
			} else {
				dir, base, head := setupGitRepo(t)
				c = newTestContext(t, nil, dir, base, head, config.Commands{})
			}
			head := c.Run.HeadSHA
			c.Agent = leftoverEvidenceAgent(c.WorkDir, "")
			if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("pre-existing work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			outcome, err := (&TestStep{}).Execute(c)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("Test claimed pre-existing work: %+v", outcome)
			}
			if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("Test committed pre-existing work: %s", got)
			}
			if got := readFile(t, filepath.Join(c.WorkDir, "feature.txt")); got != "pre-existing work\n" {
				t.Fatalf("Test changed pre-existing work: %q", got)
			}
		})
	}
}

func TestTestStep_MixedLeftoverEditsStayWithDocument(t *testing.T) {
	for _, origin := range []string{"pre-existing", "staged", "configured-command"} {
		t.Run(origin, func(t *testing.T) {
			c := documentFixture(t)
			head := c.Run.HeadSHA
			const inherited = "inherited work\n"
			const evidence = "evidence work\n"
			if origin == "configured-command" {
				c.Config.Commands.Test = "echo 'inherited work' > base.txt"
			} else {
				if err := os.WriteFile(filepath.Join(c.WorkDir, "base.txt"), []byte(inherited), 0o600); err != nil {
					t.Fatal(err)
				}
				if origin == "staged" {
					gitCmd(t, c.WorkDir, "add", "base.txt")
				}
			}
			c.Agent = leftoverEvidenceAgent(c.WorkDir, evidence)

			outcome, err := (&TestStep{}).Execute(c)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("Test did not leave mixed edits to Document: %+v", outcome)
			}
			if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head || c.Run.HeadSHA != head {
				t.Fatalf("Test committed mixed edits: head=%s run=%s", got, c.Run.HeadSHA)
			}
			if allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID); err != nil || !allowed {
				t.Fatalf("Test spent the correction budget: allowed=%v err=%v", allowed, err)
			}
			for path, want := range map[string]string{"base.txt": inherited, "feature.txt": evidence} {
				if got := readFile(t, filepath.Join(c.WorkDir, path)); got != want {
					t.Fatalf("Test lost %s: %q", path, got)
				}
			}

			c.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("Document invoked its agent before preserving mixed edits")
				return nil, nil
			}}
			outcome, err = (&DocumentStep{}).Execute(c)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || outcome.RestartFrom != types.StepReview {
				t.Fatalf("Document did not return mixed edits to Review: %+v", outcome)
			}
			if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got == head || c.Run.HeadSHA != got {
				t.Fatalf("Document did not record its boundary commit: head=%s run=%s", got, c.Run.HeadSHA)
			}
			for path, want := range map[string]string{"base.txt": strings.TrimSpace(inherited), "feature.txt": strings.TrimSpace(evidence)} {
				if got := gitCmd(t, c.WorkDir, "show", "HEAD:"+path); got != want {
					t.Fatalf("Document did not commit %s: %q", path, got)
				}
			}
			if dirty, err := worktreeDirtyPathsOutsideScratch(c); err != nil || len(dirty) != 0 {
				t.Fatalf("Document left mixed edits uncommitted: %q %v", dirty, err)
			}
			if allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID); err != nil || allowed {
				t.Fatalf("Document did not spend the correction budget: allowed=%v err=%v", allowed, err)
			}
		})
	}
}

// TestTestStep_RepairCommitsTheReportedLeftoverEdits proves the way forward:
// the fix round for the reported edit commits it through the normal Test fix
// path, validation re-runs on that head, and the worktree is clean.
func TestTestStep_RepairCommitsTheReportedLeftoverEdits(t *testing.T) {
	c := documentFixture(t)
	spendLeftoverBudget(t, c)
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("the same assertions again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			return &agent.Result{Output: json.RawMessage(`{"summary":"keep the new assertions"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	c.Fixing = true
	c.PreviousFindings = `{"findings":[{"id":"` + FindingIDTestLeftoverEdits + `","severity":"error","action":"auto-fix","file":"feature.txt","description":"uncommitted edits to feature.txt"}]}`

	outcome, err := (&TestStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.RestartFrom != "" {
		t.Fatalf("the repaired edit did not pass Test: %+v", outcome)
	}
	if subject := lastCommitMessage(t, c.WorkDir); subject != "no-mistakes(test): keep the new assertions" {
		t.Fatalf("the repair did not commit the edit: %q", subject)
	}
	if dirty, err := worktreeDirtyPathsOutsideScratch(c); err != nil || len(dirty) != 0 {
		t.Fatalf("the repair left the worktree dirty: %q %v", dirty, err)
	}
}

// TestTestStep_BlockingGateLeavesLeftoverEditsToItsFixRound proves a Test gate
// that already blocks, with budget left, is unchanged: its fix round commits
// the edits, so Test neither commits nor restarts on its own.
func TestTestStep_BlockingGateLeavesLeftoverEditsToItsFixRound(t *testing.T) {
	c := documentFixture(t)
	head := c.Run.HeadSHA
	c.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("half a fix\n"), 0o600); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","action":"auto-fix","description":"checkout fails"}],"summary":"","tested":["npm run e2e"],"testing_summary":"checkout failed","artifacts":[],"scenarios":[{"name":"user checks out","result":"fail","live":true,"evidence":"npm run e2e","reason":"500"}],"verdict":"no-go"}`)}, nil
	}}
	outcome, err := (&TestStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.RestartFrom != "" {
		t.Fatalf("the blocking gate changed: %+v", outcome)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("Test committed under a blocking gate: %s", got)
	}
	if strings.Contains(outcome.Findings, FindingIDTestLeftoverEdits) {
		t.Fatalf("a blocking gate with budget left reported the edit: %s", outcome.Findings)
	}
}

func TestTestStep_AskUserGatePreservesEvidenceEditsWithoutAutomaticCorrection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		carried bool
		action  string
	}{
		{"reported", false, types.ActionAskUser},
		{"carried", true, types.ActionAskUser},
		{"carried-default-action", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := documentFixture(t)
			head := c.Run.HeadSHA
			question := Finding{
				ID:          "intent-question",
				Severity:    types.FindingSeverityInfo,
				Action:      tc.action,
				Description: "Should this check preserve the existing behavior?",
			}
			evidence := map[string]json.RawMessage{}
			if err := json.Unmarshal([]byte(passingScenarioFindingsJSON), &evidence); err != nil {
				t.Fatal(err)
			}
			if !tc.carried {
				reported, err := json.Marshal([]Finding{question})
				if err != nil {
					t.Fatal(err)
				}
				evidence["findings"] = reported
			} else {
				c.Fixing, c.AutoFixRound = true, true
				c.PreviousFindings = `{"findings":[{"id":"selected-fix","severity":"error","action":"auto-fix","description":"fix the failing check"}]}`
				deferred, err := json.Marshal(Findings{Items: []Finding{question}})
				if err != nil {
					t.Fatal(err)
				}
				c.DeferredFindings = string(deferred)
			}
			c.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if opts.Purpose == "test-fix" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"no code changes needed"}`)}, nil
				}
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("evidence edit\n"), 0o600); err != nil {
					return nil, err
				}
				output, err := json.Marshal(evidence)
				return &agent.Result{Output: output}, err
			}}

			outcome, err := (&TestStep{}).Execute(c)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || !outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("Test bypassed the question: %+v", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || findings.Items[0].Description != question.Description || findings.Items[0].ActionOrDefault() != types.ActionAskUser {
				t.Fatalf("Test lost or changed the unanswered question: %+v", findings.Items)
			}
			if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head || c.Run.HeadSHA != head {
				t.Fatalf("Test committed despite the question: git=%s run=%s", got, c.Run.HeadSHA)
			}
			if allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID); err != nil || !allowed {
				t.Fatalf("Test spent the correction budget: allowed=%v err=%v", allowed, err)
			}
			if got := readFile(t, filepath.Join(c.WorkDir, "feature.txt")); got != "evidence edit\n" {
				t.Fatalf("Test lost the evidence edit: %q", got)
			}
		})
	}
}

// TestDocumentStep_SendsTestLeftoversBackToTestOnceTheBudgetIsSpent is the
// defence in depth: edits that still reach Document from Test after the
// run's correction is spent go back to Test, which owns them, instead of
// killing the run.
func TestDocumentStep_SendsTestLeftoversBackToTestOnceTheBudgetIsSpent(t *testing.T) {
	c := documentFixture(t)
	spendLeftoverBudget(t, c)
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("left by test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := (&DocumentStep{}).Execute(c)
	if err != nil {
		t.Fatalf("Document killed the run over Test's edits: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepTest {
		t.Fatalf("Document did not return the edits to Test: %+v", outcome)
	}
	if got, err := os.ReadFile(filepath.Join(c.WorkDir, "feature.txt")); err != nil || string(got) != "left by test\n" {
		t.Fatalf("the edit was lost: %q %v", got, err)
	}
	c.Agent = leftoverEvidenceAgent(c.WorkDir, "")
	outcome, err = (&TestStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.RestartFrom != "" || !outcome.NeedsApproval || !outcome.AutoFixable || !pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) {
		t.Fatalf("Test did not park for the returned edit: %+v", outcome)
	}
}

// TestDocumentStep_StillRefusesEditsNoTestStepOwns proves the clean-entry
// protection holds for edits Test did not leave: with the budget spent and
// Review the last completed step, Document refuses, keeps its wording, and
// names the step and the way forward.
func TestDocumentStep_StillRefusesEditsNoTestStepOwns(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	remote := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, dir, "clone", "--bare", dir, remote)
	gitCmd(t, dir, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, dir, "remote", "set-url", "origin", remote)
	c.Repo.UpstreamURL = remote
	if _, err := guardPRContextWithSelection(c, types.StepRebase, pipeline.PRTargetSelection{TargetBranch: "main"}, false); err != nil {
		t.Fatal(err)
	}
	review, err := c.DB.InsertStepResult(c.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DB.SetStepFindings(review.ID, `{"findings":[],"reviewed_paths":["feature.txt"]}`); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.CompleteReviewStep(review.ID, c.Run.ID, head, 0, 1, "review.log"); err != nil {
		t.Fatal(err)
	}
	recordReviewApproval(t, c, head)
	spendLeftoverBudget(t, c)
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("nobody owns this\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = (&DocumentStep{}).Execute(c)
	if err == nil {
		t.Fatal("Document accepted edits no step owns")
	}
	for _, want := range []string{"Document requires a clean worktree", "feature.txt", "the review step left them uncommitted", "already committed one set of leftover edits", "git -C"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not say %q", err, want)
		}
	}
}
