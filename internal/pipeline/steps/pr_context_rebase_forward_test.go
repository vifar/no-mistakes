package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPRContextGuardAcceptsTheRunsOwnRebaseAdvance covers the case measured on
// run 01M5HC8ADAB4AP32KNMVJMR42B (vifar/no-mistakes#136): the target branch
// advanced, so the run rebased - and the post-rebase guard then refused the
// run's OWN rebase:
//
//	check PR context after rebase: PR comparison changed after rebase; start a
//	new run for the current target and head: run is not eligible for a target
//	advance rewind: PR identity or local head differs from the recorded
//	comparison
//
// Both facts are needed, exactly as in production: the TARGET advanced (so
// the comparison changed and the rewind machinery classifies it), and the run
// rebased (so the local head moved to a commit that is NOT a descendant of
// the one the receipt names).
//
// A rebase REWRITES the head: the same logical changes land on a new base, so
// the ancestry proof the other steps use cannot apply to it.
func TestPRContextGuardAcceptsTheRunsOwnRebaseAdvance(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	selection := pipeline.PRTargetSelection{PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
		t.Fatal(err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	preRebaseHead := receipt.LocalHeadSHA

	// The TARGET advances: main moves on. This is what forces the rebase.
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "upstream.txt")
	gitCmd(t, dir, "commit", "-m", "upstream advances")
	newTarget := gitCmd(t, dir, "rev-parse", "HEAD")

	// The run REBASES onto the new target: a NEW commit, not a descendant of
	// the one it replaces.
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", newTarget)
	rebasedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if rebasedHead == preRebaseHead {
		t.Fatal("the rebase did not change the head")
	}
	if isAncestor(t.Context(), dir, preRebaseHead, rebasedHead) {
		t.Fatal("the rebased head is a descendant; the fixture must model a REWRITE")
	}
	if got := gitCmd(t, dir, "show", "HEAD:feature.txt"); got != "feature code" {
		t.Fatalf("rebase lost the submitted feature: %q", got)
	}
	// The rebase leaves the worktree on the rebased head; the guard reads the
	// worktree's own head to build the candidate comparison.
	gitCmd(t, dir, "checkout", "-B", "feature", rebasedHead)

	// The run's own record: it is now working on the rebased head, and the
	// target it compared against is the advanced one.
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, rebasedHead); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = rebasedHead

	// The post-rebase guard: the Rebase step completed, so the receipt must
	// follow the run's own rebase.
	sctx.Config.Rebase.MaxTargetAdvanceRewinds = 3
	sctx.PRContextAfterStep = true
	sctx.PRContextRebaseCompleted = true
	decision, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false)
	if err != nil {
		t.Fatalf("the guard refused the run's own rebase: %v", err)
	}
	// The target advanced, so the run rewinds to Rebase - the same outcome a
	// target advance without a rebase produces. What must NOT happen is the
	// refusal that told the operator to start a new run.
	if decision.RestartFrom != types.StepRebase {
		t.Fatalf("the run's own rebase asked for a restart from %q, want %q", decision.RestartFrom, types.StepRebase)
	}
	advanced, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.LocalHeadSHA != rebasedHead {
		t.Fatalf("the receipt did not follow the run's own rebase: %+v", advanced)
	}
}

// TestPRContextGuardRefusesAForeignHeadMoveAfterRebase is the negative half:
// the acceptance above must not become a general licence for a head that the
// run did not cause.
//
// After a completed Rebase, another publisher moves the branch. The run's own
// record still names its rebased head, so the moved head is NOT the run's
// head, and the guard must refuse it exactly as it refused an unexplained
// change before the fix.
func TestPRContextGuardRefusesAForeignHeadMoveAfterRebase(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	selection := pipeline.PRTargetSelection{PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
		t.Fatal(err)
	}

	// The run rebases onto the advanced target, exactly as in the accepted case.
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "upstream.txt")
	gitCmd(t, dir, "commit", "-m", "upstream advances")
	newTarget := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", newTarget)
	rebasedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-B", "feature", rebasedHead)
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, rebasedHead); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = rebasedHead

	// Another publisher moves the branch after the rebase. The worktree now
	// holds THEIR head, which is not the run's head.
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "another publisher moved the branch")

	sctx.Config.Rebase.MaxTargetAdvanceRewinds = 3
	sctx.PRContextAfterStep = true
	sctx.PRContextRebaseCompleted = true
	decision, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, false)
	if err == nil {
		t.Fatalf("accepted a foreign head move after the run's own rebase: %+v", decision)
	}
	if !strings.Contains(err.Error(), "PR comparison changed after rebase") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	t.Logf("refused the foreign head move as required: %v", err)
}

// A rewritten local head is not sufficient authority. The intent for #174
// requires a completed Rebase and unchanged PR, source, target and forge head.
// Each case breaks exactly one of those conditions and must preserve the receipt.
func TestPRContextGuardRefusesUnprovenRebaseAdvance(t *testing.T) {
	for _, boundary := range []string{"incomplete", "PR identity", "source repository", "source branch", "target branch", "forge head"} {
		t.Run(boundary, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			selected := pipeline.PRTargetSelection{
				PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo",
				SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main",
			}
			if _, err := guardPRContextWithSelection(c, types.StepPR, selected, false); err != nil {
				t.Fatal(err)
			}
			before, err := c.DB.GetRunPRContext(c.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "checkout", "main")
			if err := os.WriteFile(filepath.Join(dir, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "upstream.txt")
			gitCmd(t, dir, "commit", "-m", "advance target")
			gitCmd(t, dir, "branch", "release")
			gitCmd(t, dir, "checkout", "feature")
			gitCmd(t, dir, "rebase", "main")
			c.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
				t.Fatal(err)
			}
			c.Config.Rebase.MaxTargetAdvanceRewinds = 3
			c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
			switch boundary {
			case "incomplete":
				c.PRContextRebaseCompleted = false
			case "PR identity":
				selected.PRURL = "https://github.com/test/repo/pull/43"
			case "source repository":
				selected.SourceRepo = "other/repo"
			case "source branch":
				selected.SourceBranch = "other"
			case "target branch":
				selected.TargetBranch = "release"
			case "forge head":
				selected.ForgeHeadSHA = c.Run.HeadSHA
			}
			decision, err := guardPRContextWithSelection(c, types.StepRebase, selected, false)
			if err == nil {
				t.Fatalf("accepted %s: %+v", boundary, decision)
			}
			after, readErr := c.DB.GetRunPRContext(c.Run.ID)
			if readErr != nil || after == nil || *after != *before {
				t.Fatalf("refusal changed the receipt: before=%+v after=%+v error=%v", before, after, readErr)
			}
			t.Logf("refused %s and preserved the receipt: %v", boundary, err)
		})
	}
}
