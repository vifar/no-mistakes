package main

import (
	"bytes"
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
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\nprintf 'original stdout\\n'\nprintf 'original stderr\\n' >&2\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	invoke("config", "core.hooksPath", hooks)
	head := invoke("rev-parse", "HEAD")
	// Native Git is the output oracle, including its hook stream routing.
	native := exec.Command(realGit, "checkout", "--", "tracked.txt")
	native.Dir = dir
	var nativeStdout, nativeStderr bytes.Buffer
	native.Stdout, native.Stderr = &nativeStdout, &nativeStderr
	if err := native.Run(); err == nil {
		t.Fatal("native hook failure swallowed")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Fatalf("native checkout = %v, want 37", err)
	}
	env, err := git.AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return git.Run(context.Background(), dir, args...) }, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, "NM_TEST_ATTRIBUTION_DISPATCH=1")
	cmd := exec.Command(realGit, "checkout", "--", "tracked.txt")
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("production hook failure swallowed: stdout %q, stderr %q", stdout.String(), stderr.String())
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Errorf("production owned checkout = %v, want 37", err)
	}
	if stdout.String() != nativeStdout.String() || stderr.String() != nativeStderr.String() {
		t.Errorf("production owned checkout changed native output: stdout %q, stderr %q; want stdout %q, stderr %q", stdout.String(), stderr.String(), nativeStdout.String(), nativeStderr.String())
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
			} else if tc.name == "internal" && !strings.Contains(string(out), "missing-policy") {
				t.Errorf("internal policy failure lost its diagnostic: %q", out)
			} else if tc.name == "delegated" && string(out) != "original stdout\noriginal stderr\n" {
				t.Errorf("delegated hook output changed: %q", out)
			}
		})
	}
}
