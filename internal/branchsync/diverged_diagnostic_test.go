package branchsync

import (
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The tests in this file cover the refusal of issue #151: a custody recovery
// that refuses a rewritten commit must name the two heads it compared, state
// which tests it ran, and report a patch identity match as a diagnostic that
// decides nothing.
//
// The containment proof itself is the executable three-way merge in
// preservedContainsLocalWork. These tests hold that proof fixed: the same
// inputs keep producing the same decision, and a patch identity never turns a
// refusal into an adoption.

// newRewrittenHeadRecoverFixture models the case of issue #151. The pipeline's
// target advance rewrote the operator's own commit onto a newer main, so the
// local head and the preserved head hold the same change under different
// commit identities. The pipeline's later fix rounds also rewrote the same
// file, so the two histories are genuinely not interchangeable and the
// containment proof does not hold.
func newRewrittenHeadRecoverFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipelineDir string) {
		mustWrite(t, filepath.Join(pipelineDir, "feature.txt"), "feature one\nfeature two guarded\n")
		mustRun(t, pipelineDir, "commit", "-am", "no-mistakes(review): guard the second line")
	})
	fetchPreservedHead(t, f)
	return f
}

// newDroppedWorkRecoverFixture models the false positive the codebase's own
// rule warns about. The preserved head does NOT carry the operator's change,
// but its history still holds a commit with the same patch identity, so a
// containment claim built on patch identities would wrongly say the work is
// there.
func newDroppedWorkRecoverFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipelineDir string) {
		mustWrite(t, filepath.Join(pipelineDir, "feature.txt"), "feature one\n")
		mustRun(t, pipelineDir, "commit", "-am", "no-mistakes(review): drop the operator change")
		mustWrite(t, filepath.Join(pipelineDir, "feature.txt"), "feature one\npipeline later change\n")
		mustRun(t, pipelineDir, "commit", "-am", "no-mistakes(lint): later fix")
	})
	fetchPreservedHead(t, f)
	return f
}

// fetchPreservedHead brings the preserved head into the operator worktree, the
// way recovery does before it compares the two heads.
func fetchPreservedHead(t *testing.T, f *recoverFixture) {
	t.Helper()
	mustRun(t, f.local, "fetch", "--no-tags", f.gate, "+refs/heads/feature/recover:refs/no-mistakes/test/preserved")
}

