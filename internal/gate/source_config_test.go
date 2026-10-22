package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Issue #183: a run started from a linked worktree of a registered working
// checkout must leave the source repository's shared .git/config byte-identical.
//
// The failure this covers was seen on 2026-10-22 on a workstation whose
// no-mistakes install already carried the #121/#131 fixes (#169): the source
// checkout's .git/config was rewritten with core.bare=true while a run was
// active from a linked worktree of that repository. A working checkout whose
// config says core.bare=true stops being a work tree, and every tool that
// resolves the repository from that checkout - the editor's VCS driver, for
// one - fails until an operator repairs the file by hand.
//
// The environment is part of the reproduction. A T3 lane, a Git hook, and an
// agent all run with GIT_DIR/GIT_COMMON_DIR exported, naming the repository
// they are working in. A Git command that inherits those variables operates on
// THAT repository instead of the directory the caller named, so a configuration
// write aimed at a run worktree lands in the source checkout's shared config.
// The fix therefore has to hold the whole path together: the registration, the
// push to the gate, the run's worktree setup, the rebase, and the branch
// recovery that moves the source checkout's own branch.
//
// The assertion is byte identity, not "core.bare is still false": no key may be
// added, removed, or rewritten in the source checkout's shared configuration.
func TestIssue183RunFromLinkedWorktreeLeavesSourceConfigByteIdentical(t *testing.T) {
	ctx := context.Background()
	work := setupTestRepo(t)
	// A registered submodule, so the preparation step's submodule
	// registration has a key to write. A write that REWRITES an existing
	// value would be invisible here; a write that ADDS a key is not.
	if err := os.WriteFile(filepath.Join(work, ".gitmodules"), []byte("[submodule \"dependency\"]\n\tpath = dependency\n\turl = https://example.com/dependency.git\n\tupdate = none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", work, "add", ".gitmodules").CombinedOutput(); err != nil {
		t.Fatalf("stage .gitmodules: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "commit", "-m", "add submodule").CombinedOutput(); err != nil {
		t.Fatalf("commit .gitmodules: %v: %s", err, out)
	}
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)

	repo, _, err := Init(ctx, d, p, work)
	if err != nil {
		t.Fatalf("register the working checkout: %v", err)
	}
	gateDir := p.RepoDir(repo.ID)
	sourceConfig := filepath.Join(work, ".git", "config")
	before, err := os.ReadFile(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}

	// The linked worktree a T3 lane creates for this repository.
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", work, "worktree", "add", "-b", "feature", linked).CombinedOutput(); err != nil {
		t.Fatalf("create linked worktree: %v: %s", err, out)
	}

	// The environment a T3 lane, a Git hook, or an agent hands to a child
	// process: the repository it is working in, named in the environment.
	t.Setenv("GIT_DIR", filepath.Join(work, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(work, ".git"))

	// 1. Push the branch to the gate. The gate's receive hooks are removed
	//    first: in a test binary they would resolve back to this binary and
	//    run the whole suite, which is not what this fixture is about.
	for _, hook := range []string{"pre-receive", "post-receive"} {
		if err := os.Remove(filepath.Join(gateDir, "hooks", hook)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove the gate %s hook: %v", hook, err)
		}
	}
	head, err := gitpkg.Run(ctx, linked, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitpkg.Run(ctx, work, "push", gateDir, "HEAD:refs/heads/feature"); err != nil {
		t.Fatalf("push to the gate: %v", err)
	}

	// 2. Run start: the gate hands the run a worktree, and the run's identity
	//    is copied into it.
	runWorktree := filepath.Join(t.TempDir(), "run")
	if err := gitpkg.WorktreeAdd(ctx, gateDir, runWorktree, head); err != nil {
		t.Fatalf("create the run worktree: %v", err)
	}
	if err := gitpkg.CopyLocalUserIdentity(ctx, work, runWorktree); err != nil {
		t.Fatalf("copy the local identity into the run worktree: %v", err)
	}

	// 3. Preparation: the submodule registration the Prepare step performs
	//    in the run worktree.
	if err := gitpkg.RegisterWorktreeSubmodules(ctx, runWorktree); err != nil {
		t.Fatalf("register the run worktree's submodules: %v", err)
	}

	// 3. Gate worktree setup: the gate's hook isolation and its shared-config
	//    baseline.
	if _, err := gitpkg.EnsureHooksPathIsolation(ctx, gateDir); err != nil {
		t.Fatalf("isolate the gate hooks path: %v", err)
	}
	if err := gitpkg.RecordSharedConfigBaseline(ctx, gateDir); err != nil {
		t.Fatalf("record the gate shared-config baseline: %v", err)
	}

	// 4. The rebase step runs in the run worktree.
	if _, err := gitpkg.Run(ctx, runWorktree, "fetch", "--no-tags", gateDir, "+refs/heads/feature:refs/remotes/gate/feature"); err != nil {
		t.Fatalf("fetch the gate branch into the run worktree: %v", err)
	}
	if _, err := gitpkg.Run(ctx, runWorktree, "rebase", "refs/remotes/gate/feature"); err != nil {
		t.Fatalf("rebase in the run worktree: %v", err)
	}

	// 5. The recovery that moves the source checkout's own branch to the
	//    pipeline-pushed head. This is the one step that operates on the
	//    source checkout, and it is a shared-ref and worktree write.
	if _, err := gitpkg.Run(ctx, work, "update-ref", "refs/heads/feature", head); err != nil {
		t.Fatalf("move the source branch: %v", err)
	}
	if _, err := gitpkg.Run(ctx, work, "reset", "--hard", head); err != nil {
		t.Fatalf("reset the source checkout: %v", err)
	}

	after, err := os.ReadFile(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the run rewrote the source checkout's shared configuration\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}
