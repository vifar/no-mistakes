package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterWorktreeSubmodulesMatchesGitInitialization(t *testing.T) {
	ctx := context.Background()
	for _, remote := range []string{"https://example.com/team/project.git", "ssh://git@example.com/team/project.git", "git@example.com:team/project.git", "git@example.com:project.git", "/srv/team/project.git", "../team/project.git"} {
		for _, relative := range []string{"../module.git", "./module.git"} {
			t.Run(remote+"/"+relative, func(t *testing.T) {
				bare, wt := issue131Gate(t)
				module := initTestRepo(t)
				addSubmodule(t, wt, "dependency", module)
				runGitOrFatal(t, wt, "config", "--file", ".gitmodules", "submodule.dependency.url", relative)
				runGitOrFatal(t, wt, "config", "--file", ".gitmodules", "submodule.dependency.update", "rebase")
				runGitOrFatal(t, wt, "config", "--worktree", "remote.origin.url", remote)
				runGitOrFatal(t, wt, "config", "--local", "--remove-section", "submodule.dependency")
				runGitOrFatal(t, wt, "submodule", "init")
				want := map[string]string{}
				for _, field := range []string{"url", "update"} {
					key := "submodule.dependency." + field
					want[key] = runGitOrFatal(t, wt, "config", "--get", key)
				}
				runGitOrFatal(t, wt, "config", "--local", "--remove-section", "submodule.dependency")
				if err := RecordSharedConfigBaseline(ctx, bare); err != nil {
					t.Fatal(err)
				}
				guard, err := ProtectSharedConfig(ctx, bare)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
				if err := RegisterWorktreeSubmodules(ctx, wt); err != nil {
					t.Fatal(err)
				}
				for key, value := range want {
					if got := runGitOrFatal(t, wt, "config", "--worktree", "--get", key); got != value {
						t.Fatalf("%s = %q, Git init produced %q", key, got, value)
					}
				}
				if err := guard.Check(ctx); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRegisterWorktreeSubmodulesPreservesOverridesAndActiveSelection(t *testing.T) {
	ctx := context.Background()
	bare, wt := issue131Gate(t)
	module := initTestRepo(t)
	addSubmodule(t, wt, "dependency", module)
	runGitOrFatal(t, wt, "submodule", "deinit", "--force", "dependency")
	runGitOrFatal(t, wt, "config", "--worktree", "submodule.dependency.url", module)
	runGitOrFatal(t, wt, "config", "--worktree", "submodule.dependency.update", "none")
	runGitOrFatal(t, wt, "config", "--worktree", "submodule.active", "other")
	if err := RecordSharedConfigBaseline(ctx, bare); err != nil {
		t.Fatal(err)
	}
	guard, err := ProtectSharedConfig(ctx, bare)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := RegisterWorktreeSubmodules(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, wt, "submodule", "update", "--init", "--recursive"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "dependency", ".git")); !os.IsNotExist(err) {
		t.Fatalf("inactive dependency checkout: %v", err)
	}
	for key, want := range map[string]string{"submodule.dependency.url": module, "submodule.dependency.update": "none", "submodule.active": "other"} {
		if got := strings.TrimSpace(runGitOrFatal(t, wt, "config", "--worktree", "--get", key)); got != want {
			t.Fatalf("override %s = %q, want %q", key, got, want)
		}
	}
	if err := guard.Check(ctx); err != nil {
		t.Fatal(err)
	}
}
