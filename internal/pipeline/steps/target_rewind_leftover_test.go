package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTargetRewindCommitsLeftoverValidationEdits is the measured controller
// failure (issue #113): the Test agent left uncommitted edits in the run
// worktree, the target advanced, and the rewind integration refused with
// git's "cannot rebase: You have unstaged changes". The edits must be
// committed as a pipeline correction so the rewind can proceed.
func TestTargetRewindCommitsLeftoverValidationEdits(t *testing.T) {
	c, selected := rewindFixture(t)
	head := c.Run.HeadSHA

	// The target advances under the run.
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
	if err != nil || decision.RestartFrom != types.StepRebase {
		t.Fatalf("target advance did not rewind: %+v %v", decision, err)
	}

	// The Test agent edited a tracked file and did not commit it.
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("validated by the test agent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "feature.txt")

	// The rewind integration now runs against a dirty worktree.
	c.PRContextAfterStep = false
	out, err := (&RebaseStep{}).Execute(c)
	if err != nil {
		t.Fatalf("rewind refused on a dirty worktree: %v", err)
	}
	if out == nil || out.NeedsApproval {
		t.Fatalf("rewind did not integrate: %+v", out)
	}

	// The agent's edit survived, as a pipeline commit.
	content, err := os.ReadFile(filepath.Join(c.WorkDir, "feature.txt"))
	if err != nil || string(content) != "validated by the test agent\n" {
		t.Fatalf("the agent's edit was lost: %q %v", content, err)
	}
	subject := gitCmd(t, c.WorkDir, "log", "-1", "--format=%s")
	if !strings.Contains(subject, "test") {
		t.Fatalf("the leftover commit is not attributed to the step that left it: %q", subject)
	}
	// The commit is attributed to the pipeline, not to an AI.
	body := gitCmd(t, c.WorkDir, "log", "-1", "--format=%B")
	for _, forbidden := range []string{"Co-Authored-By", "Claude", "GPT", "Generated with"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the leftover commit carries AI attribution %q: %q", forbidden, body)
		}
	}
	// The rewind is complete: the pending rewind is consumed and the new head
	// carries the advanced target.
	c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	c.PRContextRebaseCompleted = false
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil || pending != nil {
		t.Fatalf("pending rewind did not complete: %+v %v", pending, err)
	}
	rebased := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if rebased == head {
		t.Fatal("the rewind did not move the head")
	}
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", pendingTargetSHA(t, c), rebased); err != nil {
		t.Fatalf("the rewound head does not contain the advanced target: %v", err)
	}
	// The worktree is clean apart from the agent's own edits, which are now
	// committed: a second rewind would find nothing to commit.
	committed, err := commitLeftoverValidationEdits(c)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("a second commit happened with nothing left to commit")
	}
}

