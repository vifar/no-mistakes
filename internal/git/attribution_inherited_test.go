package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttributionInheritedFixturePolicy(t *testing.T) {
	if os.Getenv("NM_ATTRIBUTION_FIXTURE_CHILD") == "1" {
		dir := initTestRepo(t)
		const message = "existing fixture commit\n\nCodex-Session: fixture-data"
		run(t, dir, "git", "commit", "--allow-empty", "-m", message)
		if got := run(t, dir, "git", "log", "-1", "--format=%B"); got != message {
			t.Fatalf("outer policy changed unrelated fixture: %q", got)
		}
		hooks := filepath.Join(t.TempDir(), "hooks")
		if err := os.Mkdir(hooks, 0700); err != nil {
			t.Fatal(err)
		}
		hook := "#!/bin/sh\nprintf '\\nSigned-off-by: Fixture Person <fixture@example.com>\\nCodex-Session: own-hook\\n' >> \"$1\"\nprintf 'ran\\n' >> \"$0.ran\"\n"
		if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(hook), 0700); err != nil {
			t.Fatal(err)
		}
		run(t, dir, "git", "config", "core.hooksPath", hooks)
		before := run(t, dir, "git", "rev-parse", "HEAD")
		env, err := AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, before, nil)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "commit", "--allow-empty", "--no-verify", "-m", "own fixture correction\n\nCodex-Session: new")
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("own policy commit: %v\n%s", err, out)
		}
		if got := run(t, dir, "git", "log", "-1", "--format=%B"); got != "own fixture correction\n\nSigned-off-by: Fixture Person <fixture@example.com>" {
			t.Fatalf("own policy message = %q", got)
		}
		if data, err := os.ReadFile(filepath.Join(hooks, "prepare-commit-msg.ran")); err != nil || string(data) != "ran\n" {
			t.Fatalf("own hook effect = %q, %v", data, err)
		}
		return
	}
	dir := initTestRepo(t)
	head := run(t, dir, "git", "rev-parse", "HEAD")
	env, err := AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "owned correction\n\nCodex-Session: new")
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("owned commit: %v\n%s", err, out)
	}
	if got := run(t, dir, "git", "log", "-1", "--format=%B"); got != "owned correction" {
		t.Fatalf("owned correction escaped filtering: %q", got)
	}
	pkg, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("go", "test", "-p", "4", "-vet=off", "./internal/git", "-run", "^TestAttributionInheritedFixturePolicy$", "-count=1", "-v", "-timeout", "90s")
	child.Dir = filepath.Join(pkg, "../..")
	child.Env = append(env, "NM_ATTRIBUTION_FIXTURE_CHILD=1")
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("child go test under actual policy environment: %v\n%s", err, out)
	}
}

func TestAttributionInheritedHookPrecedence(t *testing.T) {
	for _, channel := range []string{"process", "explicit"} {
		t.Run(channel, func(t *testing.T) {
			dir := initTestRepo(t)
			hooks := filepath.Join(t.TempDir(), "hooks")
			if err := os.Mkdir(hooks, 0700); err != nil {
				t.Fatal(err)
			}
			hook := "#!/bin/sh\nprintf '\\nSigned-off-by: Original Person <original@example.com>\\nCodex-Session: inherited\\n' >> \"$1\"\nprintf 'ran\\n' >> \"$0.ran\"\n"
			if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(hook), 0700); err != nil {
				t.Fatal(err)
			}
			parameters := "'core.hooksPath=" + hooks + "' 'issue80.retained=original' 'issue80.precedence=old'"
			var input []string
			if channel == "process" {
				t.Setenv("GIT_CONFIG_PARAMETERS", parameters)
				input = os.Environ()
			} else {
				input = append(os.Environ(), "GIT_CONFIG_PARAMETERS="+parameters)
			}
			head := run(t, dir, "git", "rev-parse", "HEAD")
			env, err := AttributionHookEnv(dir, input, func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, head, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"issue80.retained", "issue80.precedence"} {
				got, err := RunWithEnv(context.Background(), dir, env, "config", "--get", key)
				want := "original"
				if strings.HasSuffix(key, "precedence") {
					want = "old"
				}
				if err != nil || got != want {
					t.Fatalf("inherited config %s = %q, %v", key, got, err)
				}
			}
			got, err := RunWithEnv(context.Background(), dir, env, "-c", "issue80.precedence=new", "config", "--get", "issue80.precedence")
			if err != nil || got != "new" {
				t.Fatalf("command precedence = %q, %v", got, err)
			}
			cmd := exec.Command("git", "commit", "--allow-empty", "--no-verify", "-m", "owned correction\n\nCodex-Session: new")
			cmd.Dir, cmd.Env = dir, env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("commit with inherited config: %v\n%s", err, out)
			}
			if got := run(t, dir, "git", "log", "-1", "--format=%B"); got != "owned correction\n\nSigned-off-by: Original Person <original@example.com>" {
				t.Fatalf("stored message = %q", got)
			}
			if data, err := os.ReadFile(filepath.Join(hooks, "prepare-commit-msg.ran")); err != nil || string(data) != "ran\n" {
				t.Fatalf("original hook side effect = %q, %v", data, err)
			}
		})
	}
}
