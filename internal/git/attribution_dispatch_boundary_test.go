package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestAttributionDelegatedDispatcherOutput(t *testing.T) {
	dir := initTestRepo(t)
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "config", "core.hooksPath", hooks)
	head := run(t, dir, "git", "rev-parse", "HEAD")
	for _, status := range []int{2, 37} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			script := fmt.Sprintf("#!/bin/sh\nprintf 'hook stdout %d\\n'\nprintf 'hook stderr %d\\n' >&2\nexit %d\n", status, status, status)
			if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			invoke := func(env []string) (int, string) {
				t.Helper()
				cmd := exec.Command("git", "hook", "run", "post-checkout", "--", head, head, "0")
				cmd.Dir, cmd.Env = dir, env
				out, err := cmd.CombinedOutput()
				if err == nil {
					return 0, string(out)
				}
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("hook execution: %v %s", err, out)
				}
				return exit.ExitCode(), string(out)
			}
			want, wantOutput := invoke(os.Environ())
			if want != status {
				t.Fatalf("native hook exit %d, want %d", want, status)
			}
			env, err := AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, head, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, gotOutput := invoke(env)
			if got != want || gotOutput != wantOutput {
				t.Errorf("delegated TestMain result = (%d,%q), native = (%d,%q)", got, gotOutput, want, wantOutput)
			}
		})
	}
}

func TestAttributionInternalRawExitError(t *testing.T) {
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	cmd := exec.Command(realGit, "rev-parse", "--absolute-git-dir")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(cwd))
	err = cmd.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 128 {
		t.Fatalf("internal Git fixture error = %v, want raw exit 128", err)
	}
	if got := AttributionHookResult(err, io.Discard); got != 1 {
		t.Errorf("raw internal subprocess error classified as delegated: got %d, want policy status 1", got)
	}
}

func TestAttributionInternalTestMainDispatch(t *testing.T) {
	dir := initTestRepo(t)
	head := run(t, dir, "git", "rev-parse", "HEAD")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-rewrite"), []byte("#!/bin/sh\nprintf 'original hook output\\n' >&2\nexit 37\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "config", "core.hooksPath", hooks)
	env, err := AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(run(t, dir, "git", "rev-parse", "--absolute-git-dir"), "no-mistakes-attribution", "policy.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, combined := range []bool{false, true} {
		t.Run(fmt.Sprint(combined), func(t *testing.T) {
			cwd, input := t.TempDir(), ""
			args := []string{"__sanitize-commit-message", policy, "post-checkout", head, head, "1"}
			want := "git rev-parse"
			if combined {
				cwd = dir
				recreated := run(t, dir, "git", "commit-tree", head+"^{tree}", "-p", head, "-m", "recreated")
				input = head + " " + recreated + "\n"
				args = []string{"__sanitize-commit-message", policy, "post-rewrite", "rebase"}
				want = "replays"
				if err := os.WriteFile(filepath.Join(filepath.Dir(policy), "replays"), []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(executable, args...)
			cmd.Dir, cmd.Env, cmd.Stdin = cwd, append(env, "GIT_CEILING_DIRECTORIES="+filepath.Dir(cwd)), strings.NewReader(input)
			out, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 || !strings.Contains(string(out), want) {
				t.Errorf("internal TestMain failure = (%v,%q), want status 1 and %q diagnostic", err, out, want)
			}
			if combined && !strings.Contains(string(out), "original hook output") {
				t.Errorf("original hook output lost: %q", out)
			}
		})
	}
}
