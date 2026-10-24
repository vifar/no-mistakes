package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// documentFixture builds a run that has completed Review and Test and is
// entering the Document step with a clean worktree.
func documentFixture(t *testing.T) *pipeline.StepContext {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	remote := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, dir, "clone", "--bare", dir, remote)
	gitCmd(t, dir, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, dir, "remote", "set-url", "origin", remote)
	c.Repo.UpstreamURL = remote
	c.Config.Rebase.MaxTargetAdvanceRewinds = 3
	selected := pipeline.PRTargetSelection{TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepReview, types.StepTest} {
		step, err := c.DB.InsertStepResult(c.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.DB.SetStepFindings(step.ID, `{"findings":[],"reviewed_paths":["feature.txt"]}`); err != nil {
			t.Fatal(err)
		}
		if name == types.StepReview {
			err = c.DB.CompleteReviewStep(step.ID, c.Run.ID, head, 0, 1, "review.log")
		} else {
			err = c.DB.CompleteStep(step.ID, 0, 1, "step.log")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	recordReviewApproval(t, c, head)
	return c
}

// TestDocumentStep_CommitsThePredecessorLeftoverEditsAndRestartsFromReview
// is the measured general-root failure: the Test agent edited a tracked file
// and did not commit it, and the Document step refused with "Document
// requires a clean worktree so convergence can safely discard only its own
// edits", killing the run.
//
// The edits are preserved as an attributed pipeline commit and the run
// returns to Review, so they are reviewed before anything is published.
func TestDocumentStep_CommitsThePredecessorLeftoverEditsAndRestartsFromReview(t *testing.T) {
	c := documentFixture(t)
	// The Test agent left an edit it did not commit.
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("validated by the test agent\n"), 0600); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&DocumentStep{}).Execute(c)
	if err != nil {
		t.Fatalf("the document step killed the run over the predecessor's edits: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("the document step did not ask for a re-review: %+v", outcome)
	}
	// The edit survived, as a pipeline commit attributed to the step that
	// left it.
	content, err := os.ReadFile(filepath.Join(c.WorkDir, "feature.txt"))
	if err != nil || string(content) != "validated by the test agent\n" {
		t.Fatalf("the agent's edit was lost: %q %v", content, err)
	}
	subject := gitCmd(t, c.WorkDir, "log", "-1", "--format=%s")
	if !strings.Contains(subject, "test") {
		t.Fatalf("the leftover commit is not attributed to the step that left it: %q", subject)
	}
	body := gitCmd(t, c.WorkDir, "log", "-1", "--format=%B")
	for _, forbidden := range []string{"Co-Authored-By", "Claude", "GPT", "Generated with"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the leftover commit carries AI attribution %q: %q", forbidden, body)
		}
	}
	// The worktree is clean now, so Document's own clean-entry contract
	// holds on the next pass.
	status := gitCmd(t, c.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("the worktree is still dirty: %q", status)
	}
	// The run's head follows the commit.
	if c.Run.HeadSHA != gitCmd(t, c.WorkDir, "rev-parse", "HEAD") {
		t.Fatalf("the run head did not follow the commit: %s", c.Run.HeadSHA)
	}
}

// TestDocumentStep_RefusesWhenTheLeftoverEditsCannotBeCommitted proves a
// refusal still fails closed, and names the step and the paths.
func TestDocumentStep_RefusesWhenTheLeftoverEditsCannotBeCommitted(t *testing.T) {
	c := documentFixture(t)
	// A protected path makes the staging refuse.
	c.Config.ProtectedPaths = []string{"feature.txt"}
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("validated by the test agent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := (&DocumentStep{}).Execute(c)
	if err == nil {
		t.Fatal("a protected leftover edit was committed")
	}
	message := err.Error()
	if !strings.Contains(message, "test step left uncommitted edits") {
		t.Fatalf("the refusal does not name the step: %v", err)
	}
	if !strings.Contains(message, "feature.txt") {
		t.Fatalf("the refusal does not name the dirty paths: %v", err)
	}
}

// TestDocumentStep_RefusesASecondLeftoverCommit proves the loop is bounded:
// a run preserves ONE predecessor's leftover edits and sends them through
// re-review. A second one is not committed: Test's edits go back to Test,
// which reports them for a repair (see test_leftover_test.go).
func TestDocumentStep_RefusesASecondLeftoverCommit(t *testing.T) {
	c := documentFixture(t)
	// The first leftover edit is preserved.
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("validated by the test agent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outcome, err := (&DocumentStep{}).Execute(c)
	if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("the first leftover edit was not preserved: %+v %v", outcome, err)
	}
	// The second one is refused.
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("and again\n"), 0600); err != nil {
		t.Fatal(err)
	}
	headAfterFirst := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	outcome, err = (&DocumentStep{}).Execute(c)
	if err != nil || outcome == nil || outcome.RestartFrom != types.StepTest {
		t.Fatalf("the second set was not returned to Test: %+v %v", outcome, err)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != headAfterFirst {
		t.Fatalf("a second leftover commit was allowed: %s", got)
	}
	// The budget is spent exactly once.
	allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("the leftover-validation budget was not charged")
	}
}

