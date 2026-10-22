package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newCLIRewrittenHeadRecoverFixture reproduces the custody state of issue
// #151 at the CLI surface: the pipeline's target advance rewrote the
// operator's own commit onto a newer main, and the pipeline's later fix rounds
// rewrote the same file again, so the local head and the preserved head hold
// the same change under different commit identities while the two histories
// are genuinely not interchangeable.
func newCLIRewrittenHeadRecoverFixture(t *testing.T) cliRecoverFixture {
	t.Helper()
	nmHome := filepath.Join(t.TempDir(), "nm-home")
	t.Setenv("NM_HOME", nmHome)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	cliGit(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	base := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "-b", "feature/recover")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "feature")
	submitted := cliGit(t, local, "rev-parse", "HEAD")

	// The target advances, so the pipeline's rebase rewrites the operator's
	// own commit onto a newer main.
	cliGit(t, local, "checkout", "main")
	if err := os.WriteFile(filepath.Join(local, "upstream.txt"), []byte("upstream advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "upstream.txt")
	cliGit(t, local, "commit", "-m", "upstream advance")
	cliGit(t, local, "checkout", "feature/recover")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(registeredRoot, remote, "main")
	if err != nil {
		t.Fatal(err)
	}

	gate := p.RepoDir(repo.ID)
	cliGit(t, filepath.Dir(gate), "init", "--bare", gate)
	cliGit(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")
	pipeline := filepath.Join(root, "pipeline")
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "feature/recover")
	cliGit(t, pipeline, "rebase", "origin/main")
	if err := os.WriteFile(filepath.Join(pipeline, "file.txt"), []byte("feature guarded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipeline, "commit", "-am", "no-mistakes(review): guard the line")
	preserved := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")

	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliRecoverFixture{
		local: local, gate: gate, remote: remote, base: base, submitted: submitted,
		preserved: preserved, runID: run.ID, repoID: repo.ID,
	}
}

// TestAxiSyncRecoverDivergedNamesComparedHeadsAndTests proves the operator
// reads the two compared heads and the tests that ran straight from the CLI
// output, not only from an internal struct.
func TestAxiSyncRecoverDivergedNamesComparedHeadsAndTests(t *testing.T) {
	f := newCLIRewrittenHeadRecoverFixture(t)

	out, err := executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("diverged recover should exit 1, got %#v\n%s", err, out)
	}
	// The preserved head's parent is the pipeline's rewrite of the
	// operator's own commit, so it carries the same change under a new
	// identity. The recovery brought the preserved head into the worktree.
	match := cliGit(t, f.local, "rev-parse", "--short", cliGit(t, f.local, "rev-parse", f.preserved+"^"))
	for _, want := range []string{
		"safety: blocked_recover_diverged",
		f.submitted,
		f.preserved,
		"the only tests it ran were Git ancestry",
		"git merge-base --is-ancestor",
		"git merge-tree --write-tree --merge-base",
		"the three-way merge conflicts in file.txt",
		"the same patch identity",
		match,
		"not a containment proof",
		"git log --oneline --left-right " + f.submitted + "..." + f.preserved,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diverged refusal missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("refused recovery moved the branch to %s, want %s", got, f.submitted)
	}
}

// TestSyncRecoverDivergedNamesComparedHeadsAndTests proves the human surface
// carries the same detail as the structured one.
func TestSyncRecoverDivergedNamesComparedHeadsAndTests(t *testing.T) {
	f := newCLIRewrittenHeadRecoverFixture(t)

	out, err := executeCmd("sync", "--recover", "--yes")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("diverged recover should exit 1, got %#v\n%s", err, out)
	}
	for _, want := range []string{
		"blocked:",
		f.submitted,
		f.preserved,
		"the only tests it ran were Git ancestry",
		"git merge-tree --write-tree --merge-base",
		"the three-way merge conflicts in file.txt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diverged refusal missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("refused recovery moved the branch to %s, want %s", got, f.submitted)
	}
}
