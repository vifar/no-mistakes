package branchsync

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
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
//
// It returns the fixture and the commit that carries the same patch identity
// as the local head's own change. That commit is read straight from the
// fixture's own history, so it is an INDEPENDENT oracle: the expectation
// never comes from repeating the production identity calculation, which
// would let one shared mistake satisfy both the expectation and the code
// under test.
func newRewrittenHeadRecoverFixture(t *testing.T) (*recoverFixture, string) {
	t.Helper()
	var replay string
	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipelineDir string) {
		// The pipeline's history at this point is the rewrite of the
		// operator's own commit: the same change under a new identity.
		replay = mustRun(t, pipelineDir, "rev-parse", "HEAD")
		mustWrite(t, filepath.Join(pipelineDir, "feature.txt"), "feature one\nfeature two guarded\n")
		mustRun(t, pipelineDir, "commit", "-am", "no-mistakes(review): guard the second line")
	})
	fetchPreservedHead(t, f)
	return f, replay
}

// newDroppedWorkRecoverFixture models the false positive the codebase's own
// rule warns about. The preserved head does NOT carry the operator's change,
// but its history still holds a commit with the same patch identity, so a
// containment claim built on patch identities would wrongly say the work is
// there.
//
// It returns the fixture and the commit that carries the matching identity,
// read straight from the fixture's own history.
func newDroppedWorkRecoverFixture(t *testing.T) (*recoverFixture, string) {
	t.Helper()
	var replay string
	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipelineDir string) {
		replay = mustRun(t, pipelineDir, "rev-parse", "HEAD")
		mustWrite(t, filepath.Join(pipelineDir, "feature.txt"), "feature one\n")
		mustRun(t, pipelineDir, "commit", "-am", "no-mistakes(review): drop the operator change")
		mustWrite(t, filepath.Join(pipelineDir, "feature.txt"), "feature one\npipeline later change\n")
		mustRun(t, pipelineDir, "commit", "-am", "no-mistakes(lint): later fix")
	})
	fetchPreservedHead(t, f)
	return f, replay
}

// The repeated block below is written three times. A change to the fourth
// line of one block and a change to the fourth line of another block produce
// the same diff content, because Git's stable patch identity hashes the
// changed lines together with their context, and the context windows around
// the two blocks are identical.
const (
	collisionRepeated      = "a\nb\nc\nd\na\nb\nc\nd\na\nb\nc\nd\n"
	collisionFirstChanged  = "a\nb\nc\nX\na\nb\nc\nd\na\nb\nc\nd\n"
	collisionSecondChanged = "a\nb\nc\nd\na\nb\nc\nX\na\nb\nc\nd\n"
)

// newLocationCollisionRecoverFixture models the false positive in its
// sharpest form: the preserved head's history holds a commit whose stable
// patch identity EQUALS the local head's, because the pipeline changed a
// DIFFERENT identical block, so the identity cannot tell the two apart.
//
// The preserved head does NOT carry the operator's work: the block the
// operator changed is untouched there. A containment claim built on patch
// identities would adopt it, and the operator's unlanded work would be lost
// with it.
//
// It returns the fixture and the commit that carries the colliding identity,
// read straight from the fixture's own history.
func newLocationCollisionRecoverFixture(t *testing.T) (*recoverFixture, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), collisionRepeated)
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")

	// The operator changes the fourth line of the FIRST block.
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "file.txt"), collisionFirstChanged)
	mustRun(t, local, "commit", "-am", "operator: change the first block")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	// The target advances, and the pipeline's branch no longer carries the
	// operator's commit at all.
	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "upstream.txt"), "upstream advance\n")
	mustRun(t, local, "add", "upstream.txt")
	mustRun(t, local, "commit", "-m", "upstream advance")
	mustRun(t, local, "checkout", "feature/recover")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "reset", "--hard", "origin/main")
	// The pipeline changes the fourth line of the SECOND block: the same
	// shape of edit, in a different identical block.
	mustWrite(t, filepath.Join(pipeline, "file.txt"), collisionSecondChanged)
	mustRun(t, pipeline, "commit", "-am", "no-mistakes(review): change the second block")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	f := &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, preserved: preserved,
	}
	fetchPreservedHead(t, f)
	return f, preserved
}

