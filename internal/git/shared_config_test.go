package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #183: no-mistakes must never write the shared configuration of a
// repository it does not own.
//
// A shared configuration write is inherited by every other worktree of the
// same repository, so a key written into a registered checkout's .git/config
// reaches every tool that reads that checkout. core.bare=true is the worst
// case: it does not add a key, it REMOVES the checkout's working tree, and
// every tool that resolves the repository from the checkout then fails
// ("Failed to resolve the VCS driver") until an operator repairs the file by
// hand.

// issue183Checkout creates a normal working checkout with a commit on it.
func issue183Checkout(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init checkout: %v: %s", err, out)
	}
	runGitOrFatal(t, work, "config", "user.email", "t@t.com")
	runGitOrFatal(t, work, "config", "user.name", "T")
	runGitOrFatal(t, work, "commit", "--allow-empty", "-m", "init")
	return work
}

func issue183SharedConfig(t *testing.T, work string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(work, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A working checkout has no per-worktree configuration, so a per-worktree
// write would land in its shared configuration. The write is refused, and the
// shared configuration is left exactly as it was.
func TestIssue183SetWorktreeConfigRefusesACheckoutsSharedConfiguration(t *testing.T) {
	ctx := context.Background()
	work := issue183Checkout(t)
	before := issue183SharedConfig(t, work)

	err := SetWorktreeConfig(ctx, work, "submodule.dependency.url", "https://example.com/dependency.git")
	if err == nil {
		t.Fatal("writing per-worktree configuration into a checkout's shared configuration must be refused")
	}
	if !strings.Contains(err.Error(), "shared configuration") {
		t.Fatalf("refusal does not name the shared configuration: %v", err)
	}
	if after := issue183SharedConfig(t, work); after != before {
		t.Fatalf("the refused write changed the checkout's shared configuration\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// The same holds for a linked worktree of a checkout: it shares the
// checkout's configuration, so a per-worktree write there is a write into the
// checkout's shared configuration too.
func TestIssue183SetWorktreeConfigRefusesALinkedWorktreesSharedConfiguration(t *testing.T) {
	ctx := context.Background()
	work := issue183Checkout(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", work, "worktree", "add", "-b", "feature", linked).CombinedOutput(); err != nil {
		t.Fatalf("create linked worktree: %v: %s", err, out)
	}
	before := issue183SharedConfig(t, work)

	if err := SetWorktreeConfig(ctx, linked, "user.name", "Someone Else"); err == nil {
		t.Fatal("writing per-worktree configuration into a linked worktree's shared configuration must be refused")
	}
	if after := issue183SharedConfig(t, work); after != before {
		t.Fatalf("the refused write changed the checkout's shared configuration\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// core.bare belongs to a gate's per-worktree configuration and to nothing
// else. Without per-worktree scope in force, writing it would put
// core.bare=true into the shared configuration, where every worktree of the
// repository inherits it.
func TestIssue183RelocateCoreBareRefusesWithoutPerWorktreeScope(t *testing.T) {
	ctx := context.Background()
	work := issue183Checkout(t)
	before := issue183SharedConfig(t, work)

	err := relocateCoreBareToWorktreeScope(ctx, filepath.Join(work, ".git"))
	if err == nil {
		t.Fatal("relocating core.bare without per-worktree scope must be refused")
	}
	if after := issue183SharedConfig(t, work); after != before {
		t.Fatalf("the refused relocation changed the checkout's shared configuration\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	if strings.Contains(issue183SharedConfig(t, work), "bare = true") {
		t.Fatal("core.bare=true reached the checkout's shared configuration")
	}
}

// A Git command runs against the directory its caller named. An inherited
// GIT_DIR/GIT_COMMON_DIR names a DIFFERENT repository, and a command that
// honors it reads and writes that repository instead - which is how a run's
// configuration write reached a registered checkout it was never aimed at.
func TestIssue183InheritedRepositoryEnvironmentCannotRedirectACommand(t *testing.T) {
	ctx := context.Background()
	work := issue183Checkout(t)
	elsewhere := issue183Checkout(t)
	// The two repositories answer differently, so a redirected command is
	// visible in the answer.
	runGitOrFatal(t, elsewhere, "config", "user.name", "Somebody Else")
	before := issue183SharedConfig(t, elsewhere)

	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(elsewhere, ".git"))

	out, err := Run(ctx, work, "config", "--get", "user.name")
	if err != nil {
		t.Fatalf("read the named repository's configuration: %v", err)
	}
	if strings.TrimSpace(out) != "T" {
		t.Fatalf("the command answered for another repository: user.name = %q, want %q", strings.TrimSpace(out), "T")
	}
	if _, err := Run(ctx, work, "config", "--worktree", "core.bare", "true"); err != nil {
		t.Fatalf("write to the named repository: %v", err)
	}
	if after := issue183SharedConfig(t, elsewhere); after != before {
		t.Fatalf("an inherited GIT_DIR redirected the write into another repository\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestIssue183SetWorktreeConfigUsesGitBooleanSemantics(t *testing.T) {
	// Git's boolean contract enables worktree scope for yes, on, and 1.
	// A successful write must leave the shared file byte-identical and put
	// the requested value in config.worktree, regardless of that spelling.
	for _, value := range []string{"yes", "on", "1"} {
		t.Run(value, func(t *testing.T) {
			bare, wt := issue131Gate(t)
			config := filepath.Join(bare, "config")
			runGitOrFatal(t, bare, "config", "--file", config, "extensions.worktreeConfig", value)
			before, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetWorktreeConfig(context.Background(), wt, "user.name", "Gate Identity"); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("per-worktree write changed the shared configuration")
			}
			perWorktree := filepath.Join(bare, "worktrees", "run", "config.worktree")
			if got := strings.TrimSpace(runGitOrFatal(t, wt, "config", "--file", perWorktree, "--get", "user.name")); got != "Gate Identity" {
				t.Fatalf("per-worktree identity = %q, want Gate Identity", got)
			}
		})
	}
}

// The gate is the one repository whose shared configuration no-mistakes owns,
// so a per-worktree write there still lands in the gate's per-worktree
// configuration and the gate's shared configuration stays untouched.
func TestIssue183GatePerWorktreeWriteStaysOutOfTheSharedConfiguration(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	seedGate(t, base, bare)
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(base, "run")
	if err := WorktreeAdd(ctx, bare, wt, "main"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(bare, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetWorktreeConfig(ctx, wt, "user.name", "Gate Identity"); err != nil {
		t.Fatalf("write the gate's per-worktree identity: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(bare, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the gate's shared configuration changed\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	perWorktree := filepath.Join(bare, "worktrees", "run", "config.worktree")
	if _, err := os.Stat(perWorktree); err != nil {
		t.Fatalf("the identity did not land in the gate's per-worktree configuration: %v", err)
	}
}
