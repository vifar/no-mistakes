package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttributionRepositoryOwnership(t *testing.T) {
	for _, channel := range []string{"parameters", "count"} {
		for _, location := range []string{"separate", "nested", "linked"} {
			for _, custom := range []bool{false, true} {
				for _, noVerify := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/custom=%v/noVerify=%v", channel, location, custom, noVerify), func(t *testing.T) {
						owner := initTestRepo(t)
						foreign := initTestRepo(t)
						if location == "nested" {
							source := foreign
							foreign = filepath.Join(owner, "nested")
							run(t, owner, "git", "clone", source, foreign)
						} else if location == "linked" {
							foreign = filepath.Join(t.TempDir(), "linked")
							run(t, owner, "git", "worktree", "add", "--detach", foreign, "HEAD")
						}
						run(t, foreign, "git", "config", "user.name", "Victor I")
						run(t, foreign, "git", "config", "user.email", "vifarinde@gmail.com")
						hooks := run(t, foreign, "git", "rev-parse", "--git-path", "hooks")
						if !filepath.IsAbs(hooks) {
							hooks = filepath.Join(foreign, hooks)
						}
						if custom {
							hooks = filepath.Join(t.TempDir(), "foreign-hooks")
							run(t, foreign, "git", "config", "core.hooksPath", hooks)
						}
						if err := os.MkdirAll(hooks, 0700); err != nil {
							t.Fatal(err)
						}
						effects := filepath.Join(t.TempDir(), "effects")
						for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-rewrite"} {
							script := fmt.Sprintf("#!/bin/sh\nprintf '%s\\n' >> '%s'\n", name, effects)
							if name == "prepare-commit-msg" {
								script += "printf '\\nSigned-off-by: Foreign Person <foreign@example.com>\\nCodex-Session: own-hook\\n' >> \"$1\"\n"
							}
							if name == "post-rewrite" {
								script += "cat >> '" + effects + "'\n"
							}
							if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0700); err != nil {
								t.Fatal(err)
							}
						}
						input := os.Environ()
						if channel == "count" {
							input = append(input, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=issue80.retained", "GIT_CONFIG_VALUE_0=kept")
						} else {
							input = append(input, "GIT_CONFIG_PARAMETERS='issue80.retained=kept'")
						}
						head := run(t, owner, "git", "rev-parse", "HEAD")
						env, err := AttributionHookEnv(owner, input, func(args ...string) (string, error) { return Run(context.Background(), owner, args...) }, head, nil)
						if err != nil {
							t.Fatal(err)
						}
						invoke := func(dir string, args ...string) (string, error) {
							cmd := exec.Command("git", args...)
							cmd.Dir, cmd.Env = dir, env
							out, err := cmd.CombinedOutput()
							return string(out), err
						}
						const message = "foreign fixture  \n\nCodex-Session: fixture\n\n"
						args := []string{"commit", "--allow-empty", "-m", message}
						if noVerify {
							args = append(args, "--no-verify")
						}
						if out, err := invoke(foreign, args...); err != nil {
							t.Fatalf("foreign commit: %v\n%s", err, out)
						}
						got := run(t, foreign, "git", "log", "-1", "--format=%B")
						want := "foreign fixture\n\nCodex-Session: fixture\n\nSigned-off-by: Foreign Person <foreign@example.com>\nCodex-Session: own-hook"
						if got != want {
							t.Errorf("foreign message = %q, want %q", got, want)
						}
						rawCmd := exec.Command("git", "cat-file", "commit", "HEAD")
						rawCmd.Dir = foreign
						raw, err := rawCmd.Output()
						if err != nil {
							t.Fatal(err)
						}
						_, stored, ok := bytes.Cut(raw, []byte("\n\n"))
						if !ok || string(stored) != want+"\n" {
							t.Fatalf("raw foreign message = %q", stored)
						}
						data, err := os.ReadFile(effects)
						if err != nil {
							t.Fatal(err)
						}
						wantEffects := "pre-commit\nprepare-commit-msg\ncommit-msg\npost-commit\n"
						if noVerify {
							wantEffects = "prepare-commit-msg\npost-commit\n"
						}
						if string(data) != wantEffects {
							t.Errorf("foreign hooks = %q, want %q", data, wantEffects)
						}
						stdin := filepath.Join(t.TempDir(), "rewrite-input")
						if err := os.WriteFile(stdin, []byte("old new\n"), 0600); err != nil {
							t.Fatal(err)
						}
						cmd := exec.Command("git", "hook", "run", "--to-stdin="+stdin, "post-rewrite", "--", "amend")
						cmd.Dir, cmd.Env = foreign, env
						if out, err := cmd.CombinedOutput(); err != nil {
							t.Fatalf("rewrite: %v %s", err, out)
						}
						data, err = os.ReadFile(effects)
						if err != nil || !strings.HasSuffix(string(data), "post-rewrite\nold new\n") {
							t.Errorf("rewrite stdin = %q, %v", data, err)
						}
						run(t, foreign, "git", "config", "commit.cleanup", "strip")
						if got, err := invoke(foreign, "config", "--get", "commit.cleanup"); err != nil || strings.TrimSpace(got) != "strip" {
							t.Errorf("foreign cleanup = %q %v", got, err)
						}
						sub := filepath.Join(owner, "sub")
						if err := os.Mkdir(sub, 0700); err != nil {
							t.Fatal(err)
						}
						if out, err := invoke(sub, "commit", "--allow-empty", "--no-verify", "-m", "own correction\n\nCodex-Session: new"); err != nil {
							t.Fatalf("owner commit: %v %s", err, out)
						}
						if got := run(t, owner, "git", "log", "-1", "--format=%B"); strings.Contains(got, "Codex-Session:") {
							t.Errorf("owner escaped: %q", got)
						}
						if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 37\n"), 0700); err != nil {
							t.Fatal(err)
						}
						if _, err := invoke(foreign, "hook", "run", "pre-commit"); err == nil {
							t.Fatal("foreign failure swallowed")
						} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
							t.Fatalf("foreign hook status: %v", err)
						}
					})
				}
			}
		}
	}
}

