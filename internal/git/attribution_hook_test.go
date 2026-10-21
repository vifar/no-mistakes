package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttributionHookCommitTime(t *testing.T) {
	t.Parallel()
	for _, noVerify := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "no-verify"}[noVerify], func(t *testing.T) {
			dir := initTestRepo(t)
			submitted, err := HeadSHA(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(t.TempDir(), "original-hooks")
			if err := os.MkdirAll(original, 0700); err != nil {
				t.Fatal(err)
			}
			hook := "#!/bin/sh\nprintf 'Co-Authored-By: Codex <ai@example.com>\\nSigned-off-by: Keeper <keeper@example.com>\\n' >> \"$1\"\n"
			for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
				if err := os.WriteFile(filepath.Join(original, name), []byte(hook), 0700); err != nil {
					t.Fatal(err)
				}
			}
			run(t, dir, "git", "config", "core.hooksPath", original)
			env, err := AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, submitted, "Originated-by: bespoke-bot")
			if err != nil {
				t.Fatal(err)
			}
			message := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: no-mistakes acp:cursor <ai@example.com>\nCo-Authored-By: Claude Smith <human@example.com>\nAssisted-by: rovodev:model\nCodex-Session: abc\nOriginated-by: bespoke-bot"
			file := filepath.Join(t.TempDir(), "message")
			if err := os.WriteFile(file, []byte(message), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"commit", "--allow-empty", "-F", file}
			if noVerify {
				args = append(args, "--no-verify")
			}
			cmd := exec.Command("git", args...)
			cmd.Dir, cmd.Env = dir, env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("commit: %v\n%s", err, out)
			}
			stored, err := Run(context.Background(), dir, "log", "-1", "--format=%B")
			if err != nil {
				t.Fatal(err)
			}
			want := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: Claude Smith <human@example.com>\nSigned-off-by: Keeper <keeper@example.com>"
			if !noVerify {
				want += "\nSigned-off-by: Keeper <keeper@example.com>"
			}
			if stored != want {
				t.Fatalf("stored message = %q, want %q", stored, want)
			}
			if got, err := Run(context.Background(), dir, "config", "core.hooksPath"); err != nil || got != original {
				t.Fatalf("persistent hooks = %q, %v", got, err)
			}
			if got, err := Run(context.Background(), dir, "rev-parse", "HEAD^"); err != nil || got != submitted {
				t.Fatalf("submitted parent changed: %q, %v", got, err)
			}
		})
	}
}

func TestValidateCommitAttributionRejectsBypassWithoutRewriting(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	before, err := HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "commit", "--allow-empty", "-m", "repair widget\n\nClaude-Session: abc")
	after, err := HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCommitAttribution(before, after, func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, before); err == nil || !strings.Contains(err.Error(), "refusing pipeline commit") {
		t.Fatalf("validation = %v", err)
	}
	if got, err := HeadSHA(context.Background(), dir); err != nil || got != after {
		t.Fatalf("validation rewrote history: %q, %v", got, err)
	}
	if err := ValidateCommitAttribution(after, after, func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, before); err != nil {
		t.Fatalf("submitted commit was inspected: %v", err)
	}
}

func TestAttributionBoundaryWorktreeHookCleanup(t *testing.T) {
	dir := initTestRepo(t)
	worktree := filepath.Join(t.TempDir(), "run")
	run(t, dir, "git", "worktree", "add", "--detach", worktree, "HEAD")
	gitRun := func(args ...string) (string, error) { return Run(context.Background(), worktree, args...) }
	head, err := gitRun("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	env, err := AttributionHookEnv(worktree, os.Environ(), gitRun, head)
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := RunWithEnv(context.Background(), worktree, env, "rev-parse", "--git-path", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hooks, "prepare-commit-msg")); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "worktree", "remove", "--force", worktree)
	if _, err := os.Stat(hooks); !os.IsNotExist(err) {
		t.Fatalf("hook resource outlived worktree: %v", err)
	}
}