// newRenameRecoverFixture models a rewritten commit whose change is a file
// RENAME, which is the case where the two sides of the patch identity
// comparison used to disagree.
//
// Git's `log -p` detects renames by default and `diff-tree` does not, so a
// renamed file was measured as a rename on the search side and as a deletion
// plus an addition on the local side. The same change then got two different
// identities, and the diagnostic reported no match for work that was
// provably in the preserved head.
//
// It returns the fixture and the commit that carries the replayed rename,
// read straight from the fixture's own history.
func newRenameRecoverFixture(t *testing.T) (*recoverFixture, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")

	// The operator's own change is a rename.
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustRun(t, local, "mv", "file.txt", "renamed.txt")
	mustRun(t, local, "commit", "-am", "operator: rename the file")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	// The target advances, so the pipeline's rebase rewrites the rename onto
	// a newer main.
	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "upstream.txt"), "upstream advance\n")
	mustRun(t, local, "add", "upstream.txt")
	mustRun(t, local, "commit", "-m", "upstream advance")
	mustRun(t, local, "checkout", "feature/recover")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "rebase", "origin/main")
	// The pipeline's rewrite of the rename, read from its own history.
	replay := mustRun(t, pipeline, "rev-parse", "HEAD")
	// The pipeline's later fix round rewrote the same file, so the two
	// histories are genuinely not interchangeable and the containment proof
	// does not hold.
	mustWrite(t, filepath.Join(pipeline, "renamed.txt"), "base\nchanged by the pipeline\n")
	mustRun(t, pipeline, "commit", "-am", "no-mistakes(review): change the renamed file")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	f := &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, preserved: preserved,
	}
	fetchPreservedHead(t, f)
	return f, replay
}

// newTextconvRecoverFixture models a rewritten commit whose file has a
// configured textconv filter, which is the second way the two sides of the
// patch identity comparison used to disagree.
//
// Git applies textconv filters by default for `log -p` and not for
// `diff-tree`, so a file with such a filter was measured as its DISPLAYED
// content on the search side and as its STORED content on the local side.
// The same change then got two different identities, and the diagnostic
// reported no match for work that was provably in the preserved head.
//
// It returns the fixture and the commit that carries the replayed change,
// read straight from the fixture's own history.
func newTextconvRecoverFixture(t *testing.T) (*recoverFixture, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	// The filter rewrites the file's content, so a measured identity
	// differs from the stored one.
	converter := filepath.Join(root, "conv.sh")
	mustWrite(t, converter, "#!/bin/sh\ncat \"$1\"\necho FILTERED\n")
	if err := os.Chmod(converter, 0o755); err != nil {
		t.Fatal(err)
	}

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustRun(t, local, "config", "diff.conv.textconv", converter)
	mustWrite(t, filepath.Join(local, ".gitattributes"), "file.txt diff=conv\n")
	mustWrite(t, filepath.Join(local, "file.txt"), "stored\n")
	mustRun(t, local, "add", ".gitattributes", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")

	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "file.txt"), "changed\n")
	mustRun(t, local, "commit", "-am", "operator: change the file")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "upstream.txt"), "upstream advance\n")
	mustRun(t, local, "add", "upstream.txt")
	mustRun(t, local, "commit", "-m", "upstream advance")
	mustRun(t, local, "checkout", "feature/recover")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "rebase", "origin/main")
	replay := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustWrite(t, filepath.Join(pipeline, "file.txt"), "changed\nchanged by the pipeline\n")
	mustRun(t, pipeline, "commit", "-am", "no-mistakes(review): change the file again")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	f := &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, preserved: preserved,
	}
	fetchPreservedHead(t, f)
	return f, replay
}

// fetchPreservedHead brings the preserved head into the operator worktree, the
// way recovery does before it compares the two heads.
func fetchPreservedHead(t *testing.T, f *recoverFixture) {
	t.Helper()
	mustRun(t, f.local, "fetch", "--no-tags", f.gate, "+refs/heads/feature/recover:refs/no-mistakes/test/preserved")
}