func TestAttributionOwnerIdentityControls(t *testing.T) {
	for _, location := range []string{"linked", "symlink", "glob"} {
		for _, channel := range []string{"parameters", "count"} {
			t.Run(location+"/"+channel, func(t *testing.T) {
				base := initTestRepo(t)
				dir := base
				switch location {
				case "linked":
					dir = filepath.Join(t.TempDir(), "owned")
					run(t, base, "git", "worktree", "add", "--detach", dir, "HEAD")
				case "symlink":
					dir = filepath.Join(t.TempDir(), "alias")
					if err := os.Symlink(base, dir); err != nil {
						t.Skip(err)
					}
				case "glob":
					dir = filepath.Join(t.TempDir(), "owner[1]*?")
					run(t, base, "git", "clone", base, dir)
				}
				run(t, dir, "git", "config", "user.name", "Victor I")
				run(t, dir, "git", "config", "user.email", "vifarinde@gmail.com")
				hooks := filepath.Join(t.TempDir(), "original")
				if err := os.Mkdir(hooks, 0700); err != nil {
					t.Fatal(err)
				}
				side := filepath.Join(hooks, "effects")
				for _, name := range []string{"prepare-commit-msg", "post-rewrite", "pre-commit"} {
					script := "#!/bin/sh\n"
					switch name {
					case "prepare-commit-msg":
						script += "printf 'ran\\n' >> '" + side + "'\nprintf '\\nSigned-off-by: Keeper <keeper@example.com>\\nCodex-Session: original\\n' >> \"$1\"\n"
					case "post-rewrite":
						script += "cat >> '" + side + "'\n"
					case "pre-commit":
						script += "exit 37\n"
					}
					if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				input := os.Environ()
				if channel == "count" {
					input = append(input, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+hooks)
				} else {
					input = append(input, "GIT_CONFIG_PARAMETERS='core.hooksPath="+hooks+"'")
				}
				head := run(t, dir, "git", "rev-parse", "HEAD")
				env, err := AttributionHookEnv(dir, input, func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, head, nil)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("git", "commit", "--allow-empty", "--no-verify", "-m", "own correction\n\nCodex-Session: new")
				cmd.Dir, cmd.Env = dir, env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("owned commit: %v %s", err, out)
				}
				if got := run(t, dir, "git", "log", "-1", "--format=%B"); got != "own correction\n\nSigned-off-by: Keeper <keeper@example.com>" {
					t.Fatalf("owned message = %q", got)
				}
				stdin := filepath.Join(t.TempDir(), "rewrite-input")
				if err := os.WriteFile(stdin, []byte("old new\n"), 0600); err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command("git", "hook", "run", "--to-stdin="+stdin, "post-rewrite", "--", "amend")
				cmd.Dir, cmd.Env = dir, env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("owned stdin: %v %s", err, out)
				}
				if data, err := os.ReadFile(side); err != nil || string(data) != "ran\nold new\n" {
					t.Fatalf("owned hook effects = %q %v", data, err)
				}
				before := run(t, dir, "git", "rev-parse", "HEAD")
				cmd = exec.Command("git", "commit", "--allow-empty", "-m", "must refuse")
				cmd.Dir, cmd.Env = dir, env
				if out, err := cmd.CombinedOutput(); err == nil {
					t.Fatalf("owned hook failure swallowed: %s", out)
				}
				if got := run(t, dir, "git", "rev-parse", "HEAD"); got != before {
					t.Fatal("failed owned hook advanced head")
				}
			})
		}
	}
}
