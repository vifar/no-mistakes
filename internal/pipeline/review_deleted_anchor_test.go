package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The finding the review raised on the file the branch later deleted.
const deletedAnchorFinding = `{"findings":[{"id":"review-1","severity":"error","file":"duplicate-suite.go","description":"duplicate test suite","action":"ask-user"}]}`

// deletedAnchorShape describes the branch history a fixture builds.
type deletedAnchorShape struct {
	merge bool
	// replacement: the commit that removed the file also carried a path with
	// it, which is how git records a rewritten rename.
	replacement bool
	// addedAfter: a later commit added a path, so the restored proof's
	// fail-closed rename guard blocks the finding.
	addedAfter bool
	// selectedWhilePresent: the operator selected the finding at a head where
	// the anchor file was still there.
	selectedWhilePresent bool
}

// deletedAnchorFixture reproduces the shape of issue #170.
//
// The branch adds the file the reviewer reports on, a fix deletes it again
// BEFORE the operator selects the finding, and the selection round therefore
// reviewed a head where the file was already gone. Nothing in the delivered
// change carries the file, and the restored-anchor proof cannot fire: no round
// that selected the finding ever saw the file differ from the target, so
// `git diff <target> <that head> -- <file>` is empty.
//
// A later complete clean review confirms the file is absent, and the carried
// finding still blocks the strict gate. That is the deadlock this fix closes:
// the deletion IS the resolution, and the branch's own history has to show the
// code was removed rather than moved.
func deletedAnchorFixture(t *testing.T, shape deletedAnchorShape) (*StepContext, *StepOutcome) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("kept.go", "target\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "target")
	target, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// The branch adds the file the reviewer will report on, and changes a file
	// of its own so the delivered change still has reviewable content.
	write("duplicate-suite.go", "duplicate suite\n")
	write("kept.go", "target\nchanged\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "add the duplicate suite")
	added, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// The fix removes it again: the answer to the reviewer's question was
	// "remove", so the file is gone before the operator selects the finding.
	if shape.merge {
		execGit(t, dir, "checkout", "-b", "anchor-main")
		execGit(t, dir, "checkout", "-b", "anchor-side")
		if shape.replacement {
			write("moved-suite.go", "rewritten implementation at another anchor\n")
			execGit(t, dir, "add", ".")
		}
		execGit(t, dir, "commit", "--allow-empty", "-m", "side branch")
		execGit(t, dir, "checkout", "anchor-main")
		execGit(t, dir, "commit", "--allow-empty", "-m", "main branch")
		execGit(t, dir, "merge", "--no-ff", "--no-commit", "anchor-side")
	}
	execGit(t, dir, "--literal-pathspecs", "rm", "--", "duplicate-suite.go")
	if shape.replacement && !shape.merge {
		write("moved-suite.go", "rewritten implementation at another anchor\n")
		execGit(t, dir, "add", ".")
	}
	execGit(t, dir, "commit", "-m", "remove the duplicate suite")
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	reviewed := []string{"kept.go"}
	reviewable := []string{"kept.go"}
	if shape.replacement {
		reviewed = append(reviewed, "moved-suite.go")
		reviewable = append(reviewable, "moved-suite.go")
	}
	if shape.addedAfter {
		write("later.go", "added later\n")
		execGit(t, dir, "add", ".")
		execGit(t, dir, "commit", "-m", "add a later file")
		head, err = git.HeadSHA(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		reviewed = append(reviewed, "later.go")
		reviewable = append(reviewable, "later.go")
	}
	base, digest, err := shipping.Compare(context.Background(), dir, target, head)
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.InsertRunWithShipping(repo.ID, "strict-feature", head, target, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main", TargetSHA: target})
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := database.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "owner/repo", SourceBranch: run.Branch, TargetBranch: "main", TargetSHA: target, HeadSHA: head, MergeBaseSHA: base, DiffDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main", TargetSHA: target}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	// Round 1: the review at the head where the file existed raised the finding.
	findings := deletedAnchorFinding
	round, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, added, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Round 2: the review at the head where the file is already gone reports the
	// finding again, and the operator selects it for a fix.
	selectedHead := head
	if shape.selectedWhilePresent {
		selectedHead = added
	}
	round, err = database.InsertReviewStepRound(step.ID, 2, "initial", &findings, nil, selectedHead, 1)
	if err != nil {
		t.Fatal(err)
	}
	selection := `["review-1"]`
	if err := database.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{Ctx: context.Background(), Run: run, Repo: repo, DB: database, WorkDir: dir, GateDir: filepath.Dir(p.DB()), StepResultID: step.ID}
	outcome := &StepOutcome{ReviewedPaths: reviewed, ReviewablePaths: reviewable, ReviewApprovedHeadSHA: head, ReviewComparisonGeneration: comparison.Generation}
	return sctx, outcome
}

// presentAnchorFixture is the same journey with the anchor file still in the
// tree: it is unchanged at the run head, the merge base and the target, so it
// is not part of the delivered change and the review never covered it. The
// strict restored-anchor proof needs a round that saw the anchor differ from
// the target, and there is none, so the finding keeps blocking.
func presentAnchorFixture(t *testing.T) (*StepContext, *StepOutcome) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("kept.go", "target\n")
	write("duplicate-suite.go", "duplicate suite\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "target")
	target, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	write("kept.go", "target\nchanged\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "change another file")
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	base, digest, err := shipping.Compare(context.Background(), dir, target, head)
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.InsertRunWithShipping(repo.ID, "strict-feature", head, target, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main", TargetSHA: target})
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := database.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "owner/repo", SourceBranch: run.Branch, TargetBranch: "main", TargetSHA: target, HeadSHA: head, MergeBaseSHA: base, DiffDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main", TargetSHA: target}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := deletedAnchorFinding
	round, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, head, 1)
	if err != nil {
		t.Fatal(err)
	}
	round, err = database.InsertReviewStepRound(step.ID, 2, "initial", &findings, nil, head, 1)
	if err != nil {
		t.Fatal(err)
	}
	selection := `["review-1"]`
	if err := database.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{Ctx: context.Background(), Run: run, Repo: repo, DB: database, WorkDir: dir, GateDir: filepath.Dir(p.DB()), StepResultID: step.ID}
	outcome := &StepOutcome{ReviewedPaths: []string{"kept.go"}, ReviewablePaths: []string{"kept.go"}, ReviewApprovedHeadSHA: head, ReviewComparisonGeneration: comparison.Generation}
	return sctx, outcome
}

// A carried finding whose anchor file is gone from the run head, the merge
// base and the target is resolved by a complete clean review at that head: the
// deletion is the resolution, and nothing in the delivered change carries the
// file the finding is about.
func TestStrictReviewResolutionDeletedAnchorClearsAfterCompleteCleanReview(t *testing.T) {
	sctx, outcome := deletedAnchorFixture(t, deletedAnchorShape{})
	got, _ := resolveReviewCarry(sctx, deletedAnchorFinding, []string{"review-1"}, outcome, `{"findings":[]}`)
	if got != "" {
		t.Fatalf("carried finding on a deleted file stayed outstanding: %s", got)
	}
}

// The same deletion, selected while the anchor file was still there and with
// a path added to the branch afterwards.
//
// The restored proof cannot fire on its own there: the anchor was already gone
// at the selection head, and a path added since the selection is the
// fail-closed rewritten-rename guard. The deletion is still the resolution, so
// the two proofs are independent.
func TestStrictReviewResolutionDeletedAnchorClearsWhenSelectedWhilePresent(t *testing.T) {
	sctx, outcome := deletedAnchorFixture(t, deletedAnchorShape{addedAfter: true, selectedWhilePresent: true})
	got, _ := resolveReviewCarry(sctx, deletedAnchorFinding, []string{"review-1"}, outcome, `{"findings":[]}`)
	if got != "" {
		t.Fatalf("carried finding on a deleted file stayed outstanding: %s", got)
	}
}

// Strictness is otherwise unchanged. A finding whose file still exists, a
// partial review, a review that reports something and a finding nobody
// selected all keep blocking.
func TestStrictReviewResolutionDeletedAnchorFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		fixture func(*testing.T) (*StepContext, *StepOutcome)
		change  func(*testing.T, *StepContext, *StepOutcome) (string, []string, string)
		current string
	}{
		{
			name:    "the anchor is still there",
			fixture: presentAnchorFixture,
		},
		{
			name: "the review covered only part of the change",
			change: func(t *testing.T, s *StepContext, o *StepOutcome) (string, []string, string) {
				o.ReviewablePaths = append(o.ReviewablePaths, "other.go")
				return deletedAnchorFinding, []string{"review-1"}, `{"findings":[]}`
			},
		},
		{
			name: "the review reported a finding",
			change: func(t *testing.T, s *StepContext, o *StepOutcome) (string, []string, string) {
				return deletedAnchorFinding, []string{"review-1"}, `{"findings":[{"id":"review-9","severity":"error","file":"kept.go","description":"something else","action":"ask-user"}]}`
			},
		},
		{
			name: "nobody selected the finding",
			change: func(t *testing.T, s *StepContext, o *StepOutcome) (string, []string, string) {
				return deletedAnchorFinding, nil, `{"findings":[]}`
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx, outcome := deletedAnchorFixture(t, deletedAnchorShape{})
			if tc.fixture != nil {
				sctx, outcome = tc.fixture(t)
			}
			raw, pending, current := deletedAnchorFinding, []string{"review-1"}, `{"findings":[]}`
			if tc.change != nil {
				raw, pending, current = tc.change(t, sctx, outcome)
			}
			got, _ := resolveReviewCarry(sctx, raw, pending, outcome, current)
			if got != raw {
				t.Fatalf("unproven resolution: %s", got)
			}
		})
	}
}

// The removal has to be a removal. A commit that removed the anchor and
// carried a replacement with it is how git records a rewritten rename, and the
// code is still in the change under another name, so the finding keeps
// blocking.
func TestStrictReviewResolutionDeletedAnchorKeepsReplacedCode(t *testing.T) {
	sctx, outcome := deletedAnchorFixture(t, deletedAnchorShape{replacement: true})
	got, _ := resolveReviewCarry(sctx, deletedAnchorFinding, []string{"review-1"}, outcome, `{"findings":[]}`)
	if got != deletedAnchorFinding {
		t.Fatalf("replaced code was treated as removed: %s", got)
	}
}

func TestStrictReviewResolutionDeletedAnchorMerge(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "removal only"
		if replacement {
			name = "removal with replacement"
		}
		t.Run(name, func(t *testing.T) {
			sctx, outcome := deletedAnchorFixture(t, deletedAnchorShape{merge: true, replacement: replacement})
			parents, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-list", "--parents", "-n", "1", sctx.Run.HeadSHA)
			if err != nil || len(strings.Fields(parents)) != 3 {
				t.Fatalf("fixture head must have two parents: %q, %v", parents, err)
			}
			got, _ := resolveReviewCarry(sctx, deletedAnchorFinding, []string{"review-1"}, outcome, `{"findings":[]}`)
			want := ""
			if replacement {
				want = deletedAnchorFinding
			}
			if got != want {
				t.Fatalf("merge removal resolution = %q, want %q", got, want)
			}
		})
	}
}
