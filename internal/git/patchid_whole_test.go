package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestStablePatchIDFromDiffMeasuresOneWholeChange proves the whole-diff
// identity is the one a human measures with `git show <sha> | git patch-id
// --stable`: a commit's own change has one identity, and the same change
// written on another base has the same identity too.
func TestStablePatchIDFromDiffMeasuresOneWholeChange(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "feature.txt"), "feature one\n")
	run(t, dir, "git", "add", "feature.txt")
	run(t, dir, "git", "commit", "-m", "feature one")
	first := run(t, dir, "git", "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "feature.txt"), "feature one\nfeature two\n")
	run(t, dir, "git", "add", "feature.txt")
	run(t, dir, "git", "commit", "-m", "feature two")
	second := run(t, dir, "git", "rev-parse", "HEAD")

	firstIdentity, err := StablePatchIDFromDiff(ctx, dir, run(t, dir, "git", "show", "--format=", first))
	if err != nil {
		t.Fatalf("StablePatchIDFromDiff for the first commit: %v", err)
	}
	secondIdentity, err := StablePatchIDFromDiff(ctx, dir, run(t, dir, "git", "show", "--format=", second))
	if err != nil {
		t.Fatalf("StablePatchIDFromDiff for the second commit: %v", err)
	}
	if firstIdentity == "" || secondIdentity == "" {
		t.Fatalf("empty patch identity: %q and %q", firstIdentity, secondIdentity)
	}
	if firstIdentity == secondIdentity {
		t.Fatal("two different changes share one patch identity")
	}

	// The same change replayed onto another base keeps its identity, which
	// is what makes it usable as a diagnostic for a rewritten commit.
	run(t, dir, "git", "checkout", "-b", "other", "HEAD~2")
	writeFile(t, filepath.Join(dir, "other.txt"), "other\n")
	run(t, dir, "git", "add", "other.txt")
	run(t, dir, "git", "commit", "-m", "other base")
	run(t, dir, "git", "cherry-pick", first)
	replayed := run(t, dir, "git", "rev-parse", "HEAD")
	replayedIdentity, err := StablePatchIDFromDiff(ctx, dir, run(t, dir, "git", "show", "--format=", replayed))
	if err != nil {
		t.Fatalf("StablePatchIDFromDiff for the replayed commit: %v", err)
	}
	if replayedIdentity != firstIdentity {
		t.Fatalf("replayed identity = %q, want %q", replayedIdentity, firstIdentity)
	}
}

// TestStablePatchIdentitiesPairsEveryPatchWithItsCommit proves a multi-commit
// diff stream yields one identity per commit, each paired with the commit that
// produced it.
func TestStablePatchIdentitiesPairsEveryPatchWithItsCommit(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "feature.txt"), "feature one\n")
	run(t, dir, "git", "add", "feature.txt")
	run(t, dir, "git", "commit", "-m", "feature one")
	first := run(t, dir, "git", "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "feature.txt"), "feature one\nfeature two\n")
	run(t, dir, "git", "add", "feature.txt")
	run(t, dir, "git", "commit", "-m", "feature two")
	second := run(t, dir, "git", "rev-parse", "HEAD")

	stream := run(t, dir, "git", "log", "--format=commit %H", "-p", first+"^.."+second)
	identities, err := StablePatchIdentities(ctx, dir, stream)
	if err != nil {
		t.Fatalf("StablePatchIdentities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("identities = %d, want 2: %#v", len(identities), identities)
	}
	byCommit := map[string]string{}
	for _, identity := range identities {
		if identity.PatchID == "" || identity.Commit == "" {
			t.Fatalf("incomplete identity: %#v", identity)
		}
		byCommit[identity.Commit] = identity.PatchID
	}
	firstIdentity, err := StablePatchIDFromDiff(ctx, dir, run(t, dir, "git", "show", "--format=", first))
	if err != nil {
		t.Fatalf("StablePatchIDFromDiff for the first commit: %v", err)
	}
	if byCommit[first] != firstIdentity {
		t.Fatalf("identity for %s = %q, want %q", first, byCommit[first], firstIdentity)
	}
}

// TestRunAllowingFailureKeepsStdoutOnFailure proves a git command that
// reports its result on stdout and signals it through the exit code keeps
// that stdout: `git merge-tree --write-tree` writes the tree and the
// conflicted paths, then exits nonzero.
func TestRunAllowingFailureKeepsStdoutOnFailure(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	base := run(t, dir, "git", "rev-parse", "HEAD")
	run(t, dir, "git", "checkout", "-b", "left")
	writeFile(t, filepath.Join(dir, "README.md"), "# left\n")
	run(t, dir, "git", "commit", "-am", "left")
	left := run(t, dir, "git", "rev-parse", "HEAD")
	run(t, dir, "git", "checkout", "-b", "right", base)
	writeFile(t, filepath.Join(dir, "README.md"), "# right\n")
	run(t, dir, "git", "commit", "-am", "right")
	right := run(t, dir, "git", "rev-parse", "HEAD")

	out, err := RunAllowingFailure(ctx, dir, "merge-tree", "--write-tree", "--merge-base", base, left, right)
	if err == nil {
		t.Fatal("a conflicting merge-tree must exit nonzero")
	}
	if !strings.Contains(out, "README.md") {
		t.Fatalf("the conflicting path is missing from the captured output: %q", out)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || len(fields[0]) != 40 {
		t.Fatalf("the written tree OID is missing from the captured output: %q", out)
	}

	// The plain runner keeps its old behaviour: no stdout on failure.
	if plain, plainErr := Run(ctx, dir, "merge-tree", "--write-tree", "--merge-base", base, left, right); plainErr == nil || plain != "" {
		t.Fatalf("Run returned %q with error %v, want no output and an error", plain, plainErr)
	}
}