// TestTargetRewindKeepsUntrackedToolScratchOutOfTheBranch proves the leftover
// commit does not sweep a test harness's scratch (a live database directory,
// for example) into the pushed branch.
func TestTargetRewindKeepsUntrackedToolScratchOutOfTheBranch(t *testing.T) {
	c, selected := rewindFixture(t)
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	// The Test agent left a scratch directory behind.
	scratch := filepath.Join(c.WorkDir, ".journal-live")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "postgres.log"), []byte("scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// And a real tracked edit.
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("validated by the test agent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "feature.txt")

	c.PRContextAfterStep = false
	out, err := (&RebaseStep{}).Execute(c)
	if err != nil {
		t.Fatalf("rewind refused on a dirty worktree: %v", err)
	}
	if out == nil || out.NeedsApproval {
		t.Fatalf("rewind did not integrate: %+v", out)
	}
	// The scratch never entered the branch.
	tracked := gitCmd(t, c.WorkDir, "ls-files")
	if strings.Contains(tracked, ".journal-live") {
		t.Fatalf("tool scratch entered the branch: %s", tracked)
	}
	// The real edit did.
	if !strings.Contains(tracked, "feature.txt") {
		t.Fatalf("the agent's edit was lost: %s", tracked)
	}
}

// TestTargetRewindLeftoverCommitFailureNamesTheStepAndPaths proves a failure
// to commit the leftover edits says WHICH step left them and WHICH paths, not
// git's generic message.
func TestTargetRewindLeftoverCommitFailureNamesTheStepAndPaths(t *testing.T) {
	c, selected := rewindFixture(t)
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	// A protected path makes the staging refuse.
	c.Config.ProtectedPaths = []string{"feature.txt"}
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("validated by the test agent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.PRContextAfterStep = false
	_, err := (&RebaseStep{}).Execute(c)
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

// TestTargetRewindAttributesTheProducerFromArchivedEvidence proves the
// attribution survives the rewind's own reset. The classification
// transaction resets every step from Rebase to pending, so a lookup over the
// LIVE rows finds no completed step and blames the wrong one. The ARCHIVED
// rewind history is the evidence that keeps the attribution honest.
func TestTargetRewindAttributesTheProducerFromArchivedEvidence(t *testing.T) {
	c, selected := rewindFixture(t)
	// The run completed Review and Test before the target moved.
	for _, name := range []types.StepName{types.StepReview, types.StepTest} {
		step, err := c.DB.InsertStepResult(c.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.DB.CompleteStep(step.ID, 0, 1, "step.log"); err != nil {
			t.Fatal(err)
		}
	}
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	// The rewind's own transaction reset the live step rows.
	live, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range live {
		if row.StepName == types.StepTest && row.Status == types.StepStatusCompleted {
			t.Fatal("the fixture did not reset the live step rows")
		}
	}
	// The archived evidence still names the producer.
	producer, err := rewindProducerStep(c)
	if err != nil {
		t.Fatal(err)
	}
	if producer != string(types.StepTest) {
		t.Fatalf("the producer = %q, want test", producer)
	}
}

// TestTargetRewindWithoutLeftoverEditsCommitsNothing proves the ordinary
// rewind path is unchanged when the worktree is already clean.
func TestTargetRewindWithoutLeftoverEditsCommitsNothing(t *testing.T) {
	c, selected := rewindFixture(t)
	head := c.Run.HeadSHA
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	c.PRContextAfterStep = false
	out, err := (&RebaseStep{}).Execute(c)
	if err != nil || out == nil || out.NeedsApproval {
		t.Fatalf("rewind did not integrate: %+v %v", out, err)
	}
	if got := gitCmd(t, c.WorkDir, "rev-list", "--count", head+"..HEAD"); got != "2" {
		t.Fatalf("a clean rewind produced %s commits, want 2 (the rebase and the advanced target)", got)
	}
	subject := gitCmd(t, c.WorkDir, "log", "-1", "--format=%s")
	if strings.Contains(subject, "validation edits") {
		t.Fatalf("a clean rewind committed a leftover-edit commit: %q", subject)
	}
	// No leftover-edit commit exists anywhere in the new history.
	subjects := gitCmd(t, c.WorkDir, "log", "--format=%s", head+"..HEAD")
	if strings.Contains(subjects, "validation edits") {
		t.Fatalf("a clean rewind committed a leftover-edit commit: %q", subjects)
	}
}

// TestAdvancePendingTargetRewindHeadRefusesAnUnrelatedHead proves the pending
// rewind's head binding cannot be moved to an unrelated commit.
func TestAdvancePendingTargetRewindHeadRefusesAnUnrelatedHead(t *testing.T) {
	c, selected := rewindFixture(t)
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil || pending == nil {
		t.Fatalf("no pending rewind: %+v %v", pending, err)
	}
	other := strings.Repeat("a", 40)
	// The database refuses a head the caller could not prove is a descendant.
	if err := c.DB.AdvancePendingTargetRewindHead(c.Run.ID, pending.LocalHeadSHA, other, false); err == nil {
		t.Fatal("an unrelated head was bound to the pending rewind")
	}
	if err := c.DB.AdvancePendingTargetRewindHead(c.Run.ID, strings.Repeat("b", 40), other, true); err == nil {
		t.Fatal("a wrong expected head was accepted")
	}
	if err := c.DB.AdvancePendingTargetRewindHead(c.Run.ID, pending.LocalHeadSHA, other, true); err != nil {
		t.Fatalf("a proven descendant was refused: %v", err)
	}
	after, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil || after.LocalHeadSHA != other {
		t.Fatalf("the pending rewind's head did not follow the proven descendant: %+v %v", after, err)
	}
}