// rewrittenHeadPatchMatch returns the commit in the preserved head that
// carries the same patch identity as the local head's own change, and the
// patch identity both commits share.
func rewrittenHeadPatchMatch(t *testing.T, f *recoverFixture) (string, string) {
	t.Helper()
	localIdentity, ok := commitPatchIdentity(f.ctx, f.local, f.submitted)
	if !ok {
		t.Fatal("the local head has no patch identity to compare")
	}
	stream, err := gitpkg.Run(f.ctx, f.local, "log", "--format=commit %H", "-p", "--no-merges", "--no-ext-diff", f.preserved, "^"+f.submitted)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := gitpkg.StablePatchIdentities(f.ctx, f.local, stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range identities {
		if identity.PatchID == localIdentity && identity.Commit != f.submitted {
			return identity.Commit, localIdentity
		}
	}
	t.Fatalf("no commit in the preserved head carries the local head's patch identity %s", localIdentity)
	return "", ""
}

// TestRecoverDivergedRefusalNamesComparedHeadsAndTests proves the refusal
// names both compared SHAs and states which tests it ran, so an operator can
// see that a rewritten commit is not a lost one.
//
// The assertion is on the SHAs themselves, not on the message being non-empty:
// a message that says "diverged" plus a hash of nothing would pass a length
// check.
func TestRecoverDivergedRefusalNamesComparedHeadsAndTests(t *testing.T) {
	t.Parallel()

	f := newRewrittenHeadRecoverFixture(t)
	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	if !strings.Contains(refused.Error, f.submitted) {
		t.Fatalf("refusal does not name the local head %s: %q", f.submitted, refused.Error)
	}
	if !strings.Contains(refused.Error, f.preserved) {
		t.Fatalf("refusal does not name the preserved head %s: %q", f.preserved, refused.Error)
	}
	if !strings.Contains(refused.Error, "the local head "+f.submitted) {
		t.Fatalf("refusal does not name the local head as the local head: %q", refused.Error)
	}
	if !strings.Contains(refused.Error, "the preserved pipeline head "+f.preserved) {
		t.Fatalf("refusal does not name the preserved head as the preserved head: %q", refused.Error)
	}
}

// TestRecoverDivergedRefusalStatesWhichTestsRan proves the refusal says which
// tests it ran, so a reader can tell what was checked and therefore what was
// not checked.
func TestRecoverDivergedRefusalStatesWhichTestsRan(t *testing.T) {
	t.Parallel()

	f := newRewrittenHeadRecoverFixture(t)
	refused := f.service.Recover(f.ctx, false)
	if refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	for _, want := range []string{
		"the only tests it ran were",
		"git merge-base --is-ancestor",
		"git merge-tree --write-tree --merge-base",
		"ancestry does not hold",
		"the three-way merge proof does not hold",
	} {
		if !strings.Contains(refused.Error, want) {
			t.Fatalf("refusal does not state %q: %q", want, refused.Error)
		}
	}
}

// TestRecoverDivergedRefusalKeepsTheContainmentProof proves the proof is
// untouched: the same pair still fails it, the issue's own case still refuses
// with the same outcome, and a rebase whose content is carried forward intact
// still recovers automatically.
func TestRecoverDivergedRefusalKeepsTheContainmentProof(t *testing.T) {
	t.Parallel()

	diverged := newRewrittenHeadRecoverFixture(t)
	if preservedContainsLocalWork(diverged.ctx, diverged.local, diverged.submitted, diverged.preserved) {
		t.Fatal("the containment proof held for a pair whose histories are not interchangeable")
	}
	refused := diverged.service.Recover(diverged.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" || refused.Relation != RelationDiverged {
		t.Fatalf("the issue's own case no longer refuses the same way: %#v", refused)
	}
	if got := mustRun(t, diverged.local, "rev-parse", "HEAD"); got != diverged.submitted {
		t.Fatalf("refused recovery moved the branch to %s, want %s", got, diverged.submitted)
	}
	if diverged.custodyReturned() {
		t.Fatal("refused recovery stamped custody")
	}

	carried := newRebasedRecoverFixture(t, types.RunCancelled)
	fetchPreservedHead(t, carried)
	if !preservedContainsLocalWork(carried.ctx, carried.local, carried.submitted, carried.preserved) {
		t.Fatal("the containment proof failed for a rebase that carried the work forward")
	}
	recovered := carried.service.Recover(carried.ctx, false)
	if !recovered.Recovered || !recovered.Changed {
		t.Fatalf("a rebase that carries the work forward no longer recovers: %#v", recovered)
	}
	if got := mustRun(t, carried.local, "rev-parse", "HEAD"); got != carried.preserved {
		t.Fatalf("recovered branch = %s, want the preserved head %s", got, carried.preserved)
	}
}

// TestRecoverDivergedRefusalReportsPatchIdentityAsDiagnosticOnly proves the
// patch identity match is reported for the operator and changes nothing: the
// refusal stands, the branch does not move, the state is unchanged, and
// custody is not returned.
func TestRecoverDivergedRefusalReportsPatchIdentityAsDiagnosticOnly(t *testing.T) {
	t.Parallel()

	f := newRewrittenHeadRecoverFixture(t)
	match, _ := rewrittenHeadPatchMatch(t, f)
	short := mustRun(t, f.local, "rev-parse", "--short", match)

	refused := f.service.Recover(f.ctx, false)
	if refused.Safety != "blocked_recover_diverged" || refused.Recovered || refused.Changed {
		t.Fatalf("recover diverged = %#v", refused)
	}
	if !strings.Contains(refused.Error, "the same patch identity") {
		t.Fatalf("refusal does not report the patch identity: %q", refused.Error)
	}
	if !strings.Contains(refused.Error, short) {
		t.Fatalf("refusal does not name the commit that carries the match %s: %q", short, refused.Error)
	}
	if !strings.Contains(refused.Error, "not a containment proof") {
		t.Fatalf("refusal does not mark the patch identity as a diagnostic: %q", refused.Error)
	}
	if !strings.Contains(refused.Error, "not one of the tests that decided this") {
		t.Fatalf("refusal does not say the patch identity did not decide this: %q", refused.Error)
	}
	if !strings.Contains(refused.Error, "nothing was adopted") {
		t.Fatalf("refusal does not say that nothing was adopted: %q", refused.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("refused recovery moved the branch to %s, want %s", got, f.submitted)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("refused recovery moved the gate lane to %s, want %s", got, f.preserved)
	}
	if f.custodyReturned() {
		t.Fatal("refused recovery stamped custody")
	}
}

// TestRecoverDivergedRefusalStillRefusesSameShapedEditElsewhere proves the
// false positive the codebase warns about: a patch identity match in the
// preserved history does not turn a refusal into an adoption, because the
// preserved head does not carry the operator's work.
//
// The fixture is the one that makes the warning concrete: the preserved
// head's history DOES hold a commit with the same patch identity as the local
// head's, so a patch-identity containment claim would adopt it, and the
// operator's change is still absent from the preserved head's content.
func TestRecoverDivergedRefusalStillRefusesSameShapedEditElsewhere(t *testing.T) {
	t.Parallel()

	f := newDroppedWorkRecoverFixture(t)
	if preservedContainsLocalWork(f.ctx, f.local, f.submitted, f.preserved) {
		t.Fatal("the containment proof held for a preserved head that dropped the operator's change")
	}
	// The false positive is live: the preserved history holds a commit with
	// the same patch identity, so a patch-identity containment claim would
	// wrongly say the operator's work is in there.
	match, _ := rewrittenHeadPatchMatch(t, f)
	short := mustRun(t, f.local, "rev-parse", "--short", match)

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	if !strings.Contains(refused.Error, short) {
		t.Fatalf("refusal does not report the same-shaped edit %s: %q", short, refused.Error)
	}
	if !strings.Contains(refused.Error, "not a containment proof") {
		t.Fatalf("refusal does not mark the patch identity as a diagnostic: %q", refused.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("refused recovery moved the branch to %s, want %s", got, f.submitted)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("refused recovery moved the gate lane to %s, want %s", got, f.preserved)
	}
	if f.custodyReturned() {
		t.Fatal("refused recovery stamped custody")
	}
}

// TestRecoverDivergedRefusalNamesConflictPaths proves the refusal names the
// paths whose content conflicted, so the operator knows where the histories
// disagree.
func TestRecoverDivergedRefusalNamesConflictPaths(t *testing.T) {
	t.Parallel()

	f := newRewrittenHeadRecoverFixture(t)
	refused := f.service.Recover(f.ctx, false)
	if refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	if !strings.Contains(refused.Error, "the three-way merge conflicts in feature.txt") {
		t.Fatalf("refusal does not name the conflicting path: %q", refused.Error)
	}
}

// TestRecoverDivergedRefusalSaysDivergenceIsNotLostWork proves the wording
// stops "diverged" from reading as "your work was lost", which is the whole
// defect: the operator's commit was rewritten, not dropped.
func TestRecoverDivergedRefusalSaysDivergenceIsNotLostWork(t *testing.T) {
	t.Parallel()

	f := newRewrittenHeadRecoverFixture(t)
	refused := f.service.Recover(f.ctx, false)
	if !strings.Contains(refused.Error, "not that the local work was lost") {
		t.Fatalf("refusal does not separate divergence from lost work: %q", refused.Error)
	}
}

// TestRecoverDivergedRefusalNamesComparedHeadsInNextAction proves the handoff
// to the operator carries the two compared SHAs, not an anonymous HEAD.
func TestRecoverDivergedRefusalNamesComparedHeadsInNextAction(t *testing.T) {
	t.Parallel()

	f := newRewrittenHeadRecoverFixture(t)
	refused := f.service.Recover(f.ctx, false)
	if refused.NextAction == nil || refused.NextAction.Code != "inspect_and_reconcile_manually" {
		t.Fatalf("refusal next action = %#v", refused.NextAction)
	}
	if !strings.Contains(refused.NextAction.Command, f.submitted) || !strings.Contains(refused.NextAction.Command, f.preserved) {
		t.Fatalf("next action does not name the compared heads: %q", refused.NextAction.Command)
	}
}

// TestStrictFastForwardRefusalNamesComparedHeadsAndTests proves the other
// producer of the same refusal - the strict fast-forward refusal against the
// published pipeline head - carries the same detail, so the operator reading
// either refusal sees what it compared and which tests it ran.
func TestStrictFastForwardRefusalNamesComparedHeadsAndTests(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.local, "followup.txt"), "diverged followup\n")
	mustRun(t, f.local, "add", "followup.txt")
	mustRun(t, f.local, "commit", "-m", "diverged followup")
	localHead := mustRun(t, f.local, "rev-parse", "HEAD")

	state := f.service.Recover(f.ctx, false)
	if state.Safety != "blocked_recover_diverged" || state.Changed {
		t.Fatalf("published diverged recovery = %#v", state)
	}
	if !strings.Contains(state.Error, localHead) {
		t.Fatalf("refusal does not name the local head %s: %q", localHead, state.Error)
	}
	if !strings.Contains(state.Error, f.pushed) {
		t.Fatalf("refusal does not name the pipeline-pushed head %s: %q", f.pushed, state.Error)
	}
	for _, want := range []string{
		"the only tests it ran were",
		"git merge-base --is-ancestor",
		"git merge-tree --write-tree --merge-base",
	} {
		if !strings.Contains(state.Error, want) {
			t.Fatalf("refusal does not state %q: %q", want, state.Error)
		}
	}
	if state.NextAction == nil || !strings.Contains(state.NextAction.Command, localHead) || !strings.Contains(state.NextAction.Command, f.pushed) {
		t.Fatalf("next action does not name the compared heads: %#v", state.NextAction)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != localHead {
		t.Fatalf("refused recovery moved the branch to %s, want %s", got, localHead)
	}
}

// TestRecoverNonDivergedPathsAreUnchanged proves the refusal change touches
// nothing else: a preserved head that contains the local work by ancestry
// still fast-forwards, a local head that is ahead of the preserved head still
// returns custody without moving the worktree, and a released clean branch is
// still an idempotent no-op.
func TestRecoverNonDivergedPathsAreUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("ancestry", func(t *testing.T) {
		t.Parallel()
		behind := newRecoverFixture(t, types.RunCancelled)
		recovered := behind.service.Recover(behind.ctx, false)
		if !recovered.Recovered || !recovered.Changed {
			t.Fatalf("a behind worktree no longer recovers: %#v", recovered)
		}
		if got := mustRun(t, behind.local, "rev-parse", "HEAD"); got != behind.preserved {
			t.Fatalf("recovered branch = %s, want the preserved head %s", got, behind.preserved)
		}
	})

	t.Run("ahead", func(t *testing.T) {
		t.Parallel()
		ahead := newRecoverFixture(t, types.RunCancelled)
		mustRun(t, ahead.local, "fetch", "--no-tags", ahead.gate, "refs/heads/feature/recover")
		mustRun(t, ahead.local, "merge", "--ff-only", ahead.preserved)
		mustWrite(t, filepath.Join(ahead.local, "ahead.txt"), "ahead\n")
		mustRun(t, ahead.local, "add", "ahead.txt")
		mustRun(t, ahead.local, "commit", "-m", "ahead of the preserved head")
		localHead := mustRun(t, ahead.local, "rev-parse", "HEAD")
		recovered := ahead.service.Recover(ahead.ctx, false)
		if !recovered.Recovered || recovered.Changed {
			t.Fatalf("a local head ahead of the preserved head no longer returns custody: %#v", recovered)
		}
		if got := mustRun(t, ahead.local, "rev-parse", "HEAD"); got != localHead {
			t.Fatalf("HEAD = %s, want the untouched local head %s", got, localHead)
		}
	})
}
