package git

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitPatchIDIsTheNamedCommitOwnIdentity pins the identity of one
// commit against its first parent: the named commit's own change, and not
// another commit's.
func TestCommitPatchIDIsTheNamedCommitOwnIdentity(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "file.txt"), "first\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "first change")
	first := run(t, dir, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "other.txt"), "second\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "second change")
	second := run(t, dir, "git", "rev-parse", "HEAD")

	firstID, err := CommitPatchID(ctx, dir, first)
	if err != nil {
		t.Fatalf("CommitPatchID for the first commit: %v", err)
	}
	secondID, err := CommitPatchID(ctx, dir, second)
	if err != nil {
		t.Fatalf("CommitPatchID for the second commit: %v", err)
	}
	if firstID == "" || secondID == "" {
		t.Fatalf("empty commit identity: %q and %q", firstID, secondID)
	}
	if firstID == secondID {
		t.Fatal("two different commits reported the same identity")
	}

	// A commit with no diff of its own has no identity.
	run(t, dir, "git", "commit", "--allow-empty", "-m", "empty change")
	empty := run(t, dir, "git", "rev-parse", "HEAD")
	emptyID, err := CommitPatchID(ctx, dir, empty)
	if err != nil {
		t.Fatalf("CommitPatchID for the empty commit: %v", err)
	}
	if emptyID != "" {
		t.Fatalf("the empty commit reported identity %q", emptyID)
	}
}

// TestCommitPatchIDSurvivesARewrite is the property the diagnostic exists
// for: the same change re-applied onto a different parent has the SAME
// identity, which is why a rewrite reads as a rewrite and not as lost work.
func TestCommitPatchIDSurvivesARewrite(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	base := run(t, dir, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "file.txt"), "base\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "base file")
	base = run(t, dir, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "file.txt"), "feature\n")
	run(t, dir, "git", "commit", "-am", "operator work")
	original := run(t, dir, "git", "rev-parse", "HEAD")

	// The rewritten copy sits on a different parent - a newer main - so it is
	// a different commit object carrying the same change.
	run(t, dir, "git", "checkout", "-B", "rewritten", base)
	writeFile(t, filepath.Join(dir, "other.txt"), "newer main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "upstream(main): newer main")
	writeFile(t, filepath.Join(dir, "file.txt"), "feature\n")
	run(t, dir, "git", "commit", "-am", "operator work")
	rewritten := run(t, dir, "git", "rev-parse", "HEAD")
	if rewritten == original {
		t.Fatal("the rewritten copy is the same commit object")
	}

	originalID, err := CommitPatchID(ctx, dir, original)
	if err != nil {
		t.Fatalf("CommitPatchID for the original: %v", err)
	}
	rewrittenID, err := CommitPatchID(ctx, dir, rewritten)
	if err != nil {
		t.Fatalf("CommitPatchID for the rewritten copy: %v", err)
	}
	if originalID == "" || originalID != rewrittenID {
		t.Fatalf("the rewritten copy reported %q, want the original's %q", rewrittenID, originalID)
	}
}

// TestCommitPatchIDsCoversTheRange pins the range form: every commit in the
// range is present, keyed by its own SHA, and a commit outside the range is
// not.
func TestCommitPatchIDsCoversTheRange(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	base := run(t, dir, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "file.txt"), "one\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "one")
	one := run(t, dir, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "file.txt"), "two\n")
	run(t, dir, "git", "commit", "-am", "two")
	two := run(t, dir, "git", "rev-parse", "HEAD")

	ids, err := CommitPatchIDs(ctx, dir, two, "--not", base)
	if err != nil {
		t.Fatalf("CommitPatchIDs over the range: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("the range reported %d identities, want 2: %v", len(ids), ids)
	}
	if ids[one] == "" || ids[two] == "" {
		t.Fatalf("the range is missing a commit: %v", ids)
	}
	if ids[one] == ids[two] {
		t.Fatal("two different commits in the range reported the same identity")
	}
	if _, present := ids[base]; present {
		t.Fatal("a commit outside the range was reported")
	}
}

func TestCommitPatchIDsRejectsInvalidRangeAndCancellation(t *testing.T) {
	dir := initTestRepo(t)
	ids, err := CommitPatchIDs(context.Background(), dir, "refs/heads/missing")
	if err == nil || ids != nil {
		t.Fatalf("invalid range returned %v, %v", ids, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ids, err = CommitPatchIDs(ctx, dir, "HEAD")
	if !errors.Is(err, context.Canceled) || ids != nil {
		t.Fatalf("cancelled range returned %v, %v", ids, err)
	}
}

func TestCommitPatchIDsReadsBareRangeAndEmptyRange(t *testing.T) {
	dir := initTestRepo(t)
	writeFile(t, filepath.Join(dir, "file.txt"), "one\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "one")
	one := run(t, dir, "git", "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "bare.git")
	run(t, dir, "git", "clone", "--bare", dir, bare)
	ctx := context.Background()
	ids, err := CommitPatchIDs(ctx, bare, one+"^.."+one)
	if err != nil || len(ids) != 1 || ids[one] == "" {
		t.Fatalf("bare range returned %v, %v", ids, err)
	}
	ids, err = CommitPatchIDs(ctx, bare, one+".."+one)
	if err != nil || len(ids) != 0 {
		t.Fatalf("empty range returned %v, %v", ids, err)
	}
}

func TestCommitPatchIDsIgnoreDiffPresentationConfiguration(t *testing.T) {
	for _, setting := range []struct{ key, value string }{
		{"color.ui", "always"},
		{"color.diff", "always"},
		{"diff.external", "printf 'not a Git diff'"},
		{"diff.custom.command", "printf 'not a Git diff'"},
		{"diff.custom.textconv", "printf 'converted bytes'"},
		{"pager.log", "true"},
	} {
		t.Run(setting.key, func(t *testing.T) {
			dir := initTestRepo(t)
			ctx := context.Background()
			writeFile(t, filepath.Join(dir, "file.txt"), "feature\n")
			run(t, dir, "git", "add", "file.txt")
			run(t, dir, "git", "commit", "-m", "feature")
			head := run(t, dir, "git", "rev-parse", "HEAD")
			const patch = "diff --git a/file.txt b/file.txt\nnew file mode 100644\n--- /dev/null\n+++ b/file.txt\n@@ -0,0 +1 @@\n+feature\n"
			oracle, err := RunWithInput(ctx, dir, patch, "patch-id", "--stable")
			if err != nil || len(strings.Fields(oracle)) != 2 {
				t.Fatalf("Git identity of the specified added-file patch: %q, %v", oracle, err)
			}
			want := strings.Fields(oracle)[0]
			writeFile(t, filepath.Join(dir, ".git", "info", "attributes"), "file.txt diff=custom\n")
			run(t, dir, "git", "config", setting.key, setting.value)
			t.Setenv("GIT_PAGER", "printf 'pager output'")
			got, err := CommitPatchID(ctx, dir, head)
			if err != nil || got != want {
				t.Fatalf("single identity = %q, %v; want %s", got, err, want)
			}
			ids, err := CommitPatchIDs(ctx, dir, head+"^.."+head)
			if err != nil || len(ids) != 1 || ids[head] != want {
				t.Fatalf("range identities = %v, %v; want %s: %s", ids, err, head, want)
			}
		})
	}
}