// TestDocumentStep_RealFileBesideScratchIsNotHidden proves a real untracked
// file is preserved even when scratch sits beside it. The status parsing must
// be NUL-delimited: without it, two untracked files share one record and the
// scratch check swallows the real one.
func TestDocumentStep_RealFileBesideScratchIsNotHidden(t *testing.T) {
	c := documentFixture(t)
	// The Test agent left scratch and a real new test file side by side.
	scratch := filepath.Join(c.WorkDir, ".journal-live")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "postgres.log"), []byte("scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.WorkDir, "new_test.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dirty, err := worktreeDirtyPathsOutsideScratch(c)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, path := range dirty {
		if path == "new_test.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the real file beside the scratch was hidden: %+v", dirty)
	}
	// The step preserves the real file and sends the run back to Review.
	outcome, err := (&DocumentStep{}).Execute(c)
	if err != nil {
		t.Fatalf("the documentation pass failed: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("the real file did not cause a re-review: %+v", outcome)
	}
	// The real file is preserved, and the scratch never enters the branch.
	tracked := gitCmd(t, c.WorkDir, "ls-files")
	if !strings.Contains(tracked, "new_test.go") {
		t.Fatalf("the real file was lost: %s", tracked)
	}
	if strings.Contains(tracked, ".journal-live") {
		t.Fatalf("tool scratch entered the branch: %s", tracked)
	}
}

// TestDocumentStep_ToolScratchAloneDoesNotBlockTheStep proves recognized tool
// scratch is not an edit: a test harness's live database left behind by the
// Test agent must not stop the documentation pass, and it must never enter
// the branch.
func TestDocumentStep_ToolScratchAloneDoesNotBlockTheStep(t *testing.T) {
	c := documentFixture(t)
	// The document agent finds nothing to document, so the pass completes.
	c.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"nothing to document"}`)}, nil
	}}
	scratch := filepath.Join(c.WorkDir, ".journal-live")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "postgres.log"), []byte("scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outcome, err := (&DocumentStep{}).Execute(c)
	if err != nil {
		t.Fatalf("tool scratch stopped the documentation pass: %v", err)
	}
	if outcome != nil && outcome.RestartFrom != "" {
		t.Fatalf("tool scratch caused a restart: %+v", outcome)
	}
	tracked := gitCmd(t, c.WorkDir, "ls-files")
	if strings.Contains(tracked, ".journal-live") {
		t.Fatalf("tool scratch entered the branch: %s", tracked)
	}
}

func TestDocumentStep_PreservesInheritedScratchAcrossRollback(t *testing.T) {
	for _, initialStaged := range []bool{false, true} {
		for _, action := range []string{"untracked", "staged", "committed"} {
			t.Run(fmt.Sprintf("initial_staged_%t/%s", initialStaged, action), func(t *testing.T) {
				c := documentFixture(t)
				head := c.Run.HeadSHA
				c.Config.Document.MaxCommitsPerRun = 1
				path := ".journal-live/postgres.log"
				full := filepath.Join(c.WorkDir, path)
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte("live database log\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if initialStaged {
					gitCmd(t, c.WorkDir, "add", path)
				}
				c.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					if action == "committed" {
						gitCmd(t, c.WorkDir, "add", "-A")
					}
					for _, text := range []string{"first proposal\n", "second proposal\n"} {
						if err := os.WriteFile(filepath.Join(c.WorkDir, "guide.md"), []byte(text), 0600); err != nil {
							t.Fatal(err)
						}
						if action != "committed" {
							gitCmd(t, c.WorkDir, "reset", "--", path)
						}
						gitCmd(t, c.WorkDir, "add", "guide.md")
						gitCmd(t, c.WorkDir, "commit", "-qm", "update guide")
					}
					if action == "staged" {
						gitCmd(t, c.WorkDir, "add", "-A")
					}
					return &agent.Result{Output: []byte(`{"findings":[],"summary":"updated guide"}`)}, nil
				}}
				out, err := (&DocumentStep{}).Execute(c)
				if err != nil || out == nil || !out.NeedsApproval {
					t.Fatalf("over-budget proposal did not park cleanly: %+v %v", out, err)
				}
				if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
					t.Fatalf("proposal head survived: %s", got)
				}
				if _, err := os.Stat(filepath.Join(c.WorkDir, "guide.md")); !os.IsNotExist(err) {
					t.Fatalf("proposal file survived: %v", err)
				}
				if got, err := os.ReadFile(full); err != nil || string(got) != "live database log\n" {
					t.Fatalf("inherited scratch lost: %q %v", got, err)
				}
				dirty, err := worktreeDirtyPathsOutsideScratch(c)
				if err != nil || len(dirty) != 0 {
					t.Fatalf("rollback left real edits: %q %v", dirty, err)
				}
			})
		}
	}
}

func TestDocumentStep_StagedScratchDoesNotSpendCommitBudget(t *testing.T) {
	c := documentFixture(t)
	c.Config.Document.MaxCommitsPerRun = 1
	path := filepath.Join(c.WorkDir, ".journal-live/postgres.log")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "-A")
	c.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, c.WorkDir, "reset", "--", ".journal-live/postgres.log")
		if err := os.WriteFile(filepath.Join(c.WorkDir, "guide.md"), []byte("accepted guide\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, c.WorkDir, "add", "guide.md")
		gitCmd(t, c.WorkDir, "commit", "-qm", "update guide")
		gitCmd(t, c.WorkDir, "add", "-A")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"updated guide"}`)}, nil
	}}
	out, err := (&DocumentStep{}).Execute(c)
	if err != nil || out == nil || out.NeedsApproval {
		t.Fatalf("scratch rejected an allowed commit: %+v %v", out, err)
	}
	if got, err := os.ReadFile(filepath.Join(c.WorkDir, "guide.md")); err != nil || string(got) != "accepted guide\n" {
		t.Fatalf("allowed proposal lost: %q %v", got, err)
	}
	if got := gitCmd(t, c.WorkDir, "ls-tree", "-r", "--name-only", "HEAD", "--", ".journal-live"); got != "" {
		t.Fatalf("scratch published: %s", got)
	}
}