// shellPatchID measures a commit's stable patch identity with Git directly,
// the way the issue's own measurement did.
//
// It deliberately does not use the production helper. The test's expectation
// must not be produced by the same calculation as the code under test, or a
// shared mistake would satisfy both and the test would prove nothing. Git is
// invoked without a shell, so the commit is never interpolated into a
// command line.
func shellPatchID(t *testing.T, dir, commit string) string {
	t.Helper()
	show := exec.Command("git", "show", commit)
	show.Dir = dir
	diff, err := show.Output()
	if err != nil {
		t.Fatalf("git show %s: %v", commit, err)
	}
	patchID := exec.Command("git", "patch-id", "--stable")
	patchID.Dir = dir
	patchID.Stdin = bytes.NewReader(diff)
	out, err := patchID.Output()
	if err != nil {
		t.Fatalf("git patch-id --stable: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("git patch-id --stable returned no identity for %s", commit)
	}
	return fields[0]
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

	f, _ := newRewrittenHeadRecoverFixture(t)
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

	f, _ := newRewrittenHeadRecoverFixture(t)
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

	diverged, _ := newRewrittenHeadRecoverFixture(t)
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

	f, match := newRewrittenHeadRecoverFixture(t)
	// The fixture's own history is the oracle: Git says these two commits
	// carry the same stable patch identity, measured without the production
	// helper.
	localIdentity := shellPatchID(t, f.local, f.submitted)
	if got := shellPatchID(t, f.local, match); got != localIdentity {
		t.Fatalf("the fixture's replay %s does not share the local head's patch identity %s (got %s)", match, localIdentity, got)
	}
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

	f, match := newDroppedWorkRecoverFixture(t)
	if preservedContainsLocalWork(f.ctx, f.local, f.submitted, f.preserved) {
		t.Fatal("the containment proof held for a preserved head that dropped the operator's change")
	}
	// The false positive is live: the preserved history holds a commit with
	// the same patch identity, so a patch-identity containment claim would
	// wrongly say the operator's work is in there. The identity is measured
	// with Git directly, so the expectation never comes from the production
	// calculation it is checking.
	localIdentity := shellPatchID(t, f.local, f.submitted)
	if got := shellPatchID(t, f.local, match); got != localIdentity {
		t.Fatalf("the fixture's replay %s does not share the local head's patch identity %s (got %s)", match, localIdentity, got)
	}
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

// firstRepeatedBlock returns the first four lines of a repeated-block file,
// which is the block the operator changed in the location collision fixture.
func firstRepeatedBlock(content string) string {
	lines := strings.SplitN(content, "\n", 5)
	if len(lines) < 5 {
		return content
	}
	return strings.Join(lines[:4], "\n")
}

// TestRecoverDivergedRefusalStillRefusesLocationCollision proves the false
// positive the codebase warns about, in the form the warning actually names:
// "a same-shaped edit to a different identical block".
//
// The preserved head's history holds a commit whose stable patch identity
// EQUALS the local head's, because the pipeline changed a different identical
// block in the same file. The preserved head does NOT carry the operator's
// work, so a containment claim built on patch identities would adopt it and
// the operator's unlanded code would be lost with it.
//
// The refusal must stand, the branch must not move, the gate lane must not
// move, and custody must not be stamped.
func TestRecoverDivergedRefusalStillRefusesLocationCollision(t *testing.T) {
	t.Parallel()

	f, match := newLocationCollisionRecoverFixture(t)

	// The collision is real, measured with Git directly: the two commits
	// carry the same stable patch identity even though they changed
	// different blocks.
	localIdentity := shellPatchID(t, f.local, f.submitted)
	if got := shellPatchID(t, f.local, match); got != localIdentity {
		t.Fatalf("the colliding commit %s does not share the local head's patch identity %s (got %s)", match, localIdentity, got)
	}

	// The operator's work is NOT in the preserved head: the block the
	// operator changed is untouched there.
	preservedContent := mustRun(t, f.local, "show", f.preserved+":file.txt")
	if strings.Contains(firstRepeatedBlock(preservedContent), "X") {
		t.Fatalf("the preserved head carries the operator's change, so the false positive is not live:\n%s", preservedContent)
	}
	localContent := mustRun(t, f.local, "show", f.submitted+":file.txt")
	if !strings.Contains(firstRepeatedBlock(localContent), "X") {
		t.Fatalf("the local head does not carry the operator's change:\n%s", localContent)
	}

	if preservedContainsLocalWork(f.ctx, f.local, f.submitted, f.preserved) {
		t.Fatal("the containment proof held for a preserved head that does not carry the operator's work")
	}

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	short := mustRun(t, f.local, "rev-parse", "--short", match)
	if !strings.Contains(refused.Error, short) {
		t.Fatalf("refusal does not report the colliding commit %s: %q", short, refused.Error)
	}
	if !strings.Contains(refused.Error, "not a containment proof") {
		t.Fatalf("refusal does not mark the patch identity as a diagnostic: %q", refused.Error)
	}
	if !strings.Contains(refused.Error, "not one of the tests that decided this") {
		t.Fatalf("refusal does not say the patch identity did not decide this: %q", refused.Error)
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

// TestRecoverDivergedRefusalFindsRenamedReplay proves the diagnostic does not
// report a false negative about rewritten work when the change is a rename.
//
// Git's `log -p` detects renames by default and `diff-tree` does not, so the
// same renamed file was measured two different ways and the replayed rename
// in the preserved head was not recognised. The refusal then told the
// operator that no commit carried the change, which is untrue.
func TestRecoverDivergedRefusalFindsRenamedReplay(t *testing.T) {
	t.Parallel()

	f, replay := newRenameRecoverFixture(t)
	if preservedContainsLocalWork(f.ctx, f.local, f.submitted, f.preserved) {
		t.Fatal("the containment proof held for a pair whose histories are not interchangeable")
	}

	// The replayed rename carries the local head's change, measured with
	// Git directly.
	localIdentity := shellPatchID(t, f.local, f.submitted)
	if got := shellPatchID(t, f.local, replay); got != localIdentity {
		t.Fatalf("the fixture's replay %s does not share the local head's patch identity %s (got %s)", replay, localIdentity, got)
	}

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	short := mustRun(t, f.local, "rev-parse", "--short", replay)
	if !strings.Contains(refused.Error, short) {
		t.Fatalf("refusal does not name the commit that carries the replayed rename %s: %q", short, refused.Error)
	}
	if !strings.Contains(refused.Error, "the same patch identity") {
		t.Fatalf("refusal does not report the patch identity: %q", refused.Error)
	}
	if strings.Contains(refused.Error, "no commit in the preserved pipeline head carries the same patch identity") {
		t.Fatalf("refusal reports a false negative about a replayed rename: %q", refused.Error)
	}
}

// TestRecoverDivergedRefusalFindsTextconvReplay proves the diagnostic does not
// report a false negative about rewritten work when the file carries a
// configured textconv filter.
//
// Git applies textconv filters by default for `log -p` and not for
// `diff-tree`, so the same change was measured as its displayed content on
// one side and as its stored content on the other, and the replayed change in
// the preserved head was not recognised.
func TestRecoverDivergedRefusalFindsTextconvReplay(t *testing.T) {
	t.Parallel()

	f, replay := newTextconvRecoverFixture(t)
	if preservedContainsLocalWork(f.ctx, f.local, f.submitted, f.preserved) {
		t.Fatal("the containment proof held for a pair whose histories are not interchangeable")
	}

	// The replayed change carries the local head's change, measured with
	// Git directly and without a textconv filter.
	localIdentity := shellPatchID(t, f.local, f.submitted)
	if got := shellPatchID(t, f.local, replay); got != localIdentity {
		t.Fatalf("the fixture's replay %s does not share the local head's patch identity %s (got %s)", replay, localIdentity, got)
	}

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	short := mustRun(t, f.local, "rev-parse", "--short", replay)
	if !strings.Contains(refused.Error, short) {
		t.Fatalf("refusal does not name the commit that carries the replayed change %s: %q", short, refused.Error)
	}
	if strings.Contains(refused.Error, "no commit in the preserved pipeline head carries the same patch identity") {
		t.Fatalf("refusal reports a false negative about a replayed change: %q", refused.Error)
	}
}

// TestRecoverDivergedRefusalNamesConflictPaths proves the refusal names the
// paths whose content conflicted, so the operator knows where the histories
// disagree.
func TestRecoverDivergedRefusalNamesConflictPaths(t *testing.T) {
	t.Parallel()

	f, _ := newRewrittenHeadRecoverFixture(t)
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

	f, _ := newRewrittenHeadRecoverFixture(t)
	refused := f.service.Recover(f.ctx, false)
	if !strings.Contains(refused.Error, "not that the local work was lost") {
		t.Fatalf("refusal does not separate divergence from lost work: %q", refused.Error)
	}
}

// TestRecoverDivergedRefusalNamesComparedHeadsInNextAction proves the handoff
// to the operator carries the two compared SHAs, not an anonymous HEAD.
func TestRecoverDivergedRefusalNamesComparedHeadsInNextAction(t *testing.T) {
	t.Parallel()

	f, _ := newRewrittenHeadRecoverFixture(t)
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

	t.Run("released", func(t *testing.T) {
		t.Parallel()
		released := newUnmovedRecoverFixture(t, types.RunCancelled)
		assertReleasedNoOpRecover(t, released, false, released.submitted)
	})
}
