package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestAttributionProductionDispatchStatus(t *testing.T) {
	dir := t.TempDir()
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Git: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	invoke("init")
	invoke("config", "user.name", "Victor I")
	invoke("config", "user.email", "vifarinde@gmail.com")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("add", "tracked.txt")
	invoke("commit", "-m", "initial")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	invoke("config", "core.hooksPath", hooks)
	head := invoke("rev-parse", "HEAD")
	env, err := git.AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return git.Run(context.Background(), dir, args...) }, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, "NM_TEST_ATTRIBUTION_DISPATCH=1")
	cmd := exec.Command(realGit, "checkout", "--", "tracked.txt")
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("production hook failure swallowed: %s", out)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Errorf("production owned checkout = %v, want 37", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	metadata := invoke("rev-parse", "--absolute-git-dir")
	policy := filepath.Join(metadata, "no-mistakes-attribution", "policy.json")
	for _, tc := range []struct {
		name, policy string
		want         int
	}{{"delegated", policy, 37}, {"internal", filepath.Join(dir, "missing-policy"), 1}} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(executable, "__sanitize-commit-message", tc.policy, "post-checkout", head, head, "1")
			cmd.Dir, cmd.Env = dir, env
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("dispatch succeeded unexpectedly: %s", out)
			} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != tc.want {
				t.Errorf("production dispatcher = %v, want %d", err, tc.want)
			}
		})
	}
}
