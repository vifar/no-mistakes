package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestAttributionInternalGitFailureDispatch(t *testing.T) {
	repo := t.TempDir()
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	invoke("init")
	invoke("config", "user.name", "Victor I")
	invoke("config", "user.email", "vifarinde@gmail.com")
	invoke("commit", "--allow-empty", "-m", "submitted fixture")
	head := invoke("rev-parse", "HEAD")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"post-checkout", "post-rewrite"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\nprintf 'original hook output\\n' >&2\nexit 37\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	invoke("config", "core.hooksPath", hooks)
	env, err := git.AttributionHookEnv(repo, os.Environ(), func(args ...string) (string, error) {
		return git.Run(context.Background(), repo, args...)
	}, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, "NM_TEST_ATTRIBUTION_DISPATCH=1")
	policy := filepath.Join(invoke("rev-parse", "--absolute-git-dir"), "no-mistakes-attribution", "policy.json")
	executable := filepath.Join(t.TempDir(), "no-mistakes")
	build := exec.Command("go", "build", "-p", "4", "-o", executable, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production dispatcher: %v %s", err, out)
	}
	dispatch := func(cwd string, input string, args ...string) (int, string) {
		t.Helper()
		cmd := exec.Command(executable, append([]string{"__sanitize-commit-message", policy}, args...)...)
		cmd.Dir, cmd.Env, cmd.Stdin = cwd, append(env, "GIT_CEILING_DIRECTORIES="+filepath.Dir(cwd)), strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("dispatcher execution: %v %s", err, out)
		}
		return exit.ExitCode(), string(out)
	}
	t.Run("internal_git_without_delegation", func(t *testing.T) {
		status, out := dispatch(t.TempDir(), "", "post-checkout", head, head, "1")
		if status != 1 {
			t.Errorf("internal Git failure returned %d, want policy status 1; %s", status, out)
		}
		if !strings.Contains(out, "git rev-parse") {
			t.Errorf("internal failure diagnostic lost: %q", out)
		}
	})
	t.Run("internal_replay_bookkeeping_with_failing_original_hook", func(t *testing.T) {
		recreated := invoke("commit-tree", head+"^{tree}", "-p", head, "-m", "submitted fixture")
		blocked := filepath.Join(filepath.Dir(policy), "replays")
		if err := os.WriteFile(blocked, []byte("blocked internal policy storage\n"), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(blocked)
		status, out := dispatch(repo, head+" "+recreated+"\n", "post-rewrite", "rebase")
		if status != 1 {
			t.Errorf("internal bookkeeping failure returned %d, want policy status 1; %s", status, out)
		}
		if !strings.Contains(out, "replays") {
			t.Errorf("internal bookkeeping diagnostic lost: %q", out)
		}
	})
	for _, status := range []int{2, 37} {
		t.Run(fmt.Sprintf("delegated_%d", status), func(t *testing.T) {
			expected := fmt.Sprintf("original stdout %d\noriginal stderr %d\n", status, status)
			script := fmt.Sprintf("#!/bin/sh\nprintf 'original stdout %d\\n'\nprintf 'original stderr %d\\n' >&2\nexit %d\n", status, status, status)
			if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			got, out := dispatch(repo, "", "post-checkout", head, head, "0")
			if got != status || out != expected {
				t.Errorf("delegated result = (%d,%q), want (%d,%q)", got, out, status, expected)
			}
		})
	}
}
